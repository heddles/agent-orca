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

// AgentWorkflowSpec defines a declarative DAG of agent steps.
// The controller — not the LLM — determines step ordering, threads outputs between steps,
// and enforces workflow-level budget caps. This is the deterministic complement to the
// dynamic agent-as-tool pattern.
type AgentWorkflowSpec struct {
	// Description is an optional human-readable summary of the workflow's purpose.
	// +optional
	Description string `json:"description,omitempty"`

	// Steps defines the DAG of agent executions. Each step names its dependencies via
	// DependsOn; steps with no dependencies run immediately when the workflow starts.
	// +kubebuilder:validation:MinItems=1
	Steps []WorkflowStep `json:"steps"`

	// BudgetCap limits total USD spend across all steps combined.
	// +optional
	BudgetCap *WorkflowBudgetCap `json:"budgetCap,omitempty"`

	// Timeout is the wall-clock limit for the entire workflow.
	// Steps still running when this expires are failed.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// OnStepFailure controls what happens when a step fails.
	// "stop" (default): mark the workflow Failed and do not start new steps.
	// "continue": mark the failed step Skipped and continue with dependents.
	// +kubebuilder:validation:Enum=stop;continue
	// +optional
	OnStepFailure string `json:"onStepFailure,omitempty"`

	// AdaptivePolicy enables agents to propose new steps at runtime.
	// If nil, all _propose_step calls are rejected.
	// +optional
	AdaptivePolicy *AdaptivePolicy `json:"adaptivePolicy,omitempty"`
}

// AdaptivePolicy controls dynamic step proposals from agents within the workflow.
type AdaptivePolicy struct {
	// MaxDynamicSteps caps the total number of dynamically-added steps.
	// +kubebuilder:validation:Minimum=1
	MaxDynamicSteps int `json:"maxDynamicSteps"`

	// AllowedAgentRefs restricts which Agent CRDs can be proposed.
	// If empty, any agent in the namespace may be proposed.
	// +optional
	AllowedAgentRefs []string `json:"allowedAgentRefs,omitempty"`

	// AllowLoops permits re-running a step with refined input.
	// +optional
	AllowLoops bool `json:"allowLoops,omitempty"`

	// MaxLoopIterations caps loop retries per step. Defaults to 3.
	// +kubebuilder:default=3
	// +optional
	MaxLoopIterations int `json:"maxLoopIterations,omitempty"`

	// RequireApprovalForUnlisted pauses the workflow and requests human approval
	// when an agent proposes a step using an agent not in AllowedAgentRefs.
	// If false, such proposals are auto-rejected.
	// +optional
	RequireApprovalForUnlisted bool `json:"requireApprovalForUnlisted,omitempty"`
}

// WorkflowStep defines a single node in the workflow DAG.
type WorkflowStep struct {
	// Name is a unique identifier for this step within the workflow.
	// Used in DependsOn references and template variables.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9\-]*$`
	Name string `json:"name"`

	// AgentRef is the name of the Agent CRD to run for this step.
	AgentRef string `json:"agentRef"`

	// Input is the task string passed to the agent as AGENTORC_INPUT.
	// Supports template variables:
	//   {{steps.<name>.output}}  — the Output of a named predecessor step
	Input string `json:"input"`

	// DependsOn lists step names that must reach a terminal phase (Succeeded or Skipped)
	// before this step is eligible to start.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// Condition is a CEL expression evaluated before this step starts.
	// The expression has access to a "steps" variable: map of step name →
	// {phase: string, output: string}. If the expression evaluates to false
	// the step is marked Skipped rather than started.
	// Example: 'steps["gather"].phase == "Succeeded"'
	// +optional
	Condition string `json:"condition,omitempty"`

	// Timeout overrides the workflow-level timeout for this specific step.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Callbacks fires webhooks when this step completes or fails.
	// +optional
	Callbacks *CallbackConfig `json:"callbacks,omitempty"`
}

// WorkflowBudgetCap limits total spend across all steps.
type WorkflowBudgetCap struct {
	// Total is the maximum USD spend across all AgentRuns in the workflow.
	// When exceeded, the currently-running step is failed and the workflow is Failed.
	Total string `json:"total"`
}

// AgentWorkflowPhase describes the lifecycle phase of an AgentWorkflow.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Cancelled
type AgentWorkflowPhase string

const (
	AgentWorkflowPhasePending   AgentWorkflowPhase = "Pending"
	AgentWorkflowPhaseRunning   AgentWorkflowPhase = "Running"
	AgentWorkflowPhaseSucceeded AgentWorkflowPhase = "Succeeded"
	AgentWorkflowPhaseFailed    AgentWorkflowPhase = "Failed"
	AgentWorkflowPhaseCancelled AgentWorkflowPhase = "Cancelled"
)

