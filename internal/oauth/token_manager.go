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

// Package oauth implements a generic, metadata-driven OAuth 2.0 credential
// manager for remote MCP servers. OAuth-only MCP servers (e.g. Slack's public MCP at
// https://mcp.slack.com/mcp) reject hand-pasted tokens with -32001 invalid_token
// because they only trust a bearer access token minted by their own OAuth flow. The
// model-router can only present a static bearer read from a mounted Secret, so this
// component obtains and refreshes OAuth access tokens (rfc8414 discovery +
// authorization_code/refresh_token grants) and writes them into the Secret that the
// MCPServer's auth.bearerToken already points at. The same logic is reused for any
// MCP server that publishes OAuth metadata — Slack is merely the first consumer.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SecretIO abstracts reading and writing Secret key/values so the manager is
// testable without a Kubernetes client. The Kubernetes implementation is k8sSecretIO.
type SecretIO interface {
	// ReadKey returns the value and whether the key exists.
	ReadKey(ctx context.Context, namespace, name, key string) ([]byte, bool, error)
	// WriteKey creates-or-updates a single key in the Secret.
	WriteKey(ctx context.Context, namespace, name, key string, value []byte) error
}

// ProtectedResourceMetadata is the response of
// /.well-known/oauth-protected-resource (RFC 9700 / MCP OAuth discovery).
type ProtectedResourceMetadata struct {
	AuthorizationServers     []string `json:"authorization_servers"`
	BearerMethodsSupported   []string `json:"bearer_methods_supported"`
	ScopesSupported          []string `json:"scopes_supported"`
	Resource                 string   `json:"resource"`
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

// AuthorizationServerMetadata is the response of
// /.well-known/oauth-authorization-server (RFC 8414).
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
}

// Metadata is the resolved, server-agnostic OAuth configuration for an MCP server.
type Metadata struct {
	AuthorizationEndpoint    string
	TokenEndpoint            string
	GrantTypes               []string
	CodeChallengeMethods     []string
	TokenEndpointAuthMethods []string
	ScopesSupported          []string
	BearerMethods            []string
	AccessTokenSecretRef     SecretKeyRef // where to write access_token
}

// SecretKeyRef references a Kubernetes Secret key.
type SecretKeyRef struct {
	Namespace string
	Name      string
	Key       string
}

// Config drives the token manager for one MCP server.
type Config struct {
	// ServerURL is the MCP server URL (e.g. https://mcp.slack.com/mcp). The issuer
	// origin is derived from it for metadata discovery.
	ServerURL string
	// ClientID / ClientSecret are the OAuth client credentials (Slack app credentials).
	ClientID     string
	ClientSecret string
	// RedirectURI is the OAuth redirect URI registered in the app.
	RedirectURI string
	// Scopes overrides discovery.scopes_supported. If empty, the server's scopes win.
	Scopes []string
	// RefreshToken is the current refresh token, if any (seeds the flow without a browser).
	RefreshToken string
	// RefreshTokenSecretRef is where to persist a (rotated) refresh token.
	RefreshTokenSecretRef SecretKeyRef
	// AccessTokenSecretRef is the Secret/key the model-router reads as the bearer
	// (referenced by MCPServer.spec.auth.bearerToken).
	AccessTokenSecretRef SecretKeyRef
	// RefreshInterval bounds how often to attempt a refresh when no expires_in is known.
	// A zero value defaults to 50m.
	RefreshInterval time.Duration
}

// TokenManager refreshes OAuth access tokens for an MCP server and persists them
// into a Secret. It is reusable across any OAuth-only MCP: discovery is driven by
// the server's published metadata, not by hardcoded endpoints.
type TokenManager struct {
	HTTPClient *http.Client
	Secrets    SecretIO
}

// New returns a TokenManager backed by the given HTTP client and Secret store.
func New(httpClient *http.Client, secrets SecretIO) *TokenManager {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &TokenManager{HTTPClient: httpClient, Secrets: secrets}
}

