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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/oauth"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Per-user OAuth connections to MCP servers (docs/mcp-oauth.md).
//
//	GET    /api/connections                              the caller's connections only (viewer)
//	POST   /api/connections/{ns}/{server}/start          begin the flow, returns {authorizeUrl} (runner)
//	GET    /api/connections/callback                     the provider's redirect target (the session)
//	DELETE /api/connections/{ns}/{server}                disconnect; ?user= is the admin's revoke
//	GET    /api/mcpservers/{ns}/{name}/connections       who is connected, never a token (admin)
//	GET    /api/mcp-oauth-apps/{ns}/{server}             the registered app, without its secret (admin)
//	PUT    /api/mcp-oauth-apps/{ns}/{server}             register or rotate the app; the secret is write-only (admin)
//	GET    /api/me/connections/required?agent={ns}/{name} servers the agent uses that need a connection
//
// The identity of a connection is the signed-in session's tenant and user, never
// a value from the request. No response carries a token, a client secret or an
// authorization code, and nothing here logs one.

const (
	kindMCPConnection  = postgresql.AuditKindMCPConnection
	connNeedsPG        = "per-user connections need the PostgreSQL store and a connection key (docs/mcp-oauth.md)"
	connNeedsUser      = "per-user connections need a signed-in user"
	maxConnectionBody  = 4 << 10
	maxOAuthAppBody    = 16 << 10
	connectionCallback = "/api/connections/callback"
	connCookieSecure   = "__Host-agentorca_mcp_oauth"
	connCookiePlain    = "agentorca_mcp_oauth"
	kindMCPServer      = "MCPServer"
)

// connectionsBackend is the flow the handlers drive (*connections.Service).
type connectionsBackend interface {
	Start(ctx context.Context, req connections.StartRequest) (connections.Started, error)
	Complete(ctx context.Context, req connections.CompleteRequest) (postgresql.MCPConnection, error)
	Disconnect(ctx context.Context, tenantID, userID string, ref connections.ServerRef) (connections.DisconnectResult, error)
	SealClientSecret(tenantID, ns, appRef, secret string) (keyID string, blob []byte, err error)
}

// connectionStore is the slice of postgresql.Store the handlers read and write.
type connectionStore interface {
	ListConnections(ctx context.Context, userID string) ([]postgresql.MCPConnection, error)
	ListServerConnections(ctx context.Context, ns, server string) ([]postgresql.ConnectionSummary, error)
	GetConnection(ctx context.Context, userID, ns, server string) (postgresql.MCPConnection, error)
	UpsertOAuthApp(ctx context.Context, a postgresql.MCPOAuthApp) (postgresql.MCPOAuthApp, bool, error)
	GetOAuthApp(ctx context.Context, ns, appRef string) (postgresql.MCPOAuthApp, error)
}

// connectionsAPI is the wiring of the connection endpoints; nil on the UIServer
// means the feature is off (no database or no key).
type connectionsAPI struct {
	backend connectionsBackend
	store   connectionStore
	// cookieSecure marks the state cookie Secure and gives it the __Host- prefix.
	cookieSecure bool
}

// SetConnections turns the connection endpoints on.
func (s *UIServer) SetConnections(backend connectionsBackend, store connectionStore, cookieSecure bool) {
	s.conns = &connectionsAPI{backend: backend, store: store, cookieSecure: cookieSecure}
}

func (s *UIServer) registerConnectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/connections", s.handleListConnections)
	mux.HandleFunc(connectionCallback, s.handleConnectionCallback)
	mux.HandleFunc("/api/connections/{ns}/{server}/start", s.handleConnectionStart)
	mux.HandleFunc("/api/connections/{ns}/{server}", s.handleConnectionDelete)
	mux.HandleFunc("/api/mcpservers/{ns}/{name}/connections", s.handleServerConnections)
	mux.HandleFunc("/api/mcp-oauth-apps/{ns}/{server}", s.handleOAuthApp)
	mux.HandleFunc("/api/me/connections/required", s.handleConnectionsRequired)
}

