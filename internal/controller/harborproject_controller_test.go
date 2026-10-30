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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
	harborfake "github.com/satoruh/harbor-operator/internal/harbor/fake"
)

const (
	// operatorNamespace is where the controller is allowed to read credentials.
	operatorNamespace = "harbor-operator-system"

	// tenantNamespace holds the HarborProject. It differs from
	// operatorNamespace so that a Secret looked up in the wrong namespace
	// fails the test rather than passing by accident.
	tenantNamespace = "team-a"

	projectResourceName = "platform"
	harborProjectName   = "platform"
	connectionName      = "harbor"
	secretName          = "harbor-credentials"
	caConfigMapName     = "harbor-ca"
	caBundlePEM         = "PEM"

	// testSyncPeriod is short and distinctive so that a requeue can be told
	// apart from defaultSyncPeriod.
	testSyncPeriod = 90 * time.Second
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := harborv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add harbor scheme: %v", err)
	}
	return s
}

type projectOption func(*harborv1alpha1.HarborProject)

// newProject builds a HarborProject as the API server would hand it over:
// defaults applied, no status.
func newProject(opts ...projectOption) *harborv1alpha1.HarborProject {
	hp := &harborv1alpha1.HarborProject{
		ObjectMeta: metav1.ObjectMeta{
			Name:       projectResourceName,
			Namespace:  tenantNamespace,
			Generation: 1,
		},
		Spec: harborv1alpha1.HarborProjectSpec{
			ConnectionRef:  harborv1alpha1.ConnectionReference{Name: connectionName},
			ProjectName:    harborProjectName,
			DeletionPolicy: harborv1alpha1.DeletionPolicyOrphan,
		},
	}
	for _, opt := range opts {
		opt(hp)
	}
	return hp
}

func withPublic(public bool) projectOption {
	return func(hp *harborv1alpha1.HarborProject) { hp.Spec.Public = &public }
}

func withAdoptExisting() projectOption {
	return func(hp *harborv1alpha1.HarborProject) { hp.Spec.AdoptExisting = true }
}

func withDeletionPolicy(policy harborv1alpha1.DeletionPolicy) projectOption {
	return func(hp *harborv1alpha1.HarborProject) { hp.Spec.DeletionPolicy = policy }
}

// bound marks the resource as already reconciled against the given Harbor
// project, which is the state every pass after the first one starts from.
func bound(id int64) projectOption {
	return func(hp *harborv1alpha1.HarborProject) {
		hp.Finalizers = append(hp.Finalizers, harborv1alpha1.HarborProjectFinalizer)
		hp.Status.ProjectID = &id
	}
}

func withFinalizer() projectOption {
	return func(hp *harborv1alpha1.HarborProject) {
		hp.Finalizers = append(hp.Finalizers, harborv1alpha1.HarborProjectFinalizer)
	}
}

// deleting stamps a deletion timestamp. The fake client refuses an object that
// carries one without a finalizer, which is also what the API server does.
func deleting() projectOption {
	return func(hp *harborv1alpha1.HarborProject) {
		now := metav1.Now()
		hp.DeletionTimestamp = &now
	}
}

func newConnection() *harborv1alpha1.HarborConnection {
	return &harborv1alpha1.HarborConnection{
		ObjectMeta: metav1.ObjectMeta{Name: connectionName},
		Spec: harborv1alpha1.HarborConnectionSpec{
			BaseURL: "https://harbor.example.com",
			CredentialsRef: harborv1alpha1.SecretKeyReference{
				Name:        secretName,
				UsernameKey: "username",
				PasswordKey: "password",
			},
			AllowedProjects: []harborv1alpha1.ProjectRule{
				{Namespace: tenantNamespace, Names: []string{harborProjectName}},
			},
			SyncPeriod: &metav1.Duration{Duration: testSyncPeriod},
		},
	}
}

func newSecret(mods ...func(*corev1.Secret)) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: operatorNamespace,
			Labels: map[string]string{
				harborv1alpha1.CredentialsLabel: harborv1alpha1.CredentialsLabelEnabled,
			},
		},
		Data: map[string][]byte{
			"username": []byte("robot$operator"),
			"password": []byte("token"),
		},
	}
	for _, mod := range mods {
		mod(s)
	}
	return s
}

type fixture struct {
	t        *testing.T
	r        *HarborProjectReconciler
	k8s      client.Client
	harbor   *harborfake.Client
	recorder *events.FakeRecorder
	key      types.NamespacedName
}