// Discover resolves OAuth Metadata for an MCP server by following RFC 8414 / the MCP
// oauth-protected-resource discovery chain from ServerURL's issuer origin.
func (m *TokenManager) Discover(ctx context.Context, serverURL string) (*Metadata, error) {
	origin, err := issuerOrigin(serverURL)
	if err != nil {
		return nil, fmt.Errorf("deriving issuer origin from %q: %w", serverURL, err)
	}
	prm, err := fetchJSON[ProtectedResourceMetadata](ctx, m.HTTPClient, origin+"/.well-known/oauth-protected-resource")
	if err != nil {
		return nil, fmt.Errorf("fetching oauth-protected-resource metadata from %s: %w", origin, err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("oauth-protected-resource metadata has no authorization_servers")
	}
	authServer := prm.AuthorizationServers[0]
	asm, err := fetchJSON[AuthorizationServerMetadata](ctx, m.HTTPClient, authServer+"/.well-known/oauth-authorization-server")
	if err != nil {
		return nil, fmt.Errorf("fetching oauth-authorization-server metadata from %s: %w", authServer, err)
	}
	if asm.TokenEndpoint == "" || asm.AuthorizationEndpoint == "" {
		return nil, fmt.Errorf("authorization-server metadata missing endpoint(s)")
	}
	return &Metadata{
		AuthorizationEndpoint:    asm.AuthorizationEndpoint,
		TokenEndpoint:            asm.TokenEndpoint,
		GrantTypes:               asm.GrantTypesSupported,
		CodeChallengeMethods:     asm.CodeChallengeMethodsSupported,
		TokenEndpointAuthMethods: asm.TokenEndpointAuthMethodsSupported,
		ScopesSupported:          asm.ScopesSupported,
		BearerMethods:            prm.BearerMethodsSupported,
	}, nil
}

// Refresh exchanges a refresh token for a fresh access token at meta.TokenEndpoint.
// It uses client_secret_post (client_id/secret in the form body), the grant type the
// server advertises, and returns the parsed token response.
func (m *TokenManager) Refresh(ctx context.Context, meta *Metadata, cfg *Config) (*TokenResponse, error) {
	if !supportsGrant(meta.GrantTypes, "refresh_token") {
		return nil, fmt.Errorf("server does not support refresh_token grant")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cfg.RefreshToken)
	if cfg.ClientID != "" {
		form.Set("client_id", cfg.ClientID)
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	return m.postToken(ctx, meta.TokenEndpoint, form)
}

// ExchangeCode exchanges an authorization code (PKCE) for access+refresh tokens.
// It is used by the one-time seed flow.
func (m *TokenManager) ExchangeCode(ctx context.Context, meta *Metadata, cfg *Config, code, codeVerifier string) (*TokenResponse, error) {
	if code == "" || codeVerifier == "" {
		return nil, errors.New("code and code_verifier are required for authorization_code exchange")
	}
	if !supportsGrant(meta.GrantTypes, "authorization_code") {
		return nil, fmt.Errorf("server does not support authorization_code grant")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	if cfg.RedirectURI != "" {
		form.Set("redirect_uri", cfg.RedirectURI)
	}
	if cfg.ClientID != "" {
		form.Set("client_id", cfg.ClientID)
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	return m.postToken(ctx, meta.TokenEndpoint, form)
}

// LoginURL returns the authorization endpoint URL for the one-time browser code
// exchange (PKCE, S256). It also returns the generated code_verifier that the caller
// must preserve and pass to ExchangeCode.
func (m *TokenManager) LoginURL(meta *Metadata, cfg *Config) (authURL, codeVerifier string, err error) {
	if !supportsGrant(meta.GrantTypes, "authorization_code") {
		return "", "", fmt.Errorf("server does not support authorization_code grant")
	}
	if !supportsString(meta.CodeChallengeMethods, "S256") {
		return "", "", fmt.Errorf("server does not support S256 PKCE")
	}
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return meta.AuthorizationEndpoint + "?" + q.Encode(), verifier, nil
}

// postToken POSTs an OAuth form-encoded body to the token endpoint and decodes the
// response. Slack wraps errors in {"ok":false,"error":"..."} (Slack-style); standard
// OAuth uses {"error":"..."} only. Both are normalized.
func (m *TokenManager) postToken(ctx context.Context, tokenEndpoint string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_post: credentials in the body (already set above).
	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decoding token response: %w (body: %s)", err, truncate(string(body), 200))
	}
	if resp.StatusCode >= 400 || tr.isFailure() {
		return nil, fmt.Errorf("token exchange failed: %s", tr.ErrorOrMessage())
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token exchange returned no access_token: %s", truncate(string(body), 200))
	}
	return &tr, nil
}

// TokenResponse is the union of standard OAuth2 and Slack-style token responses.
type TokenResponse struct {
	OK           bool   `json:"ok"` // Slack: absent means standard OAuth (treat as success unless error)
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"` // absent => token not rotated; keep existing
	ExpiresIn    int    `json:"expires_in,omitempty"`    // seconds; absent => unknown lifetime
	TokenType    string `json:"token_type,omitempty"`
	Error        string `json:"error,omitempty"` // OAuth error code (e.g. invalid_grant) or Slack error
}

func (t *TokenResponse) isFailure() bool {
	// Slack sets ok:false; standard OAuth omits ok and sets error. Normalize.
	return (t.OK == false && t.Error != "") || (t.Error != "" && t.AccessToken == "")
}

func (t *TokenResponse) ErrorOrMessage() string {
	if t.Error != "" {
		return t.Error
	}
	return "unknown error"
}

// RefreshOnce re-reads the current refresh token from its secret (so rotated tokens
// persist across cycles/restarts), exchanges it for a fresh access token, and writes
// both the access token (to AccessTokenSecretRef) and any rotated refresh token (to
// RefreshTokenSecretRef). It returns the token response so callers can schedule the
// next refresh from resp.ExpiresIn.
func (m *TokenManager) RefreshOnce(ctx context.Context, meta *Metadata, cfg *Config) (*TokenResponse, error) {
	ref := cfg.RefreshTokenSecretRef
	if ref.Name != "" && ref.Key != "" && ref.Namespace != "" {
		if rt, ok, err := m.Secrets.ReadKey(ctx, ref.Namespace, ref.Name, ref.Key); err == nil && ok {
			cfg.RefreshToken = string(rt)
		}
	}
	if cfg.RefreshToken == "" {
		return nil, errors.New("no refresh token configured; run `oauth-proxy seed` once to obtain one")
	}
	resp, err := m.Refresh(ctx, meta, cfg)
	if err != nil {
		return nil, err
	}
	if err := m.Secrets.WriteKey(ctx, cfg.AccessTokenSecretRef.Namespace, cfg.AccessTokenSecretRef.Name, cfg.AccessTokenSecretRef.Key, []byte(resp.AccessToken)); err != nil {
		return nil, fmt.Errorf("writing access token secret: %w", err)
	}
	if resp.RefreshToken != "" {
		// Persist the rotated refresh token so the next cycle uses the new one.
		if err := m.Secrets.WriteKey(ctx, cfg.RefreshTokenSecretRef.Namespace, cfg.RefreshTokenSecretRef.Name, cfg.RefreshTokenSecretRef.Key, []byte(resp.RefreshToken)); err != nil {
			// Non-fatal: the access token was written; keep the existing refresh token.
		} else {
			cfg.RefreshToken = resp.RefreshToken
		}
	}
	return resp, nil
}

// RequeueInterval returns how long after which the controller/oauth-proxy should
// refresh again: cfg.RefreshInterval when set, else 80% of the token's lifetime when
// expires_in is known, else a 50m default (Slack omits expires_in).
func RequeueInterval(resp *TokenResponse, refreshInterval time.Duration) time.Duration {
	if refreshInterval > 0 {
		return refreshInterval
	}
	if resp != nil && resp.ExpiresIn > 0 {
		return time.Duration(float64(resp.ExpiresIn)*0.8) * time.Second
	}
	return 50 * time.Minute
}

// Run periodically refreshes the access token for cfg until ctx is cancelled. It is
// the loop used by the (optional) oauth-proxy `run` subcommand. The operator
// reconciler calls RefreshOnce directly on its own requeue schedule instead.
func (m *TokenManager) Run(ctx context.Context, meta *Metadata, cfg *Config) error {
	for {
		resp, err := m.RefreshOnce(ctx, meta, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return nil // context cancelled (clean shutdown / test)
			}
			return fmt.Errorf("token refresh: %w", err)
		}
		interval := RequeueInterval(resp, cfg.RefreshInterval)
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func supportsGrant(grants []string, want string) bool {
	for _, g := range grants {
		if g == want {
			return true
		}
	}
	return false
}

func supportsString(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// fetchJSON fetches url and JSON-decodes into T.
func fetchJSON[T any](ctx context.Context, hc *http.Client, url string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding JSON from %s: %w", url, err)
	}
	return &out, nil
}
