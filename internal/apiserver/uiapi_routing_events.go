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
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// routingEventReader reads a run's routing telemetry. *postgresql.Store
// implements it.
type routingEventReader interface {
	RoutingEventsForRun(ctx context.Context, namespace, run string, limit int) ([]postgresql.RoutingEvent, error)
}

// routingEventSource returns the telemetry reader, or nil without PostgreSQL.
func (s *UIServer) routingEventSource() routingEventReader {
	if s.routingReader != nil {
		return s.routingReader
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// handleRunRoutingEvents serves GET /api/runs/{ns}/{name}/routing-events: every
// LLM call the run made, oldest first. AgentRun.status keeps only the most
// recent routing decisions, so this is the full history for long runs.
func (s *UIServer) handleRunRoutingEvents(w http.ResponseWriter, r *http.Request, ns, runName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil {
		if !slices.Contains(tenantNamespaces(tenant), ns) {
			http.Error(w, "namespace not allowed", http.StatusForbidden)
			return
		}
	}
	src := s.routingEventSource()
	if src == nil {
		http.Error(w, `{"error":"routing history needs the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := src.RoutingEventsForRun(pgScope(r.Context()), ns, runName, limit)
	if err != nil {
		slog.Error("loading routing events", "run", runName, "ns", ns, "err", err)
		http.Error(w, "loading routing events", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"events": events})
}

// routingEventLister lists routing events across runs. *postgresql.Store
// implements it.
type routingEventLister interface {
	ListRoutingEvents(ctx context.Context, q postgresql.RoutingEventQuery) ([]postgresql.RoutingEvent, error)
}

// routingEventView is a routing event with the run it belongs to, for the list
// (the stored type keeps namespace and run off the wire because the router
// posts it without them).
type routingEventView struct {
	postgresql.RoutingEvent
	Namespace string `json:"namespace"`
	Run       string `json:"run"`
}

// handleRoutingEvents serves GET /api/routing-events: routing decisions across
// the caller's runs, newest first. Filters: namespace (must be one the tenant
// may use; without it, all of them), agent, selector, outcome (ok, error,
// fallback), shadow=true, since and until (RFC 3339), limit (default 100, at
// most 500). Only the caller's tenant's rows are returned, by the tenant_id
// filter and row-level security in the store.
func (s *UIServer) handleRoutingEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRole(w, r, tenancy.RoleViewer) {
		return
	}
	lister, _ := s.routingEventSource().(routingEventLister)
	if lister == nil {
		http.Error(w, `{"error":"routing history needs the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	qv := r.URL.Query()
	includeShadow, _ := strconv.ParseBool(qv.Get("shadow"))
	query := postgresql.RoutingEventQuery{
		Agent: qv.Get("agent"), Selector: qv.Get("selector"), Outcome: qv.Get("outcome"),
		IncludeShadow: includeShadow,
	}
	if ns := qv.Get("namespace"); ns != "" {
		query.Namespaces = []string{ns}
	}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		allowed := tenantNamespaces(t)
		if ns := qv.Get("namespace"); ns != "" && !slices.Contains(allowed, ns) {
			http.Error(w, `{"error":"namespace not allowed"}`, http.StatusForbidden)
			return
		}
		if query.Namespaces == nil {
			if len(allowed) == 0 {
				jsonResponse(w, map[string]any{"events": []routingEventView{}})
				return
			}
			query.Namespaces = allowed
		}
	}
	for name, dst := range map[string]*time.Time{"since": &query.Since, "until": &query.Until} {
		if v := qv.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				http.Error(w, `{"error":"`+name+` must be an RFC 3339 time"}`, http.StatusBadRequest)
				return
			}
			*dst = t
		}
	}
	if v := qv.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			http.Error(w, `{"error":"limit must be between 1 and 500"}`, http.StatusBadRequest)
			return
		}
		query.Limit = n
	}
	events, err := lister.ListRoutingEvents(pgScope(r.Context()), query)
	if errors.Is(err, postgresql.ErrInvalidRoutingQuery) {
		http.Error(w, `{"error":"outcome must be ok, error or fallback"}`, http.StatusBadRequest)
		return
	}
	if err != nil {
		slog.Error("listing routing events", "err", err)
		http.Error(w, "listing routing events", http.StatusInternalServerError)
		return
	}
	views := make([]routingEventView, len(events))
	for i, e := range events {
		views[i] = routingEventView{RoutingEvent: e, Namespace: e.Namespace, Run: e.RunName}
	}
	jsonResponse(w, map[string]any{"events": views})
}
