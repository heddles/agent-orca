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

package connections

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/oauth"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// FlowTTL is how long a pending authorization request lives.
const FlowTTL = 5 * time.Minute

// Errors of the flow. Messages are safe to show to the user: none carries a
// token, a code or a state.
var (
	ErrNotConfigured   = errors.New("the OAuth app for this server is not registered")
	ErrNoPKCE          = errors.New("the server's authorization service does not support PKCE S256")
	ErrInsecureOAuth   = errors.New("the server's authorization endpoints must use https")
	ErrBadState        = errors.New("the connection request is unknown, expired or already used")
	ErrStateMismatch   = errors.New("the connection request does not belong to this session")
	ErrIssuerMismatch  = errors.New("the authorization response came from an unexpected issuer")
	ErrProviderDenied  = errors.New("the provider did not grant access")
	ErrNoRefreshToken  = errors.New("the provider returned no refresh token; request offline access for the app")
	ErrScopesExceeded  = errors.New("the provider granted more access than the administrator allows for this server")
	ErrConnectionInUse = errors.New("the connection cannot be used")
)

// Store is the slice of postgresql.Store the flow uses.
type Store interface {
	CreateOAuthFlow(ctx context.Context, stateHash string, f postgresql.MCPOAuthFlow) error
	ConsumeOAuthFlow(ctx context.Context, stateHash string) (postgresql.MCPOAuthFlow, error)
	GetOAuthApp(ctx context.Context, ns, appRef string) (postgresql.MCPOAuthApp, error)
	UpsertConnection(ctx context.Context, c postgresql.MCPConnection) (postgresql.MCPConnection, error)
	GetConnection(ctx context.Context, userID, ns, server string) (postgresql.MCPConnection, error)
	DeleteConnection(ctx context.Context, userID, ns, server string) (postgresql.MCPConnection, error)
	SetConnectionStatus(ctx context.Context, userID, ns, server, status string) error
	WithConnectionLock(ctx context.Context, userID, ns, server string, fn func(postgresql.MCPConnection) (*postgresql.MCPConnection, error)) error
}

