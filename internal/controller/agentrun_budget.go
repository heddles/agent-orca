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
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/router"
)

// Run budgets (AgentRun.spec.budget). The model-router counts the run's spend
// and enforces the cap; the operator owns the cap. The router reports a
// warning or an exceeded budget (internal API, budget_api.go); at "exceeded"
// with onExceeded=pause the operator asks approvers (kind "budget") to raise
// the cap. An approved raise is added to status.budget.capUSD, which the paused
// router reads back and resumes under. A rejected or expired raise fails the
// run. With onExceeded=fail the router halts the run itself, through the
// safeguard path.

// Conditions for a run's budget.
const (
	// ConditionBudgetWarning is True from the warning until the cap is raised.
	ConditionBudgetWarning = "BudgetWarning"
	// ConditionBudgetExceeded is True while a run waits for a decision on a raise.
	ConditionBudgetExceeded = "BudgetExceeded"
)

// defaultBudgetPauseTimeout applies when spec.budget.pauseTimeout is unset.
const defaultBudgetPauseTimeout = 24 * time.Hour

// budgetRequest is a budget approval's details.
type budgetRequest struct {
	Raise       int    `json:"raise"`
	SpentUSD    string `json:"spentUSD"`
	CapUSD      string `json:"capUSD"`
	RaiseByUSD  string `json:"raiseByUSD"`
	NewCapUSD   string `json:"newCapUSD"`
	Description string `json:"description"`
}

// routerBudget translates spec.budget into the router's configuration. Pausing
// needs the approval store: without it the run fails at the cap instead.
func routerBudget(run *agentorcav1alpha1.AgentRun, approvalsAvailable bool) *router.BudgetConfig {
	b := run.Spec.Budget
	if b == nil {
		return nil
	}
	maxUSD, err := strconv.ParseFloat(b.MaxUSD, 64)
	if err != nil || maxUSD <= 0 {
		return nil
	}
	action := string(agentorcav1alpha1.RunBudgetFail)
	if b.OnExceeded == agentorcav1alpha1.RunBudgetPause && approvalsAvailable {
		action = string(agentorcav1alpha1.RunBudgetPause)
	}
	return &router.BudgetConfig{MaxUSD: maxUSD, WarnAtPercent: b.WarnAtPercent, OnExceeded: action}
}

// reconcileBudget acts on the router's budget report. A non-nil result means
// the run was ended and the caller returns it.
func (r *AgentRunReconciler) reconcileBudget(ctx context.Context, run *agentorcav1alpha1.AgentRun) (*ctrl.Result, error) {
	spec, st := run.Spec.Budget, run.Status.Budget
	if spec == nil || st == nil {
		return nil, nil
	}
	switch st.State {
	case agentorcav1alpha1.RunBudgetWarning:
		r.warnBudget(ctx, run)
	case agentorcav1alpha1.RunBudgetExceeded:
		return r.handleBudgetExceeded(ctx, run)
	}
	return nil, nil
}

func (r *AgentRunReconciler) warnBudget(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	if c := meta.FindStatusCondition(run.Status.Conditions, ConditionBudgetWarning); c != nil && c.Status == metav1.ConditionTrue {
		return
	}
	st := run.Status.Budget
	msg := fmt.Sprintf("$%s of the $%s budget is spent", st.SpentUSD, st.CapUSD)
	r.setRunCondition(ctx, run, metav1.Condition{Type: ConditionBudgetWarning, Status: metav1.ConditionTrue, Reason: "NearCap", Message: msg})
	if r.Recorder != nil {
		r.Recorder.Event(run, corev1.EventTypeWarning, "BudgetWarning", msg)
	}
}

