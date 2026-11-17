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
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// The audit trail records every change made through the UI API and every
// approval decision: who (user ID), what (action, kind, namespace, name), the
// resulting resourceVersion and a short summary, under the caller's tenant ID.
//
// Failure policy. The Kubernetes write happens first and cannot be taken back,
// so the audit row is written after it, retried briefly, and if it still cannot
// be written the request is NOT failed: the client would see an error for a
// change that took effect, retry it, and apply it twice. Instead the loss is made
// loud and recoverable:
//
//   - the full event is logged as one structured error line, so it can be
//     replayed from the logs;
//   - agentorca_audit_write_failures_total counts it, for an alert;
//   - the response carries the header X-Audit-Status: failed;
//   - the change itself still carries agentorca.io/last-modified-by and
//     agentorca.io/last-modified-at on the object, so the actor is never lost.
//
// An approval decision is the same: the decision stands (it is already stored in
// the approvals table with the deciding user), and the audit row is the copy.
// Without a PostgreSQL store there is no table to write, so only the annotations
// and the log record the change; the operator says so once.

// auditAttempts is how often a failed audit write is tried before giving up, and
// auditRetryDelay the wait before the first retry (it doubles).
const (
	auditAttempts   = 3
	auditRetryDelay = 100 * time.Millisecond
)

// auditStore is the slice of postgresql.Store the audit trail uses.
type auditStore interface {
	AppendAudit(ctx context.Context, e postgresql.AuditEvent) (postgresql.AuditEvent, error)
	ListAudit(ctx context.Context, q postgresql.AuditQuery) ([]postgresql.AuditEvent, error)
}

var auditWriteFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "agentorca_audit_write_failures_total",
	Help: "Audit events that could not be written to PostgreSQL after retries. Each one is in the operator log.",
})

func init() {
	ctrlmetrics.Registry.MustRegister(auditWriteFailures)
}

// auditSink returns the audit store, or nil without PostgreSQL.
func (s *UIServer) auditSink() auditStore {
	if s.audit != nil {
		return s.audit
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// anonymousActor is the actor of a write with no identity at all.
const anonymousActor = "anonymous"

// actorOf names who is acting: the user ID, or for a caller with no user behind
// it (service account, client credentials) machine:<tenant>. Without any
// identity (authentication disabled) it is "anonymous".
func actorOf(ctx context.Context) string {
	t, ok := TenantFromContext(ctx)
	switch {
	case !ok || t == nil:
		return anonymousActor
	case t.UserID != "":
		return t.UserID
	}
	return "machine:" + t.TenantName
}

// recordAudit appends an audit event for the caller in r, following the failure
// policy above. Call it before the response body is written so the failure
// header can be set.
func (s *UIServer) recordAudit(w http.ResponseWriter, r *http.Request, e postgresql.AuditEvent) {
	e.UserID = actorOf(r.Context())
	store := s.auditSink()
	if store == nil {
		s.auditUnavailable.Do(func() {
			slog.Warn("no PostgreSQL store: changes are recorded only in agentorca.io/last-modified-* annotations and this log")
		})
		slog.Info("audit", "event", eventJSON(e))
		return
	}
	// A change is audited even if the client has gone away, so the write does not
	// use the request's cancellation; it keeps the identity (and so the tenant scope).
	ctx := pgScope(context.WithoutCancel(r.Context()))
	delay := auditRetryDelay
	var err error
	for attempt := 1; attempt <= auditAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = store.AppendAudit(attemptCtx, e)
		cancel()
		if err == nil {
			return
		}
		if attempt < auditAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}
	auditWriteFailures.Inc()
	slog.Error("audit event could not be written; recover it from this line", "err", err, "event", eventJSON(e), "attempts", strconv.Itoa(auditAttempts))
	w.Header().Set("X-Audit-Status", "failed")
}

func eventJSON(e postgresql.AuditEvent) string {
	b, _ := json.Marshal(e)
	return string(b)
}

// auditEventFor builds the event for a resource write. The resourceVersion is
// the one the write produced.
func auditEventFor(action, kind, namespace, name, resourceVersion, summary string) postgresql.AuditEvent {
	return postgresql.AuditEvent{
		Action: action, Kind: kind, Namespace: namespace, Name: name,
		ResourceVersion: resourceVersion, Summary: summary,
	}
}

// auditPage is the body of GET /api/audit.
type auditPage struct {
	Events []postgresql.AuditEvent `json:"events"`
	// NextCursor is the value to pass as ?cursor= for the next (older) page.
	// Empty on the last page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// handleAudit serves GET /api/audit: the tenant's audit events, newest first.
// It needs the admin role. Filters: kind, namespace, name, user, action;
// paging: limit (default 100, at most 500) and cursor.
func (s *UIServer) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRole(w, r, tenancy.RoleAdmin) {
		return
	}
	store := s.auditSink()
	if store == nil {
		http.Error(w, `{"error":"the audit trail needs the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	query := postgresql.AuditQuery{
		Kind: q.Get("kind"), Namespace: q.Get("namespace"), Name: q.Get("name"),
		UserID: q.Get("user"), Action: q.Get("action"),
	}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		if query.Namespace != "" && !namespaceAllowed(r.Context(), query.Namespace) {
			http.Error(w, `{"error":"namespace not allowed"}`, http.StatusForbidden)
			return
		}
		// Events about objects outside the tenant's namespaces are not its to read,
		// even under its tenant ID.
		query.Namespaces = tenantNamespaces(t)
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			http.Error(w, `{"error":"limit must be between 1 and 500"}`, http.StatusBadRequest)
			return
		}
		limit = n
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			http.Error(w, `{"error":"invalid cursor"}`, http.StatusBadRequest)
			return
		}
		query.Before = n
	}
	// One extra row says whether another page exists.
	query.Limit = limit + 1
	events, err := store.ListAudit(pgScope(r.Context()), query)
	if err != nil {
		slog.Error("listing audit events", "err", err)
		http.Error(w, `{"error":"listing audit events failed"}`, http.StatusInternalServerError)
		return
	}
	page := auditPage{Events: events}
	if len(events) > limit {
		page.Events = events[:limit]
		page.NextCursor = strconv.FormatInt(events[limit-1].ID, 10)
	}
	if page.Events == nil {
		page.Events = []postgresql.AuditEvent{}
	}
	jsonResponse(w, page)
}
