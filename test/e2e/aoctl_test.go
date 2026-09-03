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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/floppyfish14/agent-orca/test/utils"
)

// TestAOCTL validates the aoctl CLI end-to-end against a live operator:
//   - `aoctl login` exchanges client_credentials and caches the token
//   - `aoctl tasks submit` creates a task (201) using the cached token
//   - `aoctl tasks get` / `aoctl tasks ls` read it back
//
// No LLM credentials are needed: the task is created and read before the pod
// can run to completion.
var _ = Describe("aoctl CLI", Label("aoctl"), Ordered, func() {
	const (
		tenantNS     = "tenant-aoctl-ns"
		tenantName   = "aoctl-tenant"
		clientID     = "aoctl-client"
		clientSecret = "aoctl-secret"
		secretName   = "aoctl-client-secret"
		agentName    = "aoctl-agent"
		selectorName = "aoctl-selector"
		providerName = "aoctl-provider"
		providerKey  = "aoctl-provider-key"
		apiLocalPort = 19086
	)

	var (
		pfCancel func()
		aoctlBin string
		cfgDir   string
	)

	startPortForward := func() func() {
		GinkgoHelper()
		cmd := exec.Command("kubectl", "port-forward",
			"deployment/agent-orca-controller-manager",
			fmt.Sprintf("%d:%d", apiLocalPort, 8084),
			"-n", namespace)
		logf, _ := os.CreateTemp("", "e2e-pf-aoctl-*.log")
		if logf != nil {
			cmd.Stdout = logf
			cmd.Stderr = logf
		}
		Expect(cmd.Start()).To(Succeed())
		Eventually(func(g Gomega) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", apiLocalPort), time.Second)
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

	// runAOCTL runs the aoctl binary with the given args, sharing a single
	// AOCTL_CONFIG_DIR (created in BeforeAll) so a token saved by `aoctl login` is
	// visible to later `aoctl tasks ...` invocations.
	runAOCTL := func(env map[string]string, args ...string) (string, error) {
		GinkgoHelper()
		dir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		bin := filepath.Join(dir, "bin", "aoctl")
		Expect(bin).To(BeAnExistingFile(), "build aoctl first with: make aoctl")
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "AOCTL_CONFIG_DIR="+cfgDir)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	BeforeAll(func() {
		By("creating the tenant namespace + client secret")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, tenantNS))).To(Succeed())
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  client-secret: %s
`, secretName, namespace, clientSecret))).To(Succeed())

		By("registering the TenantConfig (issued, no rate limit so submit is allowed)")
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
  allowedNamespaces:
    - %s
  allowedAgents:
    - %s
`, tenantName, namespace, clientID, secretName, tenantNS, agentName))).To(Succeed())

		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "tenantconfig", tenantName,
				"-n", namespace, "-o", "jsonpath={.status.ready}"))
			g.Expect(err).NotTo(HaveOccurred())
			ready, _ := strconv.ParseBool(strings.TrimSpace(out))
			g.Expect(ready).To(BeTrue())
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		By("creating a dummy agent + model wiring")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  api-key: "e2e-placeholder"
`, providerKey, tenantNS))).To(Succeed())
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
`, providerName, tenantNS, providerKey))).To(Succeed())
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

		By("building the aoctl binary")
		dir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("go", "build", "-o", filepath.Join(dir, "bin", "aoctl"), "./cmd/aoctl"))
		Expect(err).NotTo(HaveOccurred(), "building aoctl")
		aoctlBin = filepath.Join(dir, "bin", "aoctl")
		Expect(aoctlBin).To(BeAnExistingFile())

		By("creating an isolated config dir for aoctl tokens")
		cfgDir, err = os.MkdirTemp("", "aoctl-config-*")
		Expect(err).NotTo(HaveOccurred())

		By("port-forwarding the external API (8084)")
		pfCancel = startPortForward()
	})

	AfterAll(func() {
		if pfCancel != nil {
			pfCancel()
		}
		if cfgDir != "" {
			_ = os.RemoveAll(cfgDir)
		}
		deleteResource("agent", agentName, tenantNS)
		deleteResource("modelselector", selectorName, tenantNS)
		deleteResource("modelprovider", providerName, tenantNS)
		deleteResource("secret", providerKey, tenantNS)
		deleteResource("tenantconfig", tenantName, namespace)
		deleteResource("secret", secretName, namespace)
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", tenantNS,
			"--ignore-not-found", "--wait=true", "--timeout=60s"))
	})

	It("logs in, submits, and reads a task through the CLI", func() {
		endpoint := fmt.Sprintf("http://127.0.0.1:%d", apiLocalPort)

		By("logging in (token cached to config)")
		out, err := runAOCTL(map[string]string{"AOCTL_ENDPOINT": endpoint, "AOCTL_ACP_ENDPOINT": endpoint},
			"login", "--endpoint", endpoint, "--acp-endpoint", endpoint,
			"--client-id", clientID, "--client-secret", clientSecret)
		Expect(err).NotTo(HaveOccurred(), "login: %s", out)
		Expect(out).To(ContainSubstring("logged in"), "login output: %s", out)

		By("submitting a task with the cached token")
		out, err = runAOCTL(nil, "tasks", "submit",
			"--endpoint", endpoint, "--agent", agentName, "--input", "aoctl e2e hello")
		Expect(err).NotTo(HaveOccurred(), "submit: %s", out)
		var tr struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		Expect(json.Unmarshal([]byte(strings.TrimSpace(out)), &tr)).To(Succeed(),
			"submit output was not JSON: %q", out)
		Expect(tr.ID).NotTo(BeEmpty())
		Expect(tr.Status).To(Equal("Pending"), "create response: %q", out)

		By("reading the task back via `aoctl tasks get`")
		out, err = runAOCTL(nil, "tasks", "get", tr.ID,
			"--endpoint", endpoint)
		Expect(err).NotTo(HaveOccurred(), "get: %s", out)
		Expect(out).To(ContainSubstring(agentName), "get output: %q", out)

		By("listing tasks via `aoctl tasks ls`")
		out, err = runAOCTL(nil, "tasks", "ls", "--endpoint", endpoint)
		Expect(err).NotTo(HaveOccurred(), "ls: %s", out)
		Expect(out).To(ContainSubstring(tr.ID), "ls output should contain the task: %q", out)

		By("cleaning up the created task")
		_, _ = runAOCTL(nil, "tasks", "cancel", tr.ID, "--endpoint", endpoint)
	})
})
