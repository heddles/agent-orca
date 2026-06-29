/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package security

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

func newTestRun(name string) *agentorcv1alpha1.AgentRun {
	return &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
	}
}

func TestBuildNetworkPolicy_BasicNoToolsNoRedis(t *testing.T) {
	run := newTestRun("test-run")
	np := BuildNetworkPolicy(run, "default", nil, false)

	if np.Name != "agentorc-run-test-run" {
		t.Errorf("unexpected name: %s", np.Name)
	}
	if np.Namespace != "default" {
		t.Errorf("unexpected namespace: %s", np.Namespace)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("expected empty ingress (deny all), got %d rules", len(np.Spec.Ingress))
	}
	// 443, 6443, 8082, 53 UDP, 53 TCP = 5
	if len(np.Spec.Egress) != 5 {
		t.Fatalf("expected 5 egress rules, got %d", len(np.Spec.Egress))
	}
}

func TestBuildNetworkPolicy_WithRedis(t *testing.T) {
	run := newTestRun("redis-run")
	np := BuildNetworkPolicy(run, "default", nil, true)

	// +6379 for Redis = 6
	if len(np.Spec.Egress) != 6 {
		t.Fatalf("expected 6 egress rules with Redis, got %d", len(np.Spec.Egress))
	}
}

func TestBuildNetworkPolicy_WithToolEgress(t *testing.T) {
	tools := []agentorcv1alpha1.EgressRule{
		{Host: "api.example.com", Port: 443, Protocol: "TCP"},
		{Host: "10.0.0.5", Port: 8080, Protocol: "TCP"},
	}
	run := newTestRun("tool-run")
	np := BuildNetworkPolicy(run, "default", tools, false)

	// 2 (k8s API) + 2 tools + 8082 + 2 DNS = 7
	if len(np.Spec.Egress) != 7 {
		t.Fatalf("expected 7 egress rules, got %d", len(np.Spec.Egress))
	}
}

func TestBuildNetworkPolicy_PodSelector(t *testing.T) {
	run := newTestRun("sel-run")
	np := BuildNetworkPolicy(run, "ns1", nil, false)

	want := SafeLabelValue(run.Name)
	if np.Spec.PodSelector.MatchLabels[LabelAgentRunName] != want {
		t.Errorf("pod selector: got %q want %q", np.Spec.PodSelector.MatchLabels[LabelAgentRunName], want)
	}
}

// --- Split-pod NetworkPolicy tests ---

func TestBuildRouterPodNetworkPolicy_Basic(t *testing.T) {
	run := newTestRun("split-run")
	np := BuildRouterPodNetworkPolicy(run, "default", nil, false)

	if np.Name != "agentorc-router-split-run" {
		t.Errorf("unexpected name: %s", np.Name)
	}
	// PodSelector must target the router component.
	if np.Spec.PodSelector.MatchLabels[LabelComponent] != LabelComponentRouter {
		t.Errorf("pod selector component: got %q want %q",
			np.Spec.PodSelector.MatchLabels[LabelComponent], LabelComponentRouter)
	}
	// Must allow ingress from agent pod.
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	ingress := np.Spec.Ingress[0]
	if len(ingress.From) != 1 || ingress.From[0].PodSelector == nil {
		t.Error("expected ingress from pod selector")
	}
	fromLabels := ingress.From[0].PodSelector.MatchLabels
	if fromLabels[LabelComponent] != LabelComponentAgent {
		t.Errorf("ingress from component: got %q want %q", fromLabels[LabelComponent], LabelComponentAgent)
	}
	// 443, 6443, 8082, 53 UDP, 53 TCP = 5 egress rules (no Redis, no tools)
	if len(np.Spec.Egress) != 5 {
		t.Errorf("expected 5 egress rules, got %d", len(np.Spec.Egress))
	}
}

func TestBuildRouterPodNetworkPolicy_WithRedisAndTools(t *testing.T) {
	run := newTestRun("split-redis-run")
	tools := []agentorcv1alpha1.EgressRule{
		{Host: "api.github.com", Port: 443, Protocol: "TCP"},
	}
	np := BuildRouterPodNetworkPolicy(run, "default", tools, true)

	// 443, 6443, tool(443), 6379, 8082, 53 UDP, 53 TCP = 7
	if len(np.Spec.Egress) != 7 {
		t.Errorf("expected 7 egress rules, got %d", len(np.Spec.Egress))
	}
}

func TestBuildAgentPodNetworkPolicy_Basic(t *testing.T) {
	run := newTestRun("split-agent-run")
	np := BuildAgentPodNetworkPolicy(run, "default")

	if np.Name != "agentorc-agent-split-agent-run" {
		t.Errorf("unexpected name: %s", np.Name)
	}
	// PodSelector must target the agent component.
	if np.Spec.PodSelector.MatchLabels[LabelComponent] != LabelComponentAgent {
		t.Errorf("pod selector component: got %q want %q",
			np.Spec.PodSelector.MatchLabels[LabelComponent], LabelComponentAgent)
	}
	// Must deny all ingress.
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("expected empty ingress (deny all), got %d rules", len(np.Spec.Ingress))
	}
	// Egress: to-router (8080/8082), 53 UDP, 53 TCP = 3 rules
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("expected 3 egress rules, got %d", len(np.Spec.Egress))
	}
	// First rule must have a To: pod selector targeting the router.
	toRule := np.Spec.Egress[0]
	if len(toRule.To) != 1 || toRule.To[0].PodSelector == nil {
		t.Fatal("expected egress to pod selector")
	}
	toLabels := toRule.To[0].PodSelector.MatchLabels
	if toLabels[LabelComponent] != LabelComponentRouter {
		t.Errorf("egress to component: got %q want %q", toLabels[LabelComponent], LabelComponentRouter)
	}
	if len(toRule.Ports) != 2 {
		t.Errorf("expected 2 ports (8080, 8082) in router egress rule, got %d", len(toRule.Ports))
	}
}
