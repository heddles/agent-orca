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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/floppyfish14/agent-orca/test/utils"
)

// TestExternalAPIRateLimiting validates, against a live deployed operator, that a
// tenant configured with requestsPerMinute=1 is rejected with HTTP 429 (plus a
// Retry-After header) on its second rapid task submission. It is credential-free:
// the quota check happens before any pod is scheduled, so no LLM call is made.
var _ = Describe("External API tenant rate limiting", Label("ratelimit"), Ordered, func() {
	const (
		tenantNS         = "tenant-rate-ns"
		tenantName       = "rate-tenant"
		clientID         = "rate-client"
		clientSecret     = "super-secret"
		secretName       = "rate-client-secret"
		agentName        = "rate-test-agent"
		selectorName     = "rate-selector"
		providerName     = "rate-provider"
		providerSecret   = "rate-provider-key"
		apiLocalPort     = 19084
		controllerDeploy = "agent-orca-controller-manager"
	)

	var (
		pfCancel func()
		bearer   string
	)

	startPortForward := func() func() {
		GinkgoHelper()
		cmd := exec.Command("kubectl", "port-forward",
			"deployment/"+controllerDeploy,
			fmt.Sprintf("%d:%d", apiLocalPort, 8084),
			"-n", namespace,
		)
		logf, _ := os.CreateTemp("", "e2e-pf-rl-*.log")
		if logf != nil {
			cmd.Stdout = logf
			cmd.Stderr = logf
		}
		Expect(cmd.Start()).To(Succeed(), "port-forward failed to start")
		Eventually(func(g Gomega) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", apiLocalPort), time.Second)
			if err == nil {
				_ = conn.Close()
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
		}, 60*time.Second, time.Second).Should(Succeed(),
			"port-forward on :%d did not come up within 60s", apiLocalPort)
		return func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if logf != nil {
				_ = logf.Close()
				_ = os.Remove(logf.Name())
			}
		}
	}

	BeforeAll(func() {
		By("creating the tenant namespace")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, tenantNS))).To(Succeed())

		By("registering the client secret and TenantConfig")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  client-secret: %s
`, secretName, namespace, clientSecret))).To(Succeed())

		Expect(applyYAML(fmt.Sprintf(`
apiVersion: agentorca.agentorca.io/v1alpha1
kind: TenantConfig
metadata:
  name: %s
  namespace: %s
spec:
  authMode: issued
  issued:
    clientID: %s
    clientSecretRef:
      name: %s
      key: client-secret
  targetNamespace: %s
  allowedAgents:
    - %s
  rateLimit:
    requestsPerMinute: 1
  budgetPerDayUSD: "1000.00"
`, tenantName, namespace, clientID, secretName, tenantNS, agentName))).To(Succeed())

		By("waiting for the TenantConfig to be reconciled (refreshes the auth cache)")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "tenantconfig", tenantName,
				"-n", namespace,
				"-o", "jsonpath={.status.ready}",
			))
			g.Expect(err).NotTo(HaveOccurred())
			ready, _ := strconv.ParseBool(strings.TrimSpace(out))
			g.Expect(ready).To(BeTrue(), "TenantConfig not ready")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		By("creating a dummy agent + model wiring in the tenant namespace")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  api-key: "e2e-placeholder"
`, providerSecret, tenantNS))).To(Succeed())

		Expect(applyYAML(fmt.Sprintf(`
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelProvider
metadata:
  name: %s
  namespace: %s
spec:
  litellmModel: "openai/gpt-4o"
  credentialsRef:
    name: %s
    key: api-key
  capabilities: [reasoning]
  constraints:
    contextWindow: 128000
    costPerMillionInputTokens: "3.00"
    costPerMillionOutputTokens: "12.00"
`, providerName, tenantNS, providerSecret))).To(Succeed())

		Expect(applyYAML(fmt.Sprintf(`
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelSelector
metadata:
  name: %s
  namespace: %s
spec:
  strategy: rule-based
  providers:
    - name: %s
      weight: 100
`, selectorName, tenantNS, providerName))).To(Succeed())

		Expect(applyYAML(fmt.Sprintf(`
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: %s
  namespace: %s
spec:
  modelSelectorRef: %s
  runtime:
    ociRef: "busybox:1.36"
    framework: openai-compatible
    command: ["sleep"]
    args: ["infinity"]
`, agentName, tenantNS, selectorName))).To(Succeed())

		By("port-forwarding the external API (8084)")
		pfCancel = startPortForward()

		By("obtaining an issued JWT for the tenant")
		form := fmt.Sprintf("grant_type=client_credentials&client_id=%s&client_secret=%s", clientID, clientSecret)
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/oauth/token", apiLocalPort),
			"application/x-www-form-urlencoded", strings.NewReader(form))
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK), "token exchange failed: %s", resp.Status)
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&tok)).To(Succeed())
		Expect(tok.AccessToken).NotTo(BeEmpty())
		bearer = tok.AccessToken
	})

	AfterAll(func() {
		if pfCancel != nil {
			pfCancel()
		}
		deleteResource("agent", agentName, tenantNS)
		deleteResource("modelselector", selectorName, tenantNS)
		deleteResource("modelprovider", providerName, tenantNS)
		deleteResource("secret", providerSecret, tenantNS)
		deleteResource("tenantconfig", tenantName, namespace)
		deleteResource("secret", secretName, namespace)
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", tenantNS,
			"--ignore-not-found", "--wait=true", "--timeout=60s"))
	})

	It("allows the first task submission and rate-limits the second (429 + Retry-After)", func() {
		base := fmt.Sprintf("http://127.0.0.1:%d/v1/tasks", apiLocalPort)
		body := `{"agent":"rate-test-agent","input":"e2e rate limit test"}`

		do := func() (int, string, string) {
			req, err := http.NewRequest(http.MethodPost, base, strings.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b), resp.Header.Get("Retry-After")
		}

		By("submitting the first task (within the 1/min budget)")
		code1, body1, _ := do()
		Expect(code1).To(Equal(http.StatusCreated), "first task should be accepted: %s", body1)

		By("submitting a second task immediately (expect 429)")
		code2, body2, retryAfter := do()
		Expect(code2).To(Equal(http.StatusTooManyRequests),
			"second task should be rate-limited: %s", body2)

		By("receiving a Retry-After header on the 429")
		Expect(retryAfter).NotTo(BeEmpty(), "429 should carry a Retry-After header")
		Expect(body2).To(ContainSubstring("rate limit exceeded"), "429 body should explain: %s", body2)

		// Clean up the one accepted run so it doesn't linger.
		var first struct {
			ID string `json:"id"`
		}
		Expect(json.Unmarshal([]byte(body1), &first)).To(Succeed())
		if first.ID != "" {
			req, _ := http.NewRequest(http.MethodDelete, base+"/"+first.ID, nil)
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			resp.Body.Close()
		}
	})
})
