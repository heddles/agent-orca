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
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// TestApprovalsAgainstPostgres exercises the approvals store on a real
// database (AGENTORCA_TEST_POSTGRES_DSN).
func TestApprovalsAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("AGENTORCA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AGENTORCA_TEST_POSTGRES_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}

	req := Approval{
		Namespace: "team-a", RunName: "run-1", Kind: ApprovalKindEgress, SourceKey: "sb-1/chunk-1",
		Summary: "curl wants example.com:443", Details: json.RawMessage(`{"hits":1}`),
		ApproverGroups: []string{"platform"}, ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	a, err := s.RequestApproval(ctx, req)
	if err != nil || a.Status != ApprovalPending || a.ID == "" || a.ApproverGroups[0] != "platform" {
		t.Fatalf("request = %+v, %v", a, err)
	}

	// A repeat refreshes the details of the same row.
	req.Details = json.RawMessage(`{"hits":5}`)
	again, err := s.RequestApproval(ctx, req)
	if err != nil || again.ID != a.ID || string(again.Details) != `{"hits": 5}` {
		t.Fatalf("repeat = %+v (details %s), %v; want the same row with new details", again, again.Details, err)
	}

	if list, err := s.ListApprovals(ctx, ApprovalQuery{Namespaces: []string{"team-b"}}); err != nil || len(list) != 0 {
		t.Fatalf("another tenant's namespaces see %d approvals, %v", len(list), err)
	}
	pending, err := s.ListApprovals(ctx, ApprovalQuery{Namespaces: []string{"team-a"}, Status: ApprovalPending})
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, %v", len(pending), err)
	}

	decided, err := s.DecideApproval(ctx, a.ID, ApprovalApproved, "alice@example.com", "needed for the docs build")
	if err != nil || decided.Status != ApprovalApproved || decided.DecidedBy != "alice@example.com" || decided.DecidedAt == nil {
		t.Fatalf("decide = %+v, %v", decided, err)
	}
	if _, err := s.DecideApproval(ctx, a.ID, ApprovalRejected, "bob@example.com", ""); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("second decision err = %v, want ErrApprovalNotPending", err)
	}
	// A new request for a decided source keeps its decision.
	if after, _ := s.RequestApproval(ctx, req); after.Status != ApprovalApproved {
		t.Errorf("re-request reopened a decided approval: %s", after.Status)
	}
	if err := s.MarkApprovalApplied(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetApproval(ctx, a.ID); got.AppliedAt == nil {
		t.Error("applied_at not set")
	}

	other := req
	other.SourceKey, other.ID = "sb-1/chunk-2", ""
	if _, err := s.RequestApproval(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawRunApprovals(ctx, "team-a", "run-1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ListApprovals(ctx, ApprovalQuery{Namespace: "team-a", Status: ApprovalPending}); len(left) != 0 {
		t.Errorf("%d approvals still pending after the run ended", len(left))
	}
}
