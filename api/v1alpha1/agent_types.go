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

// AgentSpec defines the desired state of an Agent.
type AgentSpec struct {
	// ModelSelectorRef names the ModelSelector that routes LLM calls for this agent.
	// +kubebuilder:validation:MinLength=1
	ModelSelectorRef string `json:"modelSelectorRef"`

	// Tools is the list of Tool names (in the same namespace) available to this agent.
	// The model-router injects these as OpenAI function definitions for every run.
	// +optional
	Tools []string `json:"tools,omitempty"`

	// SystemPrompt is the system-level instruction prepended to every conversation.
	// Leave empty to use the agent's own system prompt.
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Runtime defines the OCI image used to run the agent process.
	// +kubebuilder:validation:Required
	Runtime AgentRuntime `json:"runtime"`

	// Memory configures conversation state persistence and failure recovery.
	// +optional
	Memory *AgentMemoryConfig `json:"memory,omitempty"`

	// Resources sets default CPU/memory limits for AgentRun pods.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// ServiceAccountRef references a pre-existing ServiceAccount to use for agent pods.
	// If unset, the operator creates and manages "agentorca-agent-<name>".
	// Use this when the SA already has cloud provider annotations (IRSA, Workload Identity).
	// +optional
	ServiceAccountRef *LocalObjectRef `json:"serviceAccountRef,omitempty"`

	// CloudAuth declares cloud-provider identity bindings.
	// The operator applies the appropriate SA annotations automatically.
	// +optional
	CloudAuth *CloudAuthSpec `json:"cloudAuth,omitempty"`

	// KnowledgeBases lists KnowledgeBase names (same namespace) available to this agent.
	// When non-empty, the model-router injects _rag_search and _rag_ingest as built-in tools.
	// For finer control (e.g. confirmRequired), use KnowledgeBaseRefs instead.
	// +optional
	KnowledgeBases []string `json:"knowledgeBases,omitempty"`

	// KnowledgeBaseRefs is the structured alternative to KnowledgeBases.
	// When set, KnowledgeBases is ignored. Each ref supports per-KB options
	// such as requiring user confirmation before ingesting fixes.
	// +optional
	KnowledgeBaseRefs []KnowledgeBaseRef `json:"knowledgeBaseRefs,omitempty"`

	// GuardrailPolicyRef names the GuardrailPolicy (same namespace) applied to this agent's
	// inputs and outputs. The model-router enforces the policy's filters on every LLM call.
	// +optional
	GuardrailPolicyRef string `json:"guardrailPolicyRef,omitempty"`

	// NetworkIsolation configures network-level isolation between the agent and model-router.
	// When SplitPod is enabled, the agent and model-router run in separate Kubernetes Pods
	// with distinct NetworkPolicies, providing network-level egress isolation that the
	// default single-pod sidecar topology cannot enforce (containers in a Pod share a
	// network namespace, so NetworkPolicy cannot differentiate them).
	// +optional
	NetworkIsolation *NetworkIsolationSpec `json:"networkIsolation,omitempty"`

	// DisableClarify prevents the auto-trigger clarify safety net from activating.
	// Use this for autonomous agents that should not block waiting for human input.
	// When true, the _clarify tool is still available but auto-detection is disabled.
	// +kubebuilder:default=false
	// +optional
	DisableClarify bool `json:"disableClarify,omitempty"`
}

// NetworkIsolationSpec configures split-pod topology for agent/router network separation.
type NetworkIsolationSpec struct {
	// SplitPod runs the agent and model-router in separate Pods with distinct NetworkPolicies.
	// The agent pod's egress is restricted to the router Service (ports 8080/8082) and DNS.
	// The router pod retains all required egress (provider HTTPS, Kubernetes API, Redis, tools).
	// This provides a Kubernetes-level network guarantee that agent code cannot open arbitrary
	// outbound connections when platform policy requires all LLM traffic to go via model-router.
	// +kubebuilder:default=false
	// +optional
	SplitPod bool `json:"splitPod,omitempty"`
}

// KnowledgeBaseRef references a KnowledgeBase with per-KB options.
type KnowledgeBaseRef struct {
	// Name is the KnowledgeBase CR name (same namespace).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// ConfirmRequired when true replaces the _rag_ingest tool with _propose_fix and
	// _confirm_fix for this knowledge base. Fixes are staged in Redis and only promoted
	// to the KnowledgeBase when the user explicitly confirms they worked.
	// +optional
	ConfirmRequired bool `json:"confirmRequired,omitempty"`
}