// newFixture wires the reconciler against a fake API server holding hp. When no
// further object is given, a usable HarborConnection and credentials Secret are
// supplied; pass objects explicitly to reconcile against a broken setup.
func newFixture(t *testing.T, hc *harborfake.Client, hp *harborv1alpha1.HarborProject, objs ...client.Object) *fixture {
	t.Helper()

	if len(objs) == 0 {
		objs = []client.Object{newConnection(), newSecret()}
	}

	k8s := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append([]client.Object{hp}, objs...)...).
		WithStatusSubresource(&harborv1alpha1.HarborProject{}).
		Build()

	recorder := events.NewFakeRecorder(10)

	return &fixture{
		t:        t,
		k8s:      k8s,
		harbor:   hc,
		recorder: recorder,
		key:      client.ObjectKeyFromObject(hp),
		r: &HarborProjectReconciler{
			Client:    k8s,
			Scheme:    k8s.Scheme(),
			Recorder:  recorder,
			Namespace: operatorNamespace,
			NewClient: func(harbor.Options) (harbor.Client, error) { return hc, nil },
		},
	}
}

func (f *fixture) reconcile() (ctrl.Result, error) {
	f.t.Helper()
	return f.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: f.key})
}

// mustReconcile runs one pass and fails on an error. Most failures are reported
// through the Ready condition instead, so an error here is a real surprise.
func (f *fixture) mustReconcile() ctrl.Result {
	f.t.Helper()

	res, err := f.reconcile()
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// get returns the resource, or nil once it is gone.
func (f *fixture) get() *harborv1alpha1.HarborProject {
	f.t.Helper()

	var hp harborv1alpha1.HarborProject
	switch err := f.k8s.Get(context.Background(), f.key, &hp); {
	case err == nil:
		return &hp
	case apierrors.IsNotFound(err):
		return nil
	default:
		f.t.Fatalf("Get HarborProject: %v", err)
		return nil
	}
}

// ready reports the Ready condition, which every path is required to set.
func (f *fixture) ready() metav1.Condition {
	f.t.Helper()

	hp := f.get()
	if hp == nil {
		f.t.Fatal("HarborProject is gone; no Ready condition to read")
	}
	c := meta.FindStatusCondition(hp.Status.Conditions, harborv1alpha1.ConditionReady)
	if c == nil {
		f.t.Fatal("Ready condition is not set")
	}
	return *c
}

func (f *fixture) assertReady(t *testing.T, status metav1.ConditionStatus, reason string) {
	t.Helper()

	c := f.ready()
	if c.Status != status || c.Reason != reason {
		t.Errorf("Ready = %s/%s, want %s/%s (%s)", c.Status, c.Reason, status, reason, c.Message)
	}
	if hp := f.get(); hp != nil && c.ObservedGeneration != hp.Generation {
		t.Errorf("Ready.observedGeneration = %d, want %d", c.ObservedGeneration, hp.Generation)
	}
}

func (f *fixture) assertCalled(t *testing.T, method string, want bool) {
	t.Helper()

	if got := slices.Contains(f.harbor.Calls, method); got != want {
		t.Errorf("%s called = %t, want %t (calls: %v)", method, got, want, f.harbor.Calls)
	}
}

// projectByName finds a project in the fake Harbor.
func projectByName(t *testing.T, hc *harborfake.Client, name string) *harbor.Project {
	t.Helper()

	for _, p := range hc.Projects() {
		if p.Name == name {
			return &p
		}
	}
	return nil
}

// The first pass only takes the finalizer: a project created before the
// finalizer is persisted would be left behind if the resource were deleted in
// between.
func TestReconcileTakesFinalizerBeforeTouchingHarbor(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject())

	f.mustReconcile()

	hp := f.get()
	if !slices.Contains(hp.Finalizers, harborv1alpha1.HarborProjectFinalizer) {
		t.Errorf("finalizers = %v, want the HarborProject finalizer", hp.Finalizers)
	}
	if len(hc.Calls) != 0 {
		t.Errorf("Harbor was called before the finalizer was persisted: %v", hc.Calls)
	}
}

func TestReconcileCreatesProject(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withPublic(true)))

	f.mustReconcile() // finalizer
	res := f.mustReconcile()

	p := projectByName(t, hc, harborProjectName)
	if p == nil {
		t.Fatalf("project %q was not created (calls: %v)", harborProjectName, hc.Calls)
	}
	if !p.Public {
		t.Error("created project is private, want public")
	}

	hp := f.get()
	if hp.Status.ProjectID == nil || *hp.Status.ProjectID != p.ID {
		t.Errorf("status.projectID = %v, want %d", hp.Status.ProjectID, p.ID)
	}
	if hp.Status.ObservedGeneration != hp.Generation {
		t.Errorf("status.observedGeneration = %d, want %d", hp.Status.ObservedGeneration, hp.Generation)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)

	// Harbor cannot be watched, so the resync is the only drift detection.
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

