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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Concurrency policies of an AgentSchedule, as for a Kubernetes CronJob.
const (
	// ScheduleConcurrencyAllow starts a run even when earlier runs are active.
	ScheduleConcurrencyAllow = "Allow"
	// ScheduleConcurrencyForbid skips a tick while an earlier run is active.
	ScheduleConcurrencyForbid = "Forbid"
	// ScheduleConcurrencyReplace cancels the active runs, then starts a new one.
	ScheduleConcurrencyReplace = "Replace"
)

// Conditions of an AgentSchedule.
const (
	// ScheduleConditionReady is true while the schedule can start runs.
	ScheduleConditionReady = "Ready"
	// ScheduleConditionSuspended is true while spec.suspend is set.
	ScheduleConditionSuspended = "Suspended"
	// ScheduleConditionBudgetExhausted is true while spec.spend stops new runs.
	ScheduleConditionBudgetExhausted = "BudgetExhausted"
	// ScheduleConditionTargetMissing is true while the agent or template the
	// schedule runs is missing or not allowed for the tenant.
	ScheduleConditionTargetMissing = "TargetMissing"
	// ScheduleConditionDegraded is true after too many consecutive failed runs;
	// the schedule stops ticking until a person resumes it.
	ScheduleConditionDegraded = "Degraded"
)

// Reasons a tick was skipped, as recorded in status.recentSkips.
const (
	ScheduleSkipMissed        = "MissedSchedule"
	ScheduleSkipConcurrency   = "SkippedConcurrency"
	ScheduleSkipQuota         = "SkippedQuota"
	ScheduleSkipBudget        = "SkippedBudget"
	ScheduleSkipTargetMissing = "SkippedTargetMissing"
	ScheduleSkipInvalid       = "SkippedInvalid"
)

// AgentScheduleSpec defines when and what an AgentSchedule runs.
// +kubebuilder:validation:XValidation:rule="has(self.agentRef) != has(self.workflowTemplateRef)",message="set exactly one of agentRef and workflowTemplateRef"
// +kubebuilder:validation:XValidation:rule="!has(self.workflowTemplateRef) || (!has(self.input) && !has(self.timeout) && !has(self.budget) && !has(self.safeguards) && !has(self.result))",message="a workflowTemplateRef schedule takes parameters, not input, timeout, budget, safeguards or result: those belong to the template"
// +kubebuilder:validation:XValidation:rule="!has(self.agentRef) || !has(self.parameters)",message="parameters apply to workflowTemplateRef only"
// +kubebuilder:validation:XValidation:rule="!has(self.agentRef) || (has(self.input) && size(self.input) > 0)",message="an agentRef schedule needs an input"
type AgentScheduleSpec struct {
	// AgentRef is the Agent to run, in the schedule's namespace. Set exactly one
	// of agentRef and workflowTemplateRef.
	// +optional
	AgentRef string `json:"agentRef,omitempty"`

	// WorkflowTemplateRef is an AgentWorkflow with spec.mode: template, in the
	// schedule's namespace. Each tick creates a workflow from it.
	// +optional
	WorkflowTemplateRef string `json:"workflowTemplateRef,omitempty"`

	// Schedule is a five-field cron expression (minute hour day-of-month month
	// day-of-week) or one of @hourly, @daily, @weekly, @monthly. It is read in
	// timeZone.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Schedule string `json:"schedule"`

	// TimeZone is an IANA time zone name such as Europe/Berlin. Default UTC. A
	// wall-clock time that does not exist (a daylight-saving gap) is skipped; one
	// that occurs twice (an overlap) fires once, at its first occurrence.
	// +kubebuilder:default=UTC
	// +kubebuilder:validation:MaxLength=64
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// Input is the run's input. Only {{schedule.time}}, {{schedule.date}} and
	// {{schedule.name}} are substituted; nothing else in it is interpreted.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	Input string `json:"input,omitempty"`

	// Parameters are the workflow template's parameters. The same three
	// placeholders are substituted in the values.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ConcurrencyPolicy is Allow, Forbid (default) or Replace.
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`

	// StartingDeadlineSeconds is how late a missed tick may still start.
	// Default 3600.
	// +kubebuilder:validation:Minimum=0
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// SuccessfulRunsHistoryLimit is how many finished successful runs (or
	// workflows) are kept. Default 7.
	// +kubebuilder:validation:Minimum=0
	// +optional
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`

	// FailedRunsHistoryLimit is how many failed runs (or workflows) are kept.
	// Default 7.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`

	// Suspend stops new runs. Runs in flight are untouched.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Timeout is copied onto every created AgentRun.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Budget caps each run's model spend. Copied onto every created AgentRun.
	// An unattended run has nobody to approve a raise, so onExceeded defaults to
	// fail and pause is refused.
	// +optional
	Budget *RunBudget `json:"budget,omitempty"`

	// Safeguards are copied onto every created AgentRun.
	// +optional
	Safeguards *AgentRunSafeguards `json:"safeguards,omitempty"`

	// Result says where each run's result goes besides the run itself, which is
	// always visible in the UI.
	// +optional
	Result *ScheduleResult `json:"result,omitempty"`

	// Spend caps the schedule as a whole over a day and a month.
	// +optional
	Spend *ScheduleSpendCap `json:"spend,omitempty"`
}

