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

// ModelProviderSpec defines a connection to an LLM endpoint via a LiteLLM model string.
type ModelProviderSpec struct {
	// LiteLLMModel is the LiteLLM model identifier used to dispatch requests.
	// Examples:
	//   "anthropic/claude-sonnet-4-6"
	//   "gemini/gemini-2.0-flash"
	//   "ollama/llama3.2"
	//   "bedrock/anthropic.claude-3-5-sonnet-20241022-v2:0"
	//   "openai/gpt-4o"
	// +kubebuilder:validation:MinLength=1
	LiteLLMModel string `json:"litellmModel"`

	// BaseURL overrides the default API endpoint derived from the LiteLLM model prefix.
	// Use this for self-hosted models (e.g. "http://ollama.default.svc:11434")
	// or when the provider endpoint differs from the default.
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// CredentialsRef points to the Secret containing the provider API key.
	// The model-router sidecar mounts this secret directly; the operator never reads it.
	CredentialsRef SecretKeyRef `json:"credentialsRef"`

	// Capabilities tags this model's strengths, used by the rule-based router.
	// Known values: reasoning, code, vision, fast, long-context, cheap
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// Constraints describes the model's resource limits and pricing.
	// +optional
	Constraints ModelConstraints `json:"constraints,omitempty"`

	// LatencyProfile is a coarse latency classification used by the router.
	// +kubebuilder:validation:Enum=fast;medium;slow
	// +kubebuilder:default=medium
	// +optional
	LatencyProfile string `json:"latencyProfile,omitempty"`
}

// ModelConstraints describes a model's capacity limits and pricing.
type ModelConstraints struct {
	// ContextWindow is the maximum number of tokens the model accepts.
	// +optional
	ContextWindow int `json:"contextWindow,omitempty"`

	// MaxOutputTokens is the maximum number of tokens the model can generate.
	// +optional
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`

	// MaxRequestTokens is the effective per-request token limit for this provider.
	// When set, the router will not route requests whose estimated token count
	// exceeds this value to this provider. Use this when the provider's account-level
	// rate limit (e.g. TPM) is lower than the model's ContextWindow.
	// 0 means no per-request limit (ContextWindow is used for sizing decisions).
	// +optional
	MaxRequestTokens int `json:"maxRequestTokens,omitempty"`

	// CostPerMillionInputTokens is the USD cost per million input tokens as a decimal string.
	// Example: "3.00" (i.e. $3 per million input tokens)
	// +optional
	CostPerMillionInputTokens string `json:"costPerMillionInputTokens,omitempty"`

	// CostPerMillionOutputTokens is the USD cost per million output tokens as a decimal string.
	// Example: "15.00" (i.e. $15 per million output tokens)
	// +optional
	CostPerMillionOutputTokens string `json:"costPerMillionOutputTokens,omitempty"`
}

// ModelProviderStatus defines the observed state of a ModelProvider.
type ModelProviderStatus struct {
	// Ready indicates the provider endpoint is reachable and credentials are valid.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// LastChecked is the last time the provider was health-checked.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.litellmModel`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Latency",type=string,JSONPath=`.spec.latencyProfile`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelProvider registers an LLM endpoint for use by agents.
type ModelProvider struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelProviderSpec   `json:"spec,omitempty"`
	Status ModelProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelProviderList contains a list of ModelProvider.
type ModelProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelProvider{}, &ModelProviderList{})
}