func TestReconcileCorrectsVisibilityDrift(t *testing.T) {
	for _, want := range []bool{true, false} {
		hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName, Public: !want})
		f := newFixture(t, hc, newProject(withPublic(want), bound(7)))

		f.mustReconcile()

		f.assertCalled(t, "UpdateProject", true)
		if p := hc.Projects()[7]; p.Public != want {
			t.Errorf("project public = %t after reconcile, want %t", p.Public, want)
		}
		f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
	}
}

// An unset spec.public means the attribute is not managed, so whatever Harbor
// holds has to survive a reconcile.
func TestReconcileLeavesVisibilityAloneWhenUnset(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName, Public: true})
	f := newFixture(t, hc, newProject(bound(7)))

	f.mustReconcile()

	f.assertCalled(t, "UpdateProject", false)
	if p := hc.Projects()[7]; !p.Public {
		t.Error("project visibility was changed")
	}
}

// The label is the Secret owner's opt-in. Without it the controller must not
// read the credentials, even though RBAC would allow the get.
func TestReconcileRejectsSecretWithoutOptInLabel(t *testing.T) {
	hc := harborfake.New()
	secret := newSecret(func(s *corev1.Secret) { s.Labels = nil })
	f := newFixture(t, hc, newProject(withFinalizer()), newConnection(), secret)

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsInvalid)
	if len(hc.Calls) != 0 {
		t.Errorf("Harbor was called with rejected credentials: %v", hc.Calls)
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

func TestReconcileRejectsSecretMissingKey(t *testing.T) {
	hc := harborfake.New()
	secret := newSecret(func(s *corev1.Secret) { delete(s.Data, "password") })
	f := newFixture(t, hc, newProject(withFinalizer()), newConnection(), secret)

	f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsInvalid)
}

func TestReconcileReportsMissingConnection(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withFinalizer()), newSecret())

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonConnectionUnavailable)
	// No connection means no syncPeriod to read either.
	if res.RequeueAfter != defaultSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, defaultSyncPeriod)
	}
}

// adoptExisting governs taking a project over, not keeping one. Re-evaluating
// it on a bound resource would break every project whose flag was cleared.
func TestReconcileIgnoresAdoptExistingOnceBound(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(bound(7)))

	f.mustReconcile()

	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
	f.assertCalled(t, "GetProjectByName", false)
}

func TestReconcileRefusesUnrequestedAdoption(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(withFinalizer()))

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonAdoptionRefused)
	f.assertCalled(t, "CreateProject", false)
	if hp := f.get(); hp.Status.ProjectID != nil {
		t.Errorf("status.projectID = %d, want unset: the project was not adopted", *hp.Status.ProjectID)
	}
	// The way out may be on Harbor's side, where only looking again finds it.
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}

	if err := hc.DeleteProject(context.Background(), 7); err != nil {
		t.Fatalf("remove the project from the fake: %v", err)
	}
	f.mustReconcile()
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

