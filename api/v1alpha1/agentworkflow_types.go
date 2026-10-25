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
// +kubebuilder:validation:XValidation:rule="has(self.fromTemplate) || (has(self.steps) && size(self.steps) > 0)",message="spec.steps is required unless spec.fromTemplate is set"
// +kubebuilder:validation:XValidation:rule="!has(self.fromTemplate) || !has(self.mode) || self.mode != 'template'",message="a template cannot be created from another template"
// +kubebuilder:validation:XValidation:rule="!has(self.parameters) || (has(self.mode) && self.mode == 'template')",message="spec.parameters is only for mode: template"
type AgentWorkflowSpec struct {
	// Description is an optional human-readable summary of the workflow's purpose.
	// +optional
	Description string `json:"description,omitempty"`

	// Mode is "workflow" (default): the workflow runs. In "template" mode the
	// workflow is a definition that is validated but never run; workflows are
	// created from it with spec.fromTemplate.
	// +kubebuilder:default=workflow
	// +optional
	Mode WorkflowMode `json:"mode,omitempty"`

	// Parameters declares the typed inputs of a template. Steps refer to them as
	// {{params.<name>}} in their input. Only for mode: template.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=50
	// +optional
	Parameters []WorkflowParameter `json:"parameters,omitempty"`

	// FromTemplate creates this workflow from a template in the same namespace:
	// on its first reconcile the controller fills in the steps (and any of
	// description, timeout, budgetCap, onStepFailure and adaptivePolicy not set here)
	// from the template, with the parameters substituted into the step inputs.
	// +optional
	FromTemplate *WorkflowFromTemplate `json:"fromTemplate,omitempty"`

	// Steps defines the DAG of agent executions. Each step names its dependencies via
	// DependsOn; steps with no dependencies run immediately when the workflow starts.
	// Required unless fromTemplate is set.
	// +optional
	Steps []WorkflowStep `json:"steps,omitempty"`

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

// WorkflowMode says whether an AgentWorkflow runs or is a template.
// +kubebuilder:validation:Enum=workflow;template
type WorkflowMode string

// Workflow modes.
const (
	// WorkflowModeWorkflow is a workflow that runs.
	WorkflowModeWorkflow WorkflowMode = "workflow"
	// WorkflowModeTemplate is a definition that is validated but never run.
	WorkflowModeTemplate WorkflowMode = "template"
)

// WorkflowParameter is a typed input of a workflow template.
type WorkflowParameter struct {
	// Name is how steps refer to the parameter: {{params.<name>}}.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9_]*$`
	Name string `json:"name"`

	// Description tells a person or a connector what the parameter is for.
	// +optional
	Description string `json:"description,omitempty"`

	// Type is string (default), integer or boolean.
	// +kubebuilder:validation:Enum=string;integer;boolean
	// +kubebuilder:default=string
	// +optional
	Type string `json:"type,omitempty"`

	// Required makes the parameter mandatory unless it has a default.
	// +optional
	Required bool `json:"required,omitempty"`

	// Default is used when the instance does not set the parameter.
	// +optional
	Default *string `json:"default,omitempty"`

	// Enum restricts the value to one of these.
	// +optional
	Enum []string `json:"enum,omitempty"`

	// Pattern is a regular expression a string value must match in full.
	// +optional
	Pattern string `json:"pattern,omitempty"`

	// MaxLength caps the value's length in bytes. Defaults to 8192.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxLength int `json:"maxLength,omitempty"`
}

// WorkflowFromTemplate names a template and gives its parameters.
type WorkflowFromTemplate struct {
	// Name of the AgentWorkflow template, in the same namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Parameters sets the template's parameters by name. Values are strings;
	// integer and boolean parameters take "42" and "true".
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// WorkflowTemplateRevision records which template version an instance was built from.
type WorkflowTemplateRevision struct {
	// Name of the template.
	Name string `json:"name"`
	// Generation of the template when the instance was built from it.
	Generation int64 `json:"generation"`
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

// WorkflowStepType says what a step does.
// +kubebuilder:validation:Enum=agent;container;approval
type WorkflowStepType string

// Workflow step types.
const (
	// WorkflowStepTypeAgent runs an Agent (the default).
	WorkflowStepTypeAgent WorkflowStepType = "agent"
	// WorkflowStepTypeContainer runs a deterministic job in a pod.
	WorkflowStepTypeContainer WorkflowStepType = "container"
	// WorkflowStepTypeApproval waits for a person's decision.
	WorkflowStepTypeApproval WorkflowStepType = "approval"
)

// WorkflowStep defines a single node in the workflow DAG.
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type != 'agent') || (has(self.agentRef) && size(self.agentRef) > 0 && has(self.input))",message="an agent step needs agentRef and input"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type == 'agent' || (!has(self.agentRef) && !has(self.input))",message="agentRef and input are only for agent steps"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'container' || has(self.container)",message="a container step needs a container block"
// +kubebuilder:validation:XValidation:rule="!has(self.container) || (has(self.type) && self.type == 'container')",message="the container block is only for container steps"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'approval' || has(self.approval)",message="an approval step needs an approval block"
// +kubebuilder:validation:XValidation:rule="!has(self.approval) || (has(self.type) && self.type == 'approval')",message="the approval block is only for approval steps"
type WorkflowStep struct {
	// Name is a unique identifier for this step within the workflow.
	// Used in DependsOn references and template variables.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9\-]*$`
	Name string `json:"name"`

	// Type is agent (default): run an Agent; container: run a deterministic job in a
	// pod; approval: wait for a person to approve or reject.
	// +kubebuilder:default=agent
	// +optional
	Type WorkflowStepType `json:"type,omitempty"`

	// AgentRef is the name of the Agent CRD to run for this step. Agent steps only.
	// +optional
	AgentRef string `json:"agentRef,omitempty"`

	// Input is the task string passed to the agent as AGENTORC_INPUT. Agent steps only.
	// Supports template variables:
	//   {{steps.<name>.output}}  — the Output of a named predecessor step
	// +optional
	Input string `json:"input,omitempty"`

	// Container describes the job of a container step.
	// +optional
	Container *ContainerStep `json:"container,omitempty"`

	// Approval describes the decision an approval step waits for.
	// +optional
	Approval *ApprovalStep `json:"approval,omitempty"`

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

// ContainerStep is a deterministic job: one container in its own pod, run to
// completion. Exit code 0 is success and the tail of its output becomes the
// step's output. The pod has no service account token, a read-only root
// filesystem, runs as a non-root user and is denied all network traffic once
// the cluster's NetworkPolicy controller has programmed the rules (a second or
// two after the pod starts); /tmp and /work are writable scratch.
type ContainerStep struct {
	// Image is the container image to run. It must be allowed by the operator's
	// ALLOWED_REGISTRIES, if set.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Command overrides the image's entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the image's arguments.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env sets environment variables. Values can use {{steps.<name>.output}}; pass
	// data to the job here rather than splicing it into Command or Args.
	// +optional
	Env []StepEnvVar `json:"env,omitempty"`

	// Resources sets CPU and memory. Defaults to a small request and a 1 CPU / 512Mi limit.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// RunAsUser is the numeric user the container runs as. Defaults to 65532. Root (0) is not allowed.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`
}

// StepEnvVar is one environment variable of a container step.
type StepEnvVar struct {
	// Name of the variable.
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Name string `json:"name"`
	// Value of the variable; may use {{steps.<name>.output}}.
	Value string `json:"value"`
}

// ApprovalStep is a human gate: the workflow waits here for a decision.
type ApprovalStep struct {
	// Message is what the approver is asked, shown in the Approvals inbox. It can use
	// {{steps.<name>.output}} to show earlier results (a diff, a test report).
	// +kubebuilder:validation:MinLength=1
	Message string `json:"message"`

	// ApproverGroups are the identity-provider groups whose members may decide.
	// Empty lets any user with access to the workflow's namespace decide.
	// +optional
	ApproverGroups []string `json:"approverGroups,omitempty"`

	// Timeout is how long to wait before the step fails as undecided. Defaults to 24h.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// WorkflowBudgetCap limits total spend across all steps.
type WorkflowBudgetCap struct {
	// Total is the maximum USD spend across all AgentRuns in the workflow.
	// When exceeded, the currently-running step is failed and the workflow is Failed.
	Total string `json:"total"`
}

// AgentWorkflowPhase describes the lifecycle phase of an AgentWorkflow.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Cancelled;Template
type AgentWorkflowPhase string

const (
	// AgentWorkflowPhaseTemplate is the phase of a template (mode: template): a
	// definition that is validated but never run.
	AgentWorkflowPhaseTemplate  AgentWorkflowPhase = "Template"
	AgentWorkflowPhasePending   AgentWorkflowPhase = "Pending"
	AgentWorkflowPhaseRunning   AgentWorkflowPhase = "Running"
	AgentWorkflowPhaseSucceeded AgentWorkflowPhase = "Succeeded"
	AgentWorkflowPhaseFailed    AgentWorkflowPhase = "Failed"
	AgentWorkflowPhaseCancelled AgentWorkflowPhase = "Cancelled"
)

// WorkflowStepPhase describes the state of a single step.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Skipped;WaitingForInput;WaitingForApproval
type WorkflowStepPhase string

const (
	WorkflowStepPhasePending         WorkflowStepPhase = "Pending"
	WorkflowStepPhaseRunning         WorkflowStepPhase = "Running"
	WorkflowStepPhaseSucceeded       WorkflowStepPhase = "Succeeded"
	WorkflowStepPhaseFailed          WorkflowStepPhase = "Failed"
	WorkflowStepPhaseSkipped         WorkflowStepPhase = "Skipped"
	WorkflowStepPhaseWaitingForInput WorkflowStepPhase = "WaitingForInput"
	// WorkflowStepPhaseWaitingForApproval is an approval step waiting for a decision.
	WorkflowStepPhaseWaitingForApproval WorkflowStepPhase = "WaitingForApproval"
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

	// PodRef is the name of the pod of a container step.
	// +optional
	PodRef string `json:"podRef,omitempty"`

	// ApprovalID is the id of the approval an approval step waits on.
	// +optional
	ApprovalID string `json:"approvalID,omitempty"`

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

	// Template records the template this workflow was built from (fromTemplate).
	// +optional
	Template *WorkflowTemplateRevision `json:"template,omitempty"`

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
