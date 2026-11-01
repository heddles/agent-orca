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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

const benchJudge = "judge"

const benchAgent = "coder"

func agentFixture(name string, labels map[string]string) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "adaptive"},
	}
}

// benchScope is the scope of a benchmark that covers one Agent routing through
// the selector.
func benchScope(sel *agentorcav1alpha1.ModelSelector) benchmarkScope {
	scope := benchmarkScope{Agents: []string{benchAgent}, Providers: map[string]bool{}}
	for _, n := range providerNames(sel) {
		scope.Providers[n] = true
	}
	return scope
}

func benchmarkFixture() (*agentorcav1alpha1.RoutingBenchmark, *agentorcav1alpha1.ModelSelector) {
	sel := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "adaptive", Namespace: "ns"},
		Spec:       agentorcav1alpha1.ModelSelectorSpec{Providers: []agentorcav1alpha1.ProviderWeight{{Name: "strong"}, {Name: "weak"}}},
	}
	b := &agentorcav1alpha1.RoutingBenchmark{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: "ns", Generation: 1},
		Spec: agentorcav1alpha1.RoutingBenchmarkSpec{
			AgentRefs: []string{benchAgent},
			Judge:     &agentorcav1alpha1.JudgeSpec{ProviderRef: benchJudge},
			Budget:    agentorcav1alpha1.BenchmarkBudget{PerDayUSD: "1.00"},
		},
	}
	return b, sel
}

func benchCondition(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}

func TestAssessBenchmark_ReadyWhenEverythingResolves(t *testing.T) {
	b, sel := benchmarkFixture()
	conds := assessBenchmark(b, benchScope(sel), nil, true)
	if c := benchCondition(conds, "Ready"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}
	if hasCondition(conds, agentorcav1alpha1.BenchmarkConditionJudgeIsArm) || hasCondition(conds, agentorcav1alpha1.BenchmarkConditionPostgresUnavailable) {
		t.Errorf("unexpected warning conditions: %+v", conds)
	}
}

func TestAssessBenchmark_NotReady(t *testing.T) {
	for name, tc := range map[string]struct {
		scope    func(benchmarkScope) benchmarkScope
		missing  map[string]bool
		postgres bool
		reason   string
	}{
		"no agent matches": {
			scope:    func(s benchmarkScope) benchmarkScope { s.Agents = nil; return s },
			postgres: true, reason: reasonNoAgentsMatched,
		},
		"named agent missing": {
			scope:    func(s benchmarkScope) benchmarkScope { s.MissingAgents = []string{"ghost"}; return s },
			postgres: true, reason: reasonAgentNotFound,
		},
		"selector missing": {
			scope:    func(s benchmarkScope) benchmarkScope { s.MissingSelectors = []string{"coder -> adaptive"}; return s },
			postgres: true, reason: reasonSelectorNotFound,
		},
		"judge missing": {missing: map[string]bool{benchJudge: true}, postgres: true, reason: reasonBenchmarkProviderGap},
		"no postgres":   {postgres: false, reason: reasonPostgresUnavailable},
	} {
		b, sel := benchmarkFixture()
		scope := benchScope(sel)
		if tc.scope != nil {
			scope = tc.scope(scope)
		}
		conds := assessBenchmark(b, scope, tc.missing, tc.postgres)
		c := benchCondition(conds, "Ready")
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tc.reason {
			t.Errorf("%s: Ready = %+v, want False/%s", name, c, tc.reason)
		}
	}
	b, sel := benchmarkFixture()
	if c := benchCondition(assessBenchmark(b, benchScope(sel), nil, false), agentorcav1alpha1.BenchmarkConditionPostgresUnavailable); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("PostgresUnavailable = %+v", c)
	}
}

func TestAssessBenchmark_JudgeIsArmWarnsButStaysReady(t *testing.T) {
	b, sel := benchmarkFixture()
	b.Spec.Judge.ProviderRef = "strong"
	conds := assessBenchmark(b, benchScope(sel), nil, true)
	if c := benchCondition(conds, agentorcav1alpha1.BenchmarkConditionJudgeIsArm); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("JudgeIsArm = %+v", c)
	}
	if c := benchCondition(conds, "Ready"); c.Status != metav1.ConditionTrue {
		t.Errorf("a warning must not make the benchmark not Ready: %+v", c)
	}
	b.Spec.Judge.ProviderRef = benchJudge
	b.Spec.Shadow = &agentorcav1alpha1.ShadowSpec{Arms: agentorcav1alpha1.ShadowArmsProviders, Providers: []string{benchJudge}}
	if !judgeIsArm(b, benchScope(sel)) {
		t.Error("a judge listed as a shadow provider is an arm")
	}
}

type fakeResults struct {
	tallies []postgresql.ShadowTally
	spend   postgresql.ShadowSpend
}

func (f fakeResults) ShadowSpendSince(context.Context, string, string, time.Time, time.Time) (postgresql.ShadowSpend, error) {
	return f.spend, nil
}