func TestReconcileAdoptsExistingProject(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(withAdoptExisting(), withFinalizer()))

	f.mustReconcile()

	f.assertCalled(t, "CreateProject", false)
	hp := f.get()
	if hp.Status.ProjectID == nil || *hp.Status.ProjectID != 7 {
		t.Errorf("status.projectID = %v, want 7", hp.Status.ProjectID)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// A 404 settles it: the project is gone and can be recreated. Harbor only
// answers that way for credentials that may see the whole instance.
func TestReconcileRecreatesProjectDeletedInHarbor(t *testing.T) {
	hc := harborfake.New()
	hc.Errs["GetProjectByID"] = harbor.ErrNotFound
	f := newFixture(t, hc, newProject(bound(42)))

	f.mustReconcile()

	p := projectByName(t, hc, harborProjectName)
	if p == nil {
		t.Fatalf("project was not recreated (calls: %v)", hc.Calls)
	}
	hp := f.get()
	if hp.Status.ProjectID == nil || *hp.Status.ProjectID != p.ID {
		t.Errorf("status.projectID = %v, want %d", hp.Status.ProjectID, p.ID)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// Credentials that may list a project but not read it by ID get a 403 for a
// project that is right there. It is still the bound one.
func TestReconcileKeepsProjectListedButNotReadable(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["GetProjectByID"] = harbor.ErrInaccessible
	f := newFixture(t, hc, newProject(bound(7)))

	f.mustReconcile()

	f.assertCalled(t, "CreateProject", false)
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// The bound project is gone and another one holds its name. Whoever created
// that one, it is not the project this resource was bound to, so the same
// consent is needed as for any project found by name, whether Harbor answers
// 403 (a robot) or 404 (an administrator).
func TestReconcileRefusesReplacementOfBoundProject(t *testing.T) {
	for _, gone := range []error{harbor.ErrInaccessible, harbor.ErrNotFound} {
		t.Run(gone.Error(), func(t *testing.T) {
			hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
			hc.Errs["GetProjectByID"] = gone
			f := newFixture(t, hc, newProject(bound(42)))

			f.mustReconcile()

			f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonAdoptionRefused)
			if hp := f.get(); hp.Status.ProjectID == nil || *hp.Status.ProjectID != 42 {
				t.Errorf("status.projectID = %v, want it kept at 42", hp.Status.ProjectID)
			}
		})
	}
}

func TestReconcileRebindsToReplacementWhenAdopting(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(withAdoptExisting(), bound(42)))

	f.mustReconcile()

	f.assertCalled(t, "CreateProject", false)
	if hp := f.get(); hp.Status.ProjectID == nil || *hp.Status.ProjectID != 7 {
		t.Errorf("status.projectID = %v, want 7", hp.Status.ProjectID)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// The replacement is this resource's own: it recreated the project and failed
// before recording the new ID.
func TestReconcileRebindsToProjectItRecreated(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hp := newProject(bound(42))
	hp.Status.CreationPending = true
	f := newFixture(t, hc, hp)

	f.mustReconcile()

	if hp := f.get(); hp.Status.ProjectID == nil || *hp.Status.ProjectID != 7 {
		t.Errorf("status.projectID = %v, want 7", hp.Status.ProjectID)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// Neither the ID nor the name resolves, which a robot is told both for a
// deleted project and for one it may not see. Creating it succeeds, so it was
// deleted.
func TestReconcileRecreatesProjectAnsweredForbidden(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(bound(42)))

	f.mustReconcile()

	p := projectByName(t, hc, harborProjectName)
	if p == nil {
		t.Fatalf("project was not recreated (calls: %v)", hc.Calls)
	}
	if hp := f.get(); hp.Status.ProjectID == nil || *hp.Status.ProjectID != p.ID {
		t.Errorf("status.projectID = %v, want %d", hp.Status.ProjectID, p.ID)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// Same answers as above, but the project is there and merely hidden from these
// credentials. Harbor refuses the duplicate name, and that refusal is reported
// as a state to wait on rather than retried with backoff.
func TestReconcileReportsProjectHiddenFromCredentials(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 42, Name: harborProjectName})
	hc.Errs["GetProjectByID"] = harbor.ErrInaccessible
	hc.Errs["GetProjectByName"] = harbor.ErrNotFound
	f := newFixture(t, hc, newProject(bound(42)))

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonProjectInaccessible)
	if got := len(hc.Projects()); got != 1 {
		t.Errorf("Harbor holds %d projects, want the original one only", got)
	}
	if hp := f.get(); hp.Status.ProjectID == nil || *hp.Status.ProjectID != 42 {
		t.Errorf("status.projectID = %v, want it kept at 42", hp.Status.ProjectID)
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

// A resource that was never bound meets the same refusal when the credentials
// cannot list the project that holds its name.
func TestReconcileReportsHiddenProjectBeforeBinding(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["GetProjectByName"] = harbor.ErrNotFound
	f := newFixture(t, hc, newProject(withFinalizer()))

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonProjectInaccessible)
	if hp := f.get(); hp.Status.ProjectID != nil {
		t.Errorf("status.projectID = %d, want it unset", *hp.Status.ProjectID)
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

// Harbor rejects the name for good. Backing off would only repeat the refusal.
func TestReconcileStopsOnInvalidProjectName(t *testing.T) {
	hc := harborfake.New()
	hc.Errs["CreateProject"] = harbor.ErrInvalidName
	f := newFixture(t, hc, newProject(withFinalizer()))

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonInvalidProjectName)
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue", res.RequeueAfter)
	}
}

// An unclassified Harbor failure is returned as an error so that
// controller-runtime applies its backoff.
func TestReconcileReturnsErrorForUnclassifiedFailure(t *testing.T) {
	hc := harborfake.New()
	hc.Errs["GetProjectByName"] = errors.New("dial tcp: connection refused")
	f := newFixture(t, hc, newProject(withFinalizer()))

	_, err := f.reconcile()
	if err == nil {
		t.Fatal("Reconcile returned no error; the failure would never be retried")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonHarborError)
}

func TestReconcileDeletesProject(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()))

	f.mustReconcile()

	if _, ok := hc.Projects()[7]; ok {
		t.Error("Harbor project still exists")
	}
	if hp := f.get(); hp != nil {
		t.Errorf("resource still exists with finalizers %v", hp.Finalizers)
	}
}

func TestReconcileOrphansProject(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyOrphan), bound(7), deleting()))

	f.mustReconcile()

	f.assertCalled(t, "DeleteProject", false)
	if _, ok := hc.Projects()[7]; !ok {
		t.Error("Harbor project was deleted under the Orphan policy")
	}
	if hp := f.get(); hp != nil {
		t.Errorf("resource still exists with finalizers %v", hp.Finalizers)
	}
}

