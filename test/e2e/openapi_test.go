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
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/floppyfish14/agent-orc/test/utils"
)

// TestExternalAPIOpenAPIContract validates that a live, deployed operator serves
// its machine-readable API contract on the external ports. This is the e2e
// gate for the "integration-readiness" roadmap Stage 1.
var _ = Describe("External API OpenAPI contract", Label("openapi"), Ordered, func() {
	var (
		openAPICancel    func()
		acpCancel        func()
		openAPILocal     = 18084
		acpLocal         = 18000
		controllerDeploy = "agent-orc-controller-manager"
	)

	// startPortForward launches kubectl port-forward for <deploy>:<port> and
	// blocks until the local port accepts connections. Returns a cleanup func.
	startPortForward := func(localPort, remotePort int) func() {
		GinkgoHelper()
		cmd := exec.Command("kubectl", "port-forward",
			"deployment/"+controllerDeploy,
			fmt.Sprintf("%d:%d", localPort, remotePort),
			"-n", namespace,
		)
		logf, _ := os.CreateTemp("", "e2e-pf-openapi-*.log")
		if logf != nil {
			cmd.Stdout = logf
			cmd.Stderr = logf
		}
		Expect(cmd.Start()).To(Succeed(), "port-forward failed to start")

		// Wait for the local port to accept a TCP connection.
		Eventually(func(g Gomega) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), time.Second)
			if err == nil {
				_ = conn.Close()
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
		}, 60*time.Second, time.Second).Should(Succeed(),
			"port-forward on :%d did not come up within 60s", localPort)

		return func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if logf != nil {
				_ = logf.Close()
				_ = os.Remove(logf.Name())
			}
		}
	}

	httpGet := func(port int, path string) (int, string) {
		GinkgoHelper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	BeforeAll(func() {
		// Wait for the controller deployment to exist before port-forwarding.
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command(
				"kubectl", "get", "deployment", controllerDeploy, "-n", namespace,
			))
			g.Expect(err).NotTo(HaveOccurred(), "controller-manager deployment missing")
		}, 90*time.Second, 2*time.Second).Should(Succeed(),
			"controller-manager deployment did not appear within 90s")

		openAPICancel = startPortForward(openAPILocal, 8084)
		acpCancel = startPortForward(acpLocal, 8000)
	})

	AfterAll(func() {
		if openAPICancel != nil {
			openAPICancel()
		}
		if acpCancel != nil {
			acpCancel()
		}
	})

	It("serves the External Task API OpenAPI document on port 8084", func() {
		status, body := httpGet(openAPILocal, "/openapi.json")
		Expect(status).To(Equal(http.StatusOK), "body: %s", body)

		var doc map[string]interface{}
		Expect(json.Unmarshal([]byte(body), &doc)).To(Succeed(), "openapi.json is not JSON: %s", body)
		Expect(doc["openapi"]).To(Equal("3.1.0"))
		paths, ok := doc["paths"].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(paths).To(HaveKey("/v1/tasks"), "spec must include /v1/tasks")
		Expect(paths).To(HaveKey("/oauth/token"))
	})

	It("serves the ACP API OpenAPI document on port 8000", func() {
		status, body := httpGet(acpLocal, "/openapi.json")
		Expect(status).To(Equal(http.StatusOK), "body: %s", body)

		var doc map[string]interface{}
		Expect(json.Unmarshal([]byte(body), &doc)).To(Succeed(), "openapi.json is not JSON: %s", body)
		paths, ok := doc["paths"].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(paths).To(HaveKey("/agents"))
		Expect(paths).To(HaveKey("/runs"))
	})

	It("keeps /ping unauthenticated (liveness)", func() {
		status, body := httpGet(acpLocal, "/ping")
		Expect(status).To(Equal(http.StatusOK), "body: %s", body)
		var m map[string]string
		Expect(json.Unmarshal([]byte(body), &m)).To(Succeed())
		Expect(m["status"]).To(Equal("ok"))
	})

	It("exposes probe + metrics endpoints on the external servers", func() {
		By("External Task API (8084) probes")
		for _, path := range []string{"/healthz", "/readyz", "/version"} {
			status, body := httpGet(openAPILocal, path)
			Expect(status).To(Equal(http.StatusOK), "%s: %s", path, body)
		}
		status, body := httpGet(openAPILocal, "/metrics")
		Expect(status).To(Equal(http.StatusOK), "/metrics: %s", body)
		Expect(body).To(ContainSubstring("agentorc_external_requests_total"),
			"/metrics should expose the request counter:\n%s", body)

		By("ACP API (8000) liveness")
		status, body = httpGet(acpLocal, "/healthz")
		Expect(status).To(Equal(http.StatusOK), "/healthz: %s", body)
	})
})
