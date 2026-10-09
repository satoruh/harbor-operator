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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
	harborfake "github.com/satoruh/harbor-operator/internal/harbor/fake"
)

const testHarborVersion = "v2.15.2-a97e7b83"

type connectionFixture struct {
	t      *testing.T
	r      *HarborConnectionReconciler
	k8s    client.Client
	harbor *harborfake.Client
	key    client.ObjectKey
}

// newConnectionFixture wires the reconciler against a fake API server holding
// conn and whatever else is passed. Nothing is supplied implicitly, so a test
// that omits the Secret reconciles against a connection without one.
func newConnectionFixture(t *testing.T, conn *harborv1alpha1.HarborConnection, objs ...client.Object) *connectionFixture {
	t.Helper()

	k8s := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append([]client.Object{conn}, objs...)...).
		WithStatusSubresource(&harborv1alpha1.HarborConnection{}).
		Build()

	hc := harborfake.New()

	return &connectionFixture{
		t:      t,
		k8s:    k8s,
		harbor: hc,
		key:    client.ObjectKeyFromObject(conn),
		r: &HarborConnectionReconciler{
			Client:    k8s,
			Scheme:    k8s.Scheme(),
			Namespace: operatorNamespace,
			NewClient: func(harbor.Options) (harbor.Client, error) { return hc, nil },
		},
	}
}

func (f *connectionFixture) reconcile() (ctrl.Result, error) {
	f.t.Helper()
	return f.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: f.key})
}

func (f *connectionFixture) get() *harborv1alpha1.HarborConnection {
	f.t.Helper()

	var conn harborv1alpha1.HarborConnection
	if err := f.k8s.Get(context.Background(), f.key, &conn); err != nil {
		f.t.Fatalf("Get HarborConnection: %v", err)
	}
	return &conn
}

func (f *connectionFixture) assertReady(status metav1.ConditionStatus, reason string) *harborv1alpha1.HarborConnection {
	f.t.Helper()

	conn := f.get()
	c := meta.FindStatusCondition(conn.Status.Conditions, harborv1alpha1.ConditionReady)
	if c == nil {
		f.t.Fatal("Ready condition is not set")
	}
	if c.Status != status || c.Reason != reason {
		f.t.Errorf("Ready = %s/%s (%s), want %s/%s", c.Status, c.Reason, c.Message, status, reason)
	}
	if c.ObservedGeneration != conn.Generation {
		f.t.Errorf("Ready.observedGeneration = %d, want %d", c.ObservedGeneration, conn.Generation)
	}
	if conn.Status.ObservedGeneration != conn.Generation {
		f.t.Errorf("status.observedGeneration = %d, want %d", conn.Status.ObservedGeneration, conn.Generation)
	}
	return conn
}

func withGeneration(conn *harborv1alpha1.HarborConnection, generation int64) *harborv1alpha1.HarborConnection {
	conn.Generation = generation
	return conn
}

func TestConnectionReportsVersionWhenHarborAcceptsCredentials(t *testing.T) {
	f := newConnectionFixture(t, withGeneration(newConnection(), 3), newSecret())
	f.harbor.Version = testHarborVersion

	res, err := f.reconcile()
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	conn := f.assertReady(metav1.ConditionTrue, harborv1alpha1.ReasonConnected)
	if conn.Status.HarborVersion != testHarborVersion {
		t.Errorf("harborVersion = %q, want %s", conn.Status.HarborVersion, testHarborVersion)
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

// The opt-in label has to hold on this path too: a connection must not be able
// to send an unlabelled Secret to a URL of its author's choosing.
func TestConnectionRejectsSecretWithoutOptInLabel(t *testing.T) {
	secret := newSecret(func(s *corev1.Secret) { s.Labels = nil })
	f := newConnectionFixture(t, newConnection(), secret)

	built := false
	f.r.NewClient = func(harbor.Options) (harbor.Client, error) {
		built = true
		return f.harbor, nil
	}

	res, err := f.reconcile()
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	f.assertReady(metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsInvalid)
	if built {
		t.Error("a Harbor client was built from a Secret without the opt-in label")
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

func TestConnectionReportsMissingSecret(t *testing.T) {
	f := newConnectionFixture(t, newConnection())

	if _, err := f.reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.assertReady(metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsInvalid)
}

func TestConnectionReportsRejectedCredentials(t *testing.T) {
	conn := newConnection()
	conn.Status.HarborVersion = testHarborVersion
	f := newConnectionFixture(t, conn, newSecret())
	f.harbor.Errs["GetVersion"] = harbor.ErrUnauthenticated

	res, err := f.reconcile()
	if err != nil {
		t.Fatalf("Reconcile: %v; rejected credentials are a state to report, not an error to back off on", err)
	}

	got := f.assertReady(metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsRejected)
	if got.Status.HarborVersion != testHarborVersion {
		t.Errorf("harborVersion = %q, want the last known version to survive a failed probe", got.Status.HarborVersion)
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

func TestConnectionReturnsErrorForUnclassifiedFailure(t *testing.T) {
	f := newConnectionFixture(t, newConnection(), newSecret())
	boom := errors.New("dial tcp: connection refused")
	f.harbor.Errs["GetVersion"] = boom

	if _, err := f.reconcile(); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the Harbor failure so that controller-runtime backs off", err)
	}
	f.assertReady(metav1.ConditionFalse, harborv1alpha1.ReasonHarborError)
}

func TestConnectionRecovers(t *testing.T) {
	f := newConnectionFixture(t, newConnection(), newSecret())
	f.harbor.Errs["GetVersion"] = harbor.ErrUnauthenticated

	if _, err := f.reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.assertReady(metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsRejected)

	delete(f.harbor.Errs, "GetVersion")
	if _, err := f.reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.assertReady(metav1.ConditionTrue, harborv1alpha1.ReasonConnected)
}

func TestConnectionIgnoresMissingResource(t *testing.T) {
	f := newConnectionFixture(t, newConnection())
	f.key.Name = "gone"

	res, err := f.reconcile()
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("result = %+v, want no requeue", res)
	}
}

func TestConnectionReportsSecretReadFailureAsInternal(t *testing.T) {
	f := newConnectionFixture(t, newConnection(), newSecret())
	f.r.Client = refusingSecrets(f.k8s)

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error; the read would not be retried with backoff")
	}
	f.assertReady(metav1.ConditionFalse, harborv1alpha1.ReasonInternalError)
}
