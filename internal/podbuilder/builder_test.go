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

package podbuilder

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

func testAgent() *agentorcv1alpha1.Agent {
	return &agentorcv1alpha1.Agent{
		Spec: agentorcv1alpha1.AgentSpec{
			Runtime: agentorcv1alpha1.AgentRuntime{
				OCIRef:    "registry.example.com/my-agent:v1",
				Framework: "openai-compatible",
				Command:   []string{"/bin/agent"},
				Args:      []string{"--verbose"},
			},
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
	}
}

func testProviderVolumes() ([]corev1.Volume, []corev1.VolumeMount) {
	return []corev1.Volume{
			{Name: "provider-anthropic", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "anthropic-key"},
			}},
		}, []corev1.VolumeMount{
			{Name: "provider-anthropic", MountPath: "/etc/agentorc-providers/anthropic", ReadOnly: true},
		}
}

func TestBuild_AgentRunPod(t *testing.T) { //nolint:gocyclo

	agent := testAgent()
	pvols, pmounts := testProviderVolumes()

	pod := Build(PodConfig{
		PodName:   "agentorc-run-test-run",
		Namespace: "default",
		Labels: map[string]string{
			"agentorc.io/run":   "test-run",
			"agentorc.io/agent": "test-agent",
		},
		Agent: agent,
		AgentEnv: []corev1.EnvVar{
			{Name: "AGENTORC_RUN_ID", Value: "test-run"},
			{Name: "AGENTORC_TIMEOUT_SEC", Value: "300"},
		},
		TokenSecretName:  "agentorc-run-test-run-token",
		ServiceAccount:   "agentorc-agent-test-agent",
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: "registry.example.com/model-router:v1",
		RouterConfigName: "agentorc-run-test-run",
		ProviderVolumes:  pvols,
		ProviderMounts:   pmounts,
	})

	// Pod metadata
	if pod.Name != "agentorc-run-test-run" {
		t.Errorf("expected pod name 'agentorc-run-test-run', got %q", pod.Name)
	}
	if pod.Namespace != "default" {
		t.Errorf("expected namespace 'default', got %q", pod.Namespace)
	}
	if pod.Labels["agentorc.io/run"] != "test-run" {
		t.Errorf("expected label agentorc.io/run=test-run, got %q", pod.Labels["agentorc.io/run"])
	}

	// RestartPolicy
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicyNever, got %v", pod.Spec.RestartPolicy)
	}

	// ServiceAccount
	if pod.Spec.ServiceAccountName != "agentorc-agent-test-agent" {
		t.Errorf("expected SA 'agentorc-agent-test-agent', got %q", pod.Spec.ServiceAccountName)
	}

	// Agent container
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	agentC := pod.Spec.Containers[0]
	if agentC.Name != "agent" {
		t.Errorf("expected container name 'agent', got %q", agentC.Name)
	}
	if agentC.Image != "registry.example.com/my-agent:v1" {
		t.Errorf("expected agent image, got %q", agentC.Image)
	}
	if agentC.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("expected PullIfNotPresent, got %v", agentC.ImagePullPolicy)
	}
	if len(agentC.Command) != 1 || agentC.Command[0] != "/bin/agent" {
		t.Errorf("expected command [/bin/agent], got %v", agentC.Command)
	}
	// Agent env should include caller-supplied + framework env vars
	foundRunID := false
	for _, e := range agentC.Env {
		if e.Name == "AGENTORC_RUN_ID" && e.Value == "test-run" {
			foundRunID = true
		}
	}
	if !foundRunID {
		t.Error("expected AGENTORC_RUN_ID env var in agent container")
	}
	// EnvFrom should reference the token secret
	if len(agentC.EnvFrom) != 1 || agentC.EnvFrom[0].SecretRef.Name != "agentorc-run-test-run-token" {
		t.Errorf("expected envFrom with token secret, got %+v", agentC.EnvFrom)
	}
	// Agent resources should be set
	if agentC.Resources.Requests.Cpu().String() != "250m" {
		t.Errorf("expected CPU request 250m, got %s", agentC.Resources.Requests.Cpu().String())
	}

	// Model-router sidecar (init container)
	if len(pod.Spec.InitContainers) < 1 {
		t.Fatal("expected at least 1 init container (model-router)")
	}
	// Find the model-router — it may not be first if framework.Inject added an init container.
	var routerC *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "model-router" { //nolint:goconst

			routerC = &pod.Spec.InitContainers[i]
			break
		}
	}
	if routerC == nil { //nolint:staticcheck

		t.Fatal("model-router init container not found")
	}
	if routerC.Image != "registry.example.com/model-router:v1" { //nolint:staticcheck

		t.Errorf("expected model-router image, got %q", routerC.Image)
	}
	if routerC.RestartPolicy == nil || *routerC.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("expected model-router to have RestartPolicy=Always (native sidecar)")
	}
	// Should have 2 ports (openai, gemini); tool executor is in-process (no :8081 listener).
	if len(routerC.Ports) != 2 {
		t.Errorf("expected 2 ports on model-router, got %d", len(routerC.Ports))
	}
	// Startup probe on port 8080 (default)
	if routerC.StartupProbe == nil || routerC.StartupProbe.HTTPGet.Port.IntValue() != 8080 {
		t.Error("expected startup probe on port 8080")
	}
	// Resources should be set (defaults)
	if routerC.Resources.Requests.Cpu().String() != "100m" {
		t.Errorf("expected router CPU request 100m, got %s", routerC.Resources.Requests.Cpu().String())
	}

	// Router config volume
	foundRouterVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == RouterConfigVolume && v.ConfigMap != nil && v.ConfigMap.Name == "agentorc-run-test-run" {
			foundRouterVol = true
		}
	}
	if !foundRouterVol {
		t.Error("expected router config volume referencing ConfigMap 'agentorc-run-test-run'")
	}

	// Provider volume should be present
	foundProviderVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "provider-anthropic" {
			foundProviderVol = true
		}
	}
	if !foundProviderVol {
		t.Error("expected provider volume 'provider-anthropic'")
	}

	// Security hardening should have been applied
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("expected RunAsNonRoot=true (security hardening)")
	}
	if agentC.SecurityContext == nil || agentC.SecurityContext.ReadOnlyRootFilesystem == nil || !*agentC.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected ReadOnlyRootFilesystem=true on agent container")
	}
}

