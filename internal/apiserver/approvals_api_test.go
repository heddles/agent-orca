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

package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

type fakeApprovals struct {
	rows     map[string]postgresql.Approval
	lastList postgresql.ApprovalQuery
}

func (f *fakeApprovals) ListApprovals(_ context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error) {
	f.lastList = q
	var out []postgresql.Approval
	for _, a := range f.rows {
		if len(q.Namespaces) == 0 || slices.Contains(q.Namespaces, a.Namespace) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeApprovals) GetApproval(_ context.Context, id string) (postgresql.Approval, error) {
	a, ok := f.rows[id]
	if !ok {
		return a, sql.ErrNoRows
	}
	return a, nil
}

func (f *fakeApprovals) DecideApproval(_ context.Context, id, status, by, reason string) (postgresql.Approval, error) {
	a := f.rows[id]
	if a.Status != postgresql.ApprovalPending {
		return a, postgresql.ErrApprovalNotPending
	}
	a.Status, a.DecidedBy, a.Reason = status, by, reason
	f.rows[id] = a
	return a, nil
}

func approvalsServer() (*UIServer, *fakeApprovals) {
	f := &fakeApprovals{rows: map[string]postgresql.Approval{
		"a1": {ID: "a1", Namespace: "team-a", Status: postgresql.ApprovalPending, ApproverGroups: []string{"platform"}},
		"a2": {ID: "a2", Namespace: "team-a", Status: postgresql.ApprovalPending},
		"b1": {ID: "b1", Namespace: "team-b", Status: postgresql.ApprovalPending},
	}}
	return &UIServer{approvals: f}, f
}

func asUser(r *http.Request, user string, groups ...string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), tenantIdentityKey, &TenantIdentity{
		Namespaces: []string{"team-a"}, UserID: user, Groups: groups,
	}))
}

func TestListApprovalsIsTenantScoped(t *testing.T) {
	s, f := approvalsServer()
	rec := httptest.NewRecorder()
	s.handleListApprovals(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/approvals?status=pending", nil), "alice"))
	var body struct {
		Approvals []postgresql.Approval `json:"approvals"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || len(body.Approvals) != 2 {
		t.Fatalf("status %d, %d approvals; want team-a's two", rec.Code, len(body.Approvals))
	}
	if f.lastList.Status != postgresql.ApprovalPending {
		t.Errorf("status filter not passed: %+v", f.lastList)
	}

	rec = httptest.NewRecorder()
	s.handleListApprovals(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/approvals?namespace=team-b", nil), "alice"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("listing another tenant's namespace: %d, want 403", rec.Code)
	}
}

func TestDecideApprovalAuthorization(t *testing.T) {
	decide := func(r *http.Request) (int, *fakeApprovals) {
		s, f := approvalsServer()
		rec := httptest.NewRecorder()
		s.handleApproval(rec, r)
		return rec.Code, f
	}
	post := func(id, action string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/approvals/"+id+"/"+action, strings.NewReader(`{"reason":"ok"}`))
	}

	if code, _ := decide(post("a2", "approve")); code != http.StatusForbidden {
		t.Errorf("no signed-in user: %d, want 403", code)
	}
	if code, _ := decide(asUser(post("b1", "approve"), "alice", "platform")); code != http.StatusNotFound {
		t.Errorf("approval in another tenant's namespace: %d, want 404", code)
	}
	if code, _ := decide(asUser(post("a1", "approve"), "alice", "dev")); code != http.StatusForbidden {
		t.Errorf("user outside the approver groups: %d, want 403", code)
	}
	code, f := decide(asUser(post("a1", "approve"), "alice", "dev", "platform"))
	if code != http.StatusOK || f.rows["a1"].Status != postgresql.ApprovalApproved || f.rows["a1"].DecidedBy != "alice" || f.rows["a1"].Reason != "ok" {
		t.Errorf("approver in the group: %d, %+v", code, f.rows["a1"])
	}
	code, f = decide(asUser(post("a2", "reject"), "bob"))
	if code != http.StatusOK || f.rows["a2"].Status != postgresql.ApprovalRejected {
		t.Errorf("no approver groups: any namespace user may decide: %d, %+v", code, f.rows["a2"])
	}
	if code, _ := decide(asUser(httptest.NewRequest(http.MethodGet, "/api/approvals/a2/approve", nil), "bob")); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a decision: %d, want 405", code)
	}
}

func TestDecideApprovalTwiceConflicts(t *testing.T) {
	s, f := approvalsServer()
	a := f.rows["a2"]
	a.Status = postgresql.ApprovalApproved
	f.rows["a2"] = a
	rec := httptest.NewRecorder()
	s.handleApproval(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/approvals/a2/reject", nil), "bob"))
	if rec.Code != http.StatusConflict {
		t.Errorf("deciding a decided approval: %d, want 409", rec.Code)
	}
}
