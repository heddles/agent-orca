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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgofake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// spendStore is a state store whose LoadSpend returns a configured value or
// error and counts its calls.
type spendStore struct {
	testStore
	usd   float64
	err   error
	calls *atomic.Int32
}

func (s spendStore) LoadSpend(context.Context, string) (float64, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}
	return s.usd, s.err
}

func costScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func costRun(phase agentorcav1alpha1.AgentRunPhase, spend string) *agentorcav1alpha1.AgentRun {
	now := metav1.Now()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default", Finalizers: []string{agentRunFinalizer}},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "agent"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: phase, SpendUSD: spend, PodName: "pod-1"},
	}
	if isTerminal(phase) {
		run.Status.CompletionTime = &now
	}
	return run
}

func costReconciler(t *testing.T, store *spendStore, run *agentorcav1alpha1.AgentRun) (*AgentRunReconciler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(costScheme(t)).
		WithStatusSubresource(&agentorcav1alpha1.AgentRun{}).
		WithObjects(run).
		Build()
	r := &AgentRunReconciler{Client: cl, Scheme: cl.Scheme(), K8s: clientgofake.NewClientset()}
	if store != nil {
		r.StateStore = *store
	}
	return r, cl
}

func getRun(t *testing.T, cl client.Client) *agentorcav1alpha1.AgentRun {
	t.Helper()
	var got agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "run-1", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func wantCostCapture(t *testing.T, run *agentorcav1alpha1.AgentRun, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(run.Status.Conditions, conditionCostCapture)
	if c == nil {
		t.Fatalf("CostCapture condition missing; conditions = %+v", run.Status.Conditions)
	}
	if c.Status != status || c.Reason != reason {
		t.Fatalf("CostCapture = %s/%s, want %s/%s (%s)", c.Status, c.Reason, status, reason, c.Message)
	}
}

// Regression for #76 (1): a retry reset the run to Pending without writing the
// spend, so partial spend survived only in the checkpoint.
func TestHandlePodFailure_RetryStampsPartialSpend(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseRunning, "")
	r, cl := costReconciler(t, &spendStore{usd: 0.25}, run)

	if _, err := r.handlePodFailure(context.Background(), run, &agentorcav1alpha1.Agent{}, "oom"); err != nil {
		t.Fatalf("handlePodFailure: %v", err)
	}

	got := getRun(t, cl)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhasePending || got.Status.RestartCount != 1 {
		t.Fatalf("expected a retry (Pending, restartCount 1), got %s/%d", got.Status.Phase, got.Status.RestartCount)
	}
	if got.Status.SpendUSD != "0.250000" {
		t.Fatalf("SpendUSD during retry = %q, want 0.250000", got.Status.SpendUSD)
	}
	wantCostCapture(t, got, metav1.ConditionTrue, reasonCostComplete)
}

// Dev profile (no state store): the partial spend cannot be read, which must be
// flagged rather than left as an empty string.
func TestHandlePodFailure_RetryWithoutStateStoreFlagsIncomplete(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseRunning, "")
	r, cl := costReconciler(t, nil, run)

	if _, err := r.handlePodFailure(context.Background(), run, &agentorcav1alpha1.Agent{}, "oom"); err != nil {
		t.Fatalf("handlePodFailure: %v", err)
	}

	got := getRun(t, cl)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhasePending {
		t.Fatalf("expected a retry, got %s", got.Status.Phase)
	}
	wantCostCapture(t, got, metav1.ConditionFalse, reasonCostIncomplete)
}

// Retries that exhaust MaxRetries end in failRun, which must carry the spend.
func TestHandlePodFailure_ExhaustedRetriesFailWithSpend(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseRunning, "0.100000")
	run.Status.RestartCount = 3
	r, cl := costReconciler(t, &spendStore{usd: 0.9}, run)

	if _, err := r.handlePodFailure(context.Background(), run, &agentorcav1alpha1.Agent{}, "crash"); err != nil {
		t.Fatalf("handlePodFailure: %v", err)
	}

	got := getRun(t, cl)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if got.Status.SpendUSD != "0.900000" {
		t.Fatalf("SpendUSD = %q, want the accumulated 0.900000", got.Status.SpendUSD)
	}
}