func TestBuild_DeploymentPod(t *testing.T) {
	agent := testAgent()

	pod := Build(PodConfig{
		Namespace: "prod",
		Labels: map[string]string{
			"agentdeployment.agentorc.io": "my-deploy",
		},
		Annotations: map[string]string{
			"agentorc.io/router-config-hash": "abc123",
		},
		Agent: agent,
		AgentEnv: []corev1.EnvVar{
			{Name: "AGENTORC_AGENT", Value: "my-agent"},
			{Name: "AGENTORC_NAMESPACE", Value: "prod"},
		},
		TokenSecretName:  "agentorc-deploy-my-deploy-token",
		ServiceAccount:   "agentorc-agent-my-agent",
		RestartPolicy:    corev1.RestartPolicyAlways,
		ModelRouterImage: "registry.example.com/model-router:v1",
		RouterConfigName: "agentorc-deploy-my-deploy",
	})

	// RestartPolicy should be Always
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("expected RestartPolicyAlways, got %v", pod.Spec.RestartPolicy)
	}

	// Annotations should be set
	if pod.Annotations["agentorc.io/router-config-hash"] != "abc123" {
		t.Errorf("expected annotation, got %v", pod.Annotations)
	}

	// No readiness probe on agent (deployment, not http-mode run)
	agentC := pod.Spec.Containers[0]
	if agentC.ReadinessProbe != nil {
		t.Error("expected no readiness probe on deployment agent container")
	}

	// Model-router should have default resource limits
	var routerC *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "model-router" {
			routerC = &pod.Spec.InitContainers[i]
			break
		}
	}
	if routerC == nil { //nolint:staticcheck

		t.Fatal("model-router not found")
	}
	if routerC.Resources.Limits.Memory().String() != "256Mi" { //nolint:staticcheck

		t.Errorf("expected router memory limit 256Mi, got %s", routerC.Resources.Limits.Memory().String())
	}
}

