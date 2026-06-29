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

// AgentRunPhase describes the lifecycle phase of an AgentRun.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;HandedOff;WaitingForInput
type AgentRunPhase string

const (
	AgentRunPhasePending         AgentRunPhase = "Pending"
	AgentRunPhaseRunning         AgentRunPhase = "Running"
	AgentRunPhaseSucceeded       AgentRunPhase = "Succeeded"
	AgentRunPhaseFailed          AgentRunPhase = "Failed"
	AgentRunPhaseHandedOff       AgentRunPhase = "HandedOff"
	AgentRunPhaseWaitingForInput AgentRunPhase = "WaitingForInput"
)

// AgentRunSafeguards configures behavioral guardrails that halt a run when the agent
// appears stuck in a loop or is making no meaningful progress.
type AgentRunSafeguards struct {
	// MaxConsecutiveNoopTurns halts the run if the LLM produces this many consecutive
	// turns with no tool calls and fewer than MinSubstantiveTokens output tokens.
	// 0 disables the check.
	// +optional
	MaxConsecutiveNoopTurns int `json:"maxConsecutiveNoopTurns,omitempty"`

	// MinSubstantiveTokens is the minimum number of output tokens required for a turn
	// to count as substantive. Only evaluated when MaxConsecutiveNoopTurns > 0.
	// +kubebuilder:default=20
	// +optional
	MinSubstantiveTokens int `json:"minSubstantiveTokens,omitempty"`

	// MaxRepeatedToolCalls halts the run if the same tool is called with identical
	// arguments this many times within the run. 0 disables the check.
	// +optional
	MaxRepeatedToolCalls int `json:"maxRepeatedToolCalls,omitempty"`

	// ToolFrequencyCap halts the run if any single tool is called more than this many
	// times in total across the run. 0 disables the check.
	// +optional
	ToolFrequencyCap int `json:"toolFrequencyCap,omitempty"`
}

// LoopDetectedInfo describes a safeguard trip that caused a run to fail.
type LoopDetectedInfo struct {
	// Reason is a human-readable description of which safeguard was tripped.
	Reason string `json:"reason"`

	// TripCount is the counter value that exceeded the configured limit.
	TripCount int `json:"tripCount"`

	// ToolName is the name of the tool involved, for tool-based trips.
	// +optional
	ToolName string `json:"toolName,omitempty"`
}

// AgentRunSpec defines the desired state of an AgentRun.
type AgentRunSpec struct {
	// AgentRef names the Agent to run.
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// Input is the user task passed to the agent process via the AGENTORC_INPUT env var.
	// +kubebuilder:validation:MinLength=1
	Input string `json:"input"`

	// Timeout is the maximum duration for this run. Defaults to 5 minutes.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Callbacks configures webhooks invoked on run completion.
	// +optional
	Callbacks *CallbackConfig `json:"callbacks,omitempty"`

	// RestartPolicy controls failure recovery behaviour.
	// +optional
	RestartPolicy *AgentRunRestartPolicy `json:"restartPolicy,omitempty"`

	// ParentRunRef is the name of the parent AgentRun when this run was spawned
	// by an orchestrator agent via the agent-as-tool pattern.
	// Set automatically by the tool executor; do not set manually.
	// +optional
	ParentRunRef string `json:"parentRunRef,omitempty"`

	// PriorRunRef is the name of the previous AgentRun in this chat session.
	// When set, the model-router loads that run's accumulated message checkpoint as
	// the initial conversation history, enabling multi-turn context across runs.
	// Set automatically by the deployment chat API; do not set manually.
	// +optional
	PriorRunRef string `json:"priorRunRef,omitempty"`

	// Safeguards configures behavioral guardrails that halt the run when the agent
	// appears stuck in a loop or making no progress.
	// +optional
	Safeguards *AgentRunSafeguards `json:"safeguards,omitempty"`
}

// CallbackConfig defines webhooks triggered on AgentRun completion.
type CallbackConfig struct {
	// OnComplete is a URL that receives a POST when the run succeeds.
	// +optional
	OnComplete string `json:"onComplete,omitempty"`

	// OnFailed is a URL that receives a POST when the run fails.
	// +optional
	OnFailed string `json:"onFailed,omitempty"`
}

// AgentRunRestartPolicy controls how failures are handled.
type AgentRunRestartPolicy struct {
	// MaxRetries is the number of times to restart a failed pod before marking the run Failed.
	// +kubebuilder:default=3
	// +optional
	MaxRetries int `json:"maxRetries,omitempty"`
}

