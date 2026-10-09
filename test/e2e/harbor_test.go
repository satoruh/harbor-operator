//go:build e2e
// +build e2e

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

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/satoruh/harbor-operator/internal/harbor"
	"github.com/satoruh/harbor-operator/test/utils"
)

const (
	// harborInClusterURL is how the controller Pod reaches Harbor. The URL the
	// tests use is the node port published on the host, which from inside a
	// Pod would be the Pod's own loopback.
	harborInClusterURL = "http://harbor.harbor.svc.cluster.local"

	harborAdminUser = "admin"

	projectNamespace = "default"

	// e2eSyncPeriod is the shortest the CRD accepts. Drift correction and a
	// deletion that was blocked are only retried on this interval.
	e2eSyncPeriod = "30s"
)

// harborAdmin drives Harbor as its administrator, for the parts of a scenario
// that happen behind the operator's back.
type harborAdmin struct {
	baseURL  string
	password string
	projects harbor.Client
}

func newHarborAdmin(baseURL, password string) (*harborAdmin, error) {
	c, err := harbor.New(harbor.Options{BaseURL: baseURL, Username: harborAdminUser, Password: password})
	if err != nil {
		return nil, err
	}
	return &harborAdmin{baseURL: strings.TrimSuffix(baseURL, "/"), password: password, projects: c}, nil
}

func (h *harborAdmin) do(method, url, contentType string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.SetBasicAuth(harborAdminUser, h.password)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		return resp, payload, fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, payload)
	}
	return resp, payload, nil
}

type robotAccount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

type robotAccess struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

type robotPermission struct {
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace"`
	Access    []robotAccess `json:"access"`
}

// operatorPermissions is exactly what the README tells an administrator to
// grant, so that the scenarios below fail if that list falls short of what the
// operator needs.
var operatorPermissions = []robotPermission{
	{Kind: "system", Namespace: "/", Access: []robotAccess{
		{Resource: "project", Action: "list"},
		{Resource: "project", Action: "create"},
	}},
	{Kind: "project", Namespace: "*", Access: []robotAccess{
		{Resource: "project", Action: "read"},
		{Resource: "project", Action: "update"},
		{Resource: "project", Action: "delete"},
	}},
}

// createOnlyPermissions lets a robot create projects without being able to
// see any private one afterwards, its own included.
var createOnlyPermissions = []robotPermission{
	{Kind: "system", Namespace: "/", Access: []robotAccess{
		{Resource: "project", Action: "create"},
	}},
}

func (h *harborAdmin) createRobot(name string, permissions []robotPermission) (*robotAccount, error) {
	body, err := json.Marshal(map[string]any{
		"name":        name,
		"level":       "system",
		"duration":    -1,
		"permissions": permissions,
	})
	if err != nil {
		return nil, err
	}

	_, payload, err := h.do(http.MethodPost, h.baseURL+"/api/v2.0/robots", "application/json", body)
	if err != nil {
		return nil, err
	}
	var robot robotAccount
	if err := json.Unmarshal(payload, &robot); err != nil {
		return nil, err
	}
	return &robot, nil
}

func (h *harborAdmin) deleteRobot(id int64) error {
	_, _, err := h.do(http.MethodDelete, fmt.Sprintf("%s/api/v2.0/robots/%d", h.baseURL, id), "", nil)
	return err
}

// pushArtifact uploads the smallest thing the registry accepts as an image,
// which is enough for Harbor to count a repository in the project.
func (h *harborAdmin) pushArtifact(project, repository string) error {
	const mediaType = "application/vnd.oci.empty.v1+json"
	blob := []byte("{}")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(blob))
	name := project + "/" + repository

	resp, _, err := h.do(http.MethodPost, fmt.Sprintf("%s/v2/%s/blobs/uploads/", h.baseURL, name), "", nil)
	if err != nil {
		return err
	}
	upload := resp.Header.Get("Location")
	if strings.HasPrefix(upload, "/") {
		upload = h.baseURL + upload
	}
	separator := "?"
	if strings.Contains(upload, "?") {
		separator = "&"
	}
	if _, _, err := h.do(http.MethodPut, upload+separator+"digest="+digest, "application/octet-stream", blob); err != nil {
		return err
	}

	descriptor := map[string]any{"mediaType": mediaType, "digest": digest, "size": len(blob)}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        descriptor,
		"layers":        []any{descriptor},
	})
	if err != nil {
		return err
	}
	_, _, err = h.do(http.MethodPut, fmt.Sprintf("%s/v2/%s/manifests/v1", h.baseURL, name),
		"application/vnd.oci.image.manifest.v1+json", manifest)
	return err
}

