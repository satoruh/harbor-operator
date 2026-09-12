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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
)

const (
	// connectionRefIndex indexes HarborProject by the connection it names, so
	// that a change to a HarborConnection can enqueue the projects using it.
	connectionRefIndex = "spec.connectionRef.name"

	// defaultSyncPeriod applies when the connection carries no syncPeriod. The
	// CRD default normally fills it in; this covers objects built in tests.
	defaultSyncPeriod = 10 * time.Minute
)

// HarborProjectReconciler reconciles a HarborProject object
type HarborProjectReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

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
	// terminal marks a failure that retrying cannot fix: the spec or Harbor
	// has to change first.
	terminal bool
	err      error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func reasoned(reason, format string, args ...any) error {
	return &statusError{reason: reason, err: fmt.Errorf(format, args...)}
}

func terminal(reason, format string, args ...any) error {
	return &statusError{reason: reason, terminal: true, err: fmt.Errorf(format, args...)}
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

// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborconnections,verbs=get;list;watch
// Secrets and ConfigMaps are read uncached (see cmd/main.go), so get is
// sufficient. Scoped to the operator's own namespace: the reference types
// carry no namespace field, and this grant is the enforcement point for that.
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get,namespace=system
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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

	if p == nil {
		p, err = r.createProject(ctx, hc, hp)
		if err != nil {
			return r.report(ctx, hp, err, syncPeriod)
		}
	} else if hp.Spec.Public != nil && p.Public != *hp.Spec.Public {
		if err := hc.UpdateProject(ctx, p.ID, *hp.Spec.Public); err != nil {
			return r.report(ctx, hp, err, syncPeriod)
		}
		p.Public = *hp.Spec.Public
	}

	hp.Status.ProjectID = &p.ID
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
	// Harbor cannot be watched, so drift is only found by looking again.
	return ctrl.Result{RequeueAfter: syncPeriod}, nil
}

// createProject creates the project and reports the ID the client looked up
// afterwards. The ID is recorded immediately: losing it here would leave a
// project nobody owns until the next lookup by name recovers it.
func (r *HarborProjectReconciler) createProject(ctx context.Context, hc harbor.Client, hp *harborv1alpha1.HarborProject) (*harbor.Project, error) {
	public := hp.Spec.Public != nil && *hp.Spec.Public

	id, err := hc.CreateProject(ctx, hp.Spec.ProjectName, public)
	switch {
	case err == nil:
	case errors.Is(err, harbor.ErrInvalidName):
		return nil, terminal(harborv1alpha1.ReasonInvalidProjectName,
			"Harbor rejected the project name %q: %v", hp.Spec.ProjectName, err)
	case errors.Is(err, harbor.ErrAlreadyExists):
		// Someone created the project between the lookup and here. Retry so
		// that the next pass evaluates adoptExisting against it.
		return nil, fmt.Errorf("project %q appeared while creating it: %w", hp.Spec.ProjectName, err)
	default:
		return nil, err
	}

	return &harbor.Project{ID: id, Name: hp.Spec.ProjectName, Public: public}, nil
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
			// 403 says nothing about whether the project exists. The list API
			// only returns projects these credentials may see, so a lookup by
			// name separates "deleted" from "not permitted".
			p, nameErr := hc.GetProjectByName(ctx, hp.Spec.ProjectName)
			if nameErr == nil {
				return p, nil
			}
			if errors.Is(nameErr, harbor.ErrNotFound) {
				return nil, reasoned(harborv1alpha1.ReasonProjectInaccessible,
					"Harbor project ID %d is not accessible: it was deleted or these credentials cannot see it", *id)
			}
			return nil, nameErr
		default:
			return nil, err
		}
	}

	p, err := hc.GetProjectByName(ctx, hp.Spec.ProjectName)
	switch {
	case err == nil:
		if !hp.Spec.AdoptExisting {
			return nil, terminal(harborv1alpha1.ReasonAdoptionRefused,
				"Harbor project %q already exists; set adoptExisting to take it over", hp.Spec.ProjectName)
		}
		return p, nil
	case errors.Is(err, harbor.ErrNotFound):
		return nil, nil
	default:
		return nil, err
	}
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
		r.Recorder.Event(hp, corev1.EventTypeWarning, harborv1alpha1.ReasonDeletionBlocked, err.Error())
		res, err := r.report(ctx, hp, reasoned(harborv1alpha1.ReasonDeletionBlocked,
			"Harbor project %q still contains repositories", hp.Spec.ProjectName), syncPeriod)
		return res, false, err

	case errors.Is(err, harbor.ErrInaccessible):
		// Same 403 ambiguity as on the read path. If the project is not in the
		// list either, there is nothing left to delete.
		if _, lookupErr := hc.GetProjectByName(ctx, hp.Spec.ProjectName); errors.Is(lookupErr, harbor.ErrNotFound) {
			return ctrl.Result{}, true, nil
		}
		res, err := r.report(ctx, hp, reasoned(harborv1alpha1.ReasonProjectInaccessible,
			"Harbor project ID %d cannot be deleted with these credentials", *hp.Status.ProjectID), syncPeriod)
		return res, false, err

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
		return nil, defaultSyncPeriod, err
	}

	syncPeriod := defaultSyncPeriod
	if conn.Spec.SyncPeriod != nil {
		syncPeriod = conn.Spec.SyncPeriod.Duration
	}

	username, password, err := r.credentials(ctx, &conn)
	if err != nil {
		return nil, syncPeriod, err
	}

	caBundle, err := r.caBundle(ctx, &conn)
	if err != nil {
		return nil, syncPeriod, err
	}
	if conn.Spec.InsecureSkipVerify && conn.Spec.CABundleRef != nil {
		logf.FromContext(ctx).Info("insecureSkipVerify makes caBundleRef pointless; certificates are not verified",
			"connection", conn.Name)
	}

	hc, err := r.NewClient(harbor.Options{
		BaseURL:            conn.Spec.BaseURL,
		Username:           username,
		Password:           password,
		CABundle:           caBundle,
		InsecureSkipVerify: conn.Spec.InsecureSkipVerify,
	})
	if err != nil {
		return nil, syncPeriod, reasoned(harborv1alpha1.ReasonConnectionUnavailable, "%v", err)
	}
	return hc, syncPeriod, nil
}

