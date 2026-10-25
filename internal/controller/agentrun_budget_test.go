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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// budgetReconciler returns a reconciler and a Running run whose router reported
// the given budget state.
func budgetReconciler(t *testing.T, spec agentorcav1alpha1.RunBudget, state agentorcav1alpha1.RunBudgetState, spent string) (*AgentRunReconciler, *agentorcav1alpha1.AgentRun, *memApprovals, *record.FakeRecorder) {
	t.Helper()
	run, token := gitRun()
	run.UID = "uid-1"
	run.Spec.Budget = &spec
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseRunning
	run.Status.Budget = &agentorcav1alpha1.RunBudgetStatus{State: state, CapUSD: "2.00", SpentUSD: spent}
	r, _ := sandboxReconciler(t, newStubGateway(), run, token, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	store := &memApprovals{}
	r.Approvals = store
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	return r, run, store, rec
}

func pausingBudget() agentorcav1alpha1.RunBudget {
	return agentorcav1alpha1.RunBudget{
		MaxUSD: "2.00", OnExceeded: agentorcav1alpha1.RunBudgetPause, RaiseByUSD: "1.50",
		ApproverGroups: []string{"finance"},
	}
}

func TestRouterBudgetConfig(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{}
	if routerBudget(run, true) != nil {
		t.Error("a run without spec.budget got a router budget")
	}
	run.Spec.Budget = &agentorcav1alpha1.RunBudget{MaxUSD: "2.50", WarnAtPercent: 70, OnExceeded: agentorcav1alpha1.RunBudgetPause}
	if b := routerBudget(run, true); b == nil || b.MaxUSD != 2.5 || b.WarnAtPercent != 70 || b.OnExceeded != "pause" {
		t.Errorf("config = %+v", b)
	}
	// Pausing needs the approval store; without it the run fails at the cap.
	if b := routerBudget(run, false); b == nil || b.OnExceeded != "fail" {
		t.Errorf("without approvals = %+v, want fail", b)
	}
	run.Spec.Budget = &agentorcav1alpha1.RunBudget{MaxUSD: "0"}
	if routerBudget(run, true) != nil {
		t.Error("a zero budget must not be enforced")
	}
}

func TestBudgetExceededRequestsARaiseAndAppliesIt(t *testing.T) {
	r, run, store, events := budgetReconciler(t, pausingBudget(), agentorcav1alpha1.RunBudgetExceeded, "2.0300")
	ctx := context.Background()

	if res, err := r.reconcileBudget(ctx, run); res != nil || err != nil {
		t.Fatalf("pending: res=%v err=%v, want the run left paused", res, err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("%d approvals, want 1", len(store.rows))
	}
	a := store.rows[0]
	if a.Kind != postgresql.ApprovalKindBudget || a.Status != postgresql.ApprovalPending || a.SourceKey != "uid-1/raise-1" ||
		len(a.ApproverGroups) != 1 || a.ApproverGroups[0] != "finance" {
		t.Errorf("approval = %+v", a)
	}
	if !strings.Contains(a.Summary, "$2.00 budget") || !strings.Contains(a.Summary, "$1.50") {
		t.Errorf("summary = %q", a.Summary)
	}
	if !strings.Contains(string(a.Details), `"newCapUSD":"3.5000"`) || !strings.Contains(string(a.Details), `"spentUSD":"2.0300"`) {
		t.Errorf("details = %s", a.Details)
	}
	if time.Until(a.ExpiresAt) < 23*time.Hour {
		t.Errorf("expires in %v, want the 24h default pause timeout", time.Until(a.ExpiresAt))
	}
	if c := meta.FindStatusCondition(fetchSandboxRun(t, r.Client, run).Status.Conditions, ConditionBudgetExceeded); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("BudgetExceeded = %+v, want True", c)
	}

	// Polling again does not open a second request.
	_, _ = r.reconcileBudget(ctx, run)
	if len(store.rows) != 1 {
		t.Fatalf("%d approvals after a second poll, want still 1", len(store.rows))
	}

	approveBudgetRaise(t, r, run, store, events)
}

// approveBudgetRaise is the second half of the raise flow: an approver says yes,
// the next poll raises the cap, and spending past it asks again.
func approveBudgetRaise(t *testing.T, r *AgentRunReconciler, run *agentorcav1alpha1.AgentRun, store *memApprovals, events *record.FakeRecorder) {
	t.Helper()
	ctx := context.Background()
	a := store.rows[0]
	if _, err := store.DecideApproval(ctx, a.ID, postgresql.ApprovalApproved, "alice", "ok"); err != nil {
		t.Fatal(err)
	}
	if res, err := r.reconcileBudget(ctx, run); res != nil || err != nil {
		t.Fatalf("approved: res=%v err=%v", res, err)
	}
	got := fetchSandboxRun(t, r.Client, run)
	b := got.Status.Budget
	if b.CapUSD != "3.5000" || b.Raises != 1 || b.State != agentorcav1alpha1.RunBudgetOK {
		t.Fatalf("status.budget = %+v, want cap 3.5000, 1 raise, ok", b)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, ConditionBudgetExceeded); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("BudgetExceeded after the raise = %+v, want False", c)
	}
	if store.rows[0].AppliedAt == nil {
		t.Error("the approval was not marked applied")
	}
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseRunning {
		t.Errorf("phase = %s, want Running", got.Status.Phase)
	}
	if ev := <-events.Events; !strings.Contains(ev, "BudgetRaised") || !strings.Contains(ev, "alice") {
		t.Errorf("event = %q", ev)
	}

	// Spending past the new cap asks again, as a new request.
	got.Status.Budget.State, got.Status.Budget.SpentUSD = agentorcav1alpha1.RunBudgetExceeded, "3.5100"
	_, _ = r.reconcileBudget(ctx, got)
	if len(store.rows) != 2 || store.rows[1].SourceKey != "uid-1/raise-2" || !strings.Contains(string(store.rows[1].Details), `"newCapUSD":"5.0000"`) {
		t.Errorf("second request = %+v", store.rows)
	}
}