func (r *AgentRunReconciler) handleBudgetExceeded(ctx context.Context, run *agentorcav1alpha1.AgentRun) (*ctrl.Result, error) {
	spec, st := run.Spec.Budget, run.Status.Budget
	// Without approvals the router halts the run itself (see routerBudget).
	if spec.OnExceeded != agentorcav1alpha1.RunBudgetPause || r.Approvals == nil {
		return nil, nil
	}
	logger := log.FromContext(ctx)

	capUSD, _ := strconv.ParseFloat(st.CapUSD, 64)
	raiseBy := parseUSD(spec.RaiseByUSD)
	if raiseBy <= 0 {
		raiseBy = parseUSD(spec.MaxUSD)
	}
	n := st.Raises + 1
	req := budgetRequest{
		Raise: n, SpentUSD: st.SpentUSD, CapUSD: st.CapUSD,
		RaiseByUSD: formatUSD(raiseBy), NewCapUSD: formatUSD(capUSD + raiseBy),
	}
	req.Description = fmt.Sprintf("Run %s has spent $%s of its $%s budget. Approving raises the cap by $%s to $%s.",
		run.Name, st.SpentUSD, st.CapUSD, req.RaiseByUSD, req.NewCapUSD)
	details, _ := json.Marshal(req)
	timeout := defaultBudgetPauseTimeout
	if spec.PauseTimeout != nil && spec.PauseTimeout.Duration > 0 {
		timeout = spec.PauseTimeout.Duration
	}
	stored, err := r.Approvals.RequestApproval(ctx, postgresql.Approval{
		Tenant:         run.Labels["agentorca.io/tenant"],
		Namespace:      run.Namespace,
		RunName:        run.Name,
		Kind:           postgresql.ApprovalKindBudget,
		SourceKey:      fmt.Sprintf("%s/raise-%d", run.UID, n),
		Summary:        fmt.Sprintf("%s reached its $%s budget: raise it by $%s?", run.Name, st.CapUSD, req.RaiseByUSD),
		Details:        details,
		ApproverGroups: spec.ApproverGroups,
		ExpiresAt:      time.Now().Add(timeout),
	})
	if err != nil {
		logger.Error(err, "recording the budget approval")
		return nil, nil // the run stays paused; the next poll retries
	}
	if stored.Created && stored.Status == postgresql.ApprovalPending && r.ApprovalNotifier != nil {
		if err := r.ApprovalNotifier.NotifyApproval(ctx, stored); err != nil {
			logger.Error(err, "notifying approvers", "approval", stored.ID)
		}
	}
	if stored.Status == postgresql.ApprovalPending && time.Now().After(stored.ExpiresAt) {
		if decided, err := r.Approvals.DecideApproval(ctx, stored.ID, postgresql.ApprovalExpired, "", "no decision within the pause timeout"); err == nil {
			stored = decided
		}
	}

	switch stored.Status {
	case postgresql.ApprovalPending:
		r.setRunCondition(ctx, run, metav1.Condition{
			Type: ConditionBudgetExceeded, Status: metav1.ConditionTrue, Reason: "WaitingForApproval",
			Message: fmt.Sprintf("paused at $%s of the $%s budget; waiting for an approver to raise it", st.SpentUSD, st.CapUSD),
		})
		return nil, nil
	case postgresql.ApprovalApproved:
		if err := r.applyBudgetRaise(ctx, run, req, stored); err != nil {
			logger.Error(err, "applying the budget raise", "approval", stored.ID)
		}
		return nil, nil
	default: // rejected, expired or withdrawn
		if err := r.Approvals.MarkApprovalApplied(ctx, stored.ID); err != nil {
			logger.Error(err, "marking the budget decision applied", "approval", stored.ID)
		}
		reason := fmt.Sprintf("budget of $%s reached and the raise was %s", st.CapUSD, stored.Status)
		if stored.DecidedBy != "" {
			reason += " by " + stored.DecidedBy
		}
		if stored.Reason != "" {
			reason += ": " + stored.Reason
		}
		res, err := r.failRun(ctx, run, reason)
		return &res, err
	}
}

// applyBudgetRaise adds an approved raise to the run's cap.
func (r *AgentRunReconciler) applyBudgetRaise(ctx context.Context, run *agentorcav1alpha1.AgentRun, req budgetRequest, a postgresql.Approval) error {
	patch := client.MergeFrom(run.DeepCopy())
	st := run.Status.Budget
	st.CapUSD, st.Raises, st.State = req.NewCapUSD, req.Raise, agentorcav1alpha1.RunBudgetOK
	now := metav1.Now()
	st.UpdatedAt = &now
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: ConditionBudgetExceeded, Status: metav1.ConditionFalse, Reason: "Raised",
		Message: fmt.Sprintf("raised to $%s by %s", req.NewCapUSD, a.DecidedBy),
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: ConditionBudgetWarning, Status: metav1.ConditionFalse, Reason: "Raised", Message: "the cap was raised",
	})
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		return err
	}
	if err := r.Approvals.MarkApprovalApplied(ctx, a.ID); err != nil {
		log.FromContext(ctx).Error(err, "marking the budget raise applied", "approval", a.ID)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(run, corev1.EventTypeNormal, "BudgetRaised", "cap raised to $%s (%s)", req.NewCapUSD, a.DecidedBy)
	}
	return nil
}

// setRunCondition sets one status condition, patching only when it changes.
func (r *AgentRunReconciler) setRunCondition(ctx context.Context, run *agentorcav1alpha1.AgentRun, cond metav1.Condition) {
	if c := meta.FindStatusCondition(run.Status.Conditions, cond.Type); c != nil &&
		c.Status == cond.Status && c.Reason == cond.Reason && c.Message == cond.Message {
		return
	}
	patch := client.MergeFrom(run.DeepCopy())
	meta.SetStatusCondition(&run.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		log.FromContext(ctx).Error(err, "updating a budget condition", "condition", cond.Type)
	}
}

func parseUSD(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
