/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
)

// connectionRefIndex indexes HarborProject by the connection it names, so
// that a change to a HarborConnection can enqueue the projects using it.
const connectionRefIndex = "spec.connectionRef.name"

// HarborProjectReconciler reconciles a HarborProject object
type HarborProjectReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Namespace is the only namespace credentials are read from. The reference
	// types deliberately carry no namespace field, so this is what stops a
	// resource author from pointing the controller at an arbitrary Secret.
	Namespace string

	// NewClient builds a Harbor client. Tests replace it with a fake.
	NewClient func(harbor.Options) (harbor.Client, error)
}

// statusError carries the condition reason to report for a failure.
type statusError struct {
	reason string
	// terminal marks a failure that retrying cannot fix: the spec has to
	// change first.
	terminal bool
	// backoff marks a failure that has a reason of its own but is no state
	// of the world to wait out: it is retried with backoff like an
	// unclassified error.
	backoff bool
	err     error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func reasoned(reason, format string, args ...any) error {
	return &statusError{reason: reason, err: fmt.Errorf(format, args...)}
}

func terminal(reason, format string, args ...any) error {
	return &statusError{reason: reason, terminal: true, err: fmt.Errorf(format, args...)}
}

func failed(reason, format string, args ...any) error {
	return &statusError{reason: reason, backoff: true, err: fmt.Errorf(format, args...)}
}

// reasonFor picks the condition reason for err. Anything unclassified is
// reported as a Harbor error, which the caller retries with backoff.
func reasonFor(err error) string {
	var se *statusError
	if errors.As(err, &se) {
		return se.reason
	}
	return harborv1alpha1.ReasonHarborError
}

// update on the resource itself is for the finalizer; the controller never
// creates or deletes a HarborProject.
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborprojects,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborprojects/status,verbs=update
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborconnections,verbs=get;list;watch
// Secrets and ConfigMaps are read uncached (see cmd/main.go), so get is
// sufficient. Scoped to the operator's own namespace: the reference types
// carry no namespace field, and this grant is the enforcement point for that.
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get,namespace=system
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile brings the Harbor project in line with the resource.
func (r *HarborProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var hp harborv1alpha1.HarborProject
	if err := r.Get(ctx, req.NamespacedName, &hp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !hp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &hp)
	}

	// Take the finalizer before touching Harbor and let the next reconcile do
	// the work. A project created first could otherwise be left behind if the
	// resource were deleted before the finalizer landed.
	if controllerutil.AddFinalizer(&hp, harborv1alpha1.HarborProjectFinalizer) {
		return ctrl.Result{}, r.Update(ctx, &hp)
	}

	return r.reconcileNormal(ctx, &hp)
}

func (r *HarborProjectReconciler) reconcileNormal(ctx context.Context, hp *harborv1alpha1.HarborProject) (ctrl.Result, error) {
	hc, syncPeriod, err := r.harborClientFor(ctx, hp)
	if err != nil {
		return r.report(ctx, hp, err, syncPeriod)
	}

	p, err := r.resolveProject(ctx, hc, hp)
	if err != nil {
		return r.report(ctx, hp, err, syncPeriod)
	}

	// previous is what the resource was bound to coming into this pass; what
	// happened to the binding is only announced once the status records it.
	previous, created := hp.Status.ProjectID, false
	if p == nil {
		p, err = r.createProject(ctx, hc, hp)
		if err != nil {
			return r.report(ctx, hp, err, syncPeriod)
		}
		created = true
	} else if hp.Spec.Public != nil && p.Public != *hp.Spec.Public {
		if err := hc.UpdateProject(ctx, p.ID, *hp.Spec.Public); err != nil {
			if errors.Is(err, harbor.ErrInaccessible) {
				// The project was just read, so it exists: this 403 is about
				// permission, which backing off does nothing for.
				err = reasoned(harborv1alpha1.ReasonProjectInaccessible,
					"Harbor project %q (ID %d) cannot be updated with these credentials", p.Name, p.ID)
			}
			return r.report(ctx, hp, err, syncPeriod)
		}
		p.Public = *hp.Spec.Public
	}

	hp.Status.ProjectID = &p.ID
	hp.Status.CreationPending = false
	hp.Status.RepositoryCount = &p.RepoCount
	meta.SetStatusCondition(&hp.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             harborv1alpha1.ReasonSynced,
		Message:            fmt.Sprintf("Harbor project %q (ID %d) is in sync", p.Name, p.ID),
		ObservedGeneration: hp.Generation,
	})
	hp.Status.ObservedGeneration = hp.Generation

	if err := r.updateStatus(ctx, hp); err != nil {
		return ctrl.Result{}, err
	}
	r.announce(hp, p, previous, created)
	// Harbor cannot be watched, so drift is only found by looking again.
	return ctrl.Result{RequeueAfter: syncPeriod}, nil
}