func TestBudgetRaiseRejectedFailsTheRun(t *testing.T) {
	r, run, store, _ := budgetReconciler(t, pausingBudget(), agentorcav1alpha1.RunBudgetExceeded, "2.1000")
	ctx := context.Background()
	_, _ = r.reconcileBudget(ctx, run)
	if _, err := store.DecideApproval(ctx, store.rows[0].ID, postgresql.ApprovalRejected, "bob", "too expensive"); err != nil {
		t.Fatal(err)
	}

	res, err := r.reconcileBudget(ctx, run)
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v, want the run ended", res, err)
	}
	got := fetchSandboxRun(t, r.Client, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	for _, want := range []string{"budget of $2.00 reached", "rejected by bob", "too expensive"} {
		if !strings.Contains(got.Status.LastRestartReason, want) {
			t.Errorf("failure reason %q lacks %q", got.Status.LastRestartReason, want)
		}
	}
}

func TestBudgetPauseTimesOut(t *testing.T) {
	spec := pausingBudget()
	spec.PauseTimeout = &metav1.Duration{Duration: time.Minute}
	r, run, store, _ := budgetReconciler(t, spec, agentorcav1alpha1.RunBudgetExceeded, "2.1000")
	ctx := context.Background()
	_, _ = r.reconcileBudget(ctx, run)
	store.rows[0].ExpiresAt = time.Now().Add(-time.Second)

	res, err := r.reconcileBudget(ctx, run)
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v, want the run ended", res, err)
	}
	got := fetchSandboxRun(t, r.Client, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed || !strings.Contains(got.Status.LastRestartReason, "expired") {
		t.Errorf("phase %s, reason %q: want Failed because the raise expired", got.Status.Phase, got.Status.LastRestartReason)
	}
	if store.rows[0].Status != postgresql.ApprovalExpired {
		t.Errorf("approval = %s, want expired", store.rows[0].Status)
	}
}

func TestBudgetWarningEventOnce(t *testing.T) {
	r, run, store, events := budgetReconciler(t, pausingBudget(), agentorcav1alpha1.RunBudgetWarning, "1.7000")
	ctx := context.Background()
	for range 3 {
		got := fetchSandboxRun(t, r.Client, run)
		if res, err := r.reconcileBudget(ctx, got); res != nil || err != nil {
			t.Fatalf("res=%v err=%v", res, err)
		}
	}
	if len(events.Events) != 1 {
		t.Fatalf("%d events, want exactly one warning", len(events.Events))
	}
	if ev := <-events.Events; !strings.Contains(ev, "BudgetWarning") || !strings.Contains(ev, "$1.7000 of the $2.00 budget") {
		t.Errorf("event = %q", ev)
	}
	if len(store.rows) != 0 {
		t.Errorf("a warning opened %d approvals", len(store.rows))
	}
}

func TestBudgetFailModeLeavesTheHaltToTheRouter(t *testing.T) {
	spec := agentorcav1alpha1.RunBudget{MaxUSD: "2.00", OnExceeded: agentorcav1alpha1.RunBudgetFail}
	r, run, store, _ := budgetReconciler(t, spec, agentorcav1alpha1.RunBudgetExceeded, "2.1000")
	if res, err := r.reconcileBudget(context.Background(), run); res != nil || err != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if len(store.rows) != 0 {
		t.Errorf("fail mode opened %d approvals", len(store.rows))
	}
}

func TestBudgetUnreportedOrAbsentDoesNothing(t *testing.T) {
	r, run, store, _ := budgetReconciler(t, pausingBudget(), agentorcav1alpha1.RunBudgetOK, "")
	run.Status.Budget = nil
	if res, err := r.reconcileBudget(context.Background(), run); res != nil || err != nil || len(store.rows) != 0 {
		t.Errorf("no report: res=%v err=%v rows=%d", res, err, len(store.rows))
	}
	run.Spec.Budget = nil
	if res, err := r.reconcileBudget(context.Background(), run); res != nil || err != nil {
		t.Errorf("no budget: res=%v err=%v", res, err)
	}
}
