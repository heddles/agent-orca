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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/heddles/agent-orca/internal/oauth"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// Codes of a refused token request. They are the "error" of the operator's answer
// and what the router shows the model, so each has a message that carries no
// credential.
const (
	CodeRequired    = "connection_required"
	CodeUnavailable = "connection_unavailable"
	CodeNeedsReauth = "needs_reauth"
)

// TokenError is a refused token request: a stable code and a message that is safe
// to relay to the model.
type TokenError struct {
	Code    string
	Message string
}

func (e *TokenError) Error() string { return e.Code + ": " + e.Message }

// AsTokenError returns err as a *TokenError, or nil.
func AsTokenError(err error) *TokenError {
	var te *TokenError
	if errors.As(err, &te) {
		return te
	}
	return nil
}

const (
	// refreshWindow: an access token that expires within it is refreshed first.
	refreshWindow = 60 * time.Second
	// defaultAccessLifetime applies when the provider gives no expires_in.
	defaultAccessLifetime = 50 * time.Minute
	// lastUsedEvery bounds how often a use writes last_used_at.
	lastUsedEvery = 5 * time.Minute
)

// TokenRequest asks for a connection's access token. The identity is the
// connection owner's, resolved by the caller from the AgentRun and the Agent, never
// from anything the router sent.
type TokenRequest struct {
	TenantID string
	UserID   string
	Server   ServerRef
}

// AccessToken is a short-lived access token. The refresh token is never returned.
type AccessToken struct {
	Token     Secret
	ExpiresAt time.Time
	// AccountLabel names whose account the token acts as.
	AccountLabel string
}

// AccessToken returns the owner's access token for a server, refreshing it first
// when it expires within a minute. The connection row is locked for the whole
// call (SELECT ... FOR UPDATE), so two runs that both find the token about to
// expire do not both spend a rotating refresh token: the second waits, then reads
// the new access token. On invalid_grant the connection becomes needs_reauth and
// OnReauth is called.
func (s *Service) AccessToken(ctx context.Context, req TokenRequest) (AccessToken, error) {
	if s.Keys == nil {
		return AccessToken{}, &TokenError{CodeUnavailable, "per-user connections are not set up on this platform"}
	}
	var out AccessToken
	var refused *TokenError
	var reauth *postgresql.MCPConnection
	err := s.Store.WithConnectionLock(ctx, req.UserID, req.Server.Namespace, req.Server.Name, func(cur postgresql.MCPConnection) (*postgresql.MCPConnection, error) {
		switch {
		case cur.ServerUID != req.Server.UID:
			refused = &TokenError{CodeRequired, "the connection was made for an earlier version of this server and must be made again"}
			return nil, nil
		case cur.Status == postgresql.ConnectionNeedsReauth:
			refused = &TokenError{CodeNeedsReauth, "the connection needs to be renewed by its owner"}
			return nil, nil
		case cur.Status != postgresql.ConnectionActive:
			refused = &TokenError{CodeUnavailable, "the connection has been revoked"}
			return nil, nil
		}
		now := s.now()
		out.AccountLabel = cur.AccountName
		if tok, ok := s.cachedAccess(cur, now); ok {
			out.Token, out.ExpiresAt = Secret(tok), *cur.AccessExpiresAt
			return s.touch(cur, now), nil
		}
		next, tr, err := s.refresh(ctx, cur, req.Server, now)
		var te *oauth.TokenError
		switch {
		case errors.As(err, &te) && te.Code == "invalid_grant":
			cur.Status = postgresql.ConnectionNeedsReauth
			reauth = &cur
			refused = &TokenError{CodeNeedsReauth, "the connection needs to be renewed by its owner"}
			return &cur, nil
		case err != nil:
			return nil, err
		}
		out.Token, out.ExpiresAt = Secret(tr.AccessToken), *next.AccessExpiresAt
		return next, nil
	})
	switch {
	case errors.Is(err, postgresql.ErrConnectionNotFound):
		return AccessToken{}, &TokenError{CodeRequired, "the account has not been connected"}
	case err != nil:
		slog.Warn("MCP connection token request failed", "server", req.Server.Namespace+"/"+req.Server.Name, "err", err)
		return AccessToken{}, &TokenError{CodeUnavailable, "the provider could not be reached to renew the connection; try again"}
	}
	if reauth != nil && s.OnReauth != nil {
		s.OnReauth(ctx, *reauth)
	}
	if refused != nil {
		return AccessToken{}, refused
	}
	return out, nil
}

// cachedAccess returns the stored access token when it is still good for the window.
func (s *Service) cachedAccess(c postgresql.MCPConnection, now time.Time) (string, bool) {
	if len(c.AccessTokenEnc) == 0 || c.AccessExpiresAt == nil || c.AccessExpiresAt.Sub(now) <= refreshWindow {
		return "", false
	}
	pt, err := s.Keys.Open(c.KeyID, c.AccessTokenEnc, AAD("access", c.TenantID, c.UserID, c.ServerUID))
	if err != nil {
		return "", false // refresh instead
	}
	return string(pt), true
}

// touch records a use at most every lastUsedEvery.
func (s *Service) touch(c postgresql.MCPConnection, now time.Time) *postgresql.MCPConnection {
	if c.LastUsedAt != nil && now.Sub(*c.LastUsedAt) < lastUsedEvery {
		return nil
	}
	c.LastUsedAt = &now
	return &c
}

// refresh exchanges the refresh token for a new access token and returns the row
// to store, with a rotated refresh token when the provider sent one.
func (s *Service) refresh(ctx context.Context, cur postgresql.MCPConnection, ref ServerRef, now time.Time) (*postgresql.MCPConnection, *oauth.TokenResponse, error) {
	rt, err := s.Keys.Open(cur.KeyID, cur.RefreshTokenEnc, AAD("refresh", cur.TenantID, cur.UserID, cur.ServerUID))
	if err != nil {
		return nil, nil, fmt.Errorf("opening the refresh token: %w", err)
	}
	app, secret, err := s.app(ctx, ref, cur.TenantID)
	if err != nil {
		return nil, nil, err
	}
	meta, err := s.discover(ctx, ref.URL)
	if err != nil {
		return nil, nil, err
	}
	tr, err := s.OAuth.Refresh(ctx, meta, &oauth.Config{
		ServerURL: ref.URL, ClientID: app.ClientID, ClientSecret: secret, RefreshToken: string(rt),
		Resource: ResourceIndicator(ref.URL),
	})
	if err != nil {
		return nil, nil, err
	}
	if tr.RefreshToken == "" {
		tr.RefreshToken = string(rt) // not rotated: keep the stored one
	}
	next := cur
	if err := s.sealTokens(&next, tr); err != nil {
		return nil, nil, err
	}
	if tr.ExpiresIn <= 0 {
		t := now.Add(defaultAccessLifetime)
		next.AccessExpiresAt = &t
	}
	next.LastUsedAt = &now
	next.Status = postgresql.ConnectionActive
	return &next, tr, nil
}
