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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
)

// HarborConnectionReconciler reconciles a HarborConnection object
type HarborConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Namespace is the only namespace credentials are read from; see
	// HarborProjectReconciler.Namespace.
	Namespace string

	// NewClient builds a Harbor client. Tests replace it with a fake.
	NewClient func(harbor.Options) (harbor.Client, error)
}

// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=harbor.satoruh.org,resources=harborconnections/status,verbs=update
// Secrets and ConfigMaps are read uncached (see cmd/main.go), so get is
// sufficient. Scoped to the operator's own namespace: the reference types
// carry no namespace field, and this grant is the enforcement point for that.
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get,namespace=system

// Reconcile probes Harbor with the connection's credentials and records the
// outcome. It manages nothing in Harbor, so there is no finalizer.
func (r *HarborConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var conn harborv1alpha1.HarborConnection
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !conn.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	version, probeErr := r.probe(ctx, &conn)

	ready := metav1.Condition{
		Type:               harborv1alpha1.ConditionReady,
		ObservedGeneration: conn.Generation,
	}
	if probeErr == nil {
		conn.Status.HarborVersion = version
		ready.Status = metav1.ConditionTrue
		ready.Reason = harborv1alpha1.ReasonConnected
		ready.Message = fmt.Sprintf("Harbor %s accepted the credentials", version)
	} else {
		// HarborVersion is left as it was: it documents the last successful
		// probe.
		ready.Status = metav1.ConditionFalse
		ready.Reason = reasonFor(probeErr)
		ready.Message = probeErr.Error()
	}
	meta.SetStatusCondition(&conn.Status.Conditions, ready)
	conn.Status.ObservedGeneration = conn.Generation

	if err := r.updateStatus(ctx, &conn); err != nil {
		return ctrl.Result{}, err
	}

	var se *statusError
	if probeErr != nil && (!errors.As(probeErr, &se) || se.backoff) {
		// Unclassified, or marked as such: let controller-runtime back off.
		return ctrl.Result{}, probeErr
	}
	// Neither the Secret nor Harbor can be watched, so a fix on either side
	// is only seen by coming back.
	return ctrl.Result{RequeueAfter: syncPeriodOf(&conn)}, nil
}

func (r *HarborConnectionReconciler) probe(ctx context.Context, conn *harborv1alpha1.HarborConnection) (string, error) {
	hc, err := newHarborClient(ctx, r.Client, r.Namespace, r.NewClient, conn)
	if err != nil {
		return "", err
	}

	version, err := hc.GetVersion(ctx)
	if errors.Is(err, harbor.ErrUnauthenticated) {
		return "", reasoned(harborv1alpha1.ReasonCredentialsRejected,
			"Harbor did not accept the credentials in Secret %s/%s", r.Namespace, conn.Spec.CredentialsRef.Name)
	}
	return version, err
}

func (r *HarborConnectionReconciler) updateStatus(ctx context.Context, conn *harborv1alpha1.HarborConnection) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest harborv1alpha1.HarborConnection
		if err := r.Get(ctx, client.ObjectKeyFromObject(conn), &latest); err != nil {
			return err
		}
		latest.Status = conn.Status
		return r.Status().Update(ctx, &latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *HarborConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Namespace == "" {
		return errors.New("HarborConnectionReconciler.Namespace must be set")
	}
	if r.NewClient == nil {
		r.NewClient = harbor.New
	}

	return ctrl.NewControllerManagedBy(mgr).
		// The status written here would otherwise trigger the next reconcile.
		// A failure message can differ from one attempt to the next, and
		// each such write would then cut the backoff short.
		For(&harborv1alpha1.HarborConnection{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("harborconnection").
		Complete(r)
}