// connectionView is a connection as the UI sees it: no token, no ciphertext.
type connectionView struct {
	Namespace    string     `json:"namespace"`
	Server       string     `json:"server"`
	Scopes       []string   `json:"scopes"`
	AccountLabel string     `json:"accountLabel,omitempty"`
	Status       string     `json:"status"`
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func connViewOf(c postgresql.MCPConnection) connectionView {
	return connectionView{Namespace: c.ServerNS, Server: c.ServerName, Scopes: c.Scopes, AccountLabel: c.AccountName,
		Status: c.Status, LastUsedAt: c.LastUsedAt, CreatedAt: c.CreatedAt}
}

// connIdentity returns the signed-in session's tenant and user, or answers 403.
func connIdentity(w http.ResponseWriter, r *http.Request) (tenantID, userID string, ok bool) {
	t, found := TenantFromContext(r.Context())
	if !found || t == nil || t.TenantID == "" || t.UserID == "" {
		jsonError(w, connNeedsUser, http.StatusForbidden)
		return "", "", false
	}
	return t.TenantID, t.UserID, true
}

// connPreflight checks the role and that the feature is on.
func (s *UIServer) connPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) bool {
	if !requireRole(w, r, role) {
		return false
	}
	if s.conns == nil {
		jsonError(w, connNeedsPG, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// connectableServer loads the MCPServer a connection is for and refuses what the
// caller may not touch (not its own, not platform-shared) or what does not use
// per-user connections. It answers on failure.
func (s *UIServer) connectableServer(w http.ResponseWriter, r *http.Request, ns, name string) (connections.ServerRef, bool) {
	srv, err := s.lookupConnectableServer(r.Context(), ns, name)
	switch {
	case errors.Is(err, errConnNotFound):
		jsonError(w, "not found", http.StatusNotFound)
	case errors.Is(err, errConnNotPerUser):
		jsonError(w, "this server does not use per-user connections", http.StatusConflict)
	case err != nil:
		jsonError(w, "reading the server failed", http.StatusInternalServerError)
	default:
		return serverRefOf(srv), true
	}
	return connections.ServerRef{}, false
}

var (
	errConnNotFound   = errors.New("server not found")
	errConnNotPerUser = errors.New("server is not per-user")
)

func (s *UIServer) lookupConnectableServer(ctx context.Context, ns, name string) (*agentorcav1alpha1.MCPServer, error) {
	var srv agentorcav1alpha1.MCPServer
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &srv); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, errConnNotFound
		}
		return nil, err
	}
	if !canRead(ctx, &srv, kindMCPServer) {
		return nil, errConnNotFound
	}
	if srv.Spec.Auth == nil || srv.Spec.Auth.OAuth == nil || !srv.Spec.Auth.OAuth.PerUser() {
		return nil, errConnNotPerUser
	}
	return &srv, nil
}

func serverRefOf(srv *agentorcav1alpha1.MCPServer) connections.ServerRef {
	o := srv.Spec.Auth.OAuth
	return connections.ServerRef{Namespace: srv.Namespace, Name: srv.Name, UID: string(srv.UID), URL: srv.Spec.URL,
		AppRef: o.AppRef, UserScopes: o.UserScopes}
}

func (s *UIServer) handleListConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.connPreflight(w, r, tenancy.RoleViewer) {
		return
	}
	_, user, ok := connIdentity(w, r)
	if !ok {
		return
	}
	list, err := s.conns.store.ListConnections(pgScope(r.Context()), user)
	if err != nil {
		slog.Error("listing connections failed", "err", err)
		jsonError(w, "listing connections failed", http.StatusInternalServerError)
		return
	}
	out := make([]connectionView, 0, len(list))
	for _, c := range list {
		out = append(out, connViewOf(c))
	}
	jsonResponse(w, out)
}