// Without a projectID the resource was never bound to a project. Looking one up
// by name here could delete a project it never owned.
func TestReconcileDeletesNothingWithoutProjectID(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), withFinalizer(), deleting()))

	f.mustReconcile()

	if len(hc.Calls) != 0 {
		t.Errorf("Harbor was called for an unbound resource: %v", hc.Calls)
	}
	if _, ok := hc.Projects()[7]; !ok {
		t.Error("a project this resource never owned was deleted")
	}
	if hp := f.get(); hp != nil {
		t.Errorf("resource still exists with finalizers %v", hp.Finalizers)
	}
}

// Deleting the repositories is a decision for a human, so the resource stays.
func TestReconcileKeepsResourceWhenProjectHoldsRepositories(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName, RepoCount: 3})
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()))

	res := f.mustReconcile()

	hp := f.get()
	if hp == nil {
		t.Fatal("resource was removed while the project still holds repositories")
	}
	if !slices.Contains(hp.Finalizers, harborv1alpha1.HarborProjectFinalizer) {
		t.Error("finalizer was removed, orphaning the project")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonDeletionBlocked)
	if got := hp.Status.RepositoryCount; got == nil || *got != 3 {
		t.Errorf("repositoryCount = %v, want 3: the status must not contradict the reason deletion is blocked", got)
	}
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}

	select {
	case event := <-f.recorder.Events:
		if !strings.Contains(event, harborv1alpha1.ReasonDeletionBlocked) {
			t.Errorf("event = %q, want it to mention %s", event, harborv1alpha1.ReasonDeletionBlocked)
		}
	default:
		t.Error("no event was recorded; the block is invisible to kubectl describe")
	}
}

// A HarborConnection change has to reach the projects bound to it without
// waiting for the resync, since it may be what fixes them.
func TestProjectsForConnection(t *testing.T) {
	other := newProject()
	other.Name = "other"
	other.Spec.ConnectionRef.Name = "another-harbor"

	k8s := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(newProject(), other).
		WithIndex(&harborv1alpha1.HarborProject{}, connectionRefIndex,
			func(o client.Object) []string {
				return []string{o.(*harborv1alpha1.HarborProject).Spec.ConnectionRef.Name}
			}).
		Build()

	r := &HarborProjectReconciler{Client: k8s}
	got := r.projectsForConnection(context.Background(), newConnection())

	want := []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: tenantNamespace, Name: projectResourceName,
	}}}
	if !slices.Equal(got, want) {
		t.Errorf("projectsForConnection() = %v, want %v", got, want)
	}
}

// The connection's credentials reach every project in Harbor. Without a rule
// covering it, a resource could take over a project another namespace manages,
// publish it and delete it.
func TestReconcileRefusesProjectTheConnectionDoesNotAllow(t *testing.T) {
	tests := []struct {
		name  string
		rules []harborv1alpha1.ProjectRule
	}{
		{name: "no rules"},
		{name: "other namespace", rules: []harborv1alpha1.ProjectRule{
			{Namespace: "team-b", Names: []string{"*"}},
		}},
		{name: "other project", rules: []harborv1alpha1.ProjectRule{
			{Namespace: tenantNamespace, Names: []string{"other-*"}},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
			conn := newConnection()
			conn.Spec.AllowedProjects = tc.rules
			f := newFixture(t, hc,
				newProject(withAdoptExisting(), withPublic(true), withFinalizer()), conn, newSecret())

			res := f.mustReconcile()

			f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonNotAllowed)
			if len(hc.Calls) != 0 {
				t.Errorf("Harbor was called for a project the connection does not allow: %v", hc.Calls)
			}
			if res.RequeueAfter != testSyncPeriod {
				t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
			}
		})
	}
}

