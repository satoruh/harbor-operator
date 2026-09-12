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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	recorder *record.FakeRecorder
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

	recorder := record.NewFakeRecorder(10)

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
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName, Public: false})
	f := newFixture(t, hc, newProject(withPublic(true), bound(7)))

	f.mustReconcile()

	f.assertCalled(t, "UpdateProject", true)
	if p := hc.Projects()[7]; !p.Public {
		t.Error("project is still private after reconcile")
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
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
	// Retrying cannot help; only a spec change can, and that reconciles anyway.
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue", res.RequeueAfter)
	}
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

// A 403 says nothing about existence. The project is still reachable by name,
// so the stale ID is replaced rather than the project recreated.
func TestReconcileRebindsWhenStoredIDIsStale(t *testing.T) {
	hc := harborfake.New(harbor.Project{ID: 7, Name: harborProjectName})
	f := newFixture(t, hc, newProject(bound(42)))

	f.mustReconcile()

	f.assertCalled(t, "CreateProject", false)
	hp := f.get()
	if hp.Status.ProjectID == nil || *hp.Status.ProjectID != 7 {
		t.Errorf("status.projectID = %v, want 7", hp.Status.ProjectID)
	}
	f.assertReady(t, metav1.ConditionTrue, harborv1alpha1.ReasonSynced)
}

// Neither the ID nor the name resolves: the project was deleted or these
// credentials cannot see it, and Harbor does not say which.
func TestReconcileReportsInaccessibleProject(t *testing.T) {
	hc := harborfake.New()
	f := newFixture(t, hc, newProject(bound(42)))

	res := f.mustReconcile()

	f.assertReady(t, metav1.ConditionFalse, harborv1alpha1.ReasonProjectInaccessible)
	f.assertCalled(t, "CreateProject", false)
	if hp := f.get(); hp.Status.ProjectID == nil || *hp.Status.ProjectID != 42 {
		t.Errorf("status.projectID = %v, want it kept at 42", hp.Status.ProjectID)
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
	hc.Errs["GetProjectByName"] = harbor.ErrAlreadyExists
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
	f := newFixture(t, hc, newProject(bound(7), deleting()))

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