// connectionCookie is the name of the cookie that carries the state across the
// provider redirect. __Host- needs Secure, so plain-HTTP local dev goes without.
func (c *connectionsAPI) cookieName() string {
	if c.cookieSecure {
		return connCookieSecure
	}
	return connCookiePlain
}

func (c *connectionsAPI) setStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{Name: c.cookieName(), Value: state, Path: "/", HttpOnly: true, Secure: c.cookieSecure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(connections.FlowTTL / time.Second)})
}

func (c *connectionsAPI) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: c.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: c.cookieSecure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// connectionError maps a flow error to an answer. The text shown is one of ours,
// never the provider's, and never carries a token or code.
func connectionError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, connections.ErrNotConfigured):
		jsonError(w, "an admin has not registered the OAuth app for this server yet", http.StatusConflict)
	case errors.Is(err, connections.ErrNoPKCE), errors.Is(err, connections.ErrInsecureOAuth):
		jsonError(w, err.Error(), http.StatusBadGateway)
	case errors.Is(err, connections.ErrScopesExceeded):
		jsonError(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, connections.ErrNoKeys):
		jsonError(w, connNeedsPG, http.StatusServiceUnavailable)
	case errors.Is(err, postgresql.ErrNoTenantScope):
		jsonError(w, connNeedsUser, http.StatusForbidden)
	case errors.Is(err, postgresql.ErrConnectionNotFound):
		jsonError(w, "not found", http.StatusNotFound)
	default:
		slog.Error("connection request failed", "what", what, "err", err)
		jsonError(w, what+" failed", http.StatusBadGateway)
	}
}

type startRequest struct {
	Scopes []string `json:"scopes"`
}

func (s *UIServer) handleConnectionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.connPreflight(w, r, tenancy.RoleRunner) {
		return
	}
	tenant, user, ok := connIdentity(w, r)
	if !ok {
		return
	}
	var body startRequest
	if r.ContentLength != 0 && !readJSONBody(w, r, maxConnectionBody, &body) {
		return
	}
	ref, ok := s.connectableServer(w, r, r.PathValue("ns"), r.PathValue("server"))
	if !ok {
		return
	}
	started, err := s.conns.backend.Start(pgScope(r.Context()), connections.StartRequest{TenantID: tenant, UserID: user, Server: ref, Scopes: body.Scopes})
	if err != nil {
		connectionError(w, "starting the connection", err)
		return
	}
	s.conns.setStateCookie(w, started.State.Reveal())
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]string{"authorizeUrl": started.AuthorizeURL})
}

// Callback outcomes, as the status the UI page shows. They are codes, not text.
const (
	connOutcomeConnected = "connected"
	connOutcomeDenied    = "denied"
	connOutcomeExpired   = "expired"
	connOutcomeFailed    = "failed"
	connOutcomeScopes    = "scopes"
)

func connectionRedirect(w http.ResponseWriter, r *http.Request, outcome, server string) {
	q := url.Values{"status": {outcome}}
	if server != "" {
		q.Set("server", server)
	}
	w.Header().Set("Cache-Control", "no-store")
	// Referrer-Policy keeps the callback URL (with code and state) out of any
	// Referer header of the page we land on.
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/connections?"+q.Encode(), http.StatusFound)
}

