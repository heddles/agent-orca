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

// AgentDeploymentPhase describes the lifecycle phase of an AgentDeployment.
// +kubebuilder:validation:Enum=Creating;Running;Failed;Paused
type AgentDeploymentPhase string

const (
	AgentDeploymentPhaseCreating AgentDeploymentPhase = "Creating"
	AgentDeploymentPhaseRunning  AgentDeploymentPhase = "Running"
	AgentDeploymentPhaseFailed   AgentDeploymentPhase = "Failed"
	AgentDeploymentPhasePaused   AgentDeploymentPhase = "Paused"
)

// InputSourceType describes where the deployment gets input from.
// +kubebuilder:validation:Enum=chat;queue;pubsub;loop
type InputSourceType string

const (
	InputSourceChat   InputSourceType = "chat"
	InputSourceQueue  InputSourceType = "queue"
	InputSourcePubSub InputSourceType = "pubsub"
	InputSourceLoop   InputSourceType = "loop"
)

// InputSourceConfig defines where a deployment receives input from.
type InputSourceConfig struct {
	// Type specifies the input source: chat (API-driven), queue (Redis, etc), pubsub (Kafka, etc), or loop (self-managed).
	// +kubebuilder:default=chat
	// +optional
	Type InputSourceType `json:"type,omitempty"`

	// Config holds provider-specific settings.
	// Examples:
	//   queue: {provider: "redis", url: "redis://redis:6379", queue: "tasks"}
	//   pubsub: {provider: "kafka", brokers: "kafka:9092", topic: "agent-input"}
	//   loop: {initialPrompt: "Run continuously, answering questions..."}
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// DeploymentRestartPolicy controls pod restart behavior for long-running agents.
type DeploymentRestartPolicy struct {
	// MinBackoffSeconds is the minimum backoff duration after a failure. Defaults to 5.
	// +kubebuilder:default=5
	// +optional
	MinBackoffSeconds int `json:"minBackoffSeconds,omitempty"`

	// MaxBackoffSeconds is the maximum backoff duration after a failure. Defaults to 300.
	// +kubebuilder:default=300
	// +optional
	MaxBackoffSeconds int `json:"maxBackoffSeconds,omitempty"`

	// MaxConsecutiveFailures is the number of consecutive failures before pausing the deployment.
	// 0 means unlimited (always restart). Defaults to 10.
	// +kubebuilder:default=10
	// +optional
	MaxConsecutiveFailures int `json:"maxConsecutiveFailures,omitempty"`
}

// AgentDeploymentSpec defines the desired state of an AgentDeployment.
type AgentDeploymentSpec struct {
	// AgentRef names the Agent template to run continuously.
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// InputSource configures where this deployment receives input from.
	// +optional
	InputSource *InputSourceConfig `json:"inputSource,omitempty"`

	// RestartPolicy controls failure recovery and backoff behavior.
	// +optional
	RestartPolicy *DeploymentRestartPolicy `json:"restartPolicy,omitempty"`

	// ToolExecutionTimeoutSec is the per-tool-call ceiling (seconds) applied to every
	// AgentRun spawned from this deployment and to warm pods claimed by it. It feeds
	// RouterSafeguards.ToolExecutionTimeoutSec. Defaults to the model-router's 60s when
	// 0. Set high for long-holding tooling that exceeds the default (e.g. Sliver
	// interactive sessions, shells, pivot relays): 1800–3600. A 0 value preserves
	// today's 60s behaviour.
	// +kubebuilder:default=0
	// +optional
	ToolExecutionTimeoutSec int `json:"toolExecutionTimeoutSec,omitempty"`

	// MaxToolResultTokens overrides the per-tool-result token cap for this deployment.
	// 0 (default) = the operator computes a context-window-aware cap (~10% of the
	// model's ContextWindow, floored at 8000 and capped at 64000 tokens, and never more
	// than half of MaxRequestTokens so a single result can't by itself blow the
	// request budget) so large MCP/file/commit-patch results are not silently
	// truncated. Eyeballed against the loudest offender: router.go is ~153K chars
	// (~38k tokens) on the 1M-context poolside model — the default 64k cap reads it
	// in full. Raise further for agents that routinely read very large files, or lower
	// for spend-thrifty agents.
	// +optional
	MaxToolResultTokens int `json:"maxToolResultTokens,omitempty"`

	// Safeguards configures behavioral loop guards (MaxRepeatedToolCalls,
	// MaxConsecutiveNoopTurns, ToolFrequencyCap, etc.) for runs spawned from this
	// deployment. Omit to use the operator's conservative defaults; set any field to 0
	// to keep the default for that field. Fields that are left zero (defaulted) only
	// trip on genuine stuck loops and do NOT block legitimate repeated tool use
	// (MaxRepeatedToolCalls keys on identical tool+args, so reading many distinct
	// files — e.g. a PR review — never trips it). Reuses the AgentRunSafeguards shape.
	// +optional
	Safeguards *AgentRunSafeguards `json:"safeguards,omitempty"`

	// Replicas is the desired number of agent pod replicas. Defaults to 1.
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// CheckpointTTL is the duration to retain conversation checkpoints after completion.
	// Defaults to 2592000 (30 days).
	// +kubebuilder:default=2592000
	// +optional
	CheckpointTTL int `json:"checkpointTTL,omitempty"`

	// WarmPoolSize is the number of pre-warmed pods to maintain for fast run dispatch.
	// Warm pods have the agent and model-router already initialized, eliminating cold-start
	// latency (~15s) for chat runs. Set to 0 to disable (use classic per-run pods).
	// +kubebuilder:default=1
	// +optional
	WarmPoolSize int `json:"warmPoolSize,omitempty"`

	// MaxRequestsPerPod caps how many requests a warm pod will serve before it is
	// recycled (deleted and replaced). 0 (default) means warm pods are REUSED across
	// requests — after a run completes the pod is returned to the idle pool instead of
	// being killed, and is re-claimed for the next chat run. Age-based recycling is
	// disabled by default (see WarmPodMaxAge), so reuse is the normal path.
	//
	// Set >0 (e.g. 50) to rotate pods after that many served requests, which can be
	// useful to shed per-request state or refresh long-running connections.
	// +kubebuilder:default=0
	// +optional
	MaxRequestsPerPod int `json:"maxRequestsPerPod,omitempty"`

	// WarmPodMaxAge is the maximum age of a warm pod before it is recycled
	// (deleted and replaced). It defaults to 0 (indefinite) when unset: warm pods
	// then persist until manually deleted, the agent process exits, config-drift
	// recycle fires (see RecycleOnConfigDrift), or the MaxRequestsPerPod cap is hit.
	// This is the desired default for chat and long-trajectory workloads, where
	// pods must stay alive across runs so an in-progress session is never torn down
	// mid-task. When age recycling is disabled a long-lived SA token (the cluster
	// maximum) is minted for each pod so it can keep authenticating claim-run
	// POSTs for its entire life.
	//
	// Set this to a positive duration explicitly to OPT IN to age-based recycling,
	// e.g. 50m to recycle pods roughly once per LLM token budget window. (controller-gen
	// cannot express a duration default here, so 0 is applied in code.)
	// +optional
	WarmPodMaxAge *metav1.Duration `json:"warmPodMaxAge,omitempty"`

	// RecycleOnConfigDrift controls whether warm pods are recycled when the
	// router config changes (agent spec, providers/weights, tools, KBs,
	// guardrails). Defaults to true: a config change rolls the warm pool so
	// pods pick up the new config immediately.
	//
	// Set to false to keep warm pods alive across config changes. Pods continue
	// serving from the router-config snapshot they were created with and only
	// refresh on their next natural recycle (age cap or process exit). Combining
	// "WarmPodMaxAge: 0" with "RecycleOnConfigDrift: false" keeps warm pods
	// until they are manually deleted (or the agent process exits).
	// +kubebuilder:default=true
	// +optional
	RecycleOnConfigDrift *bool `json:"recycleOnConfigDrift,omitempty"`

	// WarmLocalCache controls the per-warm-pod local disk cache (an emptyDir) that
	// supplements the shared Redis state store. The model-router write-throughs each
	// checkpoint to the local disk AND to Redis, then reads from the local disk first
	// (falling back to Redis on a local miss). Because warm pods are reused across
	// chat runs (the same Pod survives multiple claims until recycled), the local
	// cache accelerates warm-pod resume (no Redis round-trip to hydrate the prior
	// conversation) and lets a warm pod keep serving if Redis is transiently
	// unavailable. The local cache is per-Pod only — it is wiped when the warm pod is
	// recycled/deleted; Redis remains the durable, cross-pod source of truth.
	//
	// Note: this is unrelated to the read-only-rootFilesystem emptyDir at /tmp
	// (which is scratch space for the restricted pod-security profile); this is a
	// separate disk-backed volume used to mirror checkpoints.
	//
	// Defaults to true when WarmPoolSize > 0 (a warm pool), false otherwise.
	// +optional
	WarmLocalCache *bool `json:"warmLocalCache,omitempty"`

	// WarmLocalCacheSizeMi is the SizeLimit (in Mi) for each warm pod's local cache
	// emptyDir. 0 means unlimited (bounded only by node ephemeral storage).
	// Defaults to 256.
	// +kubebuilder:default=256
	// +optional
	WarmLocalCacheSizeMi int `json:"warmLocalCacheSizeMi,omitempty"`
}

// AgentDeploymentStatus defines the observed state of an AgentDeployment.
type AgentDeploymentStatus struct {
	// Phase is the current lifecycle phase of the deployment.
	// +optional
	Phase AgentDeploymentPhase `json:"phase,omitempty"`

	// ReadyReplicas is the number of ready agent pods.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of available agent pods.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// LastUpdateTime is when the deployment was last reconciled.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// LastFailureTime is when the last pod failure occurred.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`

	// ConsecutiveFailures is the count of consecutive pod failures.
	// Resets to 0 when a pod succeeds.
	// +optional
	ConsecutiveFailures int `json:"consecutiveFailures,omitempty"`

	// PodNames lists the names of managed agent pods.
	// +optional
	PodNames []string `json:"podNames,omitempty"`

	// WarmPoolReady is the number of idle warm pods ready to accept runs.
	// +optional
	WarmPoolReady int `json:"warmPoolReady,omitempty"`

	// WarmPoolLastRecycleReason is the reason the most recent warm pod was
	// recycled (token-age-expired, stale-config, terminal-phase,
	// request-cap-<N>), or empty if no warm pod has been recycled yet. Emitted
	// as a WarmPodRecycled Kubernetes Event on the deployment as well.
	// +optional
	WarmPoolLastRecycleReason string `json:"warmPoolLastRecycleReason,omitempty"`

	// WarmPoolLastRecycleAt is when the most recent warm pod was recycled.
	// +optional
	WarmPoolLastRecycleAt *metav1.Time `json:"warmPoolLastRecycleAt,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// ContextUsedTokens is the estimated token count of the current conversation context.
	// Only populated when running in chat mode.
	// +optional
	ContextUsedTokens int `json:"contextUsedTokens,omitempty"`

	// MaxContextTokens is the maximum context window size for the selected model.
	// +optional
	MaxContextTokens int `json:"maxContextTokens,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Context",type=integer,JSONPath=`.status.contextUsedTokens`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="InputSource",type=string,JSONPath=`.spec.inputSource.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentDeployment manages a long-running agent that processes input continuously.
// Unlike AgentRun (one-time execution), AgentDeployment restarts pods on failure
// and supports multiple input sources (chat API, queues, pubsub, self-managed loops).
type AgentDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentDeploymentSpec   `json:"spec,omitempty"`
	Status AgentDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentDeploymentList contains a list of AgentDeployment.
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentDeployment{}, &AgentDeploymentList{})
}
