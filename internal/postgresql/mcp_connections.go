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

package postgresql

// Per-user OAuth connections to MCP servers (migration 0020, docs/mcp-oauth.md).
//
// This file stores opaque ciphertext. The envelope encryption lives in
// internal/connections, so the store never sees a token: a bug here cannot log
// or return one. Every query on mcp_connections filters on user_id as well as
// on the tenant: RLS isolates tenants, the predicate isolates users in one.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Connection statuses.
const (
	ConnectionActive      = "active"
	ConnectionNeedsReauth = "needs_reauth"
	ConnectionRevoked     = "revoked"
)

// ConnectionExpired (an idle connection whose tokens were wiped) is in
// mcp_connections_lifecycle.go.

// AuditKindMCPConnection is the audit kind of connection events.
const AuditKindMCPConnection = "MCPConnection"

// Audit actions of the connection lifecycle (audit_events.action is text).
const (
	AuditConnect      = "connect"
	AuditDisconnect   = "disconnect"
	AuditRevoke       = "revoke"
	AuditReauth       = "reauth_required"
	AuditAppRegister  = "app_register"
	AuditAppRotate    = "app_rotate"
	AuditModeChange   = "mode_change"
	maxScopeLen       = 256
	maxScopesPerGrant = 64
)

var (
	// ErrConnectionNotFound: no connection, app or pending flow in the call's scope.
	ErrConnectionNotFound = errors.New("connection not found")
	// ErrInvalidConnection: a row the table would not take.
	ErrInvalidConnection = errors.New("invalid connection")
)

// MCPConnection is a user's connection to an MCP server. The token fields are
// ciphertext and never leave the process as JSON.
type MCPConnection struct {
	ID          int64      `json:"id"`
	TenantID    string     `json:"tenantId,omitempty"`
	UserID      string     `json:"userId"`
	ServerNS    string     `json:"serverNamespace"`
	ServerName  string     `json:"serverName"`
	ServerUID   string     `json:"-"`
	Scopes      []string   `json:"scopes"`
	AccountName string     `json:"accountLabel,omitempty"`
	Status      string     `json:"status"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`

	RefreshTokenEnc []byte     `json:"-"`
	AccessTokenEnc  []byte     `json:"-"`
	AccessExpiresAt *time.Time `json:"-"`
	KeyID           string     `json:"-"`
}

// String never includes the ciphertext.
func (c MCPConnection) String() string {
	return fmt.Sprintf("MCPConnection{%s %s/%s user=%s status=%s}", c.TenantID, c.ServerNS, c.ServerName, c.UserID, c.Status)
}

// MCPOAuthApp is the OAuth app an admin registered for MCP servers. The client
// secret is ciphertext and is never returned by any API.
type MCPOAuthApp struct {
	TenantID        string    `json:"tenantId,omitempty"`
	ServerNS        string    `json:"namespace"`
	AppRef          string    `json:"appRef"`
	ClientID        string    `json:"clientId"`
	RedirectURI     string    `json:"redirectUri"`
	ScopesAllowed   []string  `json:"scopesAllowed"`
	HasSecret       bool      `json:"hasClientSecret"`
	UpdatedBy       string    `json:"updatedBy"`
	UpdatedAt       time.Time `json:"updatedAt"`
	ClientSecretEnc []byte    `json:"-"`
	KeyID           string    `json:"-"`
}

// MCPOAuthFlow is a pending authorization request.
type MCPOAuthFlow struct {
	TenantID        string
	UserID          string
	ServerNS        string
	ServerName      string
	ServerUID       string
	VerifierEnc     []byte
	KeyID           string
	RequestedScopes []string
	ExpiresAt       time.Time
}

// StateHash is the stored form of an OAuth state value.
func StateHash(state string) string {
	h := sha256.Sum256([]byte(state))
	return hex.EncodeToString(h[:])
}

func validScopes(scopes []string) error {
	if len(scopes) > maxScopesPerGrant {
		return fmt.Errorf("%w: too many scopes", ErrInvalidConnection)
	}
	for _, s := range scopes {
		if s == "" || len(s) > maxScopeLen || strings.ContainsAny(s, "\",{}\\ \t\r\n") {
			return fmt.Errorf("%w: bad scope %q", ErrInvalidConnection, s)
		}
	}
	return nil
}

func wrapConnection(what string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{ErrConnectionNotFound, ErrInvalidConnection, ErrNoTenantScope} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func connNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConnectionNotFound
	}
	return err
}

const connCols = `c.id, c.tenant_id, c.user_id, c.server_ns, c.server_name, c.server_uid, c.scopes,
	c.account_label, c.status, c.last_used_at, c.created_at, c.updated_at,
	c.refresh_token_enc, c.access_token_enc, c.access_expires_at, c.key_id`