func (s *UIServer) handleConnectionCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.conns == nil {
		jsonError(w, connNeedsPG, http.StatusServiceUnavailable)
		return
	}
	// The state cookie is single use: clear it whatever happens next.
	cookie := ""
	if c, err := r.Cookie(s.conns.cookieName()); err == nil {
		cookie = c.Value
	}
	s.conns.clearStateCookie(w)
	tenant, user, ok := connIdentity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ctx := pgScope(r.Context())
	conn, err := s.conns.backend.Complete(ctx, connections.CompleteRequest{
		TenantID: tenant, UserID: user, State: q.Get("state"), CookieState: cookie, Code: q.Get("code"),
		Iss: q.Get("iss"), ProviderError: q.Get("error"), AccountLabel: user,
		Resolve: func(ctx context.Context, ns, name string) (connections.ServerRef, error) {
			srv, err := s.lookupConnectableServer(withIdentityOf(ctx, r), ns, name)
			if err != nil {
				return connections.ServerRef{}, connections.ErrBadState
			}
			return serverRefOf(srv), nil
		},
	})
	if err != nil {
		connectionRedirect(w, r, outcomeOf(err), "")
		return
	}
	server := conn.ServerNS + "/" + conn.ServerName
	s.recordAudit(w, r, auditEventFor(postgresql.AuditConnect, kindMCPConnection, conn.ServerNS, conn.ServerName, "",
		fmt.Sprintf("connected account %q with scopes %s", conn.AccountName, scopeList(conn.Scopes))))
	connectionRedirect(w, r, connOutcomeConnected, server)
}

// withIdentityOf carries the request's identity into ctx, so ownership checks that
// read it work inside the flow's callbacks.
func withIdentityOf(ctx context.Context, r *http.Request) context.Context {
	if v := r.Context().Value(tenantIdentityKey); v != nil {
		return context.WithValue(ctx, tenantIdentityKey, v)
	}
	return ctx
}

func outcomeOf(err error) string {
	switch {
	case errors.Is(err, connections.ErrProviderDenied):
		return connOutcomeDenied
	case errors.Is(err, connections.ErrBadState):
		return connOutcomeExpired
	case errors.Is(err, connections.ErrScopesExceeded):
		return connOutcomeScopes
	}
	if !errors.Is(err, connections.ErrStateMismatch) && !errors.Is(err, connections.ErrIssuerMismatch) && !oauth.IsInvalidGrant(err) {
		slog.Error("completing a connection failed", "err", err)
	}
	return connOutcomeFailed
}

func scopeList(s []string) string {
	if len(s) == 0 {
		return "(none reported)"
	}
	return strings.Join(s, " ")
}

func (s *UIServer) handleConnectionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.connPreflight(w, r, tenancy.RoleRunner) {
		return
	}
	tenant, me, ok := connIdentity(w, r)
	if !ok {
		return
	}
	target, action := me, postgresql.AuditDisconnect
	if u := r.URL.Query().Get("user"); u != "" && u != me {
		// Revoking someone else's connection is the admin's one action on it.
		if !requireRole(w, r, tenancy.RoleAdmin) {
			return
		}
		target, action = u, postgresql.AuditRevoke
	}
	ns, name := r.PathValue("ns"), r.PathValue("server")
	ref := connections.ServerRef{Namespace: ns, Name: name}
	// The server may be gone or no longer per-user; the owner can still disconnect.
	if srv, err := s.lookupConnectableServer(r.Context(), ns, name); err == nil {
		ref = serverRefOf(srv)
		if o := srv.Spec.Auth.OAuth; o.RevokeOnDisconnect != nil && !*o.RevokeOnDisconnect {
			ref.URL = "" // no provider revocation
		}
	} else if !namespaceAllowed(r.Context(), ns) && !errors.Is(err, errConnNotPerUser) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	res, err := s.conns.backend.Disconnect(pgScope(r.Context()), tenant, target, ref)
	if err != nil {
		connectionError(w, "disconnecting", err)
		return
	}
	outcome := "provider revocation not attempted"
	switch {
	case res.Revoked:
		outcome = "revoked at the provider"
	case res.RevokeError != nil:
		outcome = "provider revocation failed"
		slog.Warn("revoking a connection at the provider failed", "server", ns+"/"+name, "err", res.RevokeError)
	}
	summary := fmt.Sprintf("disconnected account %q (%s)", res.Connection.AccountName, outcome)
	if action == postgresql.AuditRevoke {
		summary = fmt.Sprintf("revoked the connection of user %q (%s)", target, outcome)
	}
	s.recordAudit(w, r, auditEventFor(action, kindMCPConnection, ns, name, "", summary))
	w.WriteHeader(http.StatusNoContent)
}