// ScheduleResult routes the result of each scheduled run. Both parts are copied
// onto the created AgentRun and are handled by the run controller as for any run.
type ScheduleResult struct {
	// Egress publishes each run's result to a Kafka, Pub/Sub or Redis sink.
	// +optional
	Egress *EgressConfig `json:"egress,omitempty"`

	// Callbacks are URLs that receive a POST when a run succeeds or fails.
	// +optional
	Callbacks *CallbackConfig `json:"callbacks,omitempty"`
}

// ScheduleSpendCap bounds what a schedule may spend in a day and in a month.
type ScheduleSpendCap struct {
	// MaxUSDPerDay caps the spend of the runs a schedule starts in a UTC day.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	MaxUSDPerDay string `json:"maxUSDPerDay,omitempty"`

	// MaxUSDPerMonth caps the spend of the runs a schedule starts in a UTC month.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	MaxUSDPerMonth string `json:"maxUSDPerMonth,omitempty"`

	// OnExceeded is what happens at a cap. Only Skip exists: ticks are skipped
	// until the window rolls over. It never rewrites spec.suspend.
	// +kubebuilder:validation:Enum=Skip
	// +kubebuilder:default=Skip
	// +optional
	OnExceeded string `json:"onExceeded,omitempty"`
}

// ScheduleSkip records a tick that did not start a run.
type ScheduleSkip struct {
	// Time is the scheduled instant that was skipped.
	Time metav1.Time `json:"time"`
	// Reason is one of MissedSchedule, SkippedConcurrency, SkippedQuota,
	// SkippedBudget, SkippedTargetMissing, SkippedInvalid.
	Reason string `json:"reason"`
	// Message says why, in plain words.
	// +optional
	Message string `json:"message,omitempty"`
}

// ScheduleSpendLedger is the spend of runs that finished, so pruning a run from
// the history does not lose its cost. The operator keeps it.
type ScheduleSpendLedger struct {
	// Day is the UTC day (YYYY-MM-DD) DayUSD belongs to.
	// +optional
	Day string `json:"day,omitempty"`
	// DayUSD is the finished runs' spend in Day.
	// +optional
	DayUSD string `json:"dayUSD,omitempty"`
	// Month is the UTC month (YYYY-MM) MonthUSD belongs to.
	// +optional
	Month string `json:"month,omitempty"`
	// MonthUSD is the finished runs' spend in Month.
	// +optional
	MonthUSD string `json:"monthUSD,omitempty"`
}

// AgentScheduleStatus is the observed state of an AgentSchedule.
type AgentScheduleStatus struct {
	// LastScheduleTime is the scheduled instant of the last tick handled,
	// whether it started a run or was skipped.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is when the last successful run finished.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// NextScheduleTime is the next tick, in absolute time.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// Active lists the runs (or workflows) still going.
	// +optional
	Active []corev1.ObjectReference `json:"active,omitempty"`

	// ConsecutiveFailures counts finished runs that failed since the last
	// success. At the operator's limit the schedule becomes Degraded.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// SpentTodayUSD is what the schedule's runs spent today (UTC).
	// +optional
	SpentTodayUSD string `json:"spentTodayUSD,omitempty"`

	// SpentMonthUSD is what the schedule's runs spent this month (UTC).
	// +optional
	SpentMonthUSD string `json:"spentMonthUSD,omitempty"`

	// Ledger is the spend of finished runs, kept across history pruning.
	// +optional
	Ledger *ScheduleSpendLedger `json:"ledger,omitempty"`

	// RecentSkips is the last ten ticks that started nothing.
	// +optional
	RecentSkips []ScheduleSkip `json:"recentSkips,omitempty"`

	// Conditions: Ready, Suspended, BudgetExhausted, TargetMissing, Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CursorTime is where tick evaluation starts: ticks before it are never
	// started. It moves to the current time whenever the spec changes (and when
	// ticks were missed), so nothing fires retroactively.
	// +optional
	CursorTime *metav1.Time `json:"cursorTime,omitempty"`

	// ObservedGeneration is the spec generation the status describes. A change
	// of spec resets the cursor to now: nothing fires retroactively.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=asched
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.timeZone`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Next",type=date,JSONPath=`.status.nextScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentSchedule starts an agent (or a workflow made from a template) on a clock,
// like a CronJob starts a Job. Each tick creates an ordinary AgentRun (or
// AgentWorkflow) attributed to the schedule, not to a person.
type AgentSchedule struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentScheduleSpec   `json:"spec,omitempty"`
	Status AgentScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentScheduleList contains a list of AgentSchedule.
type AgentScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentSchedule{}, &AgentScheduleList{})
}
