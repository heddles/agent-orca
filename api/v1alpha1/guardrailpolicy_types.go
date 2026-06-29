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

// GuardrailPolicySpec defines content filtering and validation rules applied to
// agent inputs and outputs by the model-router sidecar.
type GuardrailPolicySpec struct {
	// OutputFilters are applied to LLM responses before they reach the agent/caller.
	// Filters are evaluated in order; the first "block" action stops processing.
	// +optional
	OutputFilters []GuardrailFilter `json:"outputFilters,omitempty"`

	// InputFilters are applied to user messages before they reach the LLM.
	// +optional
	InputFilters []GuardrailFilter `json:"inputFilters,omitempty"`

	// FormatValidation enforces structural constraints on the final agent output.
	// +optional
	FormatValidation *FormatValidation `json:"formatValidation,omitempty"`
}

// GuardrailFilter is a single content filter rule.
type GuardrailFilter struct {
	// Name is a human-readable identifier for this filter (used in logs and events).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type selects the filter implementation.
	// +kubebuilder:validation:Enum=regex;keyword-blocklist;topic-validation
	Type string `json:"type"`

	// Action determines what happens when the filter matches.
	// "redact" replaces matched content with the replacement string.
	// "block" rejects the entire message and returns blockMessage to the caller.
	// "warn" logs a warning event but allows the message through.
	// +kubebuilder:validation:Enum=redact;block;warn
	Action string `json:"action"`

	// Patterns are regex-based match rules. Used when Type is "regex".
	// +optional
	Patterns []RegexPattern `json:"patterns,omitempty"`

	// Keywords configures keyword blocklist matching. Used when Type is "keyword-blocklist".
	// +optional
	Keywords *KeywordConfig `json:"keywords,omitempty"`

	// AllowedTopics restricts responses to these topics. Used when Type is "topic-validation".
	// +optional
	AllowedTopics []string `json:"allowedTopics,omitempty"`

	// BlockMessage is the message returned to the caller when Action is "block".
	// +optional
	BlockMessage string `json:"blockMessage,omitempty"`
}

// RegexPattern defines a named regex match/replace rule.
type RegexPattern struct {
	// Name identifies this pattern (e.g. "ssn", "email", "credit-card").
	Name string `json:"name"`

	// Pattern is a Go-compatible regular expression.
	// +kubebuilder:validation:MinLength=1
	Pattern string `json:"pattern"`

	// Replacement is the string that replaces matched content when action is "redact".
	// +optional
	Replacement string `json:"replacement,omitempty"`
}

// KeywordConfig defines keyword blocklist sources.
type KeywordConfig struct {
	// Inline is a list of keywords to block.
	// +optional
	Inline []string `json:"inline,omitempty"`

	// SecretRef references a Secret containing a newline-delimited keyword list.
	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`
}

// FormatValidation enforces structural constraints on agent output.
type FormatValidation struct {
	// JSONSchema is a JSON Schema document that the agent's final output must conform to.
	// +optional
	JSONSchema string `json:"jsonSchema,omitempty"`

	// MaxTokens limits the maximum output token count.
	// +optional
	MaxTokens int `json:"maxTokens,omitempty"`
}

// GuardrailPolicyStatus holds the observed state of a GuardrailPolicy.
type GuardrailPolicyStatus struct {
	// Ready indicates the policy has been validated and can be enforced.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="InputFilters",type=integer,JSONPath=`.status.inputFilterCount`,priority=1
// +kubebuilder:printcolumn:name="OutputFilters",type=integer,JSONPath=`.status.outputFilterCount`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GuardrailPolicy defines content filtering and validation rules for agent inputs and outputs.
// Referenced by Agents via spec.guardrailPolicyRef, or by TenantConfig as a namespace default.
type GuardrailPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GuardrailPolicySpec   `json:"spec,omitempty"`
	Status GuardrailPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GuardrailPolicyList contains a list of GuardrailPolicy.
type GuardrailPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GuardrailPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GuardrailPolicy{}, &GuardrailPolicyList{})
}