// announce records an event when the pass changed which project the resource
// is bound to. These are the moments the controller acts on Harbor in a way
// nobody typed out: it creates a project, brings back one that was deleted, or
// takes charge of one that was already there.
func (r *HarborProjectReconciler) announce(hp *harborv1alpha1.HarborProject, p *harbor.Project, previous *int64, created bool) {
	switch {
	case created && previous != nil:
		r.Recorder.Eventf(hp, nil, corev1.EventTypeNormal, harborv1alpha1.EventProjectRecreated, "CreateProject",
			"Created Harbor project %q again (ID %d): the one this resource was bound to (ID %d) was gone", p.Name, p.ID, *previous)
	case created:
		r.Recorder.Eventf(hp, nil, corev1.EventTypeNormal, harborv1alpha1.EventProjectCreated, "CreateProject",
			"Created Harbor project %q (ID %d)", p.Name, p.ID)
	case previous == nil || *previous != p.ID:
		r.Recorder.Eventf(hp, nil, corev1.EventTypeNormal, harborv1alpha1.EventProjectAdopted, "AdoptProject",
			"Bound to existing Harbor project %q (ID %d)", p.Name, p.ID)
	}
}

// createProject creates the project and reports the ID the client looked up
// afterwards.
//
// The intent is recorded first. Should the ID be lost between Harbor creating
// the project and the status naming it (a failed lookup, a failed write, a
// restart), the next pass finds a project under the name with nothing to say
// it is this resource's own, and without adoptExisting would refuse it.
func (r *HarborProjectReconciler) createProject(ctx context.Context, hc harbor.Client, hp *harborv1alpha1.HarborProject) (*harbor.Project, error) {
	public := hp.Spec.Public != nil && *hp.Spec.Public

	if !hp.Status.CreationPending {
		hp.Status.CreationPending = true
		if err := r.updateStatus(ctx, hp); err != nil {
			return nil, err
		}
	}

	id, err := hc.CreateProject(ctx, hp.Spec.ProjectName, public)
	if err != nil && createRefused(err) {
		// Harbor answered and created nothing. Leaving the mark would let a
		// project someone else creates later pass for this resource's own.
		hp.Status.CreationPending = false
	}
	switch {
	case err == nil:
	case errors.Is(err, harbor.ErrInvalidName):
		return nil, terminal(harborv1alpha1.ReasonInvalidProjectName,
			"Harbor rejected the project name %q: %v", hp.Spec.ProjectName, err)
	case errors.Is(err, harbor.ErrAlreadyExists):
		// The lookup by name came back empty, yet the name is taken. Project
		// names are unique across Harbor whether or not the caller may see
		// them, so this is the one answer that proves the project exists: it
		// is there and these credentials cannot list it. Should it instead
		// have been created since the lookup, the next pass finds it by name.
		return nil, reasoned(harborv1alpha1.ReasonProjectInaccessible,
			"Harbor project %q exists but these credentials cannot see it", hp.Spec.ProjectName)
	default:
		return nil, err
	}

	return &harbor.Project{ID: id, Name: hp.Spec.ProjectName, Public: public}, nil
}

// createRefused reports whether err is Harbor declining to create the project,
// as opposed to a failure that leaves open whether it was created.
func createRefused(err error) bool {
	if errors.Is(err, harbor.ErrIDUnresolved) {
		return false
	}
	if errors.Is(err, harbor.ErrInvalidName) || errors.Is(err, harbor.ErrAlreadyExists) {
		return true
	}
	var apiErr *harbor.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500
}