func TestBuild_WarmPod(t *testing.T) {
	agent := testAgent()

	pod := Build(PodConfig{
		GenerateName: "warm-my-deploy-",
		Namespace:    "default",
		Labels: map[string]string{
			"agentorc.io/warm-pool":   "my-deploy",
			"agentorc.io/warm-status": "idle",
		},
		Agent: agent,
		AgentEnv: []corev1.EnvVar{
			{Name: "AGENTORC_AGENT", Value: "my-agent"},
		},
		TokenSecretName:  "warm-token-abc",
		ServiceAccount:   "agentorc-agent-my-agent",
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: "registry.example.com/model-router:v1",
		RouterConfigName: "agentorc-warm-my-deploy",
		RouterExtraPorts: []corev1.ContainerPort{
			{Name: "warm-mgmt", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		},
		RouterProbePort: 9090,
	})

	// GenerateName should be set, Name empty
	if pod.GenerateName != "warm-my-deploy-" {
		t.Errorf("expected GenerateName 'warm-my-deploy-', got %q", pod.GenerateName)
	}
	if pod.Name != "" {
		t.Errorf("expected empty Name for warm pod, got %q", pod.Name)
	}

	// RestartPolicy Never
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicyNever, got %v", pod.Spec.RestartPolicy)
	}

	// Model-router should have 3 ports (2 base + warm-mgmt)
	var routerC *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "model-router" {
			routerC = &pod.Spec.InitContainers[i]
			break
		}
	}
	if routerC == nil { //nolint:staticcheck

		t.Fatal("model-router not found")
	}
	if len(routerC.Ports) != 3 { //nolint:staticcheck

		t.Errorf("expected 3 ports on warm model-router, got %d", len(routerC.Ports))
	}
	foundWarmMgmt := false
	for _, p := range routerC.Ports {
		if p.Name == "warm-mgmt" && p.ContainerPort == 9090 {
			foundWarmMgmt = true
		}
	}
	if !foundWarmMgmt {
		t.Error("expected warm-mgmt port 9090 on model-router")
	}

	// Startup probe on port 9090
	if routerC.StartupProbe == nil || routerC.StartupProbe.HTTPGet.Port.IntValue() != 9090 {
		t.Error("expected startup probe on port 9090 for warm pod")
	}

	// Security hardening should be applied (was previously missing on warm pods)
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("expected security hardening on warm pod")
	}
}

