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

// Discoverability modes for an MCPServer's tools.
const (
	// MCPServerDiscoverabilityEnabled auto-discovers all tools at runtime via
	// tools/list (the default). spec.tools is optional.
	MCPServerDiscoverabilityEnabled = "enabled"
	// MCPServerDiscoverabilityDisabled requires tools to be explicitly declared
	// in spec.tools.
	MCPServerDiscoverabilityDisabled = "disabled"
)

// MCPServerSpec defines the desired state of an MCPServer.
type MCPServerSpec struct {
	// Transport is the MCP transport protocol.
	// +kubebuilder:validation:Enum=stdio;http;sse
	// +kubebuilder:default=stdio
	// +optional
	Transport string `json:"transport,omitempty"`

	// URL is the MCP server URL for http and sse transports.
	// If set, the server runs externally and no sidecar is injected.
	// +optional
	URL string `json:"url,omitempty"`

	// OCIRef is the OCI image reference for stdio transport servers
	// that run as a sidecar. Must be signed with Cosign unless
	// imageVerification.skip is true.
	// +optional
	OCIRef string `json:"ociRef,omitempty"`

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

	// Tools declares the tools this MCP server provides.
	// Each entry becomes a child Tool CR managed by the MCPServer controller.
	//
	// Optional when spec.discoverability is "enabled" (the default): if Tools is
	// empty the controller creates a single connector Tool CR (named after the
	// MCPServer) and the model-router discovers every tool at runtime via tools/list.
	// When spec.discoverability is "disabled", Tools is required.
	// +optional
	Tools []MCPServerTool `json:"tools,omitempty"`

	// Discoverability controls how MCP tools are surfaced to agents.
	//
	// "enabled" (default): the model-router auto-discovers all tools at runtime via
	// tools/list and exposes them to the LLM. spec.tools is optional — when omitted,
	// the controller creates one connector Tool CR (named after the MCPServer) and the
	// agent references the server by that name. Any tools declared in spec.tools are
	// still created as child Tool CRs (useful for access-control cataloguing).
	//
	// "disabled": spec.tools is required and the agent must reference each declared
	// Tool CR explicitly.
	// +kubebuilder:validation:Enum=enabled;disabled
	// +kubebuilder:default=enabled
	// +optional
	Discoverability string `json:"discoverability,omitempty"`

	// IncludePatterns is a list of glob patterns used to filter the tools the
	// model-router discovers at runtime. When non-empty, only tools whose name matches
	// at least one pattern are exposed to agents. Supports "*", "?", and "**" for
	// recursive multi-segment matching (e.g. "search/**"). Mirrors the
	// includePatterns convention used by KnowledgeBase ingestion.
	// +optional
	IncludePatterns []string `json:"includePatterns,omitempty"`

	// ExcludePatterns is a list of glob patterns to hide discovered tools. A tool
	// matching any pattern is dropped before it reaches the LLM, even if it matched
	// an IncludePatterns entry (exclude always wins). Applied after IncludePatterns.
	// +optional
	ExcludePatterns []string `json:"excludePatterns,omitempty"`

	// AllowedAgents is the explicit list of Agent names (in the same namespace) that are
	// permitted to use this MCP server. Access is denied by default — if this list is
	// empty, no agent may access the server regardless of what tools it declares.
	//
	// The agent-orca operator enforces this at AgentRun creation time: if the agent
	// running the run is not listed here, the run is failed before the pod is scheduled.
	// At runtime, the model-router sidecar sends a short-lived Kubernetes ServiceAccount
	// JWT (audience "agentorca/mcp") with every HTTP/SSE request to the MCP server so the
	// server can independently verify the caller's identity via the TokenReview API.
	//
	// Example — grant access to two agents:
	//   allowedAgents:
	//   - analyst
	//   - report-builder
	// +optional
	AllowedAgents []string `json:"allowedAgents,omitempty"`

	// NetworkEgress defines additional outbound network rules for agent pods
	// using tools from this MCP server.
	// +optional
	NetworkEgress []EgressRule `json:"networkEgress,omitempty"`

	// Resources sets CPU/memory limits for the MCP server sidecar.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// AllowApps enables MCP App rendering for this server. When false (default),
	// _meta.ui.resourceUri is ignored and no sandboxed iframe is rendered in the UI.
	// Set to true only for MCP servers whose HTML output you explicitly trust.
	// +optional
	AllowApps bool `json:"allowApps,omitempty"`
}

// MCPServerTool declares a single tool provided by an MCP server.
type MCPServerTool struct {
	// Name is the tool name. Combined with the MCPServer name to form the
	// child Tool CR name: <mcpserver>-<name>.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// Description is a human-readable description shown to the LLM.
	// +optional
	Description string `json:"description,omitempty"`

	// InputSchema is a JSON Schema object for the tool's input parameters.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	InputSchema *runtime.RawExtension `json:"inputSchema,omitempty"`
}

// MCPServerStatus defines the observed state of an MCPServer.
type MCPServerStatus struct {
	// Ready indicates all child Tools were created successfully.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Discovering is true when spec.discoverability is "enabled" and spec.tools is
	// empty, meaning the controller created a connector Tool CR and the model-router
	// will discover all tools at runtime via tools/list.
	// +optional
	Discovering bool `json:"discovering,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// ToolCount is the number of child Tool CRs managed by this MCPServer.
	// +optional
	ToolCount int `json:"toolCount,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Transport",type=string,JSONPath=`.spec.transport`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Tools",type=integer,JSONPath=`.status.toolCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MCPServer declares an MCP server and the tools it provides.
// The controller generates one child Tool CR per declared tool.
type MCPServer struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerSpec   `json:"spec,omitempty"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MCPServerList contains a list of MCPServer.
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
