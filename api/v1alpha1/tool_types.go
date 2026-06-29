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
	"k8s.io/apimachinery/pkg/runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolType describes how a tool is implemented.
// +kubebuilder:validation:Enum=regular;agent;mcp;wasm
type ToolType string

const (
	// ToolTypeRegular is a standard OCI-packaged tool.
	ToolTypeRegular ToolType = "regular"
	// ToolTypeAgent exposes another Agent as a callable tool (orchestrator pattern).
	ToolTypeAgent ToolType = "agent"
	// ToolTypeMCP wraps an MCP (Model Context Protocol) server.
	ToolTypeMCP ToolType = "mcp"
	// ToolTypeWasm runs a WASM module in a sandbox.
	ToolTypeWasm ToolType = "wasm"
)

// ToolExecutionMode defines how a tool pod or process is launched.
// +kubebuilder:validation:Enum=pod;sidecar;wasm
type ToolExecutionMode string

const (
	// ToolExecPod spawns a fresh pod per tool invocation (maximum isolation).
	ToolExecPod ToolExecutionMode = "pod"
	// ToolExecSidecar runs the tool as a sidecar in the agent pod (stateful, low-latency).
	ToolExecSidecar ToolExecutionMode = "sidecar"
	// ToolExecWasm runs the tool as a WASM module inside the model-router.
	ToolExecWasm ToolExecutionMode = "wasm"
)

// ToolSpec defines a capability available to agents.
type ToolSpec struct {
	// Type declares the tool implementation kind.
	// +kubebuilder:default=regular
	// +optional
	Type ToolType `json:"type,omitempty"`

	// OCIRef is the OCI image reference for regular, agent-type (ignored), and wasm tools.
	// Must be signed with Cosign unless imageVerification.skip is true.
	// Not required for mcp tools that use a static URL.
	// +optional
	OCIRef string `json:"ociRef,omitempty"`

	// AgentRef names the Agent CRD to expose as a tool.
	// Only used when Type is "agent".
	// +optional
	AgentRef string `json:"agentRef,omitempty"`

	// ExecutionMode controls how the tool runs.
	// Defaults to "pod" for regular tools, "sidecar" for mcp tools, "wasm" for wasm tools.
	// +optional
	ExecutionMode ToolExecutionMode `json:"executionMode,omitempty"`

	// Schema is the JSON Schema for the tool's input and output.
	// Exposed to agents as an OpenAI function definition.
	// +optional
	Schema *ToolSchema `json:"schema,omitempty"`

	// MCPConfig configures an MCP server. Only used when Type is "mcp".
	// +optional
	MCPConfig *MCPConfig `json:"mcpConfig,omitempty"`

	// NetworkEgress defines additional outbound network rules for tool pods.
	// Added to the per-run NetworkPolicy allowlist.
	// +optional
	NetworkEgress []EgressRule `json:"networkEgress,omitempty"`

	// Resources sets CPU/memory limits for the tool pod or sidecar.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// SecretRefs lists Kubernetes Secrets to inject into tool pods or MCP sidecars.
	// For pod-type tools: mounted as volumes or env vars in the tool pod.
	// For MCP sidecar tools: mounted into the model-router sidecar container.
	// +optional
	SecretRefs []SecretMount `json:"secretRefs,omitempty"`

	// CloudAuth configures cloud-provider identity for tool pods.
	// Overrides Agent-level cloudAuth for this tool.
	// +optional
	CloudAuth *CloudAuthSpec `json:"cloudAuth,omitempty"`

	// Command overrides the container entrypoint for pod-type tools.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the container command arguments for pod-type tools.
	// +optional
	Args []string `json:"args,omitempty"`
}

// ToolSchema describes the JSON Schema for tool input and output.
type ToolSchema struct {
	// Description is a human-readable description shown to the LLM.
	// +optional
	Description string `json:"description,omitempty"`

	// Input is a JSON Schema object for the tool's input parameters.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Input *runtime.RawExtension `json:"input,omitempty"`

	// Output is a JSON Schema object for the tool's output.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Output *runtime.RawExtension `json:"output,omitempty"`
}

// MCPConfig configures an MCP (Model Context Protocol) server.
type MCPConfig struct {
	// Transport is the MCP transport protocol.
	// +kubebuilder:validation:Enum=stdio;http;sse
	// +kubebuilder:default=stdio
	// +optional
	Transport string `json:"transport,omitempty"`

	// URL is the MCP server URL for http and sse transports.
	// If set, the server runs externally and no sidecar is injected.
	// +optional
	URL string `json:"url,omitempty"`

	// Args are the command-line arguments passed to the MCP server process.
	// Only used for stdio transport.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env sets environment variables for the MCP server process.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// EnvFrom sets environment variables sourced from Kubernetes Secrets.
	// These are merged with (and override) Env entries of the same name.
	// +optional
	EnvFrom []EnvVar `json:"envFrom,omitempty"`

	// Auth configures authentication for HTTP/SSE transports.
	// Not supported for stdio transport (use EnvFrom instead).
	// +optional
	Auth *MCPAuthConfig `json:"auth,omitempty"`
}

// MCPAuthConfig configures authentication for remote MCP servers.
// Exactly one of BearerToken, APIKey, or Headers should be set.
type MCPAuthConfig struct {
	// BearerToken sets the Authorization header to "Bearer <token>".
	// The token value is read from a Kubernetes Secret.
	// +optional
	BearerToken *SecretKeyRef `json:"bearerToken,omitempty"`

	// APIKey sets a custom header with a key read from a Secret.
	// +optional
	APIKey *APIKeyAuth `json:"apiKey,omitempty"`

	// Headers sets custom HTTP headers from Secrets.
	// Use this for non-standard auth schemes.
	// +optional
	Headers []AuthHeader `json:"headers,omitempty"`
}

// APIKeyAuth configures API key authentication via a custom header.
type APIKeyAuth struct {
	// SecretKeyRef references the Secret containing the API key.
	SecretKeyRef SecretKeyRef `json:"secretKeyRef"`
	// HeaderName is the HTTP header name. Defaults to "X-API-Key".
	// +kubebuilder:default="X-API-Key"
	// +optional
	HeaderName string `json:"headerName,omitempty"`
}

// AuthHeader maps an HTTP header name to a Secret key reference.
type AuthHeader struct {
	// Name is the HTTP header name (e.g., "Authorization", "X-Custom-Token").
	Name string `json:"name"`
	// SecretKeyRef references the Secret containing the header value.
	SecretKeyRef SecretKeyRef `json:"secretKeyRef"`
}

// ToolStatus defines the observed state of a Tool.
type ToolStatus struct {
	// Ready indicates the tool is validated and available.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// SignatureVerified indicates the OCI artifact signature was verified by Cosign.
	// +optional
	SignatureVerified bool `json:"signatureVerified,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="ExecMode",type=string,JSONPath=`.spec.executionMode`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tool defines a capability available to agents.
type Tool struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ToolSpec   `json:"spec,omitempty"`
	Status ToolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ToolList contains a list of Tool.
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tool{}, &ToolList{})
}