// RoutingDecision records a single model routing decision made during an AgentRun.
type RoutingDecision struct {
	// Model is the selected model identifier (e.g. "anthropic/claude-sonnet-4-6").
	Model string `json:"model"`

	// Provider is the ModelProvider CRD name that was selected.
	Provider string `json:"provider"`

	// Strategy is the routing strategy used (e.g. "rule-based", "llm-meta", "hybrid").
	Strategy string `json:"strategy,omitempty"`

	// Reason is a human-readable explanation of why this provider was chosen.
	Reason string `json:"reason,omitempty"`

	// Confidence is a 0.0–1.0 score indicating routing confidence.
	// +optional
	Confidence string `json:"confidence,omitempty"`

	// Timestamp is when the routing decision was made.
	// +optional
	Timestamp *metav1.Time `json:"timestamp,omitempty"`
}

// AgentRunStatus defines the observed state of an AgentRun.
type AgentRunStatus struct {
	// Phase is the current lifecycle phase of this run.
	// +optional
	Phase AgentRunPhase `json:"phase,omitempty"`

	// PodName is the name of the agent pod for this run.
	// +optional
	PodName string `json:"podName,omitempty"`

	// RouterPodName is the name of the model-router pod when split-pod topology is active.
	// Empty when using the default combined-pod (sidecar) topology.
	// +optional
	RouterPodName string `json:"routerPodName,omitempty"`

	// StartTime is when the agent pod started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// SpendUSD is the total USD spend for this run so far, updated periodically.
	// +optional
	SpendUSD string `json:"spendUSD,omitempty"`

	// Output is the agent's final output captured from pod stdout.
	// Truncated to 10KB; full output is in the checkpoint store.
	// +optional
	Output string `json:"output,omitempty"`

	// RawOutput is the raw pod stdout, useful for debugging.
	// Truncated to 4KB.
	// +optional
	RawOutput string `json:"rawOutput,omitempty"`

	// CheckpointRef is the storage key of the latest conversation checkpoint.
	// Used by the controller to resume a failed run.
	// +optional
	CheckpointRef string `json:"checkpointRef,omitempty"`

	// RestartCount is the number of times this run's pod has been restarted.
	// +optional
	RestartCount int `json:"restartCount,omitempty"`

	// LastRestartReason describes why the pod was last restarted.
	// +optional
	LastRestartReason string `json:"lastRestartReason,omitempty"`

	// ChildRunRefs lists the names of child AgentRuns spawned by this run.
	// +optional
	ChildRunRefs []string `json:"childRunRefs,omitempty"`

	// HandoffTarget is the Agent name this run handed off to, if Phase is HandedOff.
	// +optional
	HandoffTarget string `json:"handoffTarget,omitempty"`

	// ClarifyQuestion is the question posed to the human when Phase is WaitingForInput.
	// +optional
	ClarifyQuestion string `json:"clarifyQuestion,omitempty"`

	// ClarifyAnswer is the human's response, set via the UI API.
	// +optional
	ClarifyAnswer string `json:"clarifyAnswer,omitempty"`

	// WaitingSince records when the run entered WaitingForInput.
	// Used to enforce a waiting timeout separate from the run timeout.
	// +optional
	WaitingSince *metav1.Time `json:"waitingSince,omitempty"`

	// ContinuationRunRef is the name of the follow-up AgentRun created when
	// the human answers a clarifying question. The original run stays in
	// WaitingForInput (for audit) and the continuation carries the work forward.
	// +optional
	ContinuationRunRef string `json:"continuationRunRef,omitempty"`

	// LoopDetected is set when the run was halted by a safeguard trip.
	// The run phase will be Failed when this is set.
	// +optional
	LoopDetected *LoopDetectedInfo `json:"loopDetected,omitempty"`

	// FailureReason is a human-readable explanation of why the run failed,
	// set by the agent via the _fail built-in tool.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// InputMode mirrors Agent.spec.runtime.inputMode, cached here so the controller
	// does not need to re-fetch the Agent in handlePodSuccess.
	// +optional
	InputMode string `json:"inputMode,omitempty"`

	// EpisodicChunks is the number of episodic memory summaries generated during this run.
	// +optional
	EpisodicChunks int `json:"episodicChunks,omitempty"`

	// RoutingDecisions records the model routing decisions made during this run.
	// Populated by the controller from the resolved ModelSelector configuration
	// and updated by the model-router sidecar at runtime.
	// +optional
	RoutingDecisions []RoutingDecision `json:"routingDecisions,omitempty"`

	// ContextUsedTokens is the estimated token count of the conversation context.
	// Updated by the model-router at runtime and reflects the current size of the
	// accumulated message history.
	// +optional
	ContextUsedTokens int `json:"contextUsedTokens,omitempty"`

	// MaxContextTokens is the maximum context window size for the selected model.
	// Used to calculate how much context is available for the conversation.
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
// +kubebuilder:printcolumn:name="Spend",type=string,JSONPath=`.status.spendUSD`
// +kubebuilder:printcolumn:name="Restarts",type=integer,JSONPath=`.status.restartCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRun is an instantiation of an Agent executing a specific task.
type AgentRun struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRunSpec   `json:"spec,omitempty"`
	Status AgentRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRunList contains a list of AgentRun.
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRun{}, &AgentRunList{})
}