// resolveProject returns the project this resource is bound to, or nil when it
// has to be created.
func (r *HarborProjectReconciler) resolveProject(ctx context.Context, hc harbor.Client, hp *harborv1alpha1.HarborProject) (*harbor.Project, error) {
	if id := hp.Status.ProjectID; id != nil {
		p, err := hc.GetProjectByID(ctx, *id)
		switch {
		case err == nil:
			// Already bound. adoptExisting governs taking a project over, not
			// keeping one, so it is not re-evaluated here.
			return p, nil
		case errors.Is(err, harbor.ErrNotFound):
			// 404 settles it: the project is gone. Fall through and recreate.
		case errors.Is(err, harbor.ErrInaccessible):
			// 403 says nothing about whether the project exists, and neither
			// does an empty lookup by name: the list API leaves out what these
			// credentials may not see. Only creating it settles the question,
			// so an empty lookup goes on to try that.
			p, nameErr := hc.GetProjectByName(ctx, hp.Spec.ProjectName)
			switch {
			case nameErr == nil && p.ID == *id:
				// Listed but not readable by ID: still the bound project.
				return p, nil
			case nameErr == nil:
				// A different project holds the name now. The bound one is
				// gone, and this one is no more this resource's than any
				// other project it finds under its name.
				return takeOver(hp, p)
			case errors.Is(nameErr, harbor.ErrNotFound):
				return nil, nil
			default:
				return nil, nameErr
			}
		default:
			return nil, err
		}
	}

	p, err := hc.GetProjectByName(ctx, hp.Spec.ProjectName)
	switch {
	case err == nil:
		return takeOver(hp, p)
	case errors.Is(err, harbor.ErrNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

// takeOver decides whether hp may bind to p, a project it found under its name
// rather than one it is bound to.
func takeOver(hp *harborv1alpha1.HarborProject, p *harbor.Project) (*harbor.Project, error) {
	// A pending creation means the project is most likely the one this
	// resource asked for; see createProject.
	if !hp.Spec.AdoptExisting && !hp.Status.CreationPending {
		// Not terminal: the project may be removed from Harbor, which
		// nothing but looking again would reveal.
		return nil, reasoned(harborv1alpha1.ReasonAdoptionRefused,
			"Harbor project %q already exists; set adoptExisting to take it over", hp.Spec.ProjectName)
	}
	return p, nil
}

func (r *HarborProjectReconciler) reconcileDelete(ctx context.Context, hp *harborv1alpha1.HarborProject) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(hp, harborv1alpha1.HarborProjectFinalizer) {
		return ctrl.Result{}, nil
	}

	// A resource with no projectID was never bound to a project. Looking one up
	// by name here could delete a project this resource never owned.
	if hp.Spec.DeletionPolicy == harborv1alpha1.DeletionPolicyDelete && hp.Status.ProjectID != nil {
		if res, done, err := r.deleteProject(ctx, hp); !done {
			return res, err
		}
	}

	// Only now, with the external state settled, is it safe to let the resource
	// go: removing the finalizer first would orphan the project.
	controllerutil.RemoveFinalizer(hp, harborv1alpha1.HarborProjectFinalizer)
	return ctrl.Result{}, r.Update(ctx, hp)
}

// deleteProject removes the Harbor project. done reports whether the finalizer
// may be dropped; when it is false the caller returns res and err unchanged.
func (r *HarborProjectReconciler) deleteProject(ctx context.Context, hp *harborv1alpha1.HarborProject) (ctrl.Result, bool, error) {
	hc, syncPeriod, err := r.harborClientFor(ctx, hp)
	if err != nil {
		res, err := r.report(ctx, hp, err, syncPeriod)
		return res, false, err
	}

	err = hc.DeleteProject(ctx, *hp.Status.ProjectID)
	switch {
	case err == nil, errors.Is(err, harbor.ErrNotFound):
		return ctrl.Result{}, true, nil

	case errors.Is(err, harbor.ErrNotEmpty):
		// Deleting the repositories is a decision for a human. The resource
		// stays until they are gone; that is the intended behaviour.
		r.Recorder.Eventf(hp, nil, corev1.EventTypeWarning, harborv1alpha1.ReasonDeletionBlocked, "DeleteProject", "%s", err.Error())
		// The count on record is from the last sync and may predate the push
		// that blocks the deletion. Refreshing it is best effort: the block is
		// reported either way.
		if p, lookupErr := hc.GetProjectByID(ctx, *hp.Status.ProjectID); lookupErr == nil {
			hp.Status.RepositoryCount = &p.RepoCount
		}
		res, err := r.report(ctx, hp, reasoned(harborv1alpha1.ReasonDeletionBlocked,
			"Harbor project %q still contains repositories", hp.Spec.ProjectName), syncPeriod)
		return res, false, err

	case errors.Is(err, harbor.ErrInaccessible):
		_, lookupErr := hc.GetProjectByName(ctx, hp.Spec.ProjectName)
		switch {
		case lookupErr == nil:
			res, err := r.report(ctx, hp, reasoned(harborv1alpha1.ReasonProjectInaccessible,
				"Harbor project ID %d cannot be deleted with these credentials", *hp.Status.ProjectID), syncPeriod)
			return res, false, err

		case errors.Is(lookupErr, harbor.ErrNotFound):
			// Gone, or hidden from these credentials: Harbor says the same for
			// both, and unlike on the read path there is no creating the
			// project to find out. Holding the resource would hold it for good
			// in the ordinary case of a project that is simply gone, so it is
			// released, and the event says that nothing was confirmed.
			r.Recorder.Eventf(hp, nil, corev1.EventTypeWarning, harborv1alpha1.EventDeletionUnconfirmed, "DeleteProject",
				"Harbor project %q (ID %d) was not deleted: Harbor does not show it to these credentials, so it is already gone or they may not see it",
				hp.Spec.ProjectName, *hp.Status.ProjectID)
			return ctrl.Result{}, true, nil

		default:
			res, err := r.report(ctx, hp, lookupErr, syncPeriod)
			return res, false, err
		}

	default:
		return ctrl.Result{}, false, err
	}
}

// harborClientFor resolves the connection this resource names, along with the
// resync interval to use for it.
func (r *HarborProjectReconciler) harborClientFor(ctx context.Context, hp *harborv1alpha1.HarborProject) (harbor.Client, time.Duration, error) {
	var conn harborv1alpha1.HarborConnection
	if err := r.Get(ctx, types.NamespacedName{Name: hp.Spec.ConnectionRef.Name}, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, defaultSyncPeriod, reasoned(harborv1alpha1.ReasonConnectionUnavailable,
				"HarborConnection %q not found", hp.Spec.ConnectionRef.Name)
		}
		return nil, defaultSyncPeriod, failed(harborv1alpha1.ReasonInternalError,
			"read HarborConnection %q: %v", hp.Spec.ConnectionRef.Name, err)
	}

	syncPeriod := syncPeriodOf(&conn)

	// Checked before the credentials are even read: a resource the connection
	// does not cover must not reach Harbor on any path, deletion included.
	if !allows(&conn, hp.Namespace, hp.Spec.ProjectName) {
		return nil, syncPeriod, reasoned(harborv1alpha1.ReasonNotAllowed,
			"HarborConnection %q does not allow namespace %q to manage Harbor project %q",
			conn.Name, hp.Namespace, hp.Spec.ProjectName)
	}

	hc, err := newHarborClient(ctx, r.Client, r.Namespace, r.NewClient, &conn)
	if err != nil {
		return nil, syncPeriod, err
	}
	return hc, syncPeriod, nil
}

// report records a failure on the resource and decides how to come back to it.
func (r *HarborProjectReconciler) report(ctx context.Context, hp *harborv1alpha1.HarborProject, cause error, syncPeriod time.Duration) (ctrl.Result, error) {
	meta.SetStatusCondition(&hp.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reasonFor(cause),
		Message:            cause.Error(),
		ObservedGeneration: hp.Generation,
	})
	hp.Status.ObservedGeneration = hp.Generation

	if err := r.updateStatus(ctx, hp); err != nil {
		return ctrl.Result{}, err
	}

	var se *statusError
	if !errors.As(cause, &se) || se.backoff {
		// Unclassified, or marked as such: let controller-runtime back off.
		return ctrl.Result{}, cause
	}
	if se.terminal {
		// Retrying changes nothing. A spec edit bumps the generation and
		// triggers a fresh reconcile on its own.
		return ctrl.Result{}, nil
	}
	// Explained failures are states of the world, not errors of ours. Come back
	// on the regular interval rather than hammering with backoff.
	return ctrl.Result{RequeueAfter: syncPeriod}, nil
}

