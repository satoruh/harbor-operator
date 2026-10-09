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
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
	harborfake "github.com/satoruh/harbor-operator/internal/harbor/fake"
)

// unstableProjectName is the one project scriptedHarbor refuses to look up.
const unstableProjectName = "unstable"

// scriptedHarbor is the fake Harbor with one scripted fault: looking up
// unstableProjectName fails, and never with the same message twice.
type scriptedHarbor struct {
	*harborfake.Client
	unstableLookups atomic.Int64
	readsByID       atomic.Int64
}

func (s *scriptedHarbor) GetProjectByID(ctx context.Context, id int64) (*harbor.Project, error) {
	s.readsByID.Add(1)
	return s.Client.GetProjectByID(ctx, id)
}

func newScriptedHarbor() *scriptedHarbor {
	return &scriptedHarbor{Client: harborfake.New()}
}

func (s *scriptedHarbor) GetProjectByName(ctx context.Context, name string) (*harbor.Project, error) {
	if name != unstableProjectName {
		return s.Client.GetProjectByName(ctx, name)
	}
	// The local port is what makes real network errors differ between
	// attempts.
	n := s.unstableLookups.Add(1)
	return nil, fmt.Errorf("read tcp 10.0.0.1:%d->10.0.0.2:443: read: connection reset by peer", 40000+n)
}

var _ = Describe("controllers against an API server", Ordered, func() {
	const namespace = "default"

	BeforeAll(func() {
		Expect(k8sClient.Create(ctx, newSecret())).To(Succeed())
		conn := newConnection()
		conn.Spec.BaseURL = "https://harbor.example.invalid"
		conn.Spec.AllowedProjects = []harborv1alpha1.ProjectRule{{Namespace: namespace, Names: []string{"*"}}}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
	})

	project := func(name, projectName string, policy harborv1alpha1.DeletionPolicy) *harborv1alpha1.HarborProject {
		return &harborv1alpha1.HarborProject{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: harborv1alpha1.HarborProjectSpec{
				ConnectionRef:  harborv1alpha1.ConnectionReference{Name: connectionName},
				ProjectName:    projectName,
				DeletionPolicy: policy,
			},
		}
	}

	readyReason := func(obj client.Object, conditions func() []metav1.Condition) func() string {
		return func() string {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				return err.Error()
			}
			c := meta.FindStatusCondition(conditions(), harborv1alpha1.ConditionReady)
			if c == nil {
				return ""
			}
			return c.Reason
		}
	}

	It("reports the connection as connected", func() {
		conn := &harborv1alpha1.HarborConnection{ObjectMeta: metav1.ObjectMeta{Name: connectionName}}
		Eventually(readyReason(conn, func() []metav1.Condition { return conn.Status.Conditions })).
			WithTimeout(10 * time.Second).Should(Equal(harborv1alpha1.ReasonConnected))
	})

	It("carries a project from creation to deletion", func() {
		hp := project("lifecycle", "lifecycle", harborv1alpha1.DeletionPolicyDelete)
		Expect(k8sClient.Create(ctx, hp)).To(Succeed())

		// Reaching Synced takes two passes: the first only adds the
		// finalizer and relies on that write to trigger the second.
		Eventually(readyReason(hp, func() []metav1.Condition { return hp.Status.Conditions })).
			WithTimeout(10 * time.Second).Should(Equal(harborv1alpha1.ReasonSynced))
		Expect(suiteHarbor.Projects()).To(HaveLen(1))

		Expect(k8sClient.Delete(ctx, hp)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(hp), hp))
		}).WithTimeout(10 * time.Second).Should(BeTrue())
		Expect(suiteHarbor.Projects()).To(BeEmpty())
	})

	It("backs off when Harbor fails with a different message each time", func() {
		hp := project("unstable", unstableProjectName, harborv1alpha1.DeletionPolicyOrphan)
		Expect(k8sClient.Create(ctx, hp)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, hp)).To(Succeed()) })

		Eventually(suiteHarbor.unstableLookups.Load).WithTimeout(10 * time.Second).Should(BeNumerically(">", 0))

		// Every failed pass writes its message to the status. If that write
		// triggered the next pass, the retries would run back to back; with
		// the backoff intact, doubling from 5ms allows about ten in 3s.
		Consistently(suiteHarbor.unstableLookups.Load).
			WithTimeout(3 * time.Second).WithPolling(100 * time.Millisecond).
			Should(BeNumerically("<", 20))
	})

	// The resync is 90s here, so a pass seen within seconds of the edit can
	// only have been triggered by it.
	It("reconciles a project again when its connection changes", func() {
		hp := project("follows-connection", "follows-connection", harborv1alpha1.DeletionPolicyDelete)
		Expect(k8sClient.Create(ctx, hp)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, hp)).To(Succeed()) })
		Eventually(readyReason(hp, func() []metav1.Condition { return hp.Status.Conditions })).
			WithTimeout(10 * time.Second).Should(Equal(harborv1alpha1.ReasonSynced))

		Consistently(suiteHarbor.readsByID.Load).WithTimeout(time.Second).Should(BeZero())

		conn := &harborv1alpha1.HarborConnection{ObjectMeta: metav1.ObjectMeta{Name: connectionName}}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(conn), conn)).To(Succeed())
		conn.Spec.BaseURL = "https://harbor-2.example.invalid"
		Expect(k8sClient.Update(ctx, conn)).To(Succeed())

		Eventually(suiteHarbor.readsByID.Load).WithTimeout(10 * time.Second).Should(BeNumerically(">", 0))
	})

	It("rejects a change of projectName", func() {
		hp := project("immutable-name", "immutable-name", harborv1alpha1.DeletionPolicyOrphan)
		Expect(k8sClient.Create(ctx, hp)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, hp)).To(Succeed()) })

		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(hp), hp); err != nil {
				return err
			}
			hp.Spec.ProjectName = "another-name"
			return k8sClient.Update(ctx, hp)
		}).Should(MatchError(ContainSubstring("projectName is immutable")))
	})

	It("rejects a change of connectionRef", func() {
		hp := project("immutable-connection", "immutable-connection", harborv1alpha1.DeletionPolicyOrphan)
		Expect(k8sClient.Create(ctx, hp)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, hp)).To(Succeed()) })

		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(hp), hp); err != nil {
				return err
			}
			hp.Spec.ConnectionRef.Name = "another-harbor"
			return k8sClient.Update(ctx, hp)
		}).Should(MatchError(ContainSubstring("connectionRef is immutable")))
	})
})
