package controller

import (
	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// providerConfigFromSpec maps a ModelProvider CR to the router's ProviderConfig.
// It is the single place CRD fields reach the model-router sidecar; both the
// AgentRun and AgentDeployment controllers build their provider lists through it.
func providerConfigFromSpec(name string, mp *agentorcav1alpha1.ModelProvider, weight int, routingHint, apiKeyFile string) router.ProviderConfig {
	return router.ProviderConfig{
		Name:               name,
		LiteLLMModel:       mp.Spec.LiteLLMModel,
		APIKeyFile:         apiKeyFile,
		Weight:             weight,
		Capabilities:       mp.Spec.Capabilities,
		ContextWindow:      mp.Spec.Constraints.ContextWindow,
		MaxOutputTokens:    mp.Spec.Constraints.MaxOutputTokens,
		MaxRequestTokens:   mp.Spec.Constraints.MaxRequestTokens,
		LatencyProfile:     mp.Spec.LatencyProfile,
		CostPerInputToken:  parseFloat(mp.Spec.Constraints.CostPerMillionInputTokens) / 1e6,
		CostPerOutputToken: parseFloat(mp.Spec.Constraints.CostPerMillionOutputTokens) / 1e6,
		BaseURL:            mp.Spec.BaseURL,
		WorkspaceID:        mp.Spec.WorkspaceID,
		RoutingHint:        routingHint,
	}
}
