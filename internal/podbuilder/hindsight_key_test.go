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
)

func hindsightKeyRef() *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "agentorca-hindsight-api-key"},
		Key:                  "HINDSIGHT_API_KEY",
	}
}

func envByName(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func assertKeyEnv(t *testing.T, c *corev1.Container, want bool) {
	t.Helper()
	ev := envByName(c, "HINDSIGHT_API_KEY")
	if !want {
		if ev != nil {
			t.Fatalf("%s container has HINDSIGHT_API_KEY without a configured key", c.Name)
		}
		return
	}
	if ev == nil || ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s container is missing HINDSIGHT_API_KEY secretKeyRef: %+v", c.Name, ev)
	}
	ref := ev.ValueFrom.SecretKeyRef
	if ref.Name != "agentorca-hindsight-api-key" || ref.Key != "HINDSIGHT_API_KEY" {
		t.Fatalf("unexpected secretKeyRef %+v", ref)
	}
	if ev.Value != "" {
		t.Fatal("the key must be referenced from a Secret, never set inline")
	}
}

func baseConfig() PodConfig {
	return PodConfig{
		PodName:          "p",
		Namespace:        "default",
		Agent:            testAgent(),
		TokenSecretName:  "token",
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: "router:v1",
		RouterConfigName: "config",
	}
}

func routerContainerOf(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range list {
			if list[i].Name == "model-router" {
				return &list[i]
			}
		}
	}
	t.Fatal("model-router container not found")
	return nil
}

// Regression for #58: the router must receive the hindsight key, and only the
// router -- the agent container shares the pod but must never see it.
func TestBuild_HindsightAPIKeyReachesRouterOnly(t *testing.T) {
	cfg := baseConfig()
	cfg.HindsightAPIKeySecretRef = hindsightKeyRef()
	pod := Build(cfg)

	assertKeyEnv(t, routerContainerOf(t, pod), true)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "model-router" {
			continue
		}
		assertKeyEnv(t, &pod.Spec.Containers[i], false)
	}
}

func TestBuild_NoHindsightAPIKeyByDefault(t *testing.T) {
	assertKeyEnv(t, routerContainerOf(t, Build(baseConfig())), false)
}

func TestBuildRouterOnly_HindsightAPIKey(t *testing.T) {
	cfg := baseConfig()
	cfg.HindsightAPIKeySecretRef = hindsightKeyRef()
	assertKeyEnv(t, routerContainerOf(t, BuildRouterOnly(cfg)), true)

	assertKeyEnv(t, routerContainerOf(t, BuildRouterOnly(baseConfig())), false)
}

func TestBuildAgentOnly_NeverGetsHindsightAPIKey(t *testing.T) {
	cfg := baseConfig()
	cfg.HindsightAPIKeySecretRef = hindsightKeyRef()
	pod := BuildAgentOnly(cfg)
	for i := range pod.Spec.Containers {
		assertKeyEnv(t, &pod.Spec.Containers[i], false)
	}
}