// Regression for #76 (3): a store error used to become "" -- a silent zero on
// exactly the run that needs billing. It must keep the earlier value and say so.
func TestStampSpend_StoreErrorKeepsLowerBoundAndFlagsIncomplete(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseRunning, "0.100000")
	r, _ := costReconciler(t, &spendStore{err: errors.New("redis: connection refused")}, run)

	r.stampSpend(context.Background(), run)

	if run.Status.SpendUSD != "0.100000" {
		t.Fatalf("SpendUSD = %q; a failed read must not blank an earlier value", run.Status.SpendUSD)
	}
	wantCostCapture(t, run, metav1.ConditionFalse, reasonCostIncomplete)
}

func TestStampSpend_StoreErrorOnFreshRunIsIncompleteNotZero(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, _ := costReconciler(t, &spendStore{err: errors.New("boom")}, run)

	r.stampSpend(context.Background(), run)

	wantCostCapture(t, run, metav1.ConditionFalse, reasonCostIncomplete)
	if c := meta.FindStatusCondition(run.Status.Conditions, conditionCostCapture); c.Message == "" {
		t.Fatal("the condition must say why spend is missing")
	}
}

// An expired store key reads back as 0; that must not erase a stamped value.
func TestStampSpend_ZeroReadDoesNotEraseStampedSpend(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseRunning, "0.400000")
	r, _ := costReconciler(t, &spendStore{usd: 0}, run)

	r.stampSpend(context.Background(), run)

	if run.Status.SpendUSD != "0.400000" {
		t.Fatalf("SpendUSD = %q, want the earlier 0.400000", run.Status.SpendUSD)
	}
}

// A run that cost nothing is complete, and distinguishable from an untracked one.
func TestStampSpend_FreeRunIsCompleteWithEmptySpend(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseSucceeded, "")
	r, _ := costReconciler(t, &spendStore{usd: 0}, run)

	r.stampSpend(context.Background(), run)

	if run.Status.SpendUSD != "" {
		t.Fatalf("SpendUSD = %q, want empty for a free run", run.Status.SpendUSD)
	}
	wantCostCapture(t, run, metav1.ConditionTrue, reasonCostComplete)
}

// Regression for #76 (2): a run the router ended via _fail short-circuited in
// handlePodSuccess before any spend was loaded.
func TestHandlePodSuccess_AlreadyFailedRunGetsSpend(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, cl := costReconciler(t, &spendStore{usd: 1.5}, run)

	if _, err := r.handlePodSuccess(context.Background(), run, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}}); err != nil {
		t.Fatalf("handlePodSuccess: %v", err)
	}

	got := getRun(t, cl)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase changed to %s; the _fail result must be kept", got.Status.Phase)
	}
	if got.Status.SpendUSD != "1.500000" {
		t.Fatalf("SpendUSD = %q, want 1.500000", got.Status.SpendUSD)
	}
}

func TestHandlePodSuccess_AlreadyFailedRunWithBrokenStoreIsFlagged(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, cl := costReconciler(t, &spendStore{err: errors.New("down")}, run)

	if _, err := r.handlePodSuccess(context.Background(), run, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}}); err != nil {
		t.Fatalf("handlePodSuccess: %v", err)
	}

	got := getRun(t, cl)
	wantCostCapture(t, got, metav1.ConditionFalse, reasonCostIncomplete)
	if got.Status.SpendUSD != "" {
		t.Fatalf("SpendUSD = %q, want empty (unknown) alongside the Incomplete condition", got.Status.SpendUSD)
	}
}

// A _done run (router-set Succeeded) has the same gap as _fail.
func TestEnsureSpendStamped_DoneRun(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseSucceeded, "")
	r, cl := costReconciler(t, &spendStore{usd: 0.75}, run)

	if _, err := r.ensureSpendStamped(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if got := getRun(t, cl); got.Status.SpendUSD != "0.750000" {
		t.Fatalf("SpendUSD = %q, want 0.750000", got.Status.SpendUSD)
	}
}