// AgentRuntime describes the agent process container image and framework.
type AgentRuntime struct {
	// OCIRef is the OCI image reference for the agent process.
	// Must be signed with Cosign unless imageVerification.skip is set.
	// +kubebuilder:validation:MinLength=1
	OCIRef string `json:"ociRef"`

	// Framework hints to the operator which env var injection tier to use.
	// Supported values:
	//   openai-compatible  - inject OPENAI_BASE_URL + OPENAI_API_KEY (default)
	//   autogen            - inject OAI_CONFIG_LIST via init container
	//   semantic-kernel    - inject appsettings via init container
	//   langgraph          - openai-compatible + inject LANGGRAPH_CHECKPOINT_URL
	//   shim               - hosts-file shim for frameworks that hardcode their endpoint
	// +kubebuilder:validation:Enum=openai-compatible;autogen;semantic-kernel;langgraph;shim
	// +kubebuilder:default=openai-compatible
	// +optional
	Framework string `json:"framework,omitempty"`

	// ShimTarget is the hostname to intercept when Framework is "shim".
	// Example: "api.openai.com", "generativelanguage.googleapis.com"
	// +optional
	ShimTarget string `json:"shimTarget,omitempty"`

	// Command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the container command arguments.
	// +optional
	Args []string `json:"args,omitempty"`

	// InputMode controls how the operator delivers the run input to the agent.
	// env  - inject AGENTORC_INPUT env var (default)
	// http - POST the input as JSON to the agent's HTTP endpoint
	// +kubebuilder:validation:Enum=env;http
	// +optional
	InputMode string `json:"inputMode,omitempty"`

	// InputPort is the TCP port the agent's HTTP server listens on (http mode only).
	// Defaults to 8000.
	// +optional
	InputPort int `json:"inputPort,omitempty"`

	// InputPath is the HTTP path to POST the run input to (http mode only).
	// Defaults to /invoke.
	// +optional
	InputPath string `json:"inputPath,omitempty"`

	// SecurityContextOverride opts the agent container (only the "agent" container,
	// never the model-router sidecar) out of the default restricted PodSecurityStandard.
	//
	// Intended ONLY for purpose-built agents that require elevated privileges the
	// restricted baseline forbids — e.g. a red-team pwnbox that must bring up a VPN
	// tun device (requires NET_ADMIN + /dev/net/tun). When set, the operator mounts
	// /dev/net/tun into the agent container and applies the requested posture.
	//
	// Admission is gated by a validating webhook: the agent's namespace must carry the
	// label `agentorca.io/enable-privileged-pods: "true"` AND the Agent must reference a
	// GuardrailPolicyRef. These checks fail‑closed.
	// +optional
	SecurityContextOverride *PodSecurityOverride `json:"securityContextOverride,omitempty"`

	// SecretRefs lists Kubernetes Secrets to mount into the agent container as volumes
	// (or, if MountPath is empty, inject as env vars). Unlike Tool.spec.secretRefs —
	// which target the model-router sidecar / tool pods — these mount directly into the
	// agent container. Useful for agent containers that need an on-disk credential the
	// agent code reads directly, e.g. an HTB OpenVPN ``.ovpn`` config mounted at
	// ``/etc/htb``. Mounted read-only.
	// +optional
	SecretRefs []SecretMount `json:"secretRefs,omitempty"`
}

// PodSecurityOverride describes a scoped relaxation of the restricted pod-security
// baseline for the agent container of an Agent. See AgentRuntime.
type PodSecurityOverride struct {
	// Privileged grants the agent container full privileges (CAP_NET_ADMIN,
	// /dev/net/tun device access, seccomp/capability enforcement disabled). Use this
	// for tun-device VPNs. Mutually exclusive with AddCapabilities.
	// +optional
	Privileged bool `json:"privileged,omitempty"`

	// AddCapabilities adds specific capabilities to the agent container while keeping
	// the restricted profile otherwise (e.g. ["NET_ADMIN"] + a /dev/net/tun hostPath
	// mount). Mutually exclusive with Privileged.
	// +optional
	AddCapabilities []string `json:"addCapabilities,omitempty"`

	// RunAsUser overrides the default non-root UID (65532). 0 (root) is permitted only
	// when Privileged is true or a capability that requires root (e.g. NET_ADMIN to
	// create network interfaces) is added; the webhook enforces this.
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// ReadOnlyRootFilesystem overrides the default read-only root filesystem of the
	// agent container. When omitted and Privileged is true, defaults to writable
	// (false) since some security tooling writes helpers/logs to the root FS; when
	// omitted and non-privileged, retains the restricted default (true).
	// +optional
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
}