func scanConnection(row interface{ Scan(...any) error }) (MCPConnection, error) {
	var c MCPConnection
	var scopes string
	err := row.Scan(&c.ID, &c.TenantID, &c.UserID, &c.ServerNS, &c.ServerName, &c.ServerUID, &scopes,
		&c.AccountName, &c.Status, &c.LastUsedAt, &c.CreatedAt, &c.UpdatedAt,
		&c.RefreshTokenEnc, &c.AccessTokenEnc, &c.AccessExpiresAt, &c.KeyID)
	c.Scopes = parsePgTextArray(scopes)
	return c, err
}

// UpsertConnection stores a user's connection, replacing an earlier one for the
// same server. The tenant is the call's.
func (s *Store) UpsertConnection(ctx context.Context, c MCPConnection) (MCPConnection, error) {
	if c.UserID == "" || c.ServerNS == "" || c.ServerName == "" || c.ServerUID == "" || len(c.RefreshTokenEnc) == 0 || c.KeyID == "" {
		return MCPConnection{}, fmt.Errorf("%w: user, server, uid, token and key id are required", ErrInvalidConnection)
	}
	if err := validScopes(c.Scopes); err != nil {
		return MCPConnection{}, err
	}
	var out MCPConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, c.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		out, err = scanConnection(tx.QueryRowContext(ctx, `
			INSERT INTO mcp_connections AS c (tenant_id, user_id, server_ns, server_name, server_uid, scopes, account_label,
				refresh_token_enc, access_token_enc, access_expires_at, key_id, status)
			VALUES ($1, $2, $3, $4, $5, $6::text[], $7, $8, $9, $10, $11, 'active')
			ON CONFLICT (tenant_id, user_id, server_ns, server_name) DO UPDATE SET
				server_uid = EXCLUDED.server_uid, scopes = EXCLUDED.scopes, account_label = EXCLUDED.account_label,
				refresh_token_enc = EXCLUDED.refresh_token_enc, access_token_enc = EXCLUDED.access_token_enc,
				access_expires_at = EXCLUDED.access_expires_at, key_id = EXCLUDED.key_id,
				status = 'active', updated_at = now()
			RETURNING `+connCols,
			tenant, c.UserID, c.ServerNS, c.ServerName, c.ServerUID, pgTextArray(c.Scopes), c.AccountName,
			c.RefreshTokenEnc, nullBytes(c.AccessTokenEnc), c.AccessExpiresAt, c.KeyID))
		return err
	})
	return out, wrapConnection("storing the connection", err)
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// GetConnection returns one user's connection to one server.
func (s *Store) GetConnection(ctx context.Context, userID, ns, server string) (MCPConnection, error) {
	var c MCPConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		c, err = scanConnection(tx.QueryRowContext(ctx, `
			SELECT `+connCols+` FROM mcp_connections c
			WHERE c.user_id = $1 AND c.server_ns = $2 AND c.server_name = $3 AND ($4::text = '*' OR c.tenant_id = $4)
			ORDER BY c.id LIMIT 1`, userID, ns, server, scope))
		return connNotFound(err)
	})
	if err != nil {
		return MCPConnection{}, wrapConnection("reading the connection", err)
	}
	return c, nil
}

// ListConnections returns the connections of one user, oldest first.
func (s *Store) ListConnections(ctx context.Context, userID string) ([]MCPConnection, error) {
	out := []MCPConnection{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+connCols+` FROM mcp_connections c
			WHERE c.user_id = $1 AND ($2::text = '*' OR c.tenant_id = $2)
			ORDER BY c.created_at, c.id`, userID, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			c, err := scanConnection(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapConnection("listing connections", err)
	}
	return out, nil
}

// ConnectionSummary is what an admin sees of a user's connection: who and what
// state, never a token.
type ConnectionSummary struct {
	UserID      string     `json:"userId"`
	AccountName string     `json:"accountLabel,omitempty"`
	Status      string     `json:"status"`
	Scopes      []string   `json:"scopes"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// ListServerConnections returns the connections to one server for the admin
// view. It selects no ciphertext column at all.
func (s *Store) ListServerConnections(ctx context.Context, ns, server string) ([]ConnectionSummary, error) {
	out := []ConnectionSummary{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT user_id, account_label, status, scopes, last_used_at, created_at FROM mcp_connections
			WHERE server_ns = $1 AND server_name = $2 AND ($3::text = '*' OR tenant_id = $3)
			ORDER BY created_at, id`, ns, server, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c ConnectionSummary
			var scopes string
			if err := rows.Scan(&c.UserID, &c.AccountName, &c.Status, &scopes, &c.LastUsedAt, &c.CreatedAt); err != nil {
				return err
			}
			c.Scopes = parsePgTextArray(scopes)
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapConnection("listing the server's connections", err)
	}
	return out, nil
}