// A rule withdrawn after binding must stop the deletion too, or removing the
// resource would still delete a project it may no longer touch.
func TestReconcileDeletesNothingTheConnectionDoesNotAllow(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	conn := newConnection()
	conn.Spec.AllowedProjects = nil
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()), conn, newSecret())

	f.mustReconcile()

	if len(hc.Calls) != 0 {
		t.Errorf("Harbor was called: %v", hc.Calls)
	}
	if f.get() == nil {
		t.Fatal("resource was released although its project could not be deleted")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonNotAllowed)
}

func TestMatchName(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"web", "web", true},
		{"api", "api-staging", false},
		{"*", "anything", true},
		{"team-a-*", "team-a-web", true},
		{"team-b-*", "team-b-", true},
		{"team-c-*", "team-cd", false},
		{"*-prod", "web-prod", true},
		{"*-prod", "web-production", false},
		{"a*b*c", "a-b-c", true},
		{"a*b*c", "a-c-b", false},
		{"a*a", "a", false},
		{"team-[ab]", "team-a", false},
	}
	for _, tc := range tests {
		if got := matchName(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchName(%q, %q) = %t, want %t", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func (f *fixture) assertEvent(t *testing.T, reason string) {
	t.Helper()

	select {
	case event := <-f.recorder.Events:
		if !strings.Contains(event, reason) {
			t.Errorf("event = %q, want it to mention %s", event, reason)
		}
	default:
		t.Errorf("no event was recorded, want %s", reason)
	}
}

// Harbor answers 403 and lists nothing for a project that is gone and for one
// these credentials may not see. Either way there is nothing this controller
// can delete, but only one of the two is a deletion, so the release must not
// pass in silence.
func TestReconcileReleasesResourceWhenDeletionCannotBeConfirmed(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["DeleteProject"] = harbor.ErrInaccessible
	hc.Errs["GetProjectByName"] = harbor.ErrNotFound
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()))

	f.mustReconcile()

	if hp := f.get(); hp != nil {
		t.Errorf("resource still exists with finalizers %v", hp.Finalizers)
	}
	f.assertEvent(t, harborv1alpha1.EventDeletionUnconfirmed)
}

// The project is listed, so it exists and the 403 is a matter of permission.
// Releasing the resource would orphan a project it was told to delete.
func TestReconcileKeepsResourceWhenDeletionIsForbidden(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["DeleteProject"] = harbor.ErrInaccessible
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()))

	res := f.mustReconcile()

	if f.get() == nil {
		t.Fatal("resource was released although the project still exists")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonProjectInaccessible)
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

// A lookup that failed says nothing about the project. Reading it as "not
// listed" would release the resource on a Harbor hiccup.
func TestReconcileKeepsResourceWhenDeletionLookupFails(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["DeleteProject"] = harbor.ErrInaccessible
	hc.Errs["GetProjectByName"] = errors.New("502 bad gateway")
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()))

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error; the lookup would not be retried with backoff")
	}
	if f.get() == nil {
		t.Fatal("resource was released on a failed lookup")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonHarborError)
}

func TestReconcileKeepsResourceWhenDeletionFails(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["DeleteProject"] = errors.New("dial tcp: connection refused")
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()))

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error")
	}
	if f.get() == nil {
		t.Fatal("resource was released although the deletion failed")
	}
}

// Without the connection there is no way to reach Harbor, and the Delete
// policy is a promise that the project goes with the resource.
func TestReconcileKeepsResourceWhenConnectionIsMissing(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(
		withDeletionPolicy(harborv1alpha1.DeletionPolicyDelete), bound(7), deleting()), newSecret())

	f.mustReconcile()

	if f.get() == nil {
		t.Fatal("resource was released without its project being deleted")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonConnectionUnavailable)
}

// The project was read a moment ago, so a 403 on the update is a missing
// permission: a state to report, not a failure to back off on.
func TestReconcileReportsForbiddenUpdate(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["UpdateProject"] = harbor.ErrInaccessible
	f := newFixture(t, hc, newProject(withPublic(true), bound(7)))

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonProjectInaccessible)
	if res.RequeueAfter != testSyncPeriod {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, testSyncPeriod)
	}
}

func TestReconcileReturnsErrorForFailedUpdate(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	hc.Errs["UpdateProject"] = errors.New("502 bad gateway")
	f := newFixture(t, hc, newProject(withPublic(true), bound(7)))

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error; the update would not be retried with backoff")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonHarborError)
}

