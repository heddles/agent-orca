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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

const workflowPollInterval = 10 * time.Second

// AgentWorkflowReconciler reconciles AgentWorkflow objects.
type AgentWorkflowReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentworkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentworkflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentworkflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentruns,verbs=get;list;watch;create;delete

func (r *AgentWorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var wf agentorcv1alpha1.AgentWorkflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal workflows need no further reconciliation.
	if isWorkflowTerminal(wf.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// Enforce workflow-level timeout.
	if wf.Spec.Timeout != nil && wf.Status.StartTime != nil {
		deadline := wf.Status.StartTime.Add(wf.Spec.Timeout.Duration)
		if time.Now().After(deadline) {
			r.failWorkflow(ctx, &wf, "workflow timeout exceeded")
			return ctrl.Result{}, r.Status().Update(ctx, &wf)
		}
	}

	// Build a fast lookup: step name → current status entry.
	statusIndex := make(map[string]*agentorcv1alpha1.WorkflowStepStatus, len(wf.Status.Steps))
	for i := range wf.Status.Steps {
		statusIndex[wf.Status.Steps[i].Name] = &wf.Status.Steps[i]
	}

	// Ensure every spec step has a status entry (first reconcile).
	for _, step := range wf.Spec.Steps {
		if _, ok := statusIndex[step.Name]; !ok {
			wf.Status.Steps = append(wf.Status.Steps, agentorcv1alpha1.WorkflowStepStatus{
				Name:   step.Name,
				Phase:  agentorcv1alpha1.WorkflowStepPhasePending,
				Source: "static",
			})
		}
	}

	// Ensure every dynamic step has a status entry in DynamicStepStatuses.
	dynStatusIndex := make(map[string]*agentorcv1alpha1.WorkflowStepStatus, len(wf.Status.DynamicStepStatuses))
	for i := range wf.Status.DynamicStepStatuses {
		dynStatusIndex[wf.Status.DynamicStepStatuses[i].Name] = &wf.Status.DynamicStepStatuses[i]
	}
	for _, step := range wf.Status.DynamicSteps {
		if _, ok := dynStatusIndex[step.Name]; !ok {
			wf.Status.DynamicStepStatuses = append(wf.Status.DynamicStepStatuses, agentorcv1alpha1.WorkflowStepStatus{
				Name:   step.Name,
				Phase:  agentorcv1alpha1.WorkflowStepPhasePending,
				Source: "dynamic",
			})
		}
	}

	// Rebuild indices after potential slice growth to avoid dangling pointers.
	statusIndex = make(map[string]*agentorcv1alpha1.WorkflowStepStatus, len(wf.Status.Steps)+len(wf.Status.DynamicStepStatuses))
	for i := range wf.Status.Steps {
		statusIndex[wf.Status.Steps[i].Name] = &wf.Status.Steps[i]
	}
	for i := range wf.Status.DynamicStepStatuses {
		statusIndex[wf.Status.DynamicStepStatuses[i].Name] = &wf.Status.DynamicStepStatuses[i]
	}

	// Merge spec steps and dynamic steps into a unified list for scheduling.
	allSteps := make([]agentorcv1alpha1.WorkflowStep, 0, len(wf.Spec.Steps)+len(wf.Status.DynamicSteps))
	allSteps = append(allSteps, wf.Spec.Steps...)
	allSteps = append(allSteps, wf.Status.DynamicSteps...)

	anyRunning := false
	anyFailed := false

	for i, step := range allSteps {
		_ = i // used only for pointer stability below
		ss := statusIndex[step.Name]

		switch ss.Phase {
		case agentorcv1alpha1.WorkflowStepPhaseSucceeded, agentorcv1alpha1.WorkflowStepPhaseSkipped:
			continue

		case agentorcv1alpha1.WorkflowStepPhaseFailed:
			anyFailed = true
			if wf.Spec.OnStepFailure != "continue" {
				r.failWorkflow(ctx, &wf, fmt.Sprintf("step %q failed: %s", step.Name, ss.FailureReason))
				return ctrl.Result{}, r.Status().Update(ctx, &wf)
			}

		case agentorcv1alpha1.WorkflowStepPhaseRunning, agentorcv1alpha1.WorkflowStepPhaseWaitingForInput:
			// Check the underlying AgentRun. All mutations stay in-memory; no Status().Update
			// is called here so that the wf.Status.Steps slice is never replaced mid-loop.
			result, err := r.checkStepRun(ctx, &wf, &allSteps[i], ss)
			if err != nil {
				return result, err
			}
			if result.RequeueAfter > 0 {
				// Step is still running or waiting for input; persist any earlier state changes and requeue.
				return result, r.Status().Update(ctx, &wf)
			}
			// Step has transitioned — handle the new phase.
			switch ss.Phase {
			case agentorcv1alpha1.WorkflowStepPhaseSucceeded:
				if overage := r.budgetExceeded(&wf); overage != "" {
					r.failWorkflow(ctx, &wf, overage)
					return ctrl.Result{}, r.Status().Update(ctx, &wf)
				}
			case agentorcv1alpha1.WorkflowStepPhaseFailed:
				anyFailed = true
				if wf.Spec.OnStepFailure != "continue" {
					r.failWorkflow(ctx, &wf, fmt.Sprintf("step %q failed", step.Name))
					return ctrl.Result{}, r.Status().Update(ctx, &wf)
				}
			case agentorcv1alpha1.WorkflowStepPhaseRunning, agentorcv1alpha1.WorkflowStepPhaseWaitingForInput:
				anyRunning = true
			}

		case agentorcv1alpha1.WorkflowStepPhasePending:
			// Check if the step is eligible to start.
			if !r.depsReady(step, statusIndex, wf.Spec.OnStepFailure) {
				continue
			}
			// Evaluate optional CEL condition.
			if step.Condition != "" {
				pass, err := evalCondition(step.Condition, statusIndex)
				if err != nil {
					logger.Error(err, "evaluating step condition", "step", step.Name)
				}
				if !pass {
					ss.Phase = agentorcv1alpha1.WorkflowStepPhaseSkipped
					continue
				}
			}
			// Start the step. AgentRun creation is a real API call; all in-memory state
			// mutations are deferred to the single Status().Update at the return site.
			if err := r.startStep(ctx, &wf, &allSteps[i], ss); err != nil {
				return ctrl.Result{}, err
			}
			anyRunning = true
		}
	}

	// Check overall terminal state (both static and dynamic steps).
	allDone := true
	allStatuses := append(wf.Status.Steps, wf.Status.DynamicStepStatuses...)
	for _, ss := range allStatuses {
		if ss.Phase == agentorcv1alpha1.WorkflowStepPhasePending ||
			ss.Phase == agentorcv1alpha1.WorkflowStepPhaseRunning ||
			ss.Phase == agentorcv1alpha1.WorkflowStepPhaseWaitingForInput {
			allDone = false
			break
		}
	}

	if allDone && !anyFailed {
		now := metav1.Now()
		wf.Status.Phase = agentorcv1alpha1.AgentWorkflowPhaseSucceeded
		wf.Status.CompletionTime = &now
		wf.Status.TotalSpendUSD = r.sumSpend(allStatuses)
		return ctrl.Result{}, r.Status().Update(ctx, &wf)
	}

	if anyRunning {
		return ctrl.Result{RequeueAfter: workflowPollInterval}, r.Status().Update(ctx, &wf)
	}
	return ctrl.Result{}, r.Status().Update(ctx, &wf)
}

// startStep creates an AgentRun for the given step and marks it Running in-memory.
// The caller is responsible for persisting via Status().Update.
func (r *AgentWorkflowReconciler) startStep(
	ctx context.Context,
	wf *agentorcv1alpha1.AgentWorkflow,
	step *agentorcv1alpha1.WorkflowStep,
	ss *agentorcv1alpha1.WorkflowStepStatus,
) error {
	// Resolve template variables in the input (from both static and dynamic step outputs).
	allStatuses := append(wf.Status.Steps, wf.Status.DynamicStepStatuses...)
	input := resolveTemplates(step.Input, allStatuses)

	// Determine timeout: step-level overrides workflow-level.
	var timeout *metav1.Duration
	if step.Timeout != nil {
		timeout = step.Timeout
	} else if wf.Spec.Timeout != nil {
		// Pass remaining workflow time as the step timeout.
		remaining := wf.Spec.Timeout.Duration
		if wf.Status.StartTime != nil {
			remaining = time.Until(wf.Status.StartTime.Add(wf.Spec.Timeout.Duration))
		}
		if remaining > 0 {
			timeout = &metav1.Duration{Duration: remaining}
		}
	}

	runName := fmt.Sprintf("%s-%s", wf.Name, step.Name)
	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: wf.Namespace,
			Labels: map[string]string{
				"agentorc.io/workflow":      wf.Name,
				"agentorc.io/workflow-step": step.Name,
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			AgentRef: step.AgentRef,
			Input:    input,
			Timeout:  timeout,
		},
	}

	// Set the workflow as owner so kubectl delete agentworkflow cascades.
	if err := controllerutil.SetControllerReference(wf, run, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on step AgentRun: %w", err)
	}

	if err := r.Create(ctx, run); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating AgentRun for step %q: %w", step.Name, err)
	}

	now := metav1.Now()
	ss.Phase = agentorcv1alpha1.WorkflowStepPhaseRunning
	ss.AgentRunRef = runName
	ss.StartTime = &now

	// Set workflow phase to Running and record StartTime on first step launch.
	if wf.Status.Phase != agentorcv1alpha1.AgentWorkflowPhaseRunning {
		wf.Status.Phase = agentorcv1alpha1.AgentWorkflowPhaseRunning
		wf.Status.StartTime = &now
	}

	return nil
}