func (f fakeResults) ShadowTallies(context.Context, string, string, time.Time) ([]postgresql.ShadowTally, error) {
	return f.tallies, nil
}

func reconcileBenchmark(t *testing.T, results BenchmarkResults, extra ...client.Object) (*agentorcav1alpha1.RoutingBenchmark, ctrl.Result) {
	t.Helper()
	b, sel := benchmarkFixture()
	judge := &agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: benchJudge, Namespace: "ns"}}
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := append([]client.Object{b, sel, judge, agentFixture(benchAgent, nil)}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&agentorcav1alpha1.RoutingBenchmark{}).Build()
	r := &RoutingBenchmarkReconciler{Client: c, Scheme: scheme, Results: results,
		now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "bench"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got agentorcav1alpha1.RoutingBenchmark
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "bench"}, &got); err != nil {
		t.Fatal(err)
	}
	return &got, res
}

func TestReconcileBenchmark_PublishesResultsAndRequeues(t *testing.T) {
	got, res := reconcileBenchmark(t, fakeResults{
		tallies: []postgresql.ShadowTally{
			{Agent: benchAgent, Category: "chat", Served: 3, Shadow: 1, Ties: 2, Invalid: 1, Inconsistent: 3},
		},
		spend: postgresql.ShadowSpend{SettledUSD: 0.4, LeasedUSD: 0.7},
	})
	st := got.Status
	if st.JudgedPairs != 6 || st.SampledCalls != 7 || st.InconsistentJudgments != 3 || st.JudgeInconsistentRate != "0.500" {
		t.Errorf("counts = %+v", st)
	}
	if len(st.Categories) != 1 || st.Categories[0].Agent != benchAgent || st.Categories[0].Category != "stage:chat" || st.Categories[0].ServedWins != 3 {
		t.Errorf("categories = %+v", st.Categories)
	}
	if len(st.MatchedAgents) != 1 || st.MatchedAgents[0] != benchAgent {
		t.Errorf("matched agents = %v", st.MatchedAgents)
	}
	if st.ShadowSpendTodayUSD != "0.400000" || !st.BudgetExhausted {
		t.Errorf("spend = %s exhausted = %v; leased budget counts toward exhaustion", st.ShadowSpendTodayUSD, st.BudgetExhausted)
	}
	if res.RequeueAfter != 6*time.Hour {
		t.Errorf("requeue = %v, want the 6h default interval", res.RequeueAfter)
	}
}

// Evidence is kept per Agent, so one Agent's samples never stand in for another's.
func TestReconcileBenchmark_KeepsEachAgentsCategoriesApart(t *testing.T) {
	got, _ := reconcileBenchmark(t, fakeResults{tallies: []postgresql.ShadowTally{
		{Agent: benchAgent, Category: "chat", Served: 1},
		{Agent: "writer", Category: "chat", Shadow: 5},
		{Agent: benchAgent, Category: "tool-exec", Ties: 3},
	}})
	byKey := map[string]agentorcav1alpha1.CategorySummary{}
	for _, c := range got.Status.Categories {
		byKey[c.Agent+"/"+c.Category] = c
	}
	if len(byKey) != 3 || byKey["writer/stage:chat"].ShadowWins != 5 || byKey[benchAgent+"/stage:chat"].ShadowWins != 0 {
		t.Errorf("categories = %+v", got.Status.Categories)
	}
	// The best-sampled category comes first.
	if got.Status.Categories[0].Agent != "writer" {
		t.Errorf("first category = %+v, want the best-sampled", got.Status.Categories[0])
	}
}

func TestReconcileBenchmark_MatchesAgentsBySelectorAndName(t *testing.T) {
	b, sel := benchmarkFixture()
	b.Spec.AgentRefs = []string{"named"}
	b.Spec.AgentSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b, sel,
		agentFixture("named", nil), agentFixture("labelled", map[string]string{"team": "a"}), agentFixture("other", map[string]string{"team": "b"}),
	).Build()
	r := &RoutingBenchmarkReconciler{Client: c, Scheme: scheme}
	scope, err := r.scopeOf(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scope.Agents, ",") != "labelled,named" || len(scope.MissingAgents) != 0 {
		t.Errorf("scope = %+v, want labelled and named, not other", scope)
	}
	b.Spec.AgentRefs = []string{"named", "ghost"}
	if scope, _ = r.scopeOf(context.Background(), b); strings.Join(scope.MissingAgents, ",") != "ghost" {
		t.Errorf("missing agents = %v, want ghost", scope.MissingAgents)
	}
}

func TestReconcileBenchmark_WithoutPostgresReportsIt(t *testing.T) {
	got, _ := reconcileBenchmark(t, nil)
	if c := benchCondition(got.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonPostgresUnavailable {
		t.Errorf("Ready = %+v", c)
	}
	if got.Status.LastEvaluated != nil {
		t.Error("evaluated results without a store")
	}
}
