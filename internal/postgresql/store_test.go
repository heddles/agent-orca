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
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryHistoryBindsLimitOffsetArgs is a regression test for the bug where
// QueryHistory called QueryContext(ctx, dataQ) WITHOUT args..., so the
// `LIMIT $1 OFFSET $2` placeholders were never satisfied and every history
// query failed with "expected 2 arguments, got 0" (causing the UI to silently
// show "No historical runs found"). sqlmock's WithArgs enforces that the
// limit/offset values are actually passed to the statement.
func TestQueryHistoryBindsLimitOffsetArgs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	completed := start.Add(5 * time.Minute)

	// No-filter query: count (0 args) then data (LIMIT $1 OFFSET $2 with 2 args).
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM archived_runs")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`LIMIT \$1 OFFSET \$2`).
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "namespace", "agent_ref", "phase", "spend_usd",
			"start_time", "completion_time", "tenant",
			"context_used_tokens", "max_context_tokens",
		}).AddRow("run-1", "default", "my-agent", "Succeeded", "0.0123",
			start, completed, "acme", 42, 200000))

	s := &Store{db: db}
	page, err := s.QueryHistory(context.Background(), HistoryQuery{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("QueryHistory failed: %v (args were not bound to the statement)", err)
	}
	if page.Total != 1 {
		t.Fatalf("total = %d, want 1", page.Total)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(page.Runs))
	}
	r := page.Runs[0]
	if r.Name != "run-1" || r.Namespace != "default" || r.AgentRef != "my-agent" {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.ContextUsedTokens != 42 || r.MaxContextTokens != 200000 {
		t.Fatalf("context tokens not scanned: used=%d max=%d", r.ContextUsedTokens, r.MaxContextTokens)
	}
	if !r.StartTime.Equal(start) || !r.CompletionTime.Equal(completed) {
		t.Fatalf("timestamps not scanned: start=%v comp=%v", r.StartTime, r.CompletionTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestQueryHistoryEmptyReturnsNonNullRuns verifies that an empty result set
// serializes to `[]` (not `null`), which is what previously crashed the UI
// ("can't access property Symbol.iterator, t.runs is null").
func TestQueryHistoryEmptyReturnsNonNullRuns(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM archived_runs")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`LIMIT \$1 OFFSET \$2`).
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "namespace", "agent_ref", "phase", "spend_usd",
			"start_time", "completion_time", "tenant",
			"context_used_tokens", "max_context_tokens",
		})) // no rows added

	s := &Store{db: db}
	page, err := s.QueryHistory(context.Background(), HistoryQuery{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("QueryHistory failed: %v", err)
	}
	if page.Runs == nil {
		t.Fatal("Runs is nil — would serialize as null and crash the UI")
	}
	if len(page.Runs) != 0 {
		t.Fatalf("Runs = %d rows, want 0", len(page.Runs))
	}
	if page.Total != 0 {
		t.Fatalf("Total = %d, want 0", page.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
// are all bound to the correct positional placeholders (regression guard for
// the same args-binding bug).
func TestQueryHistoryFilteredBindsArgs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// phase filter → $1, agent_ref filter → $2, then LIMIT $3 OFFSET $4.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM archived_runs")).
		WithArgs("Succeeded", "my-agent").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`agent_ref = \$2.*LIMIT \$3 OFFSET \$4`).
		WithArgs("Succeeded", "my-agent", 25, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "namespace", "agent_ref", "phase", "spend_usd",
			"start_time", "completion_time", "tenant",
			"context_used_tokens", "max_context_tokens",
		}))

	s := &Store{db: db}
	page, err := s.QueryHistory(context.Background(), HistoryQuery{
		Limit: 25, Offset: 0, Phase: "Succeeded", AgentRef: "my-agent",
	})
	if err != nil {
		t.Fatalf("QueryHistory failed: %v", err)
	}
	if page.Total != 0 || len(page.Runs) != 0 {
		t.Fatalf("expected empty result, got total=%d runs=%d", page.Total, len(page.Runs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
