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

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Inbound channel connections (docs/inbound-channels.md).
//
//	POST   /api/inbound-connections            builder, audited: create
//	GET    /api/inbound-connections            viewer: ?namespace=&agent=
//	PATCH  /api/inbound-connections/{id}       builder, audited: {"enabled","config","secretRef"}
//	DELETE /api/inbound-connections/{id}       builder, audited
//	POST   /api/inbound-connections/{id}/test  builder: connects, reads counts, sends nothing
//	POST   /inbound/{kind}/{id}                signed by the channel, no session (inbound_receive.go)
//
// A connection is for a published agent. Its credentials are in a Secret an
// administrator labelled (inbound.SecretLabel); the API never accepts or returns
// a secret value.

const (
	kindInbound    = "InboundConnection"
	maxInboundBody = 32 << 10
	inboundNeedPG  = "inbound channels need the PostgreSQL store"
)

// inboundStore is the slice of postgresql.Store the connection endpoints use.
type inboundStore interface {
	CreateInboundConnection(ctx context.Context, c postgresql.InboundConnection) (postgresql.InboundConnection, error)
	ListInboundConnections(ctx context.Context, namespaces []string, agent string) ([]postgresql.InboundConnection, error)
	GetInboundConnection(ctx context.Context, id string) (postgresql.InboundConnection, error)
	UpdateInboundConnection(ctx context.Context, id string, p postgresql.InboundPatch) (postgresql.InboundConnection, error)
	DeleteInboundConnection(ctx context.Context, id string) (postgresql.InboundConnection, error)
}

// SetInbound enables the inbound channels: p is the pipeline the operator also
// runs its pollers with.
func (s *UIServer) SetInbound(p *inbound.Pipeline) { s.inboundPipe = p }

func (s *UIServer) inboundSource() inboundStore {
	if s.inboundConns != nil {
		return s.inboundConns
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) registerInboundRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/inbound-connections", s.handleInboundConnections)
	mux.HandleFunc("/api/inbound-connections/{id}", s.handleInboundConnection)
	mux.HandleFunc("/api/inbound-connections/{id}/test", s.handleInboundTest)
	mux.HandleFunc("/inbound/{kind}/{id}", s.handleInboundReceive)
}

func inboundFail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, postgresql.ErrInboundNotFound):
		jsonError(w, "not found", http.StatusNotFound)
	case errors.Is(err, postgresql.ErrInboundExists), errors.Is(err, postgresql.ErrTooManyInbound):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, postgresql.ErrInvalidInbound):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, postgresql.ErrNoTenantScope):
		jsonError(w, "inbound channels need a tenant identity", http.StatusForbidden)
	default:
		jsonError(w, what+" failed", http.StatusInternalServerError)
	}
}

func (s *UIServer) inboundPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (inboundStore, bool) {
	if !requireRole(w, r, role) {
		return nil, false
	}
	store := s.inboundSource()
	if store == nil || s.inboundPipe == nil {
		jsonError(w, inboundNeedPG, http.StatusServiceUnavailable)
		return nil, false
	}
	return store, true
}