func (f *fixture) assertNoEvent(t *testing.T) {
	t.Helper()

	select {
	case event := <-f.recorder.Events:
		t.Errorf("unexpected event %q", event)
	default:
	}
}

// Creating, bringing back and taking over a project are the things the
// controller does to Harbor on its own initiative. Each has to leave a trace
// beyond a status that looks the same afterwards.
func TestReconcileAnnouncesChangesOfBinding(t *testing.T) {
	tests := []struct {
		name   string
		harbor []harbor.Project
		hp     *harborv1alpha1.HarborProject
		want   string
	}{
		{name: "created", hp: newProject(withFinalizer()), want: harborv1alpha1.EventProjectCreated},
		{name: "recreated", hp: newProject(bound(42)), want: harborv1alpha1.EventProjectRecreated},
		{
			name:   "adopted",
			harbor: []harbor.Project{{ID: 7, Name: harborProjectName}},
			hp:     newProject(withAdoptExisting(), withFinalizer()),
			want:   harborv1alpha1.EventProjectAdopted,
		},
		{
			name:   "unchanged",
			harbor: []harbor.Project{{ID: 7, Name: harborProjectName}},
			hp:     newProject(bound(7)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, harborfake.New(tc.harbor...), tc.hp)

			f.mustReconcile()

			f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
			if tc.want == "" {
				f.assertNoEvent(t)
				return
			}
			f.assertEvent(t, tc.want)
			f.assertNoEvent(t)
		})
	}
}

// refusingSecrets makes the API server deny every Secret read, as it would
// with the namespaced Role missing.
func refusingSecrets(c client.Client) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("no RBAC rule"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// The API server refusing the Secret is not Harbor failing. Reported as a
// Harbor error it would send whoever reads the status to the wrong system.
func TestReconcileReportsSecretReadFailureAsInternal(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withFinalizer()))
	f.r.Client = refusingSecrets(f.k8s)

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error; the read would not be retried with backoff")
	}
	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonInternalError)
	if len(hc.Calls) != 0 {
		t.Errorf("Harbor was called: %v", hc.Calls)
	}
}

// What the reconciler reads from the connection, the Secret and the ConfigMap
// has to arrive at the Harbor client as it was written.
func TestReconcileBuildsClientFromConnection(t *testing.T) {
	conn := newConnection()
	conn.Spec.BaseURL = "https://harbor.example.com/registry"
	conn.Spec.InsecureSkipVerify = true
	conn.Spec.CredentialsRef.UsernameKey = "user"
	conn.Spec.CredentialsRef.PasswordKey = "token"
	conn.Spec.CABundleRef = &harborv1alpha1.ConfigMapKeyReference{Name: caConfigMapName, Key: "bundle.pem"}

	secret := newSecret(func(s *corev1.Secret) {
		s.Data = map[string][]byte{"user": []byte("robot$custom"), "token": []byte("s3cret")}
	})
	ca := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: caConfigMapName, Namespace: operatorNamespace},
		Data:       map[string]string{"bundle.pem": caBundlePEM},
	}

	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withFinalizer()), conn, secret, ca)
	var got harbor.Options
	f.r.NewClient = func(opts harbor.Options) (harbor.Client, error) {
		got = opts
		return hc, nil
	}

	f.mustReconcile()

	want := harbor.Options{
		BaseURL:            "https://harbor.example.com/registry",
		Username:           "robot$custom",
		Password:           "s3cret",
		CABundle:           []byte(caBundlePEM),
		InsecureSkipVerify: true,
	}
	if got.BaseURL != want.BaseURL || got.Username != want.Username || got.Password != want.Password ||
		string(got.CABundle) != string(want.CABundle) || got.InsecureSkipVerify != want.InsecureSkipVerify {
		t.Errorf("options = %+v, want %+v", got, want)
	}
}

func TestReconcileReportsUnusableCABundle(t *testing.T) {
	tests := []struct {
		name string
		objs []client.Object
	}{
		{name: "missing ConfigMap"},
		{name: "missing key", objs: []client.Object{&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: caConfigMapName, Namespace: operatorNamespace},
			Data:       map[string]string{"other": caBundlePEM},
		}}},
		// The reference carries no namespace: one elsewhere must not be found.
		{name: "ConfigMap in another namespace", objs: []client.Object{&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: caConfigMapName, Namespace: tenantNamespace},
			Data:       map[string]string{"ca.crt": caBundlePEM},
		}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newConnection()
			conn.Spec.CABundleRef = &harborv1alpha1.ConfigMapKeyReference{Name: caConfigMapName}
			hc := harborfake.New()
			f := newFixture(t, hc, newProject(withFinalizer()), append(tc.objs, conn, newSecret())...)

			f.mustReconcile()

			f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonConnectionUnavailable)
			if len(hc.Calls) != 0 {
				t.Errorf("Harbor was called without the CA bundle the connection asks for: %v", hc.Calls)
			}
		})
	}
}

