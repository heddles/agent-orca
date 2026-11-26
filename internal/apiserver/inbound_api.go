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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Inbound channel connections (docs/inbound-channels.md).
//
//	GET    /api/inbound-kinds                  viewer: the registered kinds, their settings schema and setup steps
//	POST   /api/inbound-connections            builder, audited: create
//	GET    /api/inbound-connections            viewer: ?namespace=&agent=
//	PATCH  /api/inbound-connections/{id}       builder, audited: {"enabled","config","secretRef"}
//	DELETE /api/inbound-connections/{id}       builder, audited
//	POST   /api/inbound-connections/{id}/test  builder: connects, reads counts, sends nothing
//	POST   /inbound/{kind}/{id}                signed by the channel, no session (inbound_receive.go)
//
// A connection is for a published agent. Its credentials are in a Secret an
// administrator labelled (inbound.SecretLabel), or were entered by an
// administrator and are stored encrypted (inbound_guided.go). The API never
// returns a secret value.

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
	InboundRefusals(ctx context.Context, connectionIDs []string, since time.Time) (map[string]map[string]int, error)
	SetInboundCredentials(ctx context.Context, id, keyID string, sealed []byte, config json.RawMessage) (postgresql.InboundConnection, error)
	RecordInboundRequest(ctx context.Context, id, authError string) error
	CreateInboundPairing(ctx context.Context, p postgresql.InboundPairing) error
	GetInboundPairing(ctx context.Context, connectionID, id string) (postgresql.InboundPairing, error)
	TakeInboundPairing(ctx context.Context, connectionID, id string) (postgresql.InboundPairing, error)
}

const schemeHTTPS = "https"

// refusalWindow is how far back a connection's refused messages are counted.
const refusalWindow = 24 * time.Hour

// SetInboundPublicURL sets the address channels reach this installation at, for
// example https://hooks.example.com. It is often not the one people open the UI
// with. Empty leaves the UI to use its own address.
func (s *UIServer) SetInboundPublicURL(raw string) error {
	base, err := parseInboundPublicURL(raw)
	if err != nil {
		return err
	}
	s.inboundPublicURL = base
	return nil
}

func parseInboundPublicURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("the inbound public URL must be like https://hooks.example.com, without a query or credentials")
	}
	return raw, nil
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
	mux.HandleFunc("/api/inbound-kinds", s.handleInboundKinds)
	mux.HandleFunc("/api/inbound-connections", s.handleInboundConnections)
	mux.HandleFunc("/api/inbound-connections/{id}", s.handleInboundConnection)
	mux.HandleFunc("/api/inbound-connections/{id}/test", s.handleInboundTest)
	mux.HandleFunc("/api/inbound-connections/{id}/credentials", s.handleInboundCredentials)
	mux.HandleFunc("/api/inbound-connections/{id}/pairings", s.handleInboundPairings)
	mux.HandleFunc("/api/inbound-connections/{id}/pairings/{pairing}", s.handleInboundPairing)
	mux.HandleFunc("/api/inbound-connections/{id}/pairings/{pairing}/approve", s.handleInboundPairingApprove)
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
	// URL is Path on the configured public address; empty when none is set.
	URL string `json:"url,omitempty"`
	// Refused counts the messages refused in the last 24 hours, by reason code.
	Refused map[string]int `json:"refused,omitempty"`
	// Credentials says where they are: "stored" (entered in the UI), "secret"
	// (a Kubernetes Secret) or "" (none yet). They are never returned.
	Credentials string `json:"credentials"`
	// CanStore: this installation can keep credentials entered in the UI.
	CanStore bool `json:"canStoreCredentials"`
	// SetupLink creates the platform's app with this channel's settings filled in.
	SetupLink string `json:"setupLink,omitempty"`
}

func (s *UIServer) inboundViewOf(c postgresql.InboundConnection) inboundView {
	v := inboundView{InboundConnection: c, Path: "/inbound/" + c.Kind + "/" + c.ID}
	if s.inboundPublicURL != "" {
		v.URL = s.inboundPublicURL + v.Path
	}
	switch {
	case len(c.CredentialsEnc) > 0:
		v.Credentials = credentialsStored
	case c.SecretRef != "":
		v.Credentials = credentialsSecret
	}
	v.CanStore = s.inboundKeys != nil
	// The app must be told where to send events, so the link needs the real
	// public address: a guess from the browser's address could be unreachable.
	if s.inboundPipe != nil && v.URL != "" {
		if a, ok := s.inboundPipe.Registry.Get(c.Kind); ok {
			if l, ok := a.(inbound.SetupLinker); ok {
				v.SetupLink = l.SetupLink(c.Name, v.URL)
			}
		}
	}
	return v
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
		err = inbound.CheckSettings(a, settings)
	}
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
func (s *UIServer) checkInboundSecret(w http.ResponseWriter, ctx context.Context, kind, ns, name string) bool {
	if name == "" || s.inboundPipe.Secrets == nil {
		return true
	}
	data, err := s.inboundPipe.Secrets(ctx, ns, name)
	if err != nil {
		jsonError(w, fmt.Sprintf("the Secret %q is not available to channels: it must exist in %s and an administrator must label it %s=true", name, ns, inbound.SecretLabel), http.StatusBadRequest)
		return false
	}
	if a, ok := s.inboundPipe.Registry.Get(kind); ok {
		if err := inbound.CheckSecret(a, data); err != nil {
			jsonError(w, fmt.Sprintf("the Secret %q is not usable for a %s channel: %s", name, kind, err), http.StatusBadRequest)
			return false
		}
	}
	return true
}