func (r *HarborProjectReconciler) credentials(ctx context.Context, conn *harborv1alpha1.HarborConnection) (string, string, error) {
	ref := conn.Spec.CredentialsRef
	key := types.NamespacedName{Namespace: r.Namespace, Name: ref.Name}

	var secret corev1.Secret
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid, "Secret %s not found", key)
		}
		return "", "", err
	}

	// The label is the Secret owner's opt-in. It cannot be expressed as CRD
	// validation because it lives on the referenced object, so the check has
	// to happen here.
	if secret.Labels[harborv1alpha1.CredentialsLabel] != harborv1alpha1.CredentialsLabelEnabled {
		return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid,
			"Secret %s does not carry %s=%s", key,
			harborv1alpha1.CredentialsLabel, harborv1alpha1.CredentialsLabelEnabled)
	}

	// The CRD defaults these, but an object built in code carries whatever it
	// was given.
	usernameKey := valueOr(ref.UsernameKey, "username")
	passwordKey := valueOr(ref.PasswordKey, "password")

	username, ok := secret.Data[usernameKey]
	if !ok {
		return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid, "Secret %s has no key %q", key, usernameKey)
	}
	password, ok := secret.Data[passwordKey]
	if !ok {
		return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid, "Secret %s has no key %q", key, passwordKey)
	}
	return string(username), string(password), nil
}

func (r *HarborProjectReconciler) caBundle(ctx context.Context, conn *harborv1alpha1.HarborConnection) ([]byte, error) {
	ref := conn.Spec.CABundleRef
	if ref == nil {
		return nil, nil
	}
	key := types.NamespacedName{Namespace: r.Namespace, Name: ref.Name}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, reasoned(harborv1alpha1.ReasonConnectionUnavailable, "ConfigMap %s not found", key)
		}
		return nil, err
	}

	dataKey := valueOr(ref.Key, "ca.crt")
	bundle, ok := cm.Data[dataKey]
	if !ok {
		return nil, reasoned(harborv1alpha1.ReasonConnectionUnavailable, "ConfigMap %s has no key %q", key, dataKey)
	}
	return []byte(bundle), nil
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
	if !errors.As(cause, &se) {
		// Unclassified: let controller-runtime back off.
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

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
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
		For(&harborv1alpha1.HarborProject{}).
		Watches(
			&harborv1alpha1.HarborConnection{},
			handler.EnqueueRequestsFromMapFunc(r.projectsForConnection),
		).
		Named("harborproject").
		Complete(r)
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
