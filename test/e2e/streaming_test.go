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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Token streaming", Label("streaming", "full-e2e"), Ordered, func() {
	const ns = "default"
	// UI API service port (agent-orc ui-proxy serves the run stream endpoint).
	const uiAPILocalPort = 19083

	BeforeAll(func() {
		if !streamingE2E() {
			Skip("requires STREAMING_E2E=true (model-router + LLM credentials needed)")
		}
		waitForCRDReady("agentruns.agentorc.agentorc.io")
		for _, name := range []string{"e2e-stream-single", "e2e-stream-parent"} {
			deleteResource("agentrun", name, ns)
		}
		Expect(applyYAML(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: Agent
metadata:
  name: e2e-streaming-agent
  namespace: default
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
    limits:
      cpu: 100m
      memory: 64Mi
---
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentRun
metadata:
  name: e2e-stream-single
  namespace: default
  annotations:
    agentorc.io/enforcement-mode: "off"
spec:
  agentRef: e2e-streaming-agent
  input: "e2e streaming — single agent token stream validation"
---
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentRun
metadata:
  name: e2e-stream-parent
  namespace: default
  annotations:
    agentorc.io/enforcement-mode: "off"
spec:
  agentRef: e2e-streaming-agent
  input: "e2e streaming — parent orchestrator token forwarding validation"
`)).To(Succeed())
	})

	AfterAll(func() {
		if !streamingE2E() {
			return
		}
		for _, name := range []string{"e2e-stream-single", "e2e-stream-parent"} {
			deleteResource("agentrun", name, ns)
		}
		deleteResource("agent", "e2e-streaming-agent", ns)
	})

	It("streams tokens from a single agent run", func() {
		pod := "agentorc-run-e2e-stream-single"
		waitForPodRunning(pod, ns)
		token := getRunToken("e2e-stream-single", ns)

		// Port-forward to the UI API (run stream endpoint).
		cleanupPF := portForward(ns, "svc/agent-orc-ui-proxy", uiAPILocalPort, 8083)
		defer cleanupPF()

		// Trigger inference via model-router.
		cleanupTrigger := portForward(ns, "pod/"+pod, 19080, 8080)
		defer cleanupTrigger()
		go func() {
			time.Sleep(2 * time.Second)
			postChatCompletion(19080, token)
		}()

		streamURL := fmt.Sprintf("http://127.0.0.1:%d/api/runs/%s/e2e-stream-single/stream?token=%s",
			uiAPILocalPort, ns, token)
		events := collectSSE(streamURL, "", 90*time.Second)

		assertStreamingOrder(events)
		Expect(countSSEEvents(events, "token")).To(BeNumerically(">", 0))
	})

	It("forwards child tokens to the parent SSE stream", func() {
		pod := "agentorc-run-e2e-stream-parent"
		waitForPodRunning(pod, ns)
		token := getRunToken("e2e-stream-parent", ns)

		cleanupPF := portForward(ns, "svc/agent-orc-ui-proxy", uiAPILocalPort, 8083)
		defer cleanupPF()

		cleanupTrigger := portForward(ns, "pod/"+pod, 19081, 8080)
		defer cleanupTrigger()
		go func() {
			time.Sleep(2 * time.Second)
			postChatCompletion(19081, token)
		}()

		streamURL := fmt.Sprintf("http://127.0.0.1:%d/api/runs/%s/e2e-stream-parent/stream?token=%s",
			uiAPILocalPort, ns, token)
		events := collectSSE(streamURL, "", 90*time.Second)

		assertStreamingOrder(events)
		Expect(countSSEEvents(events, "token")).To(BeNumerically(">", 0))
	})
})
