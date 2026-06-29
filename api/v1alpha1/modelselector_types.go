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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelSelectorSpec defines a routing policy across one or more ModelProviders.
type ModelSelectorSpec struct {
	// Strategy determines how the model-router selects a provider for each request.
	// - rule-based: deterministic routing via capability tags, cost, and latency SLOs
	// - llm-meta:   a cheap LLM decides which model to use for each request
	// - hybrid:     rule-based fast path with LLM meta-router for low-confidence cases
	// +kubebuilder:validation:Enum=rule-based;llm-meta;hybrid
	// +kubebuilder:default=rule-based
	// +optional
	Strategy string `json:"strategy,omitempty"`

	// Providers lists the ModelProviders available to this selector and their weights.
	// Weights are used for weighted random selection when no other routing rule applies.
	// +kubebuilder:validation:MinItems=1
	Providers []ProviderWeight `json:"providers"`

	// FallbackChain is an ordered list of ModelProvider names used when a provider
	// returns a 429 or 5xx. The router tries each in order.
	// +optional
	FallbackChain []string `json:"fallbackChain,omitempty"`

	// BudgetCap enforces spending limits per run and per day.
	// +optional
	BudgetCap *BudgetCap `json:"budgetCap,omitempty"`

	// CapabilityRouting maps capability tags to preferred provider names.
	// Example: {"code": "gpt-4o", "reasoning": "claude-sonnet", "fast": "llama3-local"}
	// +optional
	CapabilityRouting map[string]string `json:"capabilityRouting,omitempty"`

	// MetaRouter configures the LLM meta-router used in "llm-meta" and "hybrid" strategies.
	// +optional
	MetaRouter *MetaRouterConfig `json:"metaRouter,omitempty"`
}

// ProviderWeight pairs a ModelProvider name with a routing weight.
type ProviderWeight struct {
	// Name is the name of a ModelProvider in the same namespace.
	Name string `json:"name"`

	// Weight controls the probability of selecting this provider when using weighted
	// random selection. Higher values mean more traffic. Defaults to 100.
	// Set to 0 to exclude from random selection (meta-router and error fallback only).
	// +kubebuilder:default=100
	// +optional
	Weight int `json:"weight,omitempty"`

	// RoutingHint is a short description passed to the LLM meta-router to help it
	// decide when to select this provider. Example:
	//   "Use for complex, security-sensitive changes: auth, crypto, architecture."
	// +optional
	RoutingHint string `json:"routingHint,omitempty"`
}

// BudgetCap enforces USD spending limits.
type BudgetCap struct {
	// PerRun is the maximum USD spend allowed for a single AgentRun.
	// When reached the router switches to the cheapest available model,
	// then returns an error if spending still exceeds the cap.
	// Example: "0.50"
	// +optional
	PerRun string `json:"perRun,omitempty"`

	// PerDay is the maximum USD spend across all runs per calendar day.
	// Example: "50.00"
	// +optional
	PerDay string `json:"perDay,omitempty"`
}

// MetaRouterConfig configures the LLM-based meta-router.
type MetaRouterConfig struct {
	// ProviderRef is the name of a ModelProvider to use for routing decisions.
	// Should be a cheap, fast model (e.g., a small local model or a flash variant).
	ProviderRef string `json:"providerRef"`

	// Threshold is the minimum rule-router confidence score below which the
	// meta-router is invoked. Range 0.0–1.0. Defaults to 0.6.
	// +kubebuilder:validation:Pattern=`^(0(\.\d+)?|1(\.0+)?)$`
	// +kubebuilder:default="0.6"
	// +optional
	Threshold string `json:"threshold,omitempty"`
}

// ModelSelectorStatus defines the observed state of a ModelSelector.
type ModelSelectorStatus struct {
	// ActiveProviders lists ModelProvider names that are currently healthy.
	// +optional
	ActiveProviders []string `json:"activeProviders,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.strategy`
// +kubebuilder:printcolumn:name="Providers",type=integer,JSONPath=`.spec.providers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelSelector defines a routing policy across multiple ModelProviders.
type ModelSelector struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelSelectorSpec   `json:"spec,omitempty"`
	Status ModelSelectorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelSelectorList contains a list of ModelSelector.
type ModelSelectorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelSelector `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelSelector{}, &ModelSelectorList{})
}
