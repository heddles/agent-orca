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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// A Tool's class and the agent's GuardrailPolicy reach the router's config. The
// policy sets only toolApproval, no filters: it must not be dropped for lack of
// filters.
func TestBuildRouterConfigCarriesToolSideEffects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	primary, fallback, selector, agent := fallbackFixture(scheme)
	agent.Spec.Tools = []string{"lookup", "deploy", "plain"}
	agent.Spec.GuardrailPolicyRef = "factory"
	mk := func(name string, class agentorcav1alpha1.ToolSideEffect) *agentorcav1alpha1.Tool {
		return &agentorcav1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       agentorcav1alpha1.ToolSpec{OCIRef: "example.com/" + name + ":1", SideEffect: class},
		}
	}
	policy := &agentorcav1alpha1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "factory", Namespace: "default"},
		Spec: agentorcav1alpha1.GuardrailPolicySpec{ToolApproval: &agentorcav1alpha1.ToolApprovalPolicy{
			Write: agentorcav1alpha1.ToolApprovalApprove, Actuate: agentorcav1alpha1.ToolApprovalDeny,
			Timeout: &metav1.Duration{Duration: time.Hour},
		}},
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		primary, fallback, selector, agent, run, policy,
		mk("lookup", agentorcav1alpha1.ToolSideEffectRead), mk("deploy", agentorcav1alpha1.ToolSideEffectActuate), mk("plain", ""),
	).Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "test-sa", nil)
	if err != nil {
		t.Fatalf("buildRouterConfig: %v", err)
	}

	got := map[string]string{}
	for _, td := range cfg.ToolDefinitions {
		got[td.Name] = td.SideEffect
	}
	for name, want := range map[string]string{"lookup": "read", "deploy": "actuate", "plain": ""} {
		if v, ok := got[name]; !ok || v != want {
			t.Errorf("tool %q: sideEffect = %q (present=%v), want %q", name, v, ok, want)
		}
	}
	if cfg.ToolApproval == nil || cfg.ToolApproval.Write != "approve" || cfg.ToolApproval.Actuate != "deny" ||
		cfg.ToolApproval.Read != "" || cfg.ToolApproval.Unclassified != "" {
		t.Errorf("toolApproval = %+v, want write=approve actuate=deny and the rest unset", cfg.ToolApproval)
	}
}

func TestBuildRouterConfigWithoutAPolicyLeavesToolApprovalUnset(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	primary, fallback, selector, agent := fallbackFixture(scheme)
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "test-agent", Input: "ping"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primary, fallback, selector, agent, run).Build()
	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	cfg, _, _, err := r.buildRouterConfig(context.Background(), run, agent, "test-sa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolApproval != nil {
		t.Errorf("toolApproval = %+v, want nil so the router applies its defaults", cfg.ToolApproval)
	}
}

func TestToolApprovalPendingConditionClearsWhenNothingWaits(t *testing.T) {
	r, run, store, _ := budgetReconciler(t, pausingBudget(), agentorcav1alpha1.RunBudgetOK, "")
	ctx := context.Background()
	run.Status.Conditions = []metav1.Condition{{
		Type: agentorcav1alpha1.ConditionToolApprovalPending, Status: metav1.ConditionTrue,
		Reason: "ToolCallWaiting", LastTransitionTime: metav1.Now(),
	}}
	if err := r.Status().Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	for _, a := range []postgresql.Approval{
		{Kind: postgresql.ApprovalKindTool, SourceKey: "k1", Namespace: run.Namespace, RunName: run.Name},
		// An egress request waiting must not hold the tool condition.
		{Kind: postgresql.ApprovalKindEgress, SourceKey: "k2", Namespace: run.Namespace, RunName: run.Name},
	} {
		if _, err := store.RequestApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	cond := func() *metav1.Condition {
		return meta.FindStatusCondition(fetchSandboxRun(t, r.Client, run).Status.Conditions, agentorcav1alpha1.ConditionToolApprovalPending)
	}

	// A tool call still waits: the condition stays.
	r.reconcileToolApprovals(ctx, fetchSandboxRun(t, r.Client, run))
	if c := cond(); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want True while a tool call waits", c)
	}

	// Decided: the next poll clears it.
	if _, err := store.DecideApproval(ctx, store.rows[0].ID, postgresql.ApprovalApproved, "alice", ""); err != nil {
		t.Fatal(err)
	}
	r.reconcileToolApprovals(ctx, fetchSandboxRun(t, r.Client, run))
	if c := cond(); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("condition = %+v, want False once nothing waits", c)
	}
}

func TestToolApprovalConditionAbsentMeansNoStoreQuery(t *testing.T) {
	r, run, _, _ := budgetReconciler(t, pausingBudget(), agentorcav1alpha1.RunBudgetOK, "")
	r.Approvals = nil // a query would panic on the nil store
	r.reconcileToolApprovals(context.Background(), run)
}