// WorkflowStepPhase describes the state of a single step.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Skipped;WaitingForInput
type WorkflowStepPhase string

const (
	WorkflowStepPhasePending         WorkflowStepPhase = "Pending"
	WorkflowStepPhaseRunning         WorkflowStepPhase = "Running"
	WorkflowStepPhaseSucceeded       WorkflowStepPhase = "Succeeded"
	WorkflowStepPhaseFailed          WorkflowStepPhase = "Failed"
	WorkflowStepPhaseSkipped         WorkflowStepPhase = "Skipped"
	WorkflowStepPhaseWaitingForInput WorkflowStepPhase = "WaitingForInput"
)

// WorkflowStepStatus holds runtime state for a single step.
type WorkflowStepStatus struct {
	// Name matches a step name in spec.steps.
	Name string `json:"name"`

	// Phase is the current lifecycle state of this step.
	Phase WorkflowStepPhase `json:"phase"`

	// AgentRunRef is the name of the AgentRun created for this step.
	// +optional
	AgentRunRef string `json:"agentRunRef,omitempty"`

	// StartTime is when the step's AgentRun was created.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the step reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Output is the AgentRun output for this step, truncated to 10 KB.
	// Available to downstream steps via {{steps.<name>.output}}.
	// +optional
	Output string `json:"output,omitempty"`

	// SpendUSD is the USD spend recorded on the step's AgentRun.
	// +optional
	SpendUSD string `json:"spendUSD,omitempty"`

	// FailureReason is a human-readable explanation when Phase is Failed.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// Source indicates whether this step was defined statically in spec.steps
	// or added dynamically via a _propose_step call. Values: "static", "dynamic".
	// +kubebuilder:validation:Enum=static;dynamic
	// +optional
	Source string `json:"source,omitempty"`
}

// StepProposal records a dynamic step proposal from an agent, for auditability.
type StepProposal struct {
	// ProposingStep is the name of the workflow step whose agent made the proposal.
	ProposingStep string `json:"proposingStep"`

	// ProposedStep is the step definition that was proposed.
	ProposedStep WorkflowStep `json:"proposedStep"`

	// Approved indicates whether the proposal was accepted.
	Approved bool `json:"approved"`

	// Reason explains why the proposal was approved or rejected.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Timestamp is when the proposal was evaluated.
	Timestamp metav1.Time `json:"timestamp"`

	// PendingHumanApproval is true when the proposal requires human review.
	// The proposing step is paused (WaitingForInput) until a human responds.
	// +optional
	PendingHumanApproval bool `json:"pendingHumanApproval,omitempty"`
}

// AgentWorkflowStatus holds observed state for an AgentWorkflow.
type AgentWorkflowStatus struct {
	// Phase is the overall workflow lifecycle state.
	// +optional
	Phase AgentWorkflowPhase `json:"phase,omitempty"`

	// Steps holds per-step execution state, one entry per spec.steps entry.
	// +optional
	Steps []WorkflowStepStatus `json:"steps,omitempty"`

	// DynamicSteps holds steps added at runtime by agent proposals.
	// These are reconciled by the controller using the same scheduling logic as spec.steps.
	// +optional
	DynamicSteps []WorkflowStep `json:"dynamicSteps,omitempty"`

	// DynamicStepStatuses holds execution state for dynamic steps.
	// +optional
	DynamicStepStatuses []WorkflowStepStatus `json:"dynamicStepStatuses,omitempty"`

	// ProposalLog records all proposals (approved and rejected) for auditability.
	// +optional
	ProposalLog []StepProposal `json:"proposalLog,omitempty"`

	// TotalSpendUSD is the sum of SpendUSD across all step AgentRuns.
	// +optional
	TotalSpendUSD string `json:"totalSpendUSD,omitempty"`

	// StartTime is when the first step was launched.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the workflow reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Spend",type=string,JSONPath=`.status.totalSpendUSD`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentWorkflow is a declarative DAG of agent steps. The operator controller
// sequences steps, threads outputs between them, and enforces budget caps —
// without requiring the LLM to coordinate execution.
type AgentWorkflow struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentWorkflowSpec   `json:"spec,omitempty"`
	Status AgentWorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentWorkflowList contains a list of AgentWorkflow.
type AgentWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentWorkflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentWorkflow{}, &AgentWorkflowList{})
}