// TestBuild_WarmPodLocalCache verifies the disk-backed emptyDir that supplements
// Redis is attached to the model-router sidecar ONLY when WarmLocalCacheEnabled,
// and that it is absent (and one-shot pods are unaffected) otherwise.
func TestBuild_WarmPodLocalCache(t *testing.T) {
	agent := testAgent()
	sizeLimit := resource.MustParse("256Mi")

	base := PodConfig{
		GenerateName:     "warm-dep-",
		Namespace:        "default",
		Agent:            agent,
		TokenSecretName:  "token",
		ServiceAccount:   "sa",
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: "router:v1",
		RouterConfigName: "cfg",
		RouterProbePort:  9090,
	}

	t.Run("enabled attaches volume + mount + env to model-router", func(t *testing.T) {
		cfg := base
		cfg.WarmLocalCacheEnabled = true
		cfg.WarmLocalCacheSizeLimit = &sizeLimit
		pod := Build(cfg)

		var routerC *corev1.Container
		for i := range pod.Spec.InitContainers {
			if pod.Spec.InitContainers[i].Name == "model-router" {
				routerC = &pod.Spec.InitContainers[i]
			}
		}
		if routerC == nil { //nolint:staticcheck

			t.Fatal("model-router sidecar not found")
		}
		hasMount := false
		for _, m := range routerC.VolumeMounts { //nolint:staticcheck

			if m.Name == WarmCacheVolName && m.MountPath == WarmCacheMountDir {
				hasMount = true
			}
		}
		if !hasMount {
			t.Errorf("expected warm cache mount on model-router; mounts=%v", routerC.VolumeMounts)
		}
		hasEnv := false
		for _, e := range routerC.Env {
			if e.Name == "AGENTORC_WARM_CACHE_DIR" && e.Value == WarmCacheMountDir {
				hasEnv = true
			}
		}
		if !hasEnv {
			t.Errorf("expected AGENTORC_WARM_CACHE_DIR env on model-router; env=%v", routerC.Env)
		}
		hasVol := false
		var vol corev1.Volume
		for _, v := range pod.Spec.Volumes {
			if v.Name == WarmCacheVolName {
				hasVol = true
				vol = v
			}
		}
		if !hasVol {
			t.Fatal("expected warm cache volume at pod level")
		}
		if vol.EmptyDir == nil {
			t.Fatal("expected EmptyDir volume source")
		}
		if vol.EmptyDir.Medium != corev1.StorageMediumDefault {
			t.Errorf("expected disk-backed (default medium) emptyDir, got %s", vol.EmptyDir.Medium)
		}
		if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.Cmp(sizeLimit) != 0 {
			t.Errorf("expected SizeLimit 256Mi, got %v", vol.EmptyDir.SizeLimit)
		}

		// The agent container must NOT receive the cache mount/env (it reaches
		// the router over localhost).
		agentC := pod.Spec.Containers[0]
		if agentC.Name != "agent" {
			t.Fatalf("expected first container to be 'agent', got %q", agentC.Name)
		}
		for _, m := range agentC.VolumeMounts {
			if m.Name == WarmCacheVolName {
				t.Error("agent container must not mount the warm cache volume")
			}
		}
		for _, e := range agentC.Env {
			if e.Name == "AGENTORC_WARM_CACHE_DIR" {
				t.Error("agent container must not receive AGENTORC_WARM_CACHE_DIR")
			}
		}
	})

	t.Run("disabled leaves pod unchanged", func(t *testing.T) {
		cfg := base // WarmLocalCacheEnabled defaults to false
		pod := Build(cfg)
		for _, v := range pod.Spec.Volumes {
			if v.Name == WarmCacheVolName {
				t.Error("warm cache volume should be absent when disabled")
			}
		}
	})
}

func TestBuild_HTTPModeReadinessProbe(t *testing.T) {
	agent := testAgent()
	agent.Spec.Runtime.InputMode = "http"
	agent.Spec.Runtime.InputPort = 8000

	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.IntOrString{Type: intstr.Int, IntVal: 8000},
			},
		},
	}

	pod := Build(PodConfig{
		PodName:             "agentorc-run-http-run",
		Namespace:           "default",
		Agent:               agent,
		TokenSecretName:     "token",
		RestartPolicy:       corev1.RestartPolicyNever,
		ModelRouterImage:    "router:v1",
		RouterConfigName:    "config",
		AgentReadinessProbe: probe,
	})

	agentC := pod.Spec.Containers[0]
	if agentC.ReadinessProbe == nil {
		t.Error("expected readiness probe on http-mode agent")
	}
}