// Stamped runs must not touch the store again: terminal runs are reconciled
// repeatedly.
func TestEnsureSpendStamped_Idempotent(t *testing.T) {
	var calls atomic.Int32
	run := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, _ := costReconciler(t, &spendStore{usd: 0.5, calls: &calls}, run)

	for range 3 {
		if _, err := r.ensureSpendStamped(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store read %d times, want 1", n)
	}

	var freeCalls atomic.Int32
	free := costRun(agentorcav1alpha1.AgentRunPhaseSucceeded, "")
	r2, _ := costReconciler(t, &spendStore{usd: 0, calls: &freeCalls}, free)
	for range 3 {
		if _, err := r2.ensureSpendStamped(context.Background(), free); err != nil {
			t.Fatal(err)
		}
	}
	if n := freeCalls.Load(); n != 1 {
		t.Fatalf("free run: store read %d times, want 1 (Complete condition is the marker)", n)
	}
}

func TestEnsureSpendStamped_SkipsNonTerminalAndHandedOff(t *testing.T) {
	for _, phase := range []agentorcav1alpha1.AgentRunPhase{
		agentorcav1alpha1.AgentRunPhaseRunning, agentorcav1alpha1.AgentRunPhaseHandedOff,
	} {
		var calls atomic.Int32
		run := costRun(phase, "")
		r, _ := costReconciler(t, &spendStore{usd: 1, calls: &calls}, run)
		if _, err := r.ensureSpendStamped(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 0 {
			t.Errorf("phase %s: spend must not be stamped here", phase)
		}
	}
}

// A failed read is retried shortly after completion, then given up on so an
// unreachable store cannot keep every finished run in a requeue loop.
func TestEnsureSpendStamped_RetryWindow(t *testing.T) {
	recent := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, _ := costReconciler(t, &spendStore{err: errors.New("down")}, recent)
	after, err := r.ensureSpendStamped(context.Background(), recent)
	if err != nil {
		t.Fatal(err)
	}
	if after != costCaptureRetryInterval {
		t.Fatalf("recent failure: retry after %v, want %v", after, costCaptureRetryInterval)
	}

	old := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	past := metav1.NewTime(time.Now().Add(-2 * costCaptureRetryWindow))
	old.Status.CompletionTime = &past
	r2, _ := costReconciler(t, &spendStore{err: errors.New("down")}, old)
	after, err = r2.ensureSpendStamped(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("old failure: retry after %v, want to stop retrying", after)
	}
}

// An Incomplete run is retried and healed once the store is back.
func TestEnsureSpendStamped_HealsIncompleteRun(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, cl := costReconciler(t, &spendStore{err: errors.New("down")}, run)
	if _, err := r.ensureSpendStamped(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	wantCostCapture(t, getRun(t, cl), metav1.ConditionFalse, reasonCostIncomplete)

	r.StateStore = spendStore{usd: 2.0}
	latest := getRun(t, cl)
	if _, err := r.ensureSpendStamped(context.Background(), latest); err != nil {
		t.Fatal(err)
	}
	got := getRun(t, cl)
	wantCostCapture(t, got, metav1.ConditionTrue, reasonCostComplete)
	if got.Status.SpendUSD != "2.000000" {
		t.Fatalf("SpendUSD = %q, want 2.000000 after healing", got.Status.SpendUSD)
	}
}

// The terminal Reconcile is what catches _done/_fail runs end to end.
func TestReconcile_TerminalRunWithoutSpendIsStamped(t *testing.T) {
	run := costRun(agentorcav1alpha1.AgentRunPhaseFailed, "")
	r, cl := costReconciler(t, &spendStore{usd: 3.25}, run)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := getRun(t, cl); got.Status.SpendUSD != "3.250000" {
		t.Fatalf("SpendUSD = %q, want 3.250000", got.Status.SpendUSD)
	}
}
