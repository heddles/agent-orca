//go:build e2e
// +build e2e

package e2e

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/agentorc/agent-orc/test/utils"
)

// splitPodE2E groups the supporting resources and helpers for split-pod topology tests.
// All tests verify operator resource creation behaviour
// (NetworkPolicies, Service, pod labels, OPENAI_BASE_URL in token Secret) rather than
// actual LLM routing, so no model-router image is needed.
var _ = Describe("Split-pod egress isolation", Label("split-pod"), Ordered, func() {
	const (
		ns      = "default"
		runName = "e2e-split-run"
	)

	BeforeAll(func() {
		waitForCRDReady("agents.agentorc.agentorc.io")
		waitForCRDReady("agentruns.agentorc.agentorc.io")

		// Clean up from any previous run of this test.
		for _, kind := range []string{"agentrun", "agent", "modelselector", "modelprovider"} {
			deleteResource(kind, "e2e-split-provider", ns)
			deleteResource(kind, "e2e-split-selector", ns)
			deleteResource(kind, "e2e-split-agent", ns)
			deleteResource(kind, runName, ns)
		}
		deleteResource("agentrun", runName, ns)
		deleteResource("agent", "e2e-split-agent", ns)
		deleteResource("modelselector", "e2e-split-selector", ns)
		deleteResource("modelprovider", "e2e-split-provider", ns)
		deleteResource("secret", "e2e-split-api-key", ns)

		// Minimal provider credential secret.
		Expect(applyYAML(`
apiVersion: v1
kind: Secret
metadata:
  name: e2e-split-api-key
  namespace: default
stringData:
  api-key: "e2e-placeholder"
`)).To(Succeed())

		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: ModelProvider
metadata:
  name: e2e-split-provider
  namespace: default
spec:
  litellmModel: "openai/gpt-4.1"
  credentialsRef:
    name: e2e-split-api-key
    key: api-key
  capabilities: [reasoning]
  latencyProfile: medium
  constraints:
    contextWindow: 128000
    maxOutputTokens: 4096
    costPerMillionInputTokens: "3.00"
    costPerMillionOutputTokens: "12.00"
`)).To(Succeed())

		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: ModelSelector
metadata:
  name: e2e-split-selector
  namespace: default
spec:
  strategy: rule-based
  providers:
    - name: e2e-split-provider
      weight: 100
`)).To(Succeed())

		// Agent with splitPod enabled.
		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: Agent
metadata:
  name: e2e-split-agent
  namespace: default
spec:
  modelSelectorRef: e2e-split-selector
  networkIsolation:
    splitPod: true
  runtime:
    ociRef: "busybox:1.36"
    framework: openai-compatible
    command: ["sleep"]
    args: ["infinity"]
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 64Mi
`)).To(Succeed())

		// AgentRun referencing the split-pod agent.
		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentRun
metadata:
  name: ` + runName + `
  namespace: default
spec:
  agentRef: e2e-split-agent
  input: "e2e split-pod topology test"
`)).To(Succeed())
	})

	AfterAll(func() {
		deleteResource("agentrun", runName, ns)
		deleteResource("agent", "e2e-split-agent", ns)
		deleteResource("modelselector", "e2e-split-selector", ns)
		deleteResource("modelprovider", "e2e-split-provider", ns)
		deleteResource("secret", "e2e-split-api-key", ns)
	})

	It("creates the router Service for the split-pod run", func() {
		svcName := "agentorc-router-" + runName
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "service", svcName,
				"-n", ns,
				"-o", "jsonpath={.spec.type}",
			))
			g.Expect(err).NotTo(HaveOccurred(), "router Service %s not found", svcName)
			g.Expect(out).To(Equal("ClusterIP"))
		}, 60*time.Second, 2*time.Second).Should(Succeed(),
			"router Service %s did not appear within 60s", svcName)
	})

	It("router Service exposes ports 8080 (openai) and 8082 (gemini)", func() {
		svcName := "agentorc-router-" + runName
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "service", svcName,
				"-n", ns,
				"-o", `jsonpath={range .spec.ports[*]}{.port}{","}{end}`,
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("8080"), "service should expose port 8080")
			g.Expect(out).To(ContainSubstring("8082"), "service should expose port 8082")
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("creates the router pod with component=router label", func() {
		routerPodName := "agentorc-router-" + runName
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "pod", routerPodName,
				"-n", ns,
				"-o", "jsonpath={.metadata.labels.agentorc\\.io/component}",
			))
			g.Expect(err).NotTo(HaveOccurred(), "router pod %s not found", routerPodName)
			g.Expect(out).To(Equal("router"))
		}, 60*time.Second, 2*time.Second).Should(Succeed(),
			"router pod did not appear with component=router label within 60s")
	})

	It("creates the agent pod with component=agent label", func() {
		agentPodName := "agentorc-run-" + runName
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "pod", agentPodName,
				"-n", ns,
				"-o", "jsonpath={.metadata.labels.agentorc\\.io/component}",
			))
			g.Expect(err).NotTo(HaveOccurred(), "agent pod %s not found", agentPodName)
			g.Expect(out).To(Equal("agent"))
		}, 60*time.Second, 2*time.Second).Should(Succeed(),
			"agent pod did not appear with component=agent label within 60s")
	})

	It("router pod has no agent container (single-container pod)", func() {
		routerPodName := "agentorc-router-" + runName
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "pod", routerPodName,
				"-n", ns,
				"-o", `jsonpath={range .spec.containers[*]}{.name}{"\n"}{end}`,
			))
			g.Expect(err).NotTo(HaveOccurred())
			// Must have model-router container but NOT agent container.
			g.Expect(out).To(ContainSubstring("model-router"))
			g.Expect(out).NotTo(ContainSubstring("agent"),
				"router pod should not contain an agent container")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("agent pod has no model-router init container (sidecar removed)", func() {
		agentPodName := "agentorc-run-" + runName
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "pod", agentPodName,
				"-n", ns,
				"-o", `jsonpath={range .spec.initContainers[*]}{.name}{"\n"}{end}`,
			))
			g.Expect(err).NotTo(HaveOccurred())
			// The sidecar model-router init container must NOT be present.
			g.Expect(out).NotTo(ContainSubstring("model-router"),
				"agent pod should not contain the model-router sidecar in split-pod mode")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("injects the router Service URL (not localhost) into the token secret as OPENAI_BASE_URL", func() {
		secretName := "agentorc-run-" + runName + "-token"
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "secret", secretName,
				"-n", ns,
				"-o", "jsonpath={.data.OPENAI_BASE_URL}",
			))
			g.Expect(err).NotTo(HaveOccurred(), "token secret %s not found", secretName)
			g.Expect(out).NotTo(BeEmpty(), "OPENAI_BASE_URL not yet in secret")

			decoded, err := base64.StdEncoding.DecodeString(out)
			g.Expect(err).NotTo(HaveOccurred())
			url := string(decoded)

			// Must point at the router Service, not localhost.
			g.Expect(url).NotTo(ContainSubstring("localhost"),
				"OPENAI_BASE_URL should not be localhost in split-pod mode")
			g.Expect(url).To(ContainSubstring("agentorc-router-"+runName),
				"OPENAI_BASE_URL should contain the router Service name")
			g.Expect(url).To(ContainSubstring(":8080"),
				"OPENAI_BASE_URL should target port 8080")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	It("creates a router-pod NetworkPolicy (agentorc-router-<run>) with egress and agent-ingress", func() {
		npName := "agentorc-router-" + runName
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command(
				"kubectl", "get", "networkpolicy", npName, "-n", ns,
			))
			g.Expect(err).NotTo(HaveOccurred(), "router NetworkPolicy %s not found", npName)
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Router NP must have ingress rules (from agent pod).
		out, err := utils.Run(exec.Command(
			"kubectl", "get", "networkpolicy", npName,
			"-n", ns,
			"-o", "jsonpath={.spec.ingress}",
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(Equal("[]"), "router NP should have at least one ingress rule")

		// Router NP pod selector must target component=router.
		sel, err := utils.Run(exec.Command(
			"kubectl", "get", "networkpolicy", npName,
			"-n", ns,
			"-o", `jsonpath={.spec.podSelector.matchLabels.agentorc\.io/component}`,
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(Equal("router"), "router NP podSelector should match component=router")
	})

	It("creates an agent-pod NetworkPolicy (agentorc-agent-<run>) with restricted egress", func() {
		npName := "agentorc-agent-" + runName
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command(
				"kubectl", "get", "networkpolicy", npName, "-n", ns,
			))
			g.Expect(err).NotTo(HaveOccurred(), "agent NetworkPolicy %s not found", npName)
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		// Agent NP must deny all ingress (empty ingress list).
		out, err := utils.Run(exec.Command(
			"kubectl", "get", "networkpolicy", npName,
			"-n", ns,
			"-o", "jsonpath={.spec.ingress}",
		))
		Expect(err).NotTo(HaveOccurred())
		// When ingress is omitted/nil (deny-all), kubectl jsonpath returns ""; an explicit
		// empty slice serializes the same way due to omitempty on the field.
		Expect(out).To(BeEmpty(), "agent NP ingress should be absent/empty (deny all)")

		// Agent NP pod selector must target component=agent.
		sel, err := utils.Run(exec.Command(
			"kubectl", "get", "networkpolicy", npName,
			"-n", ns,
			"-o", `jsonpath={.spec.podSelector.matchLabels.agentorc\.io/component}`,
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(Equal("agent"), "agent NP podSelector should match component=agent")
	})

	It("does NOT create the combined-pod NetworkPolicy (agentorc-run-<run>)", func() {
		// In split-pod mode the controller creates two separate NPs, not the combined one.
		combinedNP := "agentorc-run-" + runName
		// Give a moment in case the controller accidentally creates it.
		Consistently(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "networkpolicy", combinedNP, "-n", ns,
			))
			// We expect the resource to NOT exist (error from kubectl get means not found).
			if err == nil {
				g.Expect(out).To(BeEmpty(), "combined NP %s should not exist in split-pod mode", combinedNP)
			}
		}, 10*time.Second, 2*time.Second).Should(Succeed())
	})

	It("records routerPodName in AgentRun status", func() {
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "agentrun", runName,
				"-n", ns,
				"-o", "jsonpath={.status.routerPodName}",
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("agentorc-router-"),
				"status.routerPodName should be set to the router pod name")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	})

	// --- Egress enforcement: verify the NetworkPolicy is actually enforced at the CNI layer ---
	//
	// These tests exec into Running pods to probe network connectivity.
	// They rely on standard Kubernetes NetworkPolicy support — no special CNI is required.
	// They must run BEFORE the GC test which deletes the AgentRun and its pods.
	Context("egress enforcement", func() {
		const probeTimeout = "5"

		BeforeEach(func() {
			// Ensure the agent pod is Running before probing.
			waitForPodPhase("agentorc-run-"+runName, ns, "Running", 4*time.Minute)
		})

		It("agent pod CANNOT connect to api.openai.com:443 (provider HTTPS blocked by NetworkPolicy)", func() {
			// Core scenario from issue #13: an agent process must not be able to reach LLM
			// providers directly — all traffic must go via the model-router pod.
			agentPod := "agentorc-run-" + runName
			_, err := execInPod(ns, agentPod, "agent",
				"nc", "-z", "-w", probeTimeout, "api.openai.com", "443",
			)
			Expect(err).To(HaveOccurred(),
				"agent pod in split-pod mode must NOT reach api.openai.com:443 — "+
					"NetworkPolicy should block all egress except to the router pod and DNS")
		})

		It("agent pod CANNOT connect to api.anthropic.com:443 (provider HTTPS blocked)", func() {
			agentPod := "agentorc-run-" + runName
			_, err := execInPod(ns, agentPod, "agent",
				"nc", "-z", "-w", probeTimeout, "api.anthropic.com", "443",
			)
			Expect(err).To(HaveOccurred(),
				"agent pod must not reach api.anthropic.com:443")
		})

		It("agent pod has DNS configured (port 53 egress permitted by NetworkPolicy)", func() {
			// Verify DNS is configured in the container and the cluster DNS server is reachable
			// on port 53. Uses /etc/resolv.conf + nc since chainguard busybox lacks nslookup.
			agentPod := "agentorc-run-" + runName
			// Read the nameserver IP from resolv.conf.
			dnsOut, err := execInPod(ns, agentPod, "agent",
				"sh", "-c", `grep "^nameserver" /etc/resolv.conf | head -1`,
			)
			Expect(err).NotTo(HaveOccurred(), "failed to read /etc/resolv.conf")
			Expect(dnsOut).To(ContainSubstring("nameserver"),
				"/etc/resolv.conf should have a nameserver — DNS is not configured")

			// Extract IP and verify port 53 is reachable (NetworkPolicy allows UDP/TCP 53).
			dnsIP, err := execInPod(ns, agentPod, "agent",
				"sh", "-c", `grep "^nameserver" /etc/resolv.conf | head -1 | sed 's/nameserver //'`,
			)
			Expect(err).NotTo(HaveOccurred())
			dnsIP = strings.TrimSpace(dnsIP)
			Expect(dnsIP).NotTo(BeEmpty())
			_, err = execInPod(ns, agentPod, "agent",
				"nc", "-z", "-w", probeTimeout, dnsIP, "53",
			)
			Expect(err).NotTo(HaveOccurred(),
				"agent pod should reach cluster DNS on port 53 (allowed by NetworkPolicy) — ip: %s", dnsIP)
		})
	})

	It("garbage-collects router Service and both pods when the AgentRun is deleted", func() {
		deleteResource("agentrun", runName, ns)

		routerSvc := "agentorc-router-" + runName
		routerPod := "agentorc-router-" + runName
		agentPod := "agentorc-run-" + runName

		Eventually(func(g Gomega) {
			for _, name := range []string{routerSvc} {
				_, err := utils.Run(exec.Command(
					"kubectl", "get", "service", name, "-n", ns,
				))
				g.Expect(err).To(HaveOccurred(),
					"resource %s should be gone after AgentRun deletion", name)
			}
		}, 60*time.Second, 2*time.Second).Should(Succeed(),
			"router Service was not GC'd within 60s")

		// Pods are deleted by ensureCleanup on AgentRun deletion; give them time.
		Eventually(func(g Gomega) {
			for _, podName := range []string{routerPod, agentPod} {
				out, _ := utils.Run(exec.Command(
					"kubectl", "get", "pod", podName, "-n", ns,
				))
				// Pod should be gone (empty output from kubectl get is sufficient —
				// kubectl exits non-zero if not found).
				_ = out
				_, err := utils.Run(exec.Command(
					"kubectl", "get", "pod", podName, "-n", ns,
				))
				g.Expect(err).To(HaveOccurred(),
					"pod %s should be gone after AgentRun deletion", podName)
			}
		}, 90*time.Second, 2*time.Second).Should(Succeed(),
			"split-pod pods were not cleaned up within 90s")
	})

	It("combined-pod path still works (backward compatibility check)", func() {
		// Verify that a run WITHOUT splitPod still creates a single combined pod
		// with the model-router sidecar — ensuring we did not break legacy topology.
		combinedRun := "e2e-combined-run"

		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: Agent
metadata:
  name: e2e-combined-agent
  namespace: default
spec:
  modelSelectorRef: e2e-split-selector
  runtime:
    ociRef: "busybox:1.36"
    framework: openai-compatible
    command: ["sleep"]
    args: ["infinity"]
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 64Mi
`)).To(Succeed())

		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentRun
metadata:
  name: ` + combinedRun + `
  namespace: default
spec:
  agentRef: e2e-combined-agent
  input: "e2e backward-compat combined-pod test"
`)).To(Succeed())

		DeferCleanup(func() {
			deleteResource("agentrun", combinedRun, ns)
			deleteResource("agent", "e2e-combined-agent", ns)
		})

		combinedPodName := "agentorc-run-" + combinedRun
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command(
				"kubectl", "get", "pod", combinedPodName,
				"-n", ns,
				"-o", `jsonpath={range .spec.initContainers[*]}{.name}{"\n"}{end}`,
			))
			g.Expect(err).NotTo(HaveOccurred())
			// Combined pod must have the model-router sidecar as an init container.
			g.Expect(out).To(ContainSubstring("model-router"),
				"combined pod should contain the model-router sidecar")
		}, 60*time.Second, 2*time.Second).Should(Succeed(),
			"combined-pod model-router sidecar not present within 60s")

		// No component label should be set on the combined pod.
		out, err := utils.Run(exec.Command(
			"kubectl", "get", "pod", combinedPodName,
			"-n", ns,
			"-o", `jsonpath={.metadata.labels.agentorc\.io/component}`,
		))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(BeEmpty(),
			"combined pod should NOT have agentorc.io/component label")

		// Token secret must use localhost URL in combined-pod mode.
		secretName := "agentorc-run-" + combinedRun + "-token"
		Eventually(func(g Gomega) {
			raw, err := utils.Run(exec.Command(
				"kubectl", "get", "secret", secretName,
				"-n", ns,
				"-o", "jsonpath={.data.OPENAI_BASE_URL}",
			))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(raw).NotTo(BeEmpty())
			decoded, decErr := base64.StdEncoding.DecodeString(raw)
			g.Expect(decErr).NotTo(HaveOccurred())
			g.Expect(string(decoded)).To(ContainSubstring("localhost"),
				"combined-pod OPENAI_BASE_URL should use localhost")
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})
})