type inboundRequest struct {
	Namespace string          `json:"namespace"`
	Agent     string          `json:"agent"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	SecretRef string          `json:"secretRef"`
	Config    json.RawMessage `json:"config"`
}

// inboundView is a connection as a browser sees it.
type inboundView struct {
	postgresql.InboundConnection
	// Path is where a channel that calls us posts: the address to give the provider.
	Path string `json:"path"`
}

func inboundViewOf(c postgresql.InboundConnection) inboundView {
	return inboundView{InboundConnection: c, Path: "/inbound/" + c.Kind + "/" + c.ID}
}

func (s *UIServer) handleInboundConnections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createInbound(w, r)
	case http.MethodGet:
		s.listInbound(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) handleInboundConnection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		s.patchInbound(w, r)
	case http.MethodDelete:
		s.deleteInbound(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// checkInboundConfig validates the settings against the adapter of kind and
// returns the adapter, or writes the answer.
func (s *UIServer) checkInboundConfig(w http.ResponseWriter, kind string, cfg json.RawMessage) bool {
	a, ok := s.inboundPipe.Registry.Get(kind)
	if !ok {
		jsonError(w, fmt.Sprintf("unknown kind %q (known: %s)", kind, strings.Join(s.inboundPipe.Registry.Kinds(), ", ")), http.StatusBadRequest)
		return false
	}
	settings, err := inbound.ParseSettings(cfg)
	if err == nil {
		err = a.ValidateConfig(settings.Adapter)
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// checkInboundSecret checks that the named Secret exists and carries the label
// that lets channels use it. The answer never says what is in it.
func (s *UIServer) checkInboundSecret(w http.ResponseWriter, ctx context.Context, ns, name string) bool {
	if name == "" || s.inboundPipe.Secrets == nil {
		return true
	}
	if _, err := s.inboundPipe.Secrets(ctx, ns, name); err != nil {
		jsonError(w, fmt.Sprintf("the Secret %q is not available to channels: it must exist in %s and an administrator must label it %s=true", name, ns, inbound.SecretLabel), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *UIServer) createInbound(w http.ResponseWriter, r *http.Request) {
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req inboundRequest
	if !readJSONBody(w, r, maxInboundBody, &req) {
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
	published, err := s.agentPublished(r.Context(), agent.Namespace, agent.Name)
	if err != nil {
		jsonError(w, "checking the agent failed", http.StatusInternalServerError)
		return
	}
	if !published {
		jsonError(w, "publish the agent before adding a channel", http.StatusConflict)
		return
	}
	if !s.checkInboundConfig(w, req.Kind, req.Config) {
		return
	}
	if !s.checkInboundSecret(w, r.Context(), agent.Namespace, req.SecretRef) {
		return
	}
	id, err := postgresql.NewInboundID()
	if err != nil {
		jsonError(w, "creating the id failed", http.StatusInternalServerError)
		return
	}
	c, err := store.CreateInboundConnection(pgScope(r.Context()), postgresql.InboundConnection{
		ID: id, Kind: req.Kind, Name: req.Name, Namespace: agent.Namespace, Agent: agent.Name,
		Config: req.Config, SecretRef: strings.TrimSpace(req.SecretRef), CreatedBy: actorOf(r.Context()),
	})
	if err != nil {
		inboundFail(w, "creating the connection", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, kindInbound, c.Namespace, c.Agent, c.ID,
		fmt.Sprintf("created %s channel %q", c.Kind, c.Name)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(inboundViewOf(c))
}

func (s *UIServer) listInbound(w http.ResponseWriter, r *http.Request) {
	store, ok := s.inboundPreflight(w, r, tenancy.RoleViewer)
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
			jsonResponse(w, []inboundView{})
			return
		}
	}
	list, err := store.ListInboundConnections(pgScope(r.Context()), namespaces, r.URL.Query().Get("agent"))
	if err != nil {
		inboundFail(w, "listing connections", err)
		return
	}
	out := make([]inboundView, 0, len(list))
	for _, c := range list {
		out = append(out, inboundViewOf(c))
	}
	jsonResponse(w, out)
}

// ownedInbound loads a connection of the caller's tenant in a namespace the caller may use.
func (s *UIServer) ownedInbound(w http.ResponseWriter, r *http.Request, store inboundStore) (postgresql.InboundConnection, bool) {
	id := r.PathValue("id")
	if !postgresql.ValidInboundID(id) {
		jsonError(w, "not found", http.StatusNotFound)
		return postgresql.InboundConnection{}, false
	}
	c, err := store.GetInboundConnection(pgScope(r.Context()), id)
	if err == nil && !namespaceAllowed(r.Context(), c.Namespace) {
		err = postgresql.ErrInboundNotFound
	}
	if err != nil {
		inboundFail(w, "reading the connection", err)
		return postgresql.InboundConnection{}, false
	}
	return c, true
}

func (s *UIServer) patchInbound(w http.ResponseWriter, r *http.Request) {
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req struct {
		Enabled   *bool            `json:"enabled"`
		Config    *json.RawMessage `json:"config"`
		SecretRef *string          `json:"secretRef"`
	}
	if !readJSONBody(w, r, maxInboundBody, &req) {
		return
	}
	if req.Enabled == nil && req.Config == nil && req.SecretRef == nil {
		jsonError(w, "nothing to change: send enabled, config or secretRef", http.StatusBadRequest)
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	if req.Config != nil {
		if !s.checkInboundConfig(w, cur.Kind, *req.Config) {
			return
		}
	}
	if req.SecretRef != nil {
		*req.SecretRef = strings.TrimSpace(*req.SecretRef)
		if !s.checkInboundSecret(w, r.Context(), cur.Namespace, *req.SecretRef) {
			return
		}
	}
	c, err := store.UpdateInboundConnection(pgScope(r.Context()), cur.ID, postgresql.InboundPatch{Enabled: req.Enabled, Config: req.Config, SecretRef: req.SecretRef})
	if err != nil {
		inboundFail(w, "updating the connection", err)
		return
	}
	what := "changed the settings of"
	if req.Enabled != nil && req.Config == nil && req.SecretRef == nil {
		what = "disabled"
		if c.Enabled {
			what = "enabled"
		}
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindInbound, c.Namespace, c.Agent, c.ID, fmt.Sprintf("%s %s channel %q", what, c.Kind, c.Name)))
	jsonResponse(w, inboundViewOf(c))
}

func (s *UIServer) deleteInbound(w http.ResponseWriter, r *http.Request) {
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	c, err := store.DeleteInboundConnection(pgScope(r.Context()), cur.ID)
	if err != nil {
		inboundFail(w, "deleting the connection", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditDelete, kindInbound, c.Namespace, c.Agent, c.ID, fmt.Sprintf("deleted %s channel %q", c.Kind, c.Name)))
	w.WriteHeader(http.StatusNoContent)
}

// handleInboundTest checks a connection without sending or changing anything.
func (s *UIServer) handleInboundTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.inboundPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.ownedInbound(w, r, store)
	if !ok {
		return
	}
	a, known := s.inboundPipe.Registry.Get(cur.Kind)
	t, canTest := a.(inbound.Tester)
	if !known || !canTest {
		jsonError(w, "this kind of channel cannot be tested", http.StatusConflict)
		return
	}
	c, err := s.inboundPipe.Resolve(r.Context(), cur)
	if err != nil {
		jsonResponse(w, inbound.TestReport{Error: err.Error()})
		return
	}
	jsonResponse(w, t.Test(r.Context(), c))
}
