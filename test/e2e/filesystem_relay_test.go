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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/heddles/agent-orca/test/utils"
)

// fsRelayE2E reports whether the filesystem relay e2e suite should run. It
// needs the ACP API reachable but no model-router or editor — the assertions
// cover the CRD→manifest policy projection and the auth/tenant contract of
// the /runs/{id}/fs/* endpoints.
func fsRelayE2E() bool {
	return os.Getenv("FS_E2E") == "true"
}

// Filesystem relay e2e fixture names/ports.
const (
	fsAgentName        = "e2e-fs-agent"
	fsTenantName       = "fs-e2e-tenant"
	fsTenantSecretName = "fs-e2e-tenant-client-secret"
	fsClientID         = "fs-e2e-client"
	fsAdminSAName      = "agentorca-admin-fs-e2e"
	fsAgentNS          = "default"
	fsACPLocalPort     = 19080
	fsAdminLocalPort   = 19085
)

// fsRequest performs an HTTP call against a locally port-forwarded API.
func fsRequest(port int, method, path, token, body string) (int, string) {
	GinkgoHelper()
	u := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, reader)
	Expect(err).NotTo(HaveOccurred())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// fsTenantToken provisions a clean tenant (with a pre-rotate so re-runs work),
// then exchanges client credentials for a bearer on the ACP API.
func fsTenantToken(adminToken string) string {
	GinkgoHelper()
	By(fmt.Sprintf("provisioning tenant %q", fsTenantName))
	deleteResource("tenantconfig", fsTenantName, namespace)
	deleteResource("secret", fsTenantSecretName, namespace)

	status, respBody := fsRequest(fsAdminLocalPort, http.MethodPost, "/admin/tenants", adminToken, fmt.Sprintf(
		`{"name":"%s","allowedNamespaces":["%s"],"clientID":"%s"}`,
		fsTenantName, "default", fsClientID))
	Expect(status).To(Equal(http.StatusCreated), "create tenant: %s", respBody)
	var tenant struct {
		ClientSecret string `json:"clientSecret"`
	}
	Expect(json.Unmarshal([]byte(respBody), &tenant)).To(Succeed())
	Expect(tenant.ClientSecret).NotTo(BeEmpty())

	By("exchanging client credentials for a tenant bearer on the ACP API")
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", fsClientID)
	form.Set("client_secret", tenant.ClientSecret)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/oauth/token", fsACPLocalPort), strings.NewReader(form.Encode()))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	Expect(resp.StatusCode).To(Equal(http.StatusOK), "token exchange: %s", body)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	Expect(json.Unmarshal(body, &tok)).To(Succeed())
	Expect(tok.AccessToken).NotTo(BeEmpty())
	return tok.AccessToken
}

