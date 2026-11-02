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
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// Approvals API (G2): list approval requests and decide them.
//
//	GET  /api/approvals?status=pending&namespace=&run=
//	POST /api/approvals/{id}/approve   {"reason": "..."}
//	POST /api/approvals/{id}/reject    {"reason": "..."}
//
// Listing is tenant-scoped like the rest of the UI API. Deciding needs an
// authenticated user (an OIDC session or user token, not a service account),
// access to the approval's namespace and, when the approval names approver
// groups, membership in one of them. The deciding user is recorded.

// approvalStore is the slice of postgresql.Store the API uses.
type approvalStore interface {
	ListApprovals(ctx context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error)
	GetApproval(ctx context.Context, id string) (postgresql.Approval, error)
	DecideApproval(ctx context.Context, id, status, by, reason string) (postgresql.Approval, error)
}

func (s *UIServer) approvalStore() approvalStore {
	if s.approvals != nil {
		return s.approvals
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.approvalStore()
	if store == nil {
		http.Error(w, `{"error":"approvals need the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	q := postgresql.ApprovalQuery{
		Status:  r.URL.Query().Get("status"),
		RunName: r.URL.Query().Get("run"),
	}
	ns := r.URL.Query().Get("namespace")
	if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil {
		q.Namespaces = tenantNamespaces(tenant)
		if len(q.Namespaces) == 0 {
			jsonResponse(w, map[string]any{"approvals": []postgresql.Approval{}})
			return
		}
		if ns != "" {
			if !slices.Contains(q.Namespaces, ns) {
				http.Error(w, "namespace not allowed", http.StatusForbidden)
				return
			}
			q.Namespaces = []string{ns}
		}
	} else if ns != "" {
		q.Namespace = ns
	}
	list, err := store.ListApprovals(r.Context(), q)
	if err != nil {
		slog.Error("listing approvals", "err", err)
		http.Error(w, "listing approvals failed", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"approvals": list})
}

// handleApproval serves POST /api/approvals/{id}/approve|reject.
func (s *UIServer) handleApproval(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/approvals/")
	id, action, ok := strings.Cut(rest, "/")
	if !ok || id == "" || (action != "approve" && action != "reject") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.approvalStore()
	if store == nil {
		http.Error(w, `{"error":"approvals need the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	tenant, ok := TenantFromContext(r.Context())
	if !ok || tenant == nil || tenant.UserID == "" {
		http.Error(w, "deciding an approval needs a signed-in user", http.StatusForbidden)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body)
	}

	a, err := store.GetApproval(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("reading approval", "id", id, "err", err)
		http.Error(w, "reading approval failed", http.StatusInternalServerError)
		return
	}
	// Approvals outside the tenant's namespaces do not exist for it.
	if !slices.Contains(tenantNamespaces(tenant), a.Namespace) {
		http.NotFound(w, r)
		return
	}
	if len(a.ApproverGroups) > 0 && !slices.ContainsFunc(tenant.Groups, func(g string) bool { return slices.Contains(a.ApproverGroups, g) }) {
		http.Error(w, "you are not in an approver group for this request", http.StatusForbidden)
		return
	}
	status := postgresql.ApprovalApproved
	if action == "reject" {
		status = postgresql.ApprovalRejected
	}
	decided, err := store.DecideApproval(r.Context(), id, status, tenant.UserID, strings.TrimSpace(body.Reason))
	if errors.Is(err, postgresql.ErrApprovalNotPending) {
		http.Error(w, "the approval is no longer pending", http.StatusConflict)
		return
	}
	if err != nil {
		slog.Error("deciding approval", "id", id, "err", err)
		http.Error(w, "deciding approval failed", http.StatusInternalServerError)
		return
	}
	slog.Info("approval decided", "id", id, "status", status, "by", tenant.UserID, "namespace", a.Namespace, "run", a.RunName)
	jsonResponse(w, decided)
}