// handleInboundKinds lists what the registered adapters offer: the form of their
// own settings, the keys their Secret needs, their capabilities and the steps to
// set them up. It is what the Channels tab draws its form from.
func (s *UIServer) handleInboundKinds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.inboundPreflight(w, r, tenancy.RoleViewer); !ok {
		return
	}
	kinds := s.inboundPipe.Registry.Kinds()
	sort.Strings(kinds)
	out := make([]inbound.KindInfo, 0, len(kinds))
	for _, k := range kinds {
		if a, ok := s.inboundPipe.Registry.Get(k); ok {
			out = append(out, inbound.Info(a))
		}
	}
	jsonResponse(w, out)
}

// completeInboundConfig lets the adapter fill in settings it can look up with
// the credentials, and returns the config to store. Without credentials, or for
// an adapter that looks nothing up, cfg comes back unchanged.
func (s *UIServer) completeInboundConfig(
	w http.ResponseWriter, ctx context.Context, sc postgresql.InboundConnection,
) (json.RawMessage, bool) {
	a, _ := s.inboundPipe.Registry.Get(sc.Kind)
	completer, ok := a.(inbound.Completer)
	if !ok || (len(sc.CredentialsEnc) == 0 && (sc.SecretRef == "" || s.inboundPipe.Secrets == nil)) {
		return sc.Config, true
	}
	c, err := s.inboundPipe.Resolve(ctx, sc)
	if err != nil {
		jsonError(w, "the credentials could not be used: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	filled, err := completer.CompleteConfig(ctx, c)
	if err != nil {
		jsonError(w, "the credentials were not accepted: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	var whole map[string]json.RawMessage
	if len(sc.Config) > 0 {
		if err := json.Unmarshal(sc.Config, &whole); err != nil {
			jsonError(w, "config is not valid", http.StatusBadRequest)
			return nil, false
		}
	}
	if whole == nil {
		whole = map[string]json.RawMessage{}
	}
	whole["adapter"] = filled
	out, err := json.Marshal(whole)
	if err != nil {
		jsonError(w, "saving the settings failed", http.StatusInternalServerError)
		return nil, false
	}
	return out, true
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
	if !s.checkInboundSecret(w, r.Context(), req.Kind, agent.Namespace, req.SecretRef) {
		return
	}
	id, err := postgresql.NewInboundID()
	if err != nil {
		jsonError(w, "creating the id failed", http.StatusInternalServerError)
		return
	}
	draft := postgresql.InboundConnection{
		ID: id, Kind: req.Kind, Name: req.Name, Namespace: agent.Namespace, Agent: agent.Name,
		Config: req.Config, SecretRef: strings.TrimSpace(req.SecretRef), CreatedBy: actorOf(r.Context()),
	}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		draft.TenantID = t.TenantID
	}
	if draft.Config, ok = s.completeInboundConfig(w, r.Context(), draft); !ok {
		return
	}
	c, err := store.CreateInboundConnection(pgScope(r.Context()), draft)
	if err != nil {
		inboundFail(w, "creating the connection", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, kindInbound, c.Namespace, c.Agent, c.ID,
		fmt.Sprintf("created %s channel %q", c.Kind, c.Name)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(s.inboundViewOf(c))
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
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	refused, err := store.InboundRefusals(pgScope(r.Context()), ids, time.Now().Add(-refusalWindow))
	if err != nil {
		// The list is still useful without the counts.
		slog.Warn("counting refused messages", "err", err)
	}
	out := make([]inboundView, 0, len(list))
	for _, c := range list {
		v := s.inboundViewOf(c)
		v.Refused = refused[c.ID]
		out = append(out, v)
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
		if !s.checkInboundSecret(w, r.Context(), cur.Kind, cur.Namespace, *req.SecretRef) {
			return
		}
	}
	if req.Config != nil || req.SecretRef != nil {
		next := cur
		if req.Config != nil {
			next.Config = *req.Config
		}
		if req.SecretRef != nil {
			next.SecretRef = *req.SecretRef
			if next.SecretRef != "" {
				next.CredentialsEnc, next.CredentialsKeyID = nil, ""
			}
		}
		filled, ok := s.completeInboundConfig(w, r.Context(), next)
		if !ok {
			return
		}
		if req.Config != nil || !bytes.Equal(filled, cur.Config) {
			req.Config = &filled
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
	jsonResponse(w, s.inboundViewOf(c))
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
