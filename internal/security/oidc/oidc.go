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

// Package oidc provides a small, testable wrapper around the OpenID Connect
// authorization-code flow on top of coreos/go-oidc and golang.org/x/oauth2.
//
// agent-orca uses it to offer browser "login via OIDC" for the UI API and an
// interactive login on the External/ACP APIs. A successful login yields an
// IDTokenPrincipal carrying the IdP's claims; the caller (the apiserver) then
// mints a short-lived agent-orca-issued JWT (see apiserver.MintSessionJWT) that
// the existing requireAuth / ExternalAuth middleware already understands, so no
// parallel auth path is introduced.
//
// OIDC is the industry standard for authenticating into web UIs; this package is
// the auth-login half of that story. Authorization (RBAC) is deliberately out of
// scope here — the principal only *carries* the user's groups so a future role
// mapper can consume them without re-architecting authentication.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config configures an OIDC login client against an external identity provider.
//
// IssuerURL/ClientID/ClientSecret/RedirectURI are the OAuth2 client credentials
// registered with the IdP. ClaimMappings selects which IdP claims map onto the
// resolved principal (used later by RBAC, for now only carried through). PKCE is
// enabled by default and strongly recommended for browser/public clients.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string // defaults to {"openid","email","profile"}
	// AccessType, when non-empty, is sent as the `access_type` auth-code request
	// parameter. Set to "offline" so Google issues a refresh token (the
	// "offline_access" scope alone is insufficient for Google). Other providers
	// ignore the parameter, so requesting it is safe and makes refresh capture
	// work across IdPs.
	AccessType    string
	PKCE          bool // default true
	ClaimMappings struct {
		UserID string // default "sub"
		Groups string // default "groups"
		Email  string // default "email"
	}
}

// DefaultScopes returned when Config.Scopes is empty.
var DefaultScopes = []string{"openid", "email", "profile"}

// IDTokenPrincipal is the resolved identity derived from a verified OIDC ID token.
// Groups are intentionally not used for authorization yet — they're carried so a
// future RBAC layer can consume them.
type IDTokenPrincipal struct {
	Subject    string         // sub
	Issuer     string         // iss — used to resolve the federated tenant
	UserID     string         // mapped from ClaimMappings.UserID
	Email      string         // mapped from ClaimMappings.Email
	Groups     []string       // mapped from ClaimMappings.Groups
	Claims     map[string]any // raw verified claims
	RawIDToken string         // the raw id_token (logging/auditing only)
	// RefreshToken is the refresh_token returned alongside the id_token (if any).
	// It lets the caller (e.g. the aoctl CLI) refresh the session without
	// re-prompting for IdP credentials. The server-side browser flow does not
	// need it and ignores it; it is purely a CLI convenience.
	RefreshToken string
}

// PrincipalProvider is the surface handlers depend on; *Provider satisfies it and
// tests can substitute a fake.
type PrincipalProvider interface {
	// AuthCodeURL returns the IdP login URL and the PKCE code_verifier (empty when
	// PKCE is disabled). nonce is the OIDC nonce bound to the ID token.
	AuthCodeURL(state, nonce string) (authURL, codeVerifier string)
	// ExchangeAndVerify exchanges the auth code for tokens, verifies the ID token
	// (signature via JWKS, iss, aud, exp/nbf, nonce) and returns the resolved
	// principal.
	ExchangeAndVerify(ctx context.Context, code, nonce, codeVerifier string) (*IDTokenPrincipal, error)
}

// Provider is the default PrincipalProvider.
type Provider struct {
	oidc   *gooidc.Provider
	oauth2 *oauth2.Config
	cfg    Config
}

// NewProvider discovers the IdP via its well-known configuration and returns a
// configured Provider. PKCE defaults to disabled unless cfg.PKCE is true.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURI == "" {
		return nil, errors.New("oidc: IssuerURL, ClientID and RedirectURI are required")
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	// go-oidc performs discovery over plain HTTP for tests; NewProvider does not
	// enforce HTTPS, so httptest-based mock issuers work as-is.
	op, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovering provider %q: %w", cfg.IssuerURL, err)
	}
	if cfg.ClaimMappings.UserID == "" {
		cfg.ClaimMappings.UserID = "sub"
	}
	if cfg.ClaimMappings.Email == "" {
		cfg.ClaimMappings.Email = "email"
	}
	if cfg.ClaimMappings.Groups == "" {
		cfg.ClaimMappings.Groups = "groups"
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURI,
		Endpoint:     op.Endpoint(),
		Scopes:       scopes,
	}
	return &Provider{oidc: op, oauth2: oauthCfg, cfg: cfg}, nil
}