// adminTokenFor creates a short-lived admin SA token for tenant management,
// mirroring the admin API e2e suite but with a suite-scoped SA name.
func adminTokenFor(saName string) string {
	GinkgoHelper()
	Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: %s
  labels:
    agentorca.io/admin: "true"
`, saName, namespace))).To(Succeed())
	out, err := utils.Run(exec.Command("kubectl", "create", "token", saName,
		"-n", namespace, "--duration=1h"))
	Expect(err).NotTo(HaveOccurred(), "kubectl create token: %s", out)
	return strings.TrimSpace(out)
}

// TestFilesystemRelay validates the ACP fs/* relay contract (issue #34):
//   - the agent manifest exposes the Agent.spec.filesystem policy, so the
//     `aoctl acp serve` bridge can enforce confinement and write approval;
//   - the bridge-side fs endpoints reject unauthenticated callers, resolve
//     unknown runs as 404, and answer pending polls (or 501 when the
//     deployment has no filesystem relay).
var _ = Describe("Filesystem relay (ACP fs/*)", Label("filesystem"), Ordered, func() {
	var (
		pfCancel    func()
		pfAdmin     func()
		tenantToken string
		relayOn     bool // whether the deployment has the filesystem relay wired
	)

	startPortForward := func(remotePort, localPort int) func() {
		GinkgoHelper()
		cmd := exec.Command("kubectl", "port-forward",
			"deployment/agent-orca-controller-manager",
			fmt.Sprintf("%d:%d", localPort, remotePort),
			"-n", namespace,
		)
		logf, _ := os.CreateTemp("", "e2e-pf-fs-*.log")
		if logf != nil {
			cmd.Stdout = logf
			cmd.Stderr = logf
		}
		Expect(cmd.Start()).To(Succeed())
		Eventually(func(g Gomega) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), time.Second)
			if err == nil {
				_ = conn.Close()
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
		}, 60*time.Second, time.Second).Should(Succeed())
		return func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if logf != nil {
				_ = logf.Close()
				_ = os.Remove(logf.Name())
			}
		}
	}

	BeforeAll(func(ctx SpecContext) {
		if !fsRelayE2E() {
			Skip("requires FS_E2E=true (ACP API + filesystem relay config needed)")
		}
		waitForCRDReady("agents.agentorca.agentorca.io")

		By("creating an agent with a filesystem policy")
		deleteResource("agent", fsAgentName, fsAgentNS)
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: %s
  namespace: %s
spec:
  modelSelectorRef: default
  runtime:
    ociRef: "python:3.12-slim"
    framework: openai-compatible
    command: ["sleep"]
    args: ["infinity"]
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
  filesystem:
    mode: readWrite
    writeApproval: true
`, fsAgentName, fsAgentNS))).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command("kubectl", "get", "agent", fsAgentName, "-n", fsAgentNS))
			g.Expect(err).NotTo(HaveOccurred())
		}, 120*time.Second, 2*time.Second).Should(Succeed())

		By("provisioning tenant credentials and port-forwards")
		pfAdmin = startPortForward(8084, fsAdminLocalPort)
		adminToken := adminTokenFor(fsAdminSAName)
		pfCancel = startPortForward(8000, fsACPLocalPort)
		tenantToken = fsTenantToken(adminToken)

		By("probing whether the filesystem relay is wired on this deployment")
		// A 404 for a missing run proves the relay handled the request; a 501
		// means the deployment has no relay (valid for non-redis deployments)
		// and the run-existence/10.1 semantics below are keyed accordingly.
		probeStatus, _ := fsRequest(fsACPLocalPort, http.MethodGet,
			"/runs/e2e-fs-probe-missing/fs/pending", tenantToken, "")
		switch probeStatus {
		case http.StatusNotFound:
			relayOn = true
		case http.StatusNotImplemented:
			relayOn = false
		default:
			Fail(fmt.Sprintf("unexpected fs pending status %d for a missing run", probeStatus))
		}
	})

	AfterAll(func(ctx SpecContext) {
		if !fsRelayE2E() {
			return
		}
		if pfCancel != nil {
			pfCancel()
		}
		if pfAdmin != nil {
			pfAdmin()
		}
		deleteResource("tenantconfig", fsTenantName, namespace)
		deleteResource("secret", fsTenantSecretName, namespace)
		deleteResource("agent", fsAgentName, fsAgentNS)
		deleteResource("serviceaccount", fsAdminSAName, namespace)
	})

	It("exposes the filesystem policy in the agent manifest", func() {
		status, body := fsRequest(fsACPLocalPort, http.MethodGet, fmt.Sprintf("/agents/%s", fsAgentName), tenantToken, "")
		Expect(status).To(Equal(http.StatusOK), "manifest: %s", body)
		var manifest struct {
			Name       string          `json:"name"`
			Filesystem *map[string]any `json:"filesystem,omitempty"`
		}
		Expect(json.Unmarshal([]byte(body), &manifest)).To(Succeed(), "bad manifest JSON: %s", body)
		Expect(manifest.Filesystem).NotTo(BeNil(), "filesystem policy must be in the manifest")
		fs := *manifest.Filesystem
		Expect(fs["mode"]).To(Equal("readWrite"))
		Expect(fs["writeApproval"]).To(Equal(true))
	})

	It("rejects unauthenticated fs polls (401)", func() {
		status, _ := fsRequest(fsACPLocalPort, http.MethodGet, "/runs/e2e-nope/fs/pending", "", "")
		Expect(status).To(Equal(http.StatusUnauthorized))
	})

	It("resolves unknown runs as 404", func() {
		status, _ := fsRequest(fsACPLocalPort, http.MethodGet,
			"/runs/e2e-really-not-a-run/fs/pending", tenantToken, "")
		if relayOn {
			Expect(status).To(Equal(http.StatusNotFound))
		} else {
			// Without a relay the endpoint short-circuits before run lookup.
			Expect(status).To(Equal(http.StatusNotImplemented))
		}
	})

	It("answers pending polls for an existing run (or 501 without a relay)", func() {
		// Create a run we own through the agent run endpoint.
		body := `{"input":[{"role":"user","parts":[{"content_type":"text/plain","content":"hi"}]}],"session":{"id":"e2e-fs-sess"}}`
		status, respBody := fsRequest(fsACPLocalPort, http.MethodPost,
			fmt.Sprintf("/agents/%s/run", fsAgentName), tenantToken, body)
		Expect(status).To(Equal(http.StatusCreated), "create run: %s", respBody)
		var created struct {
			RunID string `json:"run_id"`
		}
		Expect(json.Unmarshal([]byte(respBody), &created)).To(Succeed())
		Expect(created.RunID).NotTo(BeEmpty())

		// Poll the bridge endpoint: pending is empty, or the deployment has no
		// relay wired (valid for non-redis deployments; the flag documents it).
		pollStatus, pollBody := fsRequest(fsACPLocalPort, http.MethodGet,
			fmt.Sprintf("/runs/%s/fs/pending", created.RunID), tenantToken, "")
		switch pollStatus {
		case http.StatusOK:
			relayOn = true
			var out struct {
				Requests []map[string]any `json:"requests"`
			}
			Expect(json.Unmarshal([]byte(pollBody), &out)).To(Succeed(), "bad poll JSON: %s", pollBody)
		case http.StatusNotImplemented:
			relayOn = false
			GinkgoWriter.Printf("filesystem relay not wired on this deployment (501): %s\n", pollBody)
		default:
			Fail(fmt.Sprintf("unexpected fs pending status %d: %s", pollStatus, pollBody))
		}

		// A response post for an unknown request id on our run: 404 when the
		// relay is on (expired request), 501 when off.
		respondStatus, respBody := fsRequest(fsACPLocalPort, http.MethodPost,
			fmt.Sprintf("/runs/%s/fs/pending/no-such-req/response", created.RunID), tenantToken, `{}`)
		if relayOn {
			Expect(respondStatus).To(Equal(http.StatusNotFound), "respond: %s", respBody)
		} else {
			Expect(respondStatus).To(Equal(http.StatusNotImplemented))
		}
	})
})
