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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Chat widget keys (docs/integrations.md). The public endpoints are in widget_public.go.
//
//	POST   /api/widget-keys         builder, audited
//	GET    /api/widget-keys         viewer: ?namespace=&agent=
//	PATCH  /api/widget-keys/{id}    builder, audited: enabled, allowedOrigins, rateLimitPerMin, budgetUsdPerDay
//	DELETE /api/widget-keys/{id}    builder, audited

const (
	kindWidgetKey    = "WidgetKey"
	maxWidgetKeyBody = 8 << 10
)

// widgetKeyStore is the slice of postgresql.Store the widget endpoints use.
type widgetKeyStore interface {
	CreateWidgetKey(ctx context.Context, k postgresql.WidgetKey) (postgresql.WidgetKey, error)
	ListWidgetKeys(ctx context.Context, namespaces []string, agent string) ([]postgresql.WidgetKey, error)
	GetWidgetKey(ctx context.Context, id string) (postgresql.WidgetKey, error)
	UpdateWidgetKey(ctx context.Context, id string, p postgresql.WidgetKeyPatch) (postgresql.WidgetKey, error)
	DeleteWidgetKey(ctx context.Context, id string) (postgresql.WidgetKey, error)
}

func (s *UIServer) widgetKeySource() widgetKeyStore {
	if s.widgetKeys != nil {
		return s.widgetKeys
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) registerWidgetKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/widget-keys", s.handleWidgetKeys)
	mux.HandleFunc("/api/widget-keys/{id}", s.handleWidgetKey)
}

func widgetKeyFail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, postgresql.ErrWidgetKeyNotFound):
		jsonError(w, "not found", http.StatusNotFound)
	case errors.Is(err, postgresql.ErrWidgetKeyExists), errors.Is(err, postgresql.ErrTooManyWidgetKeys):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, postgresql.ErrInvalidWidgetKey):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, postgresql.ErrNoTenantScope):
		jsonError(w, "widget keys need a tenant identity", http.StatusForbidden)
	default:
		jsonError(w, what+" failed", http.StatusInternalServerError)
	}
}

func (s *UIServer) widgetKeyPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (widgetKeyStore, bool) {
	if !requireRole(w, r, role) {
		return nil, false
	}
	store := s.widgetKeySource()
	if store == nil {
		jsonError(w, "widget keys need the PostgreSQL store", http.StatusServiceUnavailable)
		return nil, false
	}
	return store, true
}