// A Secret of the right name in the resource's own namespace is exactly what a
// namespace field would have allowed. It must not be picked up.
func TestReconcileIgnoresSecretOutsideOperatorNamespace(t *testing.T) {
	hc := harborfake.New()
	secret := newSecret(func(s *corev1.Secret) { s.Namespace = tenantNamespace })
	f := newFixture(t, hc, newProject(withFinalizer()), newConnection(), secret)

	f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonCredentialsInvalid)
	if len(hc.Calls) != 0 {
		t.Errorf("Harbor was called: %v", hc.Calls)
	}
}

func TestReconcileReportsClientThatCannotBeBuilt(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withFinalizer()))
	f.r.NewClient = func(harbor.Options) (harbor.Client, error) {
		return nil, errors.New("harbor: CA bundle contains no PEM-encoded certificate")
	}

	f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonConnectionUnavailable)
}

// createHook wraps the fake Harbor to interfere with CreateProject.
type createHook struct {
	*harborfake.Client
	before func()
	after  func(id int64, err error) (int64, error)
}

func (c createHook) CreateProject(ctx context.Context, name string, public bool) (int64, error) {
	if c.before != nil {
		c.before()
	}
	id, err := c.Client.CreateProject(ctx, name, public)
	if c.after != nil {
		return c.after(id, err)
	}
	return id, err
}

func (f *fixture) useHarbor(hc harbor.Client) {
	f.r.NewClient = func(harbor.Options) (harbor.Client, error) { return hc, nil }
}

// The mark has to be on record before Harbor is asked: it is what a pass that
// starts after a crash right here has to go by.
func TestReconcileRecordsIntentBeforeCreating(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withFinalizer()))
	f.useHarbor(createHook{Client: hc, before: func() {
		if !f.get().Status.CreationPending {
			t.Error("CreateProject was called before creationPending was recorded")
		}
	}})

	f.mustReconcile()

	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
	if f.get().Status.CreationPending {
		t.Error("creationPending is still set after the project was bound")
	}
}

// Harbor created the project but the ID never reached the status. Without the
// mark the next pass would see a stranger's project and refuse it.
func TestReconcileBindsProjectWhoseIDWasLost(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(withFinalizer()))
	f.useHarbor(createHook{Client: hc, after: func(int64, error) (int64, error) {
		return 0, fmt.Errorf("%w: 502 bad gateway", harbor.ErrIDUnresolved)
	}})

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error for a creation whose ID was lost")
	}
	created := hc.Projects()
	if len(created) != 1 {
		t.Fatalf("Harbor holds %d projects, want the one just created", len(created))
	}

	f.useHarbor(hc)
	f.mustReconcile()

	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
	if got := len(hc.Projects()); got != 1 {
		t.Errorf("Harbor holds %d projects, want 1", got)
	}
	if hp := f.get(); hp.Status.ProjectID == nil || created[*hp.Status.ProjectID].Name != harborProjectName {
		t.Errorf("status.projectID = %v, want the ID of the project created first", hp.Status.ProjectID)
	}
}

// Once Harbor has declined, nothing was created. A project that shows up later
// is someone else's and must go through adoptExisting like any other.
func TestReconcileDropsIntentWhenHarborRefuses(t *testing.T) {
	hc := harborfake.New()
	hc.Errs["CreateProject"] = &harbor.APIError{StatusCode: 401, Code: "UNAUTHORIZED", Message: "unauthorized"}
	f := newFixture(t, hc, newProject(withFinalizer()))

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error for a 401")
	}
	if f.get().Status.CreationPending {
		t.Fatal("creationPending survived a refusal")
	}

	delete(hc.Errs, "CreateProject")
	hc.Add(harbor.Project{Name: harborProjectName})
	f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonAdoptionRefused)
}

// A failure that never got an answer leaves open whether Harbor created the
// project, so the mark has to stay.
func TestReconcileKeepsIntentWhenOutcomeIsUnknown(t *testing.T) {
	hc := harborfake.New()
	hc.Errs["CreateProject"] = errors.New("read tcp: connection reset by peer")
	f := newFixture(t, hc, newProject(withFinalizer()))

	if _, err := f.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error")
	}
	if !f.get().Status.CreationPending {
		t.Error("creationPending was dropped although the request may have reached Harbor")
	}
}