func TestBuild_FrameworkEnvVars(t *testing.T) {
	tests := []struct {
		framework string
		envName   string
		envValue  string
	}{
		{"langgraph", "OPENAI_API_BASE", "http://localhost:8080"},
		{"autogen", "OPENAI_BASE_URL", "http://localhost:8080/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.framework, func(t *testing.T) {
			agent := testAgent()
			agent.Spec.Runtime.Framework = tt.framework

			pod := Build(PodConfig{
				PodName:          "test-" + tt.framework,
				Namespace:        "default",
				Agent:            agent,
				TokenSecretName:  "token",
				RestartPolicy:    corev1.RestartPolicyNever,
				ModelRouterImage: "router:v1",
				RouterConfigName: "config",
			})

			agentC := pod.Spec.Containers[0]
			found := false
			for _, e := range agentC.Env {
				if e.Name == tt.envName && e.Value == tt.envValue {
					found = true
				}
			}
			if !found {
				t.Errorf("expected env %s=%s for framework %s", tt.envName, tt.envValue, tt.framework)
			}
		})
	}
}

func TestBuild_CustomRouterResources(t *testing.T) {
	agent := testAgent()

	custom := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("200m"),
		},
	}

	pod := Build(PodConfig{
		PodName:          "test-custom-resources",
		Namespace:        "default",
		Agent:            agent,
		TokenSecretName:  "token",
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: "router:v1",
		RouterConfigName: "config",
		RouterResources:  custom,
	})

	var routerC *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "model-router" {
			routerC = &pod.Spec.InitContainers[i]
			break
		}
	}
	if routerC == nil { //nolint:staticcheck

		t.Fatal("model-router not found")
	}
	if routerC.Resources.Requests.Cpu().String() != "200m" { //nolint:staticcheck

		t.Errorf("expected custom CPU request 200m, got %s", routerC.Resources.Requests.Cpu().String())
	}
}

func TestSanitizeVolumeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"anthropic", "anthropic"},
		{"My-Provider_v2", "my-provider-v2"},
		{"a.b.c", "a-b-c"},
		{"---leading-trailing---", "leading-trailing"},
		{"a-very-long-name-that-exceeds-the-fifty-character-limit-for-volume-names", "a-very-long-name-that-exceeds-the-fifty-character"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SanitizeVolumeName(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeVolumeName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestToolSecretFilePath(t *testing.T) {
	got := ToolSecretFilePath("github-mcp", "gh-token", "token")
	expected := "/etc/agentorc-tool-secrets/github-mcp/gh-token/token"
	if got != expected {
		t.Errorf("ToolSecretFilePath = %q, want %q", got, expected)
	}
}

func TestFrameworkEnvVars(t *testing.T) {
	const localhost = "http://localhost:8080"
	// Default framework returns nil
	if vars := FrameworkEnvVars("openai-compatible", localhost); vars != nil {
		t.Errorf("expected nil for openai-compatible, got %v", vars)
	}
	if vars := FrameworkEnvVars("", localhost); vars != nil {
		t.Errorf("expected nil for empty framework, got %v", vars)
	}
	// langgraph returns OPENAI_API_BASE
	vars := FrameworkEnvVars("langgraph", localhost)
	if len(vars) != 1 || vars[0].Name != "OPENAI_API_BASE" {
		t.Errorf("unexpected langgraph vars: %v", vars)
	}
	if vars[0].Value != localhost {
		t.Errorf("langgraph OPENAI_API_BASE: got %q, want %q", vars[0].Value, localhost)
	}
	// autogen returns OPENAI_BASE_URL
	vars = FrameworkEnvVars("autogen", localhost)
	if len(vars) != 1 || vars[0].Name != "OPENAI_BASE_URL" {
		t.Errorf("unexpected autogen vars: %v", vars)
	}
	if vars[0].Value != localhost+"/v1" {
		t.Errorf("autogen OPENAI_BASE_URL: got %q, want %q", vars[0].Value, localhost+"/v1")
	}
	// split-pod: service URL is forwarded correctly
	svcURL := "http://agentorc-router-myrun.default:8080"
	vars = FrameworkEnvVars("langgraph", svcURL)
	if vars[0].Value != svcURL {
		t.Errorf("langgraph split-pod OPENAI_API_BASE: got %q, want %q", vars[0].Value, svcURL)
	}
}