func (s *UIServer) handleServerConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.connPreflight(w, r, tenancy.RoleAdmin) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if _, ok := s.connectableServer(w, r, ns, name); !ok {
		return
	}
	list, err := s.conns.store.ListServerConnections(pgScope(r.Context()), ns, name)
	if err != nil {
		jsonError(w, "listing connections failed", http.StatusInternalServerError)
		return
	}
	counts := map[string]int{}
	for _, c := range list {
		counts[c.Status]++
	}
	jsonResponse(w, map[string]any{"connections": list, "counts": counts})
}

// oauthAppRequest is the body of PUT /api/mcp-oauth-apps/{ns}/{server}.
type oauthAppRequest struct {
	ClientID string `json:"clientId"`
	// ClientSecret is write-only. Empty on a rotation keeps the stored secret.
	ClientSecret  string   `json:"clientSecret"`
	ScopesAllowed []string `json:"scopesAllowed"`
}

type oauthAppView struct {
	Namespace     string    `json:"namespace"`
	AppRef        string    `json:"appRef"`
	Registered    bool      `json:"registered"`
	ClientID      string    `json:"clientId,omitempty"`
	HasSecret     bool      `json:"hasClientSecret"`
	RedirectURI   string    `json:"redirectUri"`
	ScopesAllowed []string  `json:"scopesAllowed"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
}

func (s *UIServer) handleOAuthApp(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPut:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.connPreflight(w, r, tenancy.RoleAdmin) {
		return
	}
	tenant, _, ok := connIdentity(w, r)
	if !ok {
		return
	}
	ns := r.PathValue("ns")
	ref, ok := s.connectableServer(w, r, ns, r.PathValue("server"))
	if !ok {
		return
	}
	redirect := s.conns.redirectURI()
	if r.Method == http.MethodGet {
		app, err := s.conns.store.GetOAuthApp(pgScope(r.Context()), ns, ref.AppRef)
		switch {
		case errors.Is(err, postgresql.ErrConnectionNotFound):
			jsonResponse(w, oauthAppView{Namespace: ns, AppRef: ref.AppRef, RedirectURI: redirect, ScopesAllowed: []string{}})
		case err != nil:
			jsonError(w, "reading the app failed", http.StatusInternalServerError)
		default:
			jsonResponse(w, appViewOf(app, redirect))
		}
		return
	}
	var req oauthAppRequest
	if !readJSONBody(w, r, maxOAuthAppBody, &req) {
		return
	}
	req.ClientID = strings.TrimSpace(req.ClientID)
	if req.ClientID == "" || len(req.ClientID) > 512 || len(req.ClientSecret) > 4096 {
		jsonError(w, "clientId is required (at most 512 characters); clientSecret at most 4096", http.StatusBadRequest)
		return
	}
	app := postgresql.MCPOAuthApp{ServerNS: ns, AppRef: ref.AppRef, ClientID: req.ClientID, RedirectURI: redirect,
		ScopesAllowed: req.ScopesAllowed, UpdatedBy: actorOf(r.Context())}
	if app.ScopesAllowed == nil {
		app.ScopesAllowed = []string{}
	}
	if req.ClientSecret != "" {
		keyID, blob, err := s.conns.backend.SealClientSecret(tenant, ns, ref.AppRef, req.ClientSecret)
		if err != nil {
			connectionError(w, "storing the app", err)
			return
		}
		app.KeyID, app.ClientSecretEnc = keyID, blob
	} else {
		app.KeyID = "none" // overwritten by the stored key id when the secret is kept
	}
	stored, created, err := s.conns.store.UpsertOAuthApp(pgScope(r.Context()), app)
	if err != nil {
		if errors.Is(err, postgresql.ErrInvalidConnection) {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		connectionError(w, "storing the app", err)
		return
	}
	action, verb := postgresql.AuditAppRotate, "updated"
	if created {
		action, verb = postgresql.AuditAppRegister, "registered"
	}
	secretNote := "client secret unchanged"
	if req.ClientSecret != "" {
		secretNote = "new client secret stored"
	}
	s.recordAudit(w, r, auditEventFor(action, kindMCPConnection, ns, ref.Name, "",
		fmt.Sprintf("%s OAuth app %q (client id %q, %s)", verb, ref.AppRef, req.ClientID, secretNote)))
	jsonResponse(w, appViewOf(stored, redirect))
}

func appViewOf(a postgresql.MCPOAuthApp, redirect string) oauthAppView {
	return oauthAppView{Namespace: a.ServerNS, AppRef: a.AppRef, Registered: true, ClientID: a.ClientID, HasSecret: a.HasSecret,
		RedirectURI: redirect, ScopesAllowed: a.ScopesAllowed, UpdatedBy: a.UpdatedBy, UpdatedAt: a.UpdatedAt}
}

// redirectURI is the fixed callback path shown to admins to register in the
// provider's app. The absolute URL is the UI's public origin plus this path; the
// operator does not know the origin, so the Service holds the full value.
func (c *connectionsAPI) redirectURI() string {
	if rp, ok := c.backend.(interface{ CallbackURL() string }); ok {
		return rp.CallbackURL()
	}
	return connectionCallback
}

// requiredServer is one MCP server an agent uses that has a per-user connection.
type requiredServer struct {
	Namespace string `json:"namespace"`
	Server    string `json:"server"`
	Mode      string `json:"mode"`
	// Connected is whether the caller has an active connection to it.
	Connected bool `json:"connected"`
}

func (s *UIServer) handleConnectionsRequired(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.connPreflight(w, r, tenancy.RoleRunner) {
		return
	}
	_, user, ok := connIdentity(w, r)
	if !ok {
		return
	}
	ns, name, found := strings.Cut(r.URL.Query().Get("agent"), "/")
	if !found || ns == "" || name == "" {
		jsonError(w, "agent must be namespace/name", http.StatusBadRequest)
		return
	}
	agent, ok := s.getAgentFor(w, r, ns, name)
	if !ok {
		return
	}
	servers, err := s.agentMCPServers(r.Context(), agent)
	if err != nil {
		jsonError(w, "reading the agent's servers failed", http.StatusInternalServerError)
		return
	}
	out := struct {
		Servers  []requiredServer `json:"servers"`
		Required []requiredServer `json:"required"`
	}{Servers: []requiredServer{}, Required: []requiredServer{}}
	for _, srv := range servers {
		o := srv.Spec.Auth.OAuth
		if o == nil || !o.PerUser() {
			continue
		}
		rs := requiredServer{Namespace: srv.Namespace, Server: srv.Name, Mode: o.Mode()}
		c, err := s.conns.store.GetConnection(pgScope(r.Context()), user, srv.Namespace, srv.Name)
		rs.Connected = err == nil && c.Status == postgresql.ConnectionActive && c.ServerUID == string(srv.UID)
		out.Servers = append(out.Servers, rs)
		// Only a mode that uses the caller's own account needs the caller to connect:
		// in maintainer mode the maintainer's connection is used, not the caller's.
		if o.Mode() == "invoker" && !rs.Connected {
			out.Required = append(out.Required, rs)
		}
	}
	jsonResponse(w, out)
}

// agentMCPServers returns the MCPServers behind an agent's tools: each Tool of
// the agent that an MCPServer owns.
func (s *UIServer) agentMCPServers(ctx context.Context, agent *agentorcav1alpha1.Agent) ([]*agentorcav1alpha1.MCPServer, error) {
	seen := map[string]bool{}
	var out []*agentorcav1alpha1.MCPServer
	for _, toolName := range agent.Spec.Tools {
		var tool agentorcav1alpha1.Tool
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: toolName}, &tool); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return nil, err
		}
		for _, ref := range tool.OwnerReferences {
			if ref.Kind != kindMCPServer || seen[ref.Name] {
				continue
			}
			seen[ref.Name] = true
			var srv agentorcav1alpha1.MCPServer
			if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: ref.Name}, &srv); err != nil {
				if client.IgnoreNotFound(err) == nil {
					continue
				}
				return nil, err
			}
			if srv.Spec.Auth != nil && srv.Spec.Auth.OAuth != nil {
				out = append(out, &srv)
			}
		}
	}
	slices.SortFunc(out, func(a, b *agentorcav1alpha1.MCPServer) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// maintainerServers returns the maintainer-mode MCP servers the agent uses.
func (s *UIServer) maintainerServers(ctx context.Context, agent *agentorcav1alpha1.Agent) ([]*agentorcav1alpha1.MCPServer, error) {
	all, err := s.agentMCPServers(ctx, agent)
	if err != nil {
		return nil, err
	}
	var out []*agentorcav1alpha1.MCPServer
	for _, srv := range all {
		if srv.Spec.Auth.OAuth.Mode() == agentorcav1alpha1.MCPConnectionMaintainer {
			out = append(out, srv)
		}
	}
	return out, nil
}

// gateMaintainerConnections refuses to publish an agent that uses a maintainer-mode
// server unless the publisher has an active connection to it: the agent will act as
// the publisher, so the publisher must have connected and so consented to that. It
// answers on refusal.
func (s *UIServer) gateMaintainerConnections(w http.ResponseWriter, r *http.Request, agent *agentorcav1alpha1.Agent) bool {
	servers, err := s.maintainerServers(r.Context(), agent)
	if err != nil {
		jsonError(w, "reading the agent's servers failed", http.StatusInternalServerError)
		return false
	}
	if len(servers) == 0 {
		return true
	}
	if s.conns == nil {
		jsonError(w, connNeedsPG, http.StatusServiceUnavailable)
		return false
	}
	_, user, ok := connIdentity(w, r)
	if !ok {
		return false
	}
	var missing []string
	for _, srv := range servers {
		c, err := s.conns.store.GetConnection(pgScope(r.Context()), user, srv.Namespace, srv.Name)
		if err != nil || c.Status != postgresql.ConnectionActive || c.ServerUID != string(srv.UID) {
			missing = append(missing, srv.Name)
		}
	}
	if len(missing) > 0 {
		jsonError(w, fmt.Sprintf("this agent acts as its publisher on %s: connect your own account first (Connections), then publish", strings.Join(missing, ", ")), http.StatusConflict)
		return false
	}
	return true
}

// recordPublisher copies the publisher into Agent.status.publishedBy when the agent
// uses a maintainer-mode server: that is whose account its runs use. It answers 500
// when it cannot, because a published agent with no recorded maintainer would have
// no usable connection.
func (s *UIServer) recordPublisher(w http.ResponseWriter, r *http.Request, agent *agentorcav1alpha1.Agent) bool {
	servers, err := s.maintainerServers(r.Context(), agent)
	if err != nil {
		jsonError(w, "reading the agent's servers failed", http.StatusInternalServerError)
		return false
	}
	if len(servers) == 0 {
		return true
	}
	orig := agent.DeepCopy()
	now := metav1.Now()
	agent.Status.PublishedBy, agent.Status.PublishedAt = actorOf(r.Context()), &now
	if err := s.crdClient.Status().Patch(r.Context(), agent, client.MergeFrom(orig)); err != nil {
		slog.Error("recording the publisher failed", "agent", agent.Namespace+"/"+agent.Name, "err", err)
		jsonError(w, "the agent was published but its maintainer could not be recorded; publish again", http.StatusInternalServerError)
		return false
	}
	return true
}
