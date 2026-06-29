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

// Package framework implements the agent framework adoption layer.
// Existing agent images run unmodified; the operator injects configuration
// using one of three tiers based on the Agent's runtime.framework field:
//
//	Tier 1 — openai-compatible (default):
//	  Inject OPENAI_BASE_URL + OPENAI_API_KEY env vars.
//	  Covers: openai SDK, LangChain, LangGraph, CrewAI, ADK (LiteLlmModel).
//
//	Tier 2 — init container config injection:
//	  Write a framework-specific config file before the agent starts.
//	  Covers: autogen (OAI_CONFIG_LIST), semantic-kernel (appsettings.json),
//	          langgraph (LANGGRAPH_CHECKPOINT_URL).
//
//	Tier 3 — hosts-file shim:
//	  Redirect a hardcoded provider hostname to localhost via /etc/hosts.
//	  Covers: native ADK GeminiLlm (generativelanguage.googleapis.com → :8082).
package framework

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

const (
	// initConfigVolume is the emptyDir used by tier-2 init containers.
	initConfigVolume = "agentorc-framework-config"
	initConfigDir    = "/etc/agentorc-framework"

	// hostsVolume is the emptyDir used by the tier-3 shim to override /etc/hosts.
	hostsVolume = "agentorc-hosts"
)

// Inject mutates the pod spec to add tier-specific framework configuration.
// routerBaseURL is the HTTP base URL the agent uses to reach the model-router
// (e.g. "http://localhost:8080" for combined-pod topology, or the router Service URL
// for split-pod topology). The port is extracted from this URL for the shim tier.
func Inject(pod *corev1.Pod, runtime agentorcv1alpha1.AgentRuntime, routerBaseURL string) {
	switch runtime.Framework {
	case "autogen":
		injectAutogen(pod, routerBaseURL)
	case "semantic-kernel":
		injectSemanticKernel(pod, routerBaseURL)
	case "langgraph":
		injectLangGraph(pod, routerBaseURL)
	case "shim":
		injectShim(pod, runtime.ShimTarget, routerBaseURL)
	default:
		// "openai-compatible" and "" — tier 1 is handled by the base pod spec
		// (OPENAI_BASE_URL + OPENAI_API_KEY already injected from the token secret).
	}
}

// injectAutogen injects an init container that writes OAI_CONFIG_LIST for AutoGen v0.4.
// AutoGen reads this JSON file to configure its LLM backend.
func injectAutogen(pod *corev1.Pod, routerBaseURL string) {
	configList := []map[string]interface{}{
		{
			"model":    "gpt-4o", // AutoGen uses this as a label; model selection is in the router
			"base_url": routerBaseURL + "/v1",
			"api_key":  "$(OPENAI_API_KEY)", // expanded from the token secret env var
		},
	}
	configJSON, _ := json.Marshal(configList)

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: initConfigVolume,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:    "agentorc-autogen-config",
		Image:   "cgr.dev/chainguard/busybox:latest",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			fmt.Sprintf(`printf '%%s' '%s' > %s/OAI_CONFIG_LIST.json`, configJSON, initConfigDir),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: initConfigVolume, MountPath: initConfigDir},
		},
	})

	// Inject env vars and volume mount into the agent container.
	agentIdx := agentContainerIndex(pod)
	if agentIdx < 0 {
		return
	}
	pod.Spec.Containers[agentIdx].Env = append(pod.Spec.Containers[agentIdx].Env,
		corev1.EnvVar{Name: "OAI_CONFIG_LIST", Value: initConfigDir + "/OAI_CONFIG_LIST.json"},
	)
	pod.Spec.Containers[agentIdx].VolumeMounts = append(
		pod.Spec.Containers[agentIdx].VolumeMounts,
		corev1.VolumeMount{Name: initConfigVolume, MountPath: initConfigDir, ReadOnly: true},
	)
}

