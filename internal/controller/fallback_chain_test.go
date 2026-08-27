/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// fallbackFixture builds the fake client objects shared by both controller tests:
// one primary provider, one fallback-only provider, one ModelSelector, one Agent.
func fallbackFixture(scheme *runtime.Scheme) ( //nolint:unparam

	primary *agentorcav1alpha1.ModelProvider,
	fallback *agentorcav1alpha1.ModelProvider,
	selector *agentorcav1alpha1.ModelSelector,
	agent *agentorcav1alpha1.Agent,
) {
	primary = &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "primary-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "openai/gpt-4o",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{
				Name: "primary-secret",
				Key:  "api-key",
			},
		},
	}
	fallback = &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "fallback-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "anthropic/claude-haiku-4-5-20251001",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{
				Name: "fallback-secret",
				Key:  "api-key",
			},
		},
	}
	selector = &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:      "rule-based",
			Providers:     []agentorcav1alpha1.ProviderWeight{{Name: "primary-provider", Weight: 100}},
			FallbackChain: []string{"fallback-provider"},
		},
	}
	agent = &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentSpec{
			ModelSelectorRef: "default",
		},
	}
	return primary,
		fallback,
		selector,
		agent
}

// TestBuildRouterConfig_FallbackProvidersLoadedWithZeroWeight verifies that
// buildRouterConfig includes fallback chain providers in cfg.Providers with
// Weight=0 so tryFallback can find them when the primary call fails.
func TestBuildRouterConfig_FallbackProvidersLoadedWithZeroWeight(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	primary, fallback, selector, agent := fallbackFixture(scheme)

	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, fallback, selector, agent, run).
		Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "test-sa", nil)
	if err != nil {
		t.Fatalf("buildRouterConfig: %v", err)
	}

	byName := make(map[string]int) // name → Weight
	for _, p := range cfg.Providers {
		byName[p.Name] = p.Weight
	}

	if w, ok := byName["primary-provider"]; !ok || w != 100 {
		t.Errorf("primary-provider: want Weight=100, got Weight=%d (present=%v)", w, ok)
	}
	if w, ok := byName["fallback-provider"]; !ok {
		t.Errorf("fallback-provider missing from cfg.Providers: tryFallback cannot reach it")
	} else if w != 0 {
		t.Errorf("fallback-provider: want Weight=0 to exclude from primary selection, got Weight=%d", w)
	}
}

// TestBuildRouterConfig_FallbackChainOrderPreserved verifies that
// cfg.FallbackChain retains the order defined in the ModelSelector so
// tryFallback tries providers in the intended priority sequence.
func TestBuildRouterConfig_FallbackChainOrderPreserved(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	mkProvider := func(name string) *agentorcav1alpha1.ModelProvider {
		return &agentorcav1alpha1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "openai/gpt-4o-mini",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: name + "-secret", Key: "api-key"},
			},
		}
	}

	p1, p2, p3 := mkProvider("primary"), mkProvider("fallback-a"), mkProvider("fallback-b")
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:      "rule-based",
			Providers:     []agentorcav1alpha1.ProviderWeight{{Name: "primary", Weight: 100}},
			FallbackChain: []string{"fallback-a", "fallback-b"},
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"},
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(p1, p2, p3, selector, agent, run).
		Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "test-sa", nil)
	if err != nil {
		t.Fatalf("buildRouterConfig: %v", err)
	}

	if len(cfg.FallbackChain) != 2 || cfg.FallbackChain[0] != "fallback-a" || cfg.FallbackChain[1] != "fallback-b" {
		t.Errorf("FallbackChain order mangled: %v", cfg.FallbackChain)
	}
}

// TestBuildDeploymentRouterConfig_FallbackProvidersLoadedWithZeroWeight mirrors
// TestBuildRouterConfig_FallbackProvidersLoadedWithZeroWeight for AgentDeployment.
func TestBuildDeploymentRouterConfig_FallbackProvidersLoadedWithZeroWeight(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	primary, fallback, selector, agent := fallbackFixture(scheme)

	deploy := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentDeploymentSpec{
			AgentRef: "test-agent",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, fallback, selector, agent, deploy).
		Build()

	r := &AgentDeploymentReconciler{Client: cl, Scheme: scheme}
	cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "test-sa", nil)
	if err != nil {
		t.Fatalf("buildDeploymentRouterConfig: %v", err)
	}

	byName := make(map[string]int)
	for _, p := range cfg.Providers {
		byName[p.Name] = p.Weight
	}

	if w, ok := byName["primary-provider"]; !ok || w != 100 {
		t.Errorf("primary-provider: want Weight=100, got Weight=%d (present=%v)", w, ok)
	}
	if w, ok := byName["fallback-provider"]; !ok {
		t.Errorf("fallback-provider missing from cfg.Providers: tryFallback cannot reach it")
	} else if w != 0 {
		t.Errorf("fallback-provider: want Weight=0 to exclude from primary selection, got Weight=%d", w)
	}
}

// TestBuildRouterConfig_FallbackProviderNotDeployed_Skipped verifies that a
// fallback provider whose ModelProvider CR does not exist (e.g. disabled in the
// model-providers chart) is silently skipped rather than blocking the run.
// This is the exact scenario that caused "dependency not ready" log spam.
func TestBuildRouterConfig_FallbackProviderNotDeployed_Skipped(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	primary := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "primary-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel:   "openai/gpt-4o",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "primary-secret", Key: "api-key"},
		},
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:      "rule-based",
			Providers:     []agentorcav1alpha1.ProviderWeight{{Name: "primary-provider", Weight: 100}},
			FallbackChain: []string{"disabled-provider"}, // no CR in cluster
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"},
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, selector, agent, run). // no disabled-provider object
		Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "test-sa", nil)
	if err != nil {
		t.Fatalf("expected no error for missing fallback provider CR, got: %v", err)
	}

	// Only the primary provider should be in cfg.Providers.
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "primary-provider" {
		t.Errorf("cfg.Providers = %v; want only [primary-provider]", cfg.Providers)
	}
}

// TestBuildRouterConfig_PrimaryAlsoInFallback_NotDuplicated verifies that a
// provider listed in both Providers and FallbackChain appears exactly once in
// cfg.Providers (de-duplicated by seenProviders).
func TestBuildRouterConfig_PrimaryAlsoInFallback_NotDuplicated(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	provider := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-provider", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel:   "openai/gpt-4o",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "shared-secret", Key: "api-key"},
		},
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:      "rule-based",
			Providers:     []agentorcav1alpha1.ProviderWeight{{Name: "shared-provider", Weight: 80}},
			FallbackChain: []string{"shared-provider"},
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"},
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, selector, agent, run).
		Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "test-sa", nil)
	if err != nil {
		t.Fatalf("buildRouterConfig: %v", err)
	}

	count := 0
	for _, p := range cfg.Providers {
		if p.Name == "shared-provider" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared-provider appears %d times in cfg.Providers, want exactly 1", count)
	}
}
