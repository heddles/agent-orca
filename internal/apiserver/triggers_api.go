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
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Webhook triggers (docs/integrations.md).
//
//	POST   /api/triggers           builder, audited: create; the secret is in this answer only
//	GET    /api/triggers           viewer: ?namespace=&agent=
//	PATCH  /api/triggers/{id}      builder, audited: {"enabled": bool}
//	DELETE /api/triggers/{id}      builder, audited
//	POST   /hooks/{id}             signed, no session (triggers_hook.go)

const (
	kindTrigger    = "Trigger"
	maxTriggerBody = 8 << 10
	triggersNeedPG = "triggers need the PostgreSQL store"
)

// triggerStore is the slice of postgresql.Store the trigger endpoints use.
type triggerStore interface {
	CreateTrigger(ctx context.Context, t postgresql.Trigger) (postgresql.Trigger, error)
	ListTriggers(ctx context.Context, namespaces []string, agent string) ([]postgresql.Trigger, error)
	GetTrigger(ctx context.Context, id string) (postgresql.Trigger, error)
	SetTriggerEnabled(ctx context.Context, id string, enabled bool) (postgresql.Trigger, error)
	DeleteTrigger(ctx context.Context, id string) (postgresql.Trigger, error)
	RecordTriggerFire(ctx context.Context, id string, at time.Time) error
}

func (s *UIServer) triggerSource() triggerStore {
	if s.triggers != nil {
		return s.triggers
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) registerTriggerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/triggers", s.handleTriggers)
	mux.HandleFunc("/api/triggers/{id}", s.handleTrigger)
	mux.HandleFunc("/hooks/{id}", s.handleHook)
}

func triggerFail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, postgresql.ErrTriggerNotFound):
		jsonError(w, "not found", http.StatusNotFound)
	case errors.Is(err, postgresql.ErrTriggerExists), errors.Is(err, postgresql.ErrTooManyTriggers):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, postgresql.ErrInvalidTrigger):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, postgresql.ErrNoTenantScope):
		jsonError(w, "triggers need a tenant identity", http.StatusForbidden)
	default:
		jsonError(w, what+" failed", http.StatusInternalServerError)
	}
}

// triggerPreflight checks the role and the store.
func (s *UIServer) triggerPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (triggerStore, bool) {
	if !requireRole(w, r, role) {
		return nil, false
	}
	store := s.triggerSource()
	if store == nil {
		jsonError(w, triggersNeedPG, http.StatusServiceUnavailable)
		return nil, false
	}
	return store, true
}

type triggerRequest struct {
	Namespace       string `json:"namespace"`
	Agent           string `json:"agent"`
	Name            string `json:"name"`
	InputPath       string `json:"inputPath"`
	RateLimitPerMin int    `json:"rateLimitPerMin"`
}

// triggerCreated is the body of POST /api/triggers: the only time the secret is shown.
type triggerCreated struct {
	Trigger  postgresql.Trigger `json:"trigger"`
	Secret   string             `json:"secret"`
	HookPath string             `json:"hookPath"`
}

func (s *UIServer) handleTriggers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createTrigger(w, r)
	case http.MethodGet:
		s.listTriggers(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) handleTrigger(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		s.patchTrigger(w, r)
	case http.MethodDelete:
		s.deleteTrigger(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) createTrigger(w http.ResponseWriter, r *http.Request) {
	store, ok := s.triggerPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req triggerRequest
	if !readJSONBody(w, r, maxTriggerBody, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
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
	reason, err := s.notPublishedReason(r.Context(), agent)
	if err != nil {
		jsonError(w, "checking the agent failed", http.StatusInternalServerError)
		return
	}
	if reason != "" {
		jsonError(w, "the agent is not published, so a trigger cannot be added. "+reason, http.StatusConflict)
		return
	}
	if req.RateLimitPerMin == 0 {
		req.RateLimitPerMin = postgresql.DefaultTriggerRatePerMin
	}
	id, secret, err := postgresql.NewTriggerCredentials()
	if err != nil {
		jsonError(w, "creating credentials failed", http.StatusInternalServerError)
		return
	}
	t, err := store.CreateTrigger(pgScope(r.Context()), postgresql.Trigger{
		ID: id, Namespace: agent.Namespace, Agent: agent.Name, Name: req.Name, SigningKey: postgresql.SigningKeyFor(secret),
		InputPath: strings.TrimSpace(req.InputPath), RateLimitPerMin: req.RateLimitPerMin, CreatedBy: actorOf(r.Context()),
	})
	if err != nil {
		triggerFail(w, "creating the trigger", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, kindTrigger, t.Namespace, t.Agent, t.ID,
		fmt.Sprintf("created webhook trigger %q (%d requests per minute)", t.Name, t.RateLimitPerMin)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(triggerCreated{Trigger: t, Secret: secret, HookPath: "/hooks/" + t.ID})
}

func (s *UIServer) listTriggers(w http.ResponseWriter, r *http.Request) {
	store, ok := s.triggerPreflight(w, r, tenancy.RoleViewer)
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
			jsonResponse(w, []postgresql.Trigger{})
			return
		}
	}
	list, err := store.ListTriggers(pgScope(r.Context()), namespaces, r.URL.Query().Get("agent"))
	if err != nil {
		triggerFail(w, "listing triggers", err)
		return
	}
	jsonResponse(w, list)
}

// ownedTrigger loads a trigger of the caller's tenant in a namespace the caller may use.
func (s *UIServer) ownedTrigger(w http.ResponseWriter, r *http.Request, store triggerStore) (postgresql.Trigger, bool) {
	id := r.PathValue("id")
	if !postgresql.ValidTriggerID(id) {
		jsonError(w, "not found", http.StatusNotFound)
		return postgresql.Trigger{}, false
	}
	t, err := store.GetTrigger(pgScope(r.Context()), id)
	if err == nil && !namespaceAllowed(r.Context(), t.Namespace) {
		err = postgresql.ErrTriggerNotFound
	}
	if err != nil {
		triggerFail(w, "reading the trigger", err)
		return postgresql.Trigger{}, false
	}
	return t, true
}

func (s *UIServer) patchTrigger(w http.ResponseWriter, r *http.Request) {
	store, ok := s.triggerPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !readJSONBody(w, r, maxTriggerBody, &req) {
		return
	}
	if req.Enabled == nil {
		jsonError(w, "enabled is required", http.StatusBadRequest)
		return
	}
	cur, ok := s.ownedTrigger(w, r, store)
	if !ok {
		return
	}
	t, err := store.SetTriggerEnabled(pgScope(r.Context()), cur.ID, *req.Enabled)
	if err != nil {
		triggerFail(w, "updating the trigger", err)
		return
	}
	state := "disabled"
	if t.Enabled {
		state = "enabled"
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindTrigger, t.Namespace, t.Agent, t.ID, state+" webhook trigger "+fmt.Sprintf("%q", t.Name)))
	jsonResponse(w, t)
}

func (s *UIServer) deleteTrigger(w http.ResponseWriter, r *http.Request) {
	store, ok := s.triggerPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.ownedTrigger(w, r, store)
	if !ok {
		return
	}
	t, err := store.DeleteTrigger(pgScope(r.Context()), cur.ID)
	if err != nil {
		triggerFail(w, "deleting the trigger", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditDelete, kindTrigger, t.Namespace, t.Agent, t.ID, "deleted webhook trigger "+fmt.Sprintf("%q", t.Name)))
	w.WriteHeader(http.StatusNoContent)
}