func (s *UIServer) handleWidgetKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createWidgetKey(w, r)
	case http.MethodGet:
		s.listWidgetKeys(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) handleWidgetKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		s.patchWidgetKey(w, r)
	case http.MethodDelete:
		s.deleteWidgetKey(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type widgetKeyRequest struct {
	Namespace       string   `json:"namespace"`
	Agent           string   `json:"agent"`
	Name            string   `json:"name"`
	AllowedOrigins  []string `json:"allowedOrigins"`
	RateLimitPerMin int      `json:"rateLimitPerMin"`
	BudgetUSDPerDay float64  `json:"budgetUsdPerDay"`
}

func (s *UIServer) createWidgetKey(w http.ResponseWriter, r *http.Request) {
	store, ok := s.widgetKeyPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req widgetKeyRequest
	if !readJSONBody(w, r, maxWidgetKeyBody, &req) {
		return
	}
	if !namespaceAllowed(r.Context(), req.Namespace) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	agent, ok := s.getAgentFor(w, r, req.Namespace, req.Agent)
	if !ok {
		return
	}
	if agent.Labels[LabelTemplate] == templateMarker {
		jsonError(w, "this agent is a template: make your own copy first", http.StatusConflict)
		return
	}
	published, err := s.agentPublished(r.Context(), agent.Namespace, agent.Name)
	if err != nil {
		jsonError(w, "checking the agent failed", http.StatusInternalServerError)
		return
	}
	if !published {
		jsonError(w, "publish the agent before adding a widget key: the widget chats through its deployment", http.StatusConflict)
		return
	}
	if req.RateLimitPerMin == 0 {
		req.RateLimitPerMin = postgresql.DefaultWidgetRatePerMin
	}
	if req.BudgetUSDPerDay == 0 {
		req.BudgetUSDPerDay = postgresql.DefaultWidgetBudgetPerDay
	}
	id, err := postgresql.NewWidgetKeyID()
	if err != nil {
		jsonError(w, "creating the key failed", http.StatusInternalServerError)
		return
	}
	k, err := store.CreateWidgetKey(pgScope(r.Context()), postgresql.WidgetKey{
		ID: id, Namespace: agent.Namespace, Agent: agent.Name, Name: strings.TrimSpace(req.Name), AllowedOrigins: req.AllowedOrigins,
		RateLimitPerMin: req.RateLimitPerMin, BudgetUSDPerDay: req.BudgetUSDPerDay, CreatedBy: actorOf(r.Context()),
	})
	if err != nil {
		widgetKeyFail(w, "creating the widget key", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, kindWidgetKey, k.Namespace, k.Agent, k.ID,
		fmt.Sprintf("created widget key %q for %s (%d messages per minute, $%v per day)", k.Name, strings.Join(k.AllowedOrigins, ", "), k.RateLimitPerMin, k.BudgetUSDPerDay)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(k)
}

func (s *UIServer) listWidgetKeys(w http.ResponseWriter, r *http.Request) {
	store, ok := s.widgetKeyPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	var namespaces []string
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		if !namespaceAllowed(r.Context(), ns) {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		namespaces = []string{ns}
	} else if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		if namespaces = tenantNamespaces(t); len(namespaces) == 0 {
			jsonResponse(w, []postgresql.WidgetKey{})
			return
		}
	}
	list, err := store.ListWidgetKeys(pgScope(r.Context()), namespaces, r.URL.Query().Get("agent"))
	if err != nil {
		widgetKeyFail(w, "listing widget keys", err)
		return
	}
	jsonResponse(w, list)
}

// ownedWidgetKey loads a key of the caller's tenant in a namespace the caller may use.
func (s *UIServer) ownedWidgetKey(w http.ResponseWriter, r *http.Request, store widgetKeyStore) (postgresql.WidgetKey, bool) {
	id := r.PathValue("id")
	if !postgresql.ValidWidgetKeyID(id) {
		jsonError(w, "not found", http.StatusNotFound)
		return postgresql.WidgetKey{}, false
	}
	k, err := store.GetWidgetKey(pgScope(r.Context()), id)
	if err == nil && !namespaceAllowed(r.Context(), k.Namespace) {
		err = postgresql.ErrWidgetKeyNotFound
	}
	if err != nil {
		widgetKeyFail(w, "reading the widget key", err)
		return postgresql.WidgetKey{}, false
	}
	return k, true
}

func (s *UIServer) patchWidgetKey(w http.ResponseWriter, r *http.Request) {
	store, ok := s.widgetKeyPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req struct {
		Enabled         *bool    `json:"enabled"`
		AllowedOrigins  []string `json:"allowedOrigins"`
		RateLimitPerMin *int     `json:"rateLimitPerMin"`
		BudgetUSDPerDay *float64 `json:"budgetUsdPerDay"`
	}
	if !readJSONBody(w, r, maxWidgetKeyBody, &req) {
		return
	}
	if req.Enabled == nil && req.AllowedOrigins == nil && req.RateLimitPerMin == nil && req.BudgetUSDPerDay == nil {
		jsonError(w, "nothing to change", http.StatusBadRequest)
		return
	}
	cur, ok := s.ownedWidgetKey(w, r, store)
	if !ok {
		return
	}
	k, err := store.UpdateWidgetKey(pgScope(r.Context()), cur.ID, postgresql.WidgetKeyPatch{
		Enabled: req.Enabled, AllowedOrigins: req.AllowedOrigins, RateLimitPerMin: req.RateLimitPerMin, BudgetUSDPerDay: req.BudgetUSDPerDay,
	})
	if err != nil {
		widgetKeyFail(w, "updating the widget key", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindWidgetKey, k.Namespace, k.Agent, k.ID,
		fmt.Sprintf("changed widget key %q: %s, origins %s, %d messages per minute, $%v per day", k.Name, onOff(k.Enabled),
			strings.Join(k.AllowedOrigins, ", "), k.RateLimitPerMin, k.BudgetUSDPerDay)))
	jsonResponse(w, k)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (s *UIServer) deleteWidgetKey(w http.ResponseWriter, r *http.Request) {
	store, ok := s.widgetKeyPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.ownedWidgetKey(w, r, store)
	if !ok {
		return
	}
	k, err := store.DeleteWidgetKey(pgScope(r.Context()), cur.ID)
	if err != nil {
		widgetKeyFail(w, "deleting the widget key", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditDelete, kindWidgetKey, k.Namespace, k.Agent, k.ID, fmt.Sprintf("deleted widget key %q", k.Name)))
	w.WriteHeader(http.StatusNoContent)
}
