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

package postgresql

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// TestRoutingEventsAgainstPostgres runs the routing telemetry SQL on a real
// database when AGENTORCA_TEST_POSTGRES_DSN names a throwaway one. sqlmock
// checks which statements run and with what arguments, never that Postgres
// accepts them; this does.
func TestRoutingEventsAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("AGENTORCA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AGENTORCA_TEST_POSTGRES_DSN not set")
	}
	store, err := NewStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyMigration(ctx); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ttft := 85
	base := RoutingEvent{
		DecisionID: "d-1", Tenant: "acme", Namespace: "ns", RunName: "run-1", Agent: "coder", Selector: "balanced",
		Strategy: "rule-based", Path: "rule:weighted", Stage: "chat", StageConf: 0.4, CategoryVersion: 1,
		Provider: "strong", Model: "openai/strong", Candidates: []string{"strong", "weak"}, Confidence: 0.5,
		PromptTokens: 190, CompletionTokens: 3, CostUSD: 0.000615, LatencyMS: 410, TTFTMS: &ttft,
		FinishReason: "stop", TurnIndex: 0,
	}
	second := base
	second.DecisionID, second.ParentDecisionID, second.Provider, second.Path = "d-2", "d-1", "weak", "fallback"
	second.CostUSD, second.AuxCostUSD, second.TTFTMS = 0.00002, 0.00001, nil
	second.ErrorClass, second.FallbackFrom = "", "strong"

	if err := store.UpsertRoutingEvents(ctx, []RoutingEvent{base, second}); err != nil {
		t.Fatalf("UpsertRoutingEvents: %v", err)
	}
	// A retried batch must not duplicate rows, and completes what was sent early.
	completed := base
	completed.ToolCalls, completed.ToolErrors = 2, 1
	if err := store.UpsertRoutingEvents(ctx, []RoutingEvent{completed}); err != nil {
		t.Fatalf("retry: %v", err)
	}

	got, err := store.RoutingEventsForRun(ctx, "ns", "run-1", 0)
	if err != nil {
		t.Fatalf("RoutingEventsForRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events after a retry, want 2: %+v", len(got), got)
	}
	byID := map[string]RoutingEvent{got[0].DecisionID: got[0], got[1].DecisionID: got[1]}
	d1, d2 := byID["d-1"], byID["d-2"]
	if d1.ToolCalls != 2 || d1.ToolErrors != 1 {
		t.Errorf("the retry did not complete the event: %+v", d1)
	}
	if d1.TTFTMS == nil || *d1.TTFTMS != 85 || d1.CostUSD < 0.000614 || d1.CostUSD > 0.000616 || d1.PromptTokens != 190 {
		t.Errorf("d-1 = %+v", d1)
	}
	if d2.ParentDecisionID != "d-1" || d2.TTFTMS != nil || d2.FallbackFrom != "strong" || d2.AuxCostUSD < 0.0000099 {
		t.Errorf("d-2 = %+v", d2)
	}

	costs, err := store.CostByModel(ctx, []string{"ns"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CostByModel: %v", err)
	}
	if len(costs) != 2 || costs[0].Provider != "strong" || costs[0].Calls != 1 {
		t.Errorf("costs = %+v", costs)
	}
	if other, err := store.CostByModel(ctx, []string{"elsewhere"}, time.Now().Add(-time.Hour)); err != nil || len(other) != 0 {
		t.Errorf("another namespace's costs = %+v, %v; want none", other, err)
	}
	if all, err := store.CostByModel(ctx, nil, time.Now().Add(-time.Hour)); err != nil || len(all) != 2 {
		t.Errorf("all namespaces = %+v, %v", all, err)
	}

	// Archiving the run records its outcome from what is already stored.
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "ns", Labels: map[string]string{"agentorca.io/tenant": "acme"}},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "0.0006"},
	}
	if err := store.ArchiveRun(ctx, run, nil); err != nil {
		t.Fatalf("ArchiveRun: %v", err)
	}
	var phase, selector string
	var turns int
	var cost float64
	if err := db.QueryRowContext(ctx, `SELECT phase, COALESCE(selector, ''), turns, total_cost_usd::float8
		FROM run_outcomes WHERE namespace = 'ns' AND run_name = 'run-1'`).Scan(&phase, &selector, &turns, &cost); err != nil {
		t.Fatalf("reading the outcome: %v", err)
	}
	if phase != "Succeeded" || selector != "balanced" || turns != 2 || cost < 0.00059 || cost > 0.00061 {
		t.Errorf("outcome = %s/%s/%d turns/%v", phase, selector, turns, cost)
	}
	// Re-archiving updates the same row.
	if err := store.ArchiveRun(ctx, run, nil); err != nil {
		t.Fatalf("second ArchiveRun: %v", err)
	}

	n, err := store.PruneRoutingEvents(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Errorf("pruned %d, %v; want 2", n, err)
	}
}