// updateStatus writes the status subresource, re-reading on conflict. A
// conflict is routine here: the resource may have changed since it was read.
func (r *HarborProjectReconciler) updateStatus(ctx context.Context, hp *harborv1alpha1.HarborProject) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest harborv1alpha1.HarborProject
		if err := r.Get(ctx, client.ObjectKeyFromObject(hp), &latest); err != nil {
			return err
		}
		latest.Status = hp.Status
		return r.Status().Update(ctx, &latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *HarborProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Namespace == "" {
		return errors.New("HarborProjectReconciler.Namespace must be set")
	}
	if r.NewClient == nil {
		r.NewClient = harbor.New
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &harborv1alpha1.HarborProject{}, connectionRefIndex,
		func(o client.Object) []string {
			return []string{o.(*harborv1alpha1.HarborProject).Spec.ConnectionRef.Name}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&harborv1alpha1.HarborProject{}, builder.WithPredicates(specOrFinalizersChanged)).
		Watches(
			&harborv1alpha1.HarborConnection{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForConnection),
			// Only the spec matters here. Status writes by the connection's
			// own reconciler would otherwise fan out to every bound project.
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("harborproject").
		Complete(r)
}

// specOrFinalizersChanged drops the updates the reconciler causes by writing
// its own status. A failed pass records the error text, which can differ on
// every attempt; if that write triggered the next pass, the retries would run
// back to back and the backoff would never apply.
//
// Finalizer changes have to pass: the pass that adds the finalizer returns
// right away and counts on that write to trigger the one doing the work.
// Deletion needs no case of its own, as the API server bumps the generation
// when it sets the deletion timestamp.
var specOrFinalizersChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			!slices.Equal(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	},
}

// projectsForConnection enqueues the projects bound to a connection, so that a
// change of URL or credentials takes effect without waiting for the resync.
func (r *HarborProjectReconciler) projectsForConnection(ctx context.Context, obj client.Object) []reconcile.Request {
	var list harborv1alpha1.HarborProjectList
	if err := r.List(ctx, &list, client.MatchingFields{connectionRefIndex: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list projects for connection", "connection", obj.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return requests
}