// AgentMemoryConfig controls conversation state persistence.
type AgentMemoryConfig struct {
	// CheckpointEvery controls how often (in LLM turns) the model-router writes a checkpoint.
	// 1 means every turn. Higher values reduce storage writes but increase recovery loss window.
	// +kubebuilder:default=1
	// +optional
	CheckpointEvery int `json:"checkpointEvery,omitempty"`

	// ResumeWindowSeconds is how long to retain state after a failure for pod restart/resume.
	// Defaults to 3600 (1 hour).
	// +kubebuilder:default=3600
	// +optional
	ResumeWindowSeconds int `json:"resumeWindowSeconds,omitempty"`

	// ArchiveOnCompletion, when true, promotes state to object storage when the run succeeds.
	// +kubebuilder:default=false
	// +optional
	ArchiveOnCompletion bool `json:"archiveOnCompletion,omitempty"`

	// EpisodicSummaryEvery summarizes the conversation every N LLM turns and injects
	// the summary as context, keeping the active context window compact.
	// 0 disables episodic memory.
	// +optional
	EpisodicSummaryEvery int `json:"episodicSummaryEvery,omitempty"`

	// SummaryModelSelectorRef names the ModelSelector used for summarization calls.
	// Defaults to the agent's own ModelSelectorRef when unset.
	// +optional
	SummaryModelSelectorRef string `json:"summaryModelSelectorRef,omitempty"`

	// LongTermMemoryRef names a KnowledgeBase (same namespace) used as persistent
	// long-term memory. The model-router automatically retrieves semantically relevant
	// memories before each turn and exposes a _memory_store tool so the agent can
	// explicitly persist facts across sessions.
	// If the KnowledgeBase is not Ready, the run proceeds without long-term memory
	// and a MemoryUnavailable event is emitted.
	// +optional
	LongTermMemoryRef string `json:"longTermMemoryRef,omitempty"`

	// Hindsight configures the hindsight memory system integration.
	// When enabled, the model-router will recall relevant memories before each
	// LLM call and retain the conversation after each turn.
	// +optional
	Hindsight *HindsightMemoryConfig `json:"hindsight,omitempty"`
}

// HindsightMemoryConfig configures the hindsight memory system integration.
type HindsightMemoryConfig struct {
	// Enabled is true when hindsight is active.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// URL is the hindsight API endpoint (e.g. http://hindsight.hindsight.svc.cluster.local:8888).
	// +optional
	URL string `json:"url,omitempty"`
	// BankIDTemplate is the template for the hindsight bank ID.
	// Can use placeholders like {namespace}, {runName}, {agentName} which will be replaced at runtime.
	// +optional
	BankIDTemplate string `json:"bankIdTemplate,omitempty"`
	// RecallBudget is the maximum number of memories to retrieve per turn.
	// +kubebuilder:default=5
	// +optional
	RecallBudget int `json:"recallBudget,omitempty"`
	// RetainOnEveryTurn is true to retain conversation after every LLM turn.
	// +kubebuilder:default=true
	// +optional
	RetainOnEveryTurn bool `json:"retainOnEveryTurn,omitempty"`
}

// AgentStatus defines the observed state of an Agent.
// Context tracking is handled at the AgentDeployment or AgentRun level,
// not on the template Agent resource itself.
type AgentStatus struct {
	// ServiceAccountName is the name of the stable SA managed by the operator.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// CloudProviderDetected is the cloud provider the operator detected at startup.
	// +optional
	CloudProviderDetected string `json:"cloudProviderDetected,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Selector",type=string,JSONPath=`.spec.modelSelectorRef`
// +kubebuilder:printcolumn:name="Framework",type=string,JSONPath=`.spec.runtime.framework`
// +kubebuilder:printcolumn:name="ServiceAccount",type=string,JSONPath=`.status.serviceAccountName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent defines a reusable agent configuration that can be instantiated as AgentRuns.
type Agent struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