// AuthCodeURL implements PrincipalProvider.
func (p *Provider) AuthCodeURL(state, nonce string) (string, string) {
	opts := []oauth2.AuthCodeOption{gooidc.Nonce(nonce)}
	if p.cfg.AccessType != "" {
		opts = append(opts, oauth2.SetAuthURLParam("access_type", p.cfg.AccessType))
	}
	codeVerifier := ""
	if p.cfg.PKCE {
		codeVerifier = randomCodeVerifier()
		opts = append(opts,
			oauth2.SetAuthURLParam("code_challenge", s256Challenge(codeVerifier)),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		)
	}
	return p.oauth2.AuthCodeURL(state, opts...), codeVerifier
}

// ExchangeAndVerify implements PrincipalProvider.
func (p *Provider) ExchangeAndVerify(ctx context.Context, code, nonce, codeVerifier string) (*IDTokenPrincipal, error) {
	opts := []oauth2.AuthCodeOption{}
	if p.cfg.PKCE && codeVerifier != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	}
	token, err := p.oauth2.Exchange(ctx, code, opts...)
	if err != nil {
		return nil, fmt.Errorf("oidc: exchanging auth code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("oidc: token response missing id_token")
	}
	verifier := p.oidc.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verifying id token: %w", err)
	}
	// go-oidc does NOT verify the nonce itself — caller's responsibility.
	if nonce != "" && idToken.Nonce != nonce {
		return nil, errors.New("oidc: nonce mismatch")
	}
	principal := &IDTokenPrincipal{
		Subject:    idToken.Subject,
		Issuer:     idToken.Issuer,
		Claims:     make(map[string]any),
		RawIDToken: rawIDToken,
	}
	if err := idToken.Claims(&principal.Claims); err != nil {
		return nil, fmt.Errorf("oidc: parsing id token claims: %w", err)
	}
	principal.UserID = claimString(principal.Claims, p.cfg.ClaimMappings.UserID)
	principal.Email = claimString(principal.Claims, p.cfg.ClaimMappings.Email)
	if g, ok := principal.Claims[p.cfg.ClaimMappings.Groups]; ok {
		principal.Groups = toStringSlice(g)
	}
	// Capture the refresh token (if the IdP issued one) so the CLI can refresh
	// the session later without re-entering credentials.
	principal.RefreshToken = token.RefreshToken
	return principal, nil
}

// Refresh exchanges a refresh_token for a fresh id_token at the IdP, verifies it
// (signature via JWKS, iss, aud — nonce is not bound across a refresh) and
// returns the new id_token plus any rotated refresh token. It is the path the
// aoctl CLI uses to keep a federated session alive beyond the id_token's short
// lifetime without re-prompting for credentials.
//
// Returns an error if the IdP did not return a new id_token (some providers omit
// it on refresh); in that case the caller should re-authenticate.
func (p *Provider) Refresh(ctx context.Context, refreshToken string) (idToken, newRefreshToken string, err error) {
	if refreshToken == "" {
		return "", "", errors.New("oidc: refresh token is empty")
	}
	// oauth2.TokenSource triggers a refresh grant when the supplied token has no
	// valid access token; the response is parsed into *oauth2.Token, whose Extra()
	// surfaces the new id_token (and rotated refresh_token).
	src := p.oauth2.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	tok, err := src.Token()
	if err != nil {
		return "", "", fmt.Errorf("oidc: refreshing token: %w", err)
	}
	newRefreshToken = tok.RefreshToken
	if newRefreshToken == "" {
		// Some IdPs rotate; some echo nothing on refresh. Keep the existing one
		// so the caller can keep trying.
		newRefreshToken = refreshToken
	}
	rawIDToken, _ := tok.Extra("id_token").(string)
	if rawIDToken == "" {
		return "", "", errors.New("oidc: refresh response did not include an id_token")
	}
	// Re-verify the freshly issued id_token (signature, iss, aud, exp). Nonce is
	// intentionally NOT checked here — refresh responses don't carry the original
	// nonce, and refresh is a trusted continuation of an already-verified session.
	verifier := p.oidc.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID})
	if _, err := verifier.Verify(ctx, rawIDToken); err != nil {
		return "", "", fmt.Errorf("oidc: verifying refreshed id token: %w", err)
	}
	return rawIDToken, newRefreshToken, nil
}

// claimString reads a claim as a string (returns "" if absent or non-string).
func claimString(claims map[string]any, key string) string {
	if key == "" || claims == nil {
		return ""
	}
	v, _ := claims[key].(string)
	return v
}

// toStringSlice normalizes a claim value into []string. Handles []string, []any,
// and a single string.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

// randomCodeVerifier returns a high-entropy PKCE code_verifier (43 chars).
func randomCodeVerifier() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// s256Challenge returns the PKCE S256 code_challenge for a verifier.
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