func (h *harborAdmin) deleteRepository(project, repository string) error {
	_, _, err := h.do(http.MethodDelete,
		fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s", h.baseURL, project, repository), "", nil)
	return err
}

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

func kubectlApply(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

// readyReason reads the reason of the Ready condition; args select the
// resource, for example "harborproject", "name", "-n", "default".
func readyReason(args ...string) (string, error) {
	return kubectl(append(append([]string{"get"}, args...),
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].reason}`)...)
}

// storeCredentials puts a robot's credentials where the operator reads them.
func storeCredentials(secret string, robot *robotAccount) error {
	if _, err := kubectl("create", "secret", "generic", secret, "-n", namespace,
		"--from-literal=username="+robot.Name, "--from-literal=password="+robot.Secret); err != nil {
		return err
	}
	_, err := kubectl("label", "secret", secret, "-n", namespace, "harbor.satoruh.org/credentials=true")
	return err
}

func connectionManifest(name, secret string) string {
	return fmt.Sprintf(`
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborConnection
metadata:
  name: %s
spec:
  baseURL: %s
  credentialsRef:
    name: %s
  allowedProjects:
    - namespace: %s
      names: ["e2e-*"]
  syncPeriod: %s
`, name, harborInClusterURL, secret, projectNamespace, e2eSyncPeriod)
}

// harborScenarios run the deployed operator against the Harbor in the cluster,
// authenticated as a robot. They must be registered inside the container that
// deploys the manager.
func harborScenarios() {
	Context("Harbor", Ordered, func() {
		var (
			admin *harborAdmin
			robot *robotAccount

			// Harbor outlives a run when the cluster is reused, so names
			// carry a suffix that keeps runs apart.
			suffix      = fmt.Sprintf("%d", time.Now().Unix())
			connection  = "e2e-" + suffix
			secret      = "e2e-credentials-" + suffix
			projectName = "e2e-" + suffix
		)

		BeforeAll(func() {
			url := os.Getenv("E2E_HARBOR_URL")
			Expect(url).NotTo(BeEmpty(), "E2E_HARBOR_URL must point at the Harbor in the test cluster")
			password := os.Getenv("E2E_HARBOR_PASSWORD")
			if password == "" {
				password = "Harbor12345"
			}

			var err error
			admin, err = newHarborAdmin(url, password)
			Expect(err).NotTo(HaveOccurred())

			By("creating a system robot with the documented permissions")
			robot, err = admin.createRobot("e2e-"+suffix, operatorPermissions)
			Expect(err).NotTo(HaveOccurred())

			By("storing its credentials in a labelled Secret")
			Expect(storeCredentials(secret, robot)).To(Succeed())

			Expect(kubectlApply(connectionManifest(connection, secret))).To(Succeed())
		})

		AfterAll(func() {
			// A project left behind by a failed scenario keeps its finalizer,
			// and the CRD removal that follows would wait on it forever.
			_, _ = kubectl("patch", "harborproject", projectName, "-n", projectNamespace,
				"--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
			_, _ = kubectl("delete", "harborproject", "--all", "-n", projectNamespace, "--wait=false")
			_, _ = kubectl("delete", "harborconnection", "--all")
			if robot != nil {
				_ = admin.deleteRobot(robot.ID)
			}
		})

		// The reference types carry no namespace, but that only limits what a
		// resource can ask for. What stops the controller from reading other
		// Secrets is this grant.
		It("can read Secrets in its own namespace only, and only by name", func() {
			canI := func(verb, ns string) string {
				// kubectl exits non-zero on "no", so the error is not a failure.
				out, _ := kubectl("auth", "can-i", verb, "secrets", "-n", ns,
					"--as", fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName))
				return out
			}
			Expect(canI("get", namespace)).To(HavePrefix("yes"))
			Expect(canI("list", namespace)).To(HavePrefix("no"))
			Expect(canI("get", projectNamespace)).To(HavePrefix("no"))
			Expect(canI("get", "kube-system")).To(HavePrefix("no"))
		})

		It("reports the connection as connected", func() {
			Eventually(func(g Gomega) {
				reason, err := readyReason("harborconnection", connection)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("Connected"))
			}).Should(Succeed())

			version, err := kubectl("get", "harborconnection", connection, "-o", "jsonpath={.status.harborVersion}")
			Expect(err).NotTo(HaveOccurred())
			Expect(version).NotTo(BeEmpty())
		})

		It("rejects credentials whose Secret lacks the opt-in label", func() {
			const unlabelled = "e2e-unlabelled"
			_, err := kubectl("create", "secret", "generic", unlabelled, "-n", namespace,
				"--from-literal=username="+robot.Name, "--from-literal=password="+robot.Secret)
			Expect(err).NotTo(HaveOccurred())
			Expect(kubectlApply(connectionManifest(unlabelled, unlabelled))).To(Succeed())
			DeferCleanup(func() {
				_, _ = kubectl("delete", "harborconnection", unlabelled)
				_, _ = kubectl("delete", "secret", unlabelled, "-n", namespace)
			})

			Eventually(func(g Gomega) {
				reason, err := readyReason("harborconnection", unlabelled)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("CredentialsInvalid"))
			}).Should(Succeed())
		})

		It("creates the project", func() {
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborProject
metadata:
  name: %s
  namespace: %s
spec:
  projectName: %s
  connectionRef:
    name: %s
  public: false
  deletionPolicy: Delete
`, projectName, projectNamespace, projectName, connection))).To(Succeed())

			Eventually(func(g Gomega) {
				reason, err := readyReason("harborproject", projectName, "-n", projectNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("Synced"))
			}).Should(Succeed())

			p, err := admin.projects.GetProjectByName(context.Background(), projectName)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Public).To(BeFalse())
		})

		// The robot could create this project; the connection is what says
		// this namespace may not ask for it.
		It("leaves Harbor alone for a project the connection does not allow", func() {
			outside := "outside-" + suffix
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborProject
metadata:
  name: %s
  namespace: %s
spec:
  projectName: %s
  connectionRef:
    name: %s
`, outside, projectNamespace, outside, connection))).To(Succeed())
			DeferCleanup(func() {
				_, _ = kubectl("delete", "harborproject", outside, "-n", projectNamespace)
			})

			Eventually(func(g Gomega) {
				reason, err := readyReason("harborproject", outside, "-n", projectNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("NotAllowed"))
			}).Should(Succeed())
			_, err := admin.projects.GetProjectByName(context.Background(), outside)
			Expect(errors.Is(err, harbor.ErrNotFound)).To(BeTrue(), "project was created: %v", err)
		})

		It("puts back a change made in Harbor", func() {
			ctx := context.Background()
			p, err := admin.projects.GetProjectByName(ctx, projectName)
			Expect(err).NotTo(HaveOccurred())
			Expect(admin.projects.UpdateProject(ctx, p.ID, true)).To(Succeed())

			Eventually(func(g Gomega) {
				p, err := admin.projects.GetProjectByName(ctx, projectName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(p.Public).To(BeFalse())
			}).Should(Succeed())
		})

		// A robot is answered 403 for a project that no longer exists, the
		// same as for one it may not see, and the list tells it nothing more.
		// Creating the project is what settles it.
		It("recreates a project that disappeared from Harbor", func() {
			ctx := context.Background()
			vanishing := "e2e-vanishing-" + suffix
			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborProject
metadata:
  name: %s
  namespace: %s
spec:
  projectName: %s
  connectionRef:
    name: %s
  deletionPolicy: Delete
`, vanishing, projectNamespace, vanishing, connection))).To(Succeed())

			var id int64
			Eventually(func(g Gomega) {
				p, err := admin.projects.GetProjectByName(ctx, vanishing)
				g.Expect(err).NotTo(HaveOccurred())
				id = p.ID
				reason, err := readyReason("harborproject", vanishing, "-n", projectNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("Synced"))
			}).Should(Succeed())

			Expect(admin.projects.DeleteProject(ctx, id)).To(Succeed())

			Eventually(func(g Gomega) {
				p, err := admin.projects.GetProjectByName(ctx, vanishing)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(p.ID).NotTo(Equal(id))
				bound, err := kubectl("get", "harborproject", vanishing, "-n", projectNamespace,
					"-o", "jsonpath={.status.projectID}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(bound).To(Equal(fmt.Sprintf("%d", p.ID)))
			}).Should(Succeed())

			_, err := kubectl("delete", "harborproject", vanishing, "-n", projectNamespace, "--timeout=60s")
			Expect(err).NotTo(HaveOccurred())
			_, err = admin.projects.GetProjectByName(ctx, vanishing)
			Expect(errors.Is(err, harbor.ErrNotFound)).To(BeTrue(), "project survived: %v", err)
		})

		// The other half of the same ambiguity: the project is there, and the
		// credentials are too narrow to see it. Harbor refuses the name, and
		// the operator has to report that instead of retrying the creation.
		It("reports a project its credentials cannot see", func() {
			ctx := context.Background()
			hidden := "e2e-hidden-" + suffix

			blind, err := admin.createRobot("e2e-blind-"+suffix, createOnlyPermissions)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = admin.deleteRobot(blind.ID) })
			Expect(storeCredentials(hidden, blind)).To(Succeed())
			Expect(kubectlApply(connectionManifest(hidden, hidden))).To(Succeed())

			id, err := admin.projects.CreateProject(ctx, hidden, false)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = admin.projects.DeleteProject(context.Background(), id) })

			Expect(kubectlApply(fmt.Sprintf(`
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborProject
metadata:
  name: %s
  namespace: %s
spec:
  projectName: %s
  connectionRef:
    name: %s
  deletionPolicy: Delete
`, hidden, projectNamespace, hidden, hidden))).To(Succeed())

			Eventually(func(g Gomega) {
				reason, err := readyReason("harborproject", hidden, "-n", projectNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("ProjectInaccessible"))
			}).Should(Succeed())

			By("leaving the project alone when the resource goes")
			_, err = kubectl("delete", "harborproject", hidden, "-n", projectNamespace, "--timeout=60s")
			Expect(err).NotTo(HaveOccurred())
			p, err := admin.projects.GetProjectByName(ctx, hidden)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.ID).To(Equal(id))
		})

		It("takes over an existing project only when asked to", func() {
			ctx := context.Background()
			existing := "e2e-existing-" + suffix
			id, err := admin.projects.CreateProject(ctx, existing, false)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = kubectl("delete", "harborproject", existing, "-n", projectNamespace)
				_ = admin.projects.DeleteProject(ctx, id)
			})

			manifest := func(adopt bool) string {
				return fmt.Sprintf(`
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborProject
metadata:
  name: %s
  namespace: %s
spec:
  projectName: %s
  connectionRef:
    name: %s
  adoptExisting: %t
`, existing, projectNamespace, existing, connection, adopt)
			}

			Expect(kubectlApply(manifest(false))).To(Succeed())
			Eventually(func(g Gomega) {
				reason, err := readyReason("harborproject", existing, "-n", projectNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("AdoptionRefused"))
			}).Should(Succeed())

			Expect(kubectlApply(manifest(true))).To(Succeed())
			Eventually(func(g Gomega) {
				bound, err := kubectl("get", "harborproject", existing, "-n", projectNamespace,
					"-o", "jsonpath={.status.projectID}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(bound).To(Equal(fmt.Sprintf("%d", id)))
			}).Should(Succeed())
		})

		It("keeps the resource while the project holds repositories, then deletes both", func() {
			const repository = "blocker"
			Expect(admin.pushArtifact(projectName, repository)).To(Succeed())

			_, err := kubectl("delete", "harborproject", projectName, "-n", projectNamespace, "--wait=false")
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				reason, err := readyReason("harborproject", projectName, "-n", projectNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("DeletionBlocked"))
			}).Should(Succeed())

			By("finding the event, which the controller may only write with its events.k8s.io grant")
			Eventually(func(g Gomega) {
				reasons, err := kubectl("get", "events.events.k8s.io", "-n", projectNamespace,
					"--field-selector", "regarding.name="+projectName, "-o", "jsonpath={.items[*].reason}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reasons).To(ContainSubstring("DeletionBlocked"))
			}).Should(Succeed())

			Expect(admin.deleteRepository(projectName, repository)).To(Succeed())

			Eventually(func(g Gomega) {
				_, err := kubectl("get", "harborproject", projectName, "-n", projectNamespace)
				g.Expect(err).To(MatchError(ContainSubstring("NotFound")))
			}).Should(Succeed())

			_, err = admin.projects.GetProjectByName(context.Background(), projectName)
			Expect(errors.Is(err, harbor.ErrNotFound)).To(BeTrue(), "project still in Harbor: %v", err)
		})
	})
}