// DeleteConnection removes one user's connection and returns it (so the caller
// can revoke the token at the provider).
func (s *Store) DeleteConnection(ctx context.Context, userID, ns, server string) (MCPConnection, error) {
	var c MCPConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		c, err = scanConnection(tx.QueryRowContext(ctx, `
			DELETE FROM mcp_connections c
			WHERE c.user_id = $1 AND c.server_ns = $2 AND c.server_name = $3 AND ($4::text = '*' OR c.tenant_id = $4)
			RETURNING `+connCols, userID, ns, server, scope))
		return connNotFound(err)
	})
	if err != nil {
		return MCPConnection{}, wrapConnection("deleting the connection", err)
	}
	return c, nil
}

// SetConnectionStatus marks a connection active, needs_reauth or revoked.
func (s *Store) SetConnectionStatus(ctx context.Context, userID, ns, server, status string) error {
	switch status {
	case ConnectionActive, ConnectionNeedsReauth, ConnectionRevoked, ConnectionExpired:
	default:
		return fmt.Errorf("%w: status %q", ErrInvalidConnection, status)
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE mcp_connections SET status = $4, updated_at = now()
			WHERE user_id = $1 AND server_ns = $2 AND server_name = $3 AND ($5::text = '*' OR tenant_id = $5)`,
			userID, ns, server, status, scope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConnectionNotFound
		}
		return nil
	})
	return wrapConnection("setting the connection status", err)
}

// WithConnectionLock runs fn on a user's connection while holding the row lock
// (SELECT ... FOR UPDATE), so two runs that both find the access token about to
// expire do not both use a rotating refresh token. fn returns the connection to
// store (nil leaves the row as it was). The caller's HTTP call to the provider
// happens inside fn: keep it short.
func (s *Store) WithConnectionLock(ctx context.Context, userID, ns, server string, fn func(MCPConnection) (*MCPConnection, error)) error {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanConnection(tx.QueryRowContext(ctx, `
			SELECT `+connCols+` FROM mcp_connections c
			WHERE c.user_id = $1 AND c.server_ns = $2 AND c.server_name = $3 AND ($4::text = '*' OR c.tenant_id = $4)
			FOR UPDATE`, userID, ns, server, scope))
		if err != nil {
			return connNotFound(err)
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE mcp_connections SET refresh_token_enc = $2, access_token_enc = $3, access_expires_at = $4, key_id = $5,
				scopes = $6::text[], status = $7, last_used_at = $8, updated_at = now()
			WHERE id = $1`, cur.ID, next.RefreshTokenEnc, nullBytes(next.AccessTokenEnc), next.AccessExpiresAt, next.KeyID,
			pgTextArray(next.Scopes), next.Status, next.LastUsedAt)
		return err
	})
	return wrapConnection("updating the connection", err)
}

const appCols = `a.tenant_id, a.server_ns, a.app_ref, a.client_id, a.redirect_uri, a.scopes_allowed, a.updated_by, a.updated_at,
	a.client_secret_enc, a.key_id`

func scanApp(row interface{ Scan(...any) error }) (MCPOAuthApp, error) {
	var a MCPOAuthApp
	var scopes string
	err := row.Scan(&a.TenantID, &a.ServerNS, &a.AppRef, &a.ClientID, &a.RedirectURI, &scopes, &a.UpdatedBy, &a.UpdatedAt,
		&a.ClientSecretEnc, &a.KeyID)
	a.ScopesAllowed = parsePgTextArray(scopes)
	a.HasSecret = len(a.ClientSecretEnc) > 0
	return a, err
}