// injectSemanticKernel writes an appsettings.json fragment for Semantic Kernel (C#/Python).
func injectSemanticKernel(pod *corev1.Pod, routerBaseURL string) {
	appsettings := map[string]interface{}{
		"OpenAI": map[string]interface{}{
			"ChatModelId": "gpt-4o",
			"Endpoint":    routerBaseURL,
			"ApiKey":      "$(OPENAI_API_KEY)",
		},
		"AzureOpenAI": map[string]interface{}{
			"Endpoint": routerBaseURL,
			"ApiKey":   "$(OPENAI_API_KEY)",
		},
	}
	settingsJSON, _ := json.MarshalIndent(appsettings, "", "  ")

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: initConfigVolume,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:    "agentorc-sk-config",
		Image:   "cgr.dev/chainguard/busybox:latest",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			fmt.Sprintf(`printf '%%s' '%s' > %s/appsettings.json`, settingsJSON, initConfigDir),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: initConfigVolume, MountPath: initConfigDir},
		},
	})

	agentIdx := agentContainerIndex(pod)
	if agentIdx < 0 {
		return
	}
	// Semantic Kernel reads AZURE_OPENAI_ENDPOINT or the appsettings file path.
	pod.Spec.Containers[agentIdx].Env = append(pod.Spec.Containers[agentIdx].Env,
		corev1.EnvVar{Name: "APPSETTINGS_PATH", Value: initConfigDir + "/appsettings.json"},
		corev1.EnvVar{Name: "AZURE_OPENAI_ENDPOINT", Value: routerBaseURL},
		corev1.EnvVar{Name: "OPENAI_ENDPOINT", Value: routerBaseURL},
	)
	pod.Spec.Containers[agentIdx].VolumeMounts = append(
		pod.Spec.Containers[agentIdx].VolumeMounts,
		corev1.VolumeMount{Name: initConfigVolume, MountPath: initConfigDir, ReadOnly: true},
	)
}

// injectLangGraph injects LANGGRAPH_CHECKPOINT_URL and the LangChain legacy base URL env var.
// The Postgres checkpoint URL is expected in the LANGGRAPH_CHECKPOINT_URL operator env var;
// if not set, LangGraph falls back to in-memory (no persistence across pod restarts).
func injectLangGraph(pod *corev1.Pod, routerBaseURL string) {
	agentIdx := agentContainerIndex(pod)
	if agentIdx < 0 {
		return
	}
	pod.Spec.Containers[agentIdx].Env = append(pod.Spec.Containers[agentIdx].Env,
		// LangChain legacy env var (used by ChatOpenAI in older LC versions).
		corev1.EnvVar{Name: "OPENAI_API_BASE", Value: routerBaseURL},
	)
	// LANGGRAPH_CHECKPOINT_URL is injected from the operator Helm value if configured.
	// If not configured here, the agent uses the model-router's own checkpointing.
}

// injectShim injects a hosts-file entry that redirects shimTarget to the router's address.
// This enables frameworks that hardcode their provider URL (e.g. native Google ADK Gemini)
// to be transparently intercepted by the model-router's Gemini-compatible endpoint (:8082).
// In combined-pod topology routerBaseURL is "http://localhost:8082"; in split-pod topology
// it is the router Service URL. Only the host part of routerBaseURL is used for /etc/hosts;
// the full URL is passed via GOOGLE_API_BASE for SDK env-var overrides.
func injectShim(pod *corev1.Pod, shimTarget string, routerBaseURL string) {
	if shimTarget == "" {
		shimTarget = "api.openai.com"
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: hostsVolume,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	// In combined-pod topology the shim redirects the shimTarget hostname to 127.0.0.1 so that
	// the agent process hits the model-router sidecar on localhost. In split-pod topology the
	// agent must reach the router Service, so the /etc/hosts shim is skipped (the router
	// Service is reachable by its DNS name without hosts manipulation).
	shimIP := "127.0.0.1"

	// Init container copies /etc/hosts and appends the shim entry.
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:    "agentorc-hosts-shim",
		Image:   "cgr.dev/chainguard/busybox:latest",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			fmt.Sprintf(
				`cp /etc/hosts /agentorc-hosts/hosts && printf '\n%s %s\n' >> /agentorc-hosts/hosts`,
				shimIP, shimTarget,
			),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: hostsVolume, MountPath: "/agentorc-hosts"},
		},
	})

	// Mount the modified hosts file over /etc/hosts in the agent container.
	// Uses subPath so only the hosts file is replaced, not the whole /etc directory.
	agentIdx := agentContainerIndex(pod)
	if agentIdx >= 0 {
		pod.Spec.Containers[agentIdx].VolumeMounts = append(
			pod.Spec.Containers[agentIdx].VolumeMounts,
			corev1.VolumeMount{
				Name:      hostsVolume,
				MountPath: "/etc/hosts",
				SubPath:   "hosts",
			},
		)
		// Point the agent at the Gemini-compatible model-router endpoint via env vars
		// that Google ADK reads when using LiteLlmModel.
		pod.Spec.Containers[agentIdx].Env = append(pod.Spec.Containers[agentIdx].Env,
			corev1.EnvVar{Name: "GOOGLE_API_BASE", Value: routerBaseURL},
		)
	}
}

// agentContainerIndex returns the index of the "agent" container in the pod spec, or -1.
func agentContainerIndex(pod *corev1.Pod) int {
	for i, c := range pod.Spec.Containers {
		if c.Name == "agent" {
			return i
		}
	}
	return -1
}
