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