// UpsertOAuthApp registers or rotates an app. It reports whether the row was
// new. A nil ClientSecretEnc keeps the stored secret (a rotation of the id or
// scopes only).
func (s *Store) UpsertOAuthApp(ctx context.Context, a MCPOAuthApp) (out MCPOAuthApp, created bool, err error) {
	if a.ServerNS == "" || a.AppRef == "" || a.ClientID == "" || a.RedirectURI == "" || a.UpdatedBy == "" || a.KeyID == "" {
		return MCPOAuthApp{}, false, fmt.Errorf("%w: namespace, appRef, clientId, redirectUri and key id are required", ErrInvalidConnection)
	}
	if err := validScopes(a.ScopesAllowed); err != nil {
		return MCPOAuthApp{}, false, err
	}
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, a.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM mcp_oauth_apps WHERE tenant_id = $1 AND server_ns = $2 AND app_ref = $3)`,
			tenant, a.ServerNS, a.AppRef).Scan(&exists); err != nil {
			return err
		}
		created = !exists
		out, err = scanApp(tx.QueryRowContext(ctx, `
			INSERT INTO mcp_oauth_apps AS a (tenant_id, server_ns, app_ref, client_id, client_secret_enc, key_id, redirect_uri, scopes_allowed, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::text[], $9)
			ON CONFLICT (tenant_id, server_ns, app_ref) DO UPDATE SET
				client_id = EXCLUDED.client_id, redirect_uri = EXCLUDED.redirect_uri, scopes_allowed = EXCLUDED.scopes_allowed,
				updated_by = EXCLUDED.updated_by, updated_at = now(),
				client_secret_enc = COALESCE(EXCLUDED.client_secret_enc, a.client_secret_enc),
				key_id = CASE WHEN EXCLUDED.client_secret_enc IS NULL THEN a.key_id ELSE EXCLUDED.key_id END
			RETURNING `+appCols,
			tenant, a.ServerNS, a.AppRef, a.ClientID, nullBytes(a.ClientSecretEnc), a.KeyID, a.RedirectURI, pgTextArray(a.ScopesAllowed), a.UpdatedBy))
		return err
	})
	return out, created, wrapConnection("storing the OAuth app", err)
}

// GetOAuthApp returns the app of a namespace and appRef.
func (s *Store) GetOAuthApp(ctx context.Context, ns, appRef string) (MCPOAuthApp, error) {
	var a MCPOAuthApp
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		a, err = scanApp(tx.QueryRowContext(ctx, `
			SELECT `+appCols+` FROM mcp_oauth_apps a
			WHERE a.server_ns = $1 AND a.app_ref = $2 AND ($3::text = '*' OR a.tenant_id = $3)
			ORDER BY a.id LIMIT 1`, ns, appRef, scope))
		return connNotFound(err)
	})
	if err != nil {
		return MCPOAuthApp{}, wrapConnection("reading the OAuth app", err)
	}
	return a, nil
}

// CreateOAuthFlow stores a pending authorization request under the hash of its state.
func (s *Store) CreateOAuthFlow(ctx context.Context, stateHash string, f MCPOAuthFlow) error {
	if stateHash == "" || f.UserID == "" || f.ServerNS == "" || f.ServerName == "" || f.ServerUID == "" || len(f.VerifierEnc) == 0 || f.KeyID == "" || f.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: incomplete flow", ErrInvalidConnection)
	}
	if err := validScopes(f.RequestedScopes); err != nil {
		return err
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, f.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		// Opportunistic sweep so abandoned flows do not pile up.
		if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth_flows WHERE expires_at < now()`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO mcp_oauth_flows (state_hash, tenant_id, user_id, server_ns, server_name, server_uid, verifier_enc, key_id, requested_scopes, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text[], $10)`,
			stateHash, tenant, f.UserID, f.ServerNS, f.ServerName, f.ServerUID, f.VerifierEnc, f.KeyID, pgTextArray(f.RequestedScopes), f.ExpiresAt)
		return err
	})
	return wrapConnection("storing the pending flow", err)
}

// ConsumeOAuthFlow loads and deletes a pending flow in one statement: a state
// can be used once. An expired flow is deleted and reported as not found. The
// call's tenant scope applies, so a state minted for another tenant finds nothing
// (and is not consumed).
func (s *Store) ConsumeOAuthFlow(ctx context.Context, stateHash string) (MCPOAuthFlow, error) {
	var f MCPOAuthFlow
	var scopes string
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		err := tx.QueryRowContext(ctx, `
			DELETE FROM mcp_oauth_flows
			WHERE state_hash = $1 AND ($2::text = '*' OR tenant_id = $2)
			RETURNING tenant_id, user_id, server_ns, server_name, server_uid, verifier_enc, key_id, requested_scopes, expires_at`,
			stateHash, scope).Scan(&f.TenantID, &f.UserID, &f.ServerNS, &f.ServerName, &f.ServerUID, &f.VerifierEnc, &f.KeyID, &scopes, &f.ExpiresAt)
		return connNotFound(err)
	})
	if err != nil {
		return MCPOAuthFlow{}, wrapConnection("consuming the pending flow", err)
	}
	f.RequestedScopes = parsePgTextArray(scopes)
	if time.Now().After(f.ExpiresAt) {
		return MCPOAuthFlow{}, ErrConnectionNotFound
	}
	return f, nil
}
