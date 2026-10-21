package controller

import (
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestProviderConfigFromSpec(t *testing.T) {
	mp := &agentorcav1alpha1.ModelProvider{Spec: agentorcav1alpha1.ModelProviderSpec{
		LiteLLMModel:   "anthropic/claude-sonnet-5-5",
		BaseURL:        "http://proxy",
		WorkspaceID:    "wrkspc_123",
		Capabilities:   []string{"code"},
		LatencyProfile: "fast",
		Constraints: agentorcav1alpha1.ModelConstraints{
			ContextWindow:              1000000,
			MaxOutputTokens:            128000,
			MaxRequestTokens:           5,
			CostPerMillionInputTokens:  "2.00",
			CostPerMillionOutputTokens: "10.00",
		},
	}}

	got := providerConfigFromSpec("p", mp, 60, "hint", "/secrets/p/api-key")

	if got.Name != "p" || got.Weight != 60 || got.RoutingHint != "hint" || got.APIKeyFile != "/secrets/p/api-key" {
		t.Errorf("caller-supplied fields wrong: %+v", got)
	}
	if got.WorkspaceID != "wrkspc_123" {
		t.Errorf("WorkspaceID = %q, want wrkspc_123", got.WorkspaceID)
	}
	if got.LiteLLMModel != "anthropic/claude-sonnet-5-5" || got.BaseURL != "http://proxy" ||
		got.LatencyProfile != "fast" || got.ContextWindow != 1000000 ||
		got.MaxOutputTokens != 128000 || got.MaxRequestTokens != 5 {
		t.Errorf("spec fields wrong: %+v", got)
	}
	if got.CostPerInputToken != 2.0/1e6 || got.CostPerOutputToken != 10.0/1e6 {
		t.Errorf("costs wrong: %v %v", got.CostPerInputToken, got.CostPerOutputToken)
	}
}