// Service runs the connection flows. The zero value is not usable.
type Service struct {
	Store Store
	Keys  *Keyring
	OAuth *oauth.TokenManager
	// RedirectURI is the fixed callback of this deployment, registered in every
	// OAuth app. It is never taken from a request.
	RedirectURI string
	// AllowInsecure accepts http authorization endpoints (tests, local dev).
	AllowInsecure bool
	Now           func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ServerRef identifies the MCP server a connection is for.
type ServerRef struct {
	Namespace string
	Name      string
	UID       string
	URL       string
	// AppRef names the registered OAuth app.
	AppRef string
	// UserScopes is the admin's upper bound on what a user may grant; empty means
	// no bound beyond the app's allowed scopes.
	UserScopes []string
}

// ResourceIndicator is the RFC 8707 resource of an MCP server URL: its canonical
// form (lower-case scheme and host, no fragment, no trailing slash on an empty path).
func ResourceIndicator(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	u.Scheme, u.Host, u.Fragment, u.User = strings.ToLower(u.Scheme), strings.ToLower(u.Host), "", nil
	return strings.TrimSuffix(u.String(), "/")
}

func (s *Service) app(ctx context.Context, ref ServerRef, tenantID string) (postgresql.MCPOAuthApp, string, error) {
	if ref.AppRef == "" {
		return postgresql.MCPOAuthApp{}, "", ErrNotConfigured
	}
	app, err := s.Store.GetOAuthApp(ctx, ref.Namespace, ref.AppRef)
	if errors.Is(err, postgresql.ErrConnectionNotFound) {
		return postgresql.MCPOAuthApp{}, "", ErrNotConfigured
	}
	if err != nil {
		return postgresql.MCPOAuthApp{}, "", err
	}
	var secret []byte
	if len(app.ClientSecretEnc) > 0 {
		secret, err = s.Keys.Open(app.KeyID, app.ClientSecretEnc, AAD("client_secret", tenantID, app.ServerNS, app.AppRef))
		if err != nil {
			return postgresql.MCPOAuthApp{}, "", fmt.Errorf("opening the client secret: %w", err)
		}
	}
	return app, string(secret), nil
}

// SealClientSecret encrypts an OAuth app's client secret for storage.
func (s *Service) SealClientSecret(tenantID, ns, appRef, secret string) (keyID string, blob []byte, err error) {
	if s.Keys == nil {
		return "", nil, ErrNoKeys
	}
	return s.Keys.Seal([]byte(secret), AAD("client_secret", tenantID, ns, appRef))
}

func (s *Service) discover(ctx context.Context, serverURL string) (*oauth.Metadata, error) {
	meta, err := s.OAuth.Discover(ctx, serverURL)
	if err != nil {
		return nil, fmt.Errorf("discovering the server's authorization service: %w", err)
	}
	if !s.AllowInsecure {
		for _, e := range []string{meta.AuthorizationEndpoint, meta.TokenEndpoint, meta.RevocationEndpoint} {
			if e != "" && !strings.HasPrefix(e, "https://") {
				return nil, ErrInsecureOAuth
			}
		}
	}
	if !slices.Contains(meta.CodeChallengeMethods, "S256") {
		return nil, ErrNoPKCE
	}
	return meta, nil
}

// StartRequest begins a connection.
type StartRequest struct {
	TenantID string
	UserID   string
	Server   ServerRef
	// Scopes are the scopes asked for; empty takes the app's allowed scopes.
	Scopes []string
}

// Started is the answer to Start: where to send the browser, and the state value
// the handler also sets as a cookie.
type Started struct {
	AuthorizeURL string
	State        Secret
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Start discovers the server's authorization service, stores a pending flow
// (5 minutes, in PostgreSQL so any replica can finish it) and returns the URL to
// send the user to.
func (s *Service) Start(ctx context.Context, req StartRequest) (Started, error) {
	if s.Keys == nil {
		return Started{}, ErrNoKeys
	}
	app, secret, err := s.app(ctx, req.Server, req.TenantID)
	if err != nil {
		return Started{}, err
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = app.ScopesAllowed
	}
	if bound := boundScopes(req.Server.UserScopes, app.ScopesAllowed); len(bound) > 0 {
		for _, sc := range scopes {
			if !slices.Contains(bound, sc) {
				return Started{}, fmt.Errorf("%w: scope %q", ErrScopesExceeded, sc)
			}
		}
	}
	meta, err := s.discover(ctx, req.Server.URL)
	if err != nil {
		return Started{}, err
	}
	state, err := randomState()
	if err != nil {
		return Started{}, err
	}
	cfg := &oauth.Config{
		ServerURL: req.Server.URL, ClientID: app.ClientID, ClientSecret: secret,
		RedirectURI: s.RedirectURI, Scopes: scopes, State: state, Resource: ResourceIndicator(req.Server.URL),
	}
	authURL, verifier, err := s.OAuth.LoginURL(meta, cfg)
	if err != nil {
		return Started{}, err
	}
	keyID, venc, err := s.Keys.Seal([]byte(verifier), AAD("verifier", req.TenantID, req.UserID, req.Server.UID))
	if err != nil {
		return Started{}, err
	}
	err = s.Store.CreateOAuthFlow(ctx, postgresql.StateHash(state), postgresql.MCPOAuthFlow{
		TenantID: req.TenantID, UserID: req.UserID, ServerNS: req.Server.Namespace, ServerName: req.Server.Name,
		ServerUID: req.Server.UID, VerifierEnc: venc, KeyID: keyID, RequestedScopes: scopes,
		ExpiresAt: s.now().Add(FlowTTL),
	})
	if err != nil {
		return Started{}, err
	}
	return Started{AuthorizeURL: authURL, State: Secret(state)}, nil
}

// boundScopes is the most a user may grant: the server's userScopes, narrowed by
// the app's allowed scopes when it lists any.
func boundScopes(userScopes, appScopes []string) []string {
	switch {
	case len(userScopes) == 0:
		return appScopes
	case len(appScopes) == 0:
		return userScopes
	}
	var out []string
	for _, s := range userScopes {
		if slices.Contains(appScopes, s) {
			out = append(out, s)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// CompleteRequest finishes a connection from the callback.
type CompleteRequest struct {
	// TenantID and UserID are the signed-in session's, never the request's.
	TenantID string
	UserID   string
	// State and Code come from the callback query; CookieState is the value of the
	// __Host- cookie set at Start; Iss is the RFC 9207 parameter when present;
	// ProviderError is the "error" parameter when the user denied access.
	State         string
	CookieState   string
	Code          string
	Iss           string
	ProviderError string
	// Resolve returns the current MCPServer for the stored namespace and name, so
	// the server's UID and URL are checked at completion, not trusted from the
	// pending row alone.
	Resolve func(ctx context.Context, ns, name string) (ServerRef, error)
	// AccountLabel is shown in the UI beside the connection.
	AccountLabel string
}

// Complete validates the callback and stores the connection. The checks, in
// order: a state exists and is single-use (it is deleted by being read); the
// cookie equals the state (CSRF); the signed-in session is the user and tenant
// the flow was minted for (login CSRF); the server still has the UID the flow
// was started for; the response issuer matches (RFC 9207); the provider returned
// a code, a refresh token and no more scope than allowed.
func (s *Service) Complete(ctx context.Context, req CompleteRequest) (postgresql.MCPConnection, error) {
	if s.Keys == nil {
		return postgresql.MCPConnection{}, ErrNoKeys
	}
	if req.State == "" {
		return postgresql.MCPConnection{}, ErrBadState
	}
	// Consume first, whatever else is wrong: a state that fails a check is burned.
	flow, err := s.Store.ConsumeOAuthFlow(ctx, postgresql.StateHash(req.State))
	if err != nil {
		if errors.Is(err, postgresql.ErrConnectionNotFound) {
			return postgresql.MCPConnection{}, ErrBadState
		}
		return postgresql.MCPConnection{}, err
	}
	if req.CookieState == "" || subtle.ConstantTimeCompare([]byte(req.CookieState), []byte(req.State)) != 1 {
		return postgresql.MCPConnection{}, ErrStateMismatch
	}
	if flow.TenantID != req.TenantID || flow.UserID != req.UserID {
		return postgresql.MCPConnection{}, ErrStateMismatch
	}
	if req.ProviderError != "" {
		return postgresql.MCPConnection{}, fmt.Errorf("%w (%s)", ErrProviderDenied, sanitizeCode(req.ProviderError))
	}
	if req.Code == "" {
		return postgresql.MCPConnection{}, ErrProviderDenied
	}
	ref, err := req.Resolve(ctx, flow.ServerNS, flow.ServerName)
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	if ref.UID != flow.ServerUID {
		return postgresql.MCPConnection{}, ErrBadState
	}
	app, secret, err := s.app(ctx, ref, req.TenantID)
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	meta, err := s.discover(ctx, ref.URL)
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	if req.Iss != "" && req.Iss != meta.Issuer {
		return postgresql.MCPConnection{}, ErrIssuerMismatch
	}
	if req.Iss == "" && meta.IssParameterSupported {
		return postgresql.MCPConnection{}, ErrIssuerMismatch
	}
	verifier, err := s.Keys.Open(flow.KeyID, flow.VerifierEnc, AAD("verifier", flow.TenantID, flow.UserID, flow.ServerUID))
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	cfg := &oauth.Config{
		ServerURL: ref.URL, ClientID: app.ClientID, ClientSecret: secret,
		RedirectURI: s.RedirectURI, Resource: ResourceIndicator(ref.URL),
	}
	tr, err := s.OAuth.ExchangeCode(ctx, meta, cfg, req.Code, string(verifier))
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	granted := flow.RequestedScopes
	if tr.Scope != "" {
		granted = strings.Fields(tr.Scope)
	}
	if bound := boundScopes(ref.UserScopes, app.ScopesAllowed); len(bound) > 0 {
		for _, g := range granted {
			if !slices.Contains(bound, g) {
				s.revokeQuietly(ctx, meta, cfg, tr)
				return postgresql.MCPConnection{}, fmt.Errorf("%w: scope %q", ErrScopesExceeded, g)
			}
		}
	}
	if tr.RefreshToken == "" {
		s.revokeQuietly(ctx, meta, cfg, tr)
		return postgresql.MCPConnection{}, ErrNoRefreshToken
	}
	conn := postgresql.MCPConnection{
		TenantID: req.TenantID, UserID: req.UserID, ServerNS: flow.ServerNS, ServerName: flow.ServerName,
		ServerUID: flow.ServerUID, Scopes: granted, AccountName: req.AccountLabel,
	}
	if err := s.sealTokens(&conn, tr); err != nil {
		return postgresql.MCPConnection{}, err
	}
	stored, err := s.Store.UpsertConnection(ctx, conn)
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	return stored, nil
}

// sealTokens encrypts the tokens of a token response onto conn.
func (s *Service) sealTokens(conn *postgresql.MCPConnection, tr *oauth.TokenResponse) error {
	keyID, renc, err := s.Keys.Seal([]byte(tr.RefreshToken), AAD("refresh", conn.TenantID, conn.UserID, conn.ServerUID))
	if err != nil {
		return err
	}
	conn.RefreshTokenEnc, conn.KeyID = renc, keyID
	_, aenc, err := s.sealAccess(keyID, tr.AccessToken, conn)
	if err != nil {
		return err
	}
	conn.AccessTokenEnc = aenc
	conn.AccessExpiresAt = nil
	if tr.ExpiresIn > 0 {
		t := s.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
		conn.AccessExpiresAt = &t
	}
	return nil
}

// sealAccess seals an access token under the same key id as the row's refresh
// token (one key id per row), which is the active key.
func (s *Service) sealAccess(_ string, token string, conn *postgresql.MCPConnection) (string, []byte, error) {
	if token == "" {
		return "", nil, nil
	}
	return s.Keys.Seal([]byte(token), AAD("access", conn.TenantID, conn.UserID, conn.ServerUID))
}

func (s *Service) revokeQuietly(ctx context.Context, meta *oauth.Metadata, cfg *oauth.Config, tr *oauth.TokenResponse) {
	tok, hint := tr.RefreshToken, "refresh_token"
	if tok == "" {
		tok, hint = tr.AccessToken, "access_token"
	}
	if err := s.OAuth.Revoke(ctx, meta, cfg, tok, hint); err != nil {
		slog.Warn("could not revoke a refused token at the provider", "err", err)
	}
}

func sanitizeCode(c string) string {
	if len(c) > 64 {
		c = c[:64]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, c)
}

// DisconnectResult reports what Disconnect did at the provider.
type DisconnectResult struct {
	Connection postgresql.MCPConnection
	// Revoked is true when the provider confirmed the revocation. A false value
	// with RevokeError nil means the provider has no revocation endpoint.
	Revoked     bool
	RevokeError error
}

// Disconnect deletes a user's connection and asks the provider to revoke the
// refresh token (RFC 7009), best effort: the row is gone either way.
func (s *Service) Disconnect(ctx context.Context, tenantID, userID string, ref ServerRef) (DisconnectResult, error) {
	conn, err := s.Store.DeleteConnection(ctx, userID, ref.Namespace, ref.Name)
	if err != nil {
		return DisconnectResult{}, err
	}
	res := DisconnectResult{Connection: conn}
	if s.Keys == nil || ref.URL == "" || ref.UID != conn.ServerUID {
		return res, nil
	}
	rt, err := s.Keys.Open(conn.KeyID, conn.RefreshTokenEnc, AAD("refresh", conn.TenantID, conn.UserID, conn.ServerUID))
	if err != nil {
		res.RevokeError = err
		return res, nil
	}
	app, secret, err := s.app(ctx, ref, tenantID)
	if err != nil {
		res.RevokeError = err
		return res, nil
	}
	meta, err := s.OAuth.Discover(ctx, ref.URL)
	if err != nil {
		res.RevokeError = err
		return res, nil
	}
	err = s.OAuth.Revoke(ctx, meta, &oauth.Config{ClientID: app.ClientID, ClientSecret: secret}, string(rt), "refresh_token")
	switch {
	case err == nil:
		res.Revoked = true
	case errors.Is(err, oauth.ErrNoRevocationEndpoint):
	default:
		res.RevokeError = err
	}
	return res, nil
}

// CallbackURL is the fixed redirect URI of this deployment, which an admin
// registers in each OAuth app at the provider.
func (s *Service) CallbackURL() string { return s.RedirectURI }