// checkStepRun polls the underlying AgentRun and advances step state in-memory on terminal.
// It does not call Status().Update — the caller is responsible for persisting.
func (r *AgentWorkflowReconciler) checkStepRun(
	ctx context.Context,
	wf *agentorcv1alpha1.AgentWorkflow,
	step *agentorcv1alpha1.WorkflowStep,
	ss *agentorcv1alpha1.WorkflowStepStatus,
) (ctrl.Result, error) {
	if ss.AgentRunRef == "" {
		return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
	}

	var run agentorcv1alpha1.AgentRun
	if err := r.Get(ctx, client.ObjectKey{Name: ss.AgentRunRef, Namespace: wf.Namespace}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			// AgentRun was deleted externally; treat as failure.
			now := metav1.Now()
			ss.Phase = agentorcv1alpha1.WorkflowStepPhaseFailed
			ss.CompletionTime = &now
			ss.FailureReason = "AgentRun was deleted"
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	switch run.Status.Phase {
	case agentorcv1alpha1.AgentRunPhaseSucceeded:
		now := metav1.Now()
		ss.Phase = agentorcv1alpha1.WorkflowStepPhaseSucceeded
		ss.CompletionTime = &now
		ss.Output = run.Status.Output
		ss.SpendUSD = run.Status.SpendUSD
		return ctrl.Result{}, nil

	case agentorcv1alpha1.AgentRunPhaseFailed:
		now := metav1.Now()
		failureReason := run.Status.RawOutput
		if failureReason == "" {
			failureReason = fmt.Sprintf("AgentRun %s failed", ss.AgentRunRef)
		}
		ss.CompletionTime = &now
		if wf.Spec.OnStepFailure == "continue" {
			// Mark as Skipped so dependents can still be evaluated.
			ss.Phase = agentorcv1alpha1.WorkflowStepPhaseSkipped
			ss.FailureReason = failureReason
			return ctrl.Result{}, nil
		}
		ss.Phase = agentorcv1alpha1.WorkflowStepPhaseFailed
		ss.FailureReason = failureReason
		return ctrl.Result{}, nil

	case agentorcv1alpha1.AgentRunPhaseWaitingForInput:
		// If the human answered and a continuation run was created, follow it.
		if run.Status.ContinuationRunRef != "" {
			ss.AgentRunRef = run.Status.ContinuationRunRef
			ss.Phase = agentorcv1alpha1.WorkflowStepPhaseRunning
			return ctrl.Result{Requeue: true}, nil
		}
		ss.Phase = agentorcv1alpha1.WorkflowStepPhaseWaitingForInput
		return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
	}

	// Still running — poll again.
	return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
}

// depsReady returns true when all DependsOn steps are in a terminal-eligible state.
func (r *AgentWorkflowReconciler) depsReady(
	step agentorcv1alpha1.WorkflowStep,
	index map[string]*agentorcv1alpha1.WorkflowStepStatus,
	onFailure string,
) bool {
	for _, dep := range step.DependsOn {
		s, ok := index[dep]
		if !ok {
			return false
		}
		if s.Phase == agentorcv1alpha1.WorkflowStepPhaseSucceeded {
			continue
		}
		if s.Phase == agentorcv1alpha1.WorkflowStepPhaseSkipped && onFailure == "continue" {
			continue
		}
		return false
	}
	return true
}

// failWorkflow marks the workflow as Failed in-memory.
// The caller is responsible for persisting via Status().Update.
func (r *AgentWorkflowReconciler) failWorkflow(ctx context.Context, wf *agentorcv1alpha1.AgentWorkflow, reason string) {
	log.FromContext(ctx).Info("workflow failed", "reason", reason)
	now := metav1.Now()
	wf.Status.Phase = agentorcv1alpha1.AgentWorkflowPhaseFailed
	wf.Status.CompletionTime = &now
	wf.Status.TotalSpendUSD = r.sumSpend(wf.Status.Steps)
}

// budgetExceeded returns a non-empty reason string if the workflow-level budget is exceeded.
func (r *AgentWorkflowReconciler) budgetExceeded(wf *agentorcv1alpha1.AgentWorkflow) string {
	if wf.Spec.BudgetCap == nil || wf.Spec.BudgetCap.Total == "" {
		return ""
	}
	cap := parseFloatSafe(wf.Spec.BudgetCap.Total)
	if cap <= 0 {
		return ""
	}
	spent := 0.0
	for _, ss := range wf.Status.Steps {
		spent += parseFloatSafe(ss.SpendUSD)
	}
	if spent >= cap {
		return fmt.Sprintf("workflow budget cap %.4f USD exceeded (spent %.4f USD)", cap, spent)
	}
	return ""
}

// sumSpend returns the total spend across all steps as a formatted string.
func (r *AgentWorkflowReconciler) sumSpend(steps []agentorcv1alpha1.WorkflowStepStatus) string {
	total := 0.0
	for _, ss := range steps {
		total += parseFloatSafe(ss.SpendUSD)
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%.6f", total)
}

// resolveTemplates replaces {{steps.<name>.output}} placeholders in input.
func resolveTemplates(input string, steps []agentorcv1alpha1.WorkflowStepStatus) string {
	for _, ss := range steps {
		placeholder := fmt.Sprintf("{{steps.%s.output}}", ss.Name)
		input = strings.ReplaceAll(input, placeholder, ss.Output)
	}
	return input
}

// evalCondition evaluates a simple CEL-like condition string.
// Currently supports expressions of the form: steps["<name>"].phase == "<value>"
// A full CEL library can be wired in without changing the call sites.
func evalCondition(condition string, index map[string]*agentorcv1alpha1.WorkflowStepStatus) (bool, error) {
	// Simple built-in evaluator: steps["<name>"].phase == "<phase>"
	// Format: steps["gather"].phase == "Succeeded"
	cond := strings.TrimSpace(condition)
	if strings.Contains(cond, "==") {
		parts := strings.SplitN(cond, "==", 2)
		lhs := strings.TrimSpace(parts[0])
		rhs := strings.Trim(strings.TrimSpace(parts[1]), `"'`)

		if strings.HasPrefix(lhs, `steps["`) && strings.HasSuffix(lhs, `"].phase`) {
			stepName := strings.TrimSuffix(strings.TrimPrefix(lhs, `steps["`), `"].phase`)
			if ss, ok := index[stepName]; ok {
				return string(ss.Phase) == rhs, nil
			}
			return false, nil
		}
		if strings.HasPrefix(lhs, `steps["`) && strings.HasSuffix(lhs, `"].output`) {
			stepName := strings.TrimSuffix(strings.TrimPrefix(lhs, `steps["`), `"].output`)
			if ss, ok := index[stepName]; ok {
				return ss.Output == rhs, nil
			}
			return false, nil
		}
	}
	return true, fmt.Errorf("unsupported condition expression: %q (only steps[\"name\"].phase == \"value\" is supported)", condition)
}

func isWorkflowTerminal(phase agentorcv1alpha1.AgentWorkflowPhase) bool {
	return phase == agentorcv1alpha1.AgentWorkflowPhaseSucceeded ||
		phase == agentorcv1alpha1.AgentWorkflowPhaseFailed ||
		phase == agentorcv1alpha1.AgentWorkflowPhaseCancelled
}

func parseFloatSafe(s string) float64 {
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// SetupWithManager registers the controller with the Manager.
func (r *AgentWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcv1alpha1.AgentWorkflow{}).
		Owns(&agentorcv1alpha1.AgentRun{}).
		Named("agentworkflow").
		Complete(r)
}
