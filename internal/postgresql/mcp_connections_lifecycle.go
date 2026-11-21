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

// The lifecycle queries of per-user MCP connections (migration 0021,
// docs/mcp-oauth.md): the status counts behind the metrics, the idle sweep and the
// key-rotation scan. The background jobs call them with the platform scope.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ConnectionExpired is a connection whose tokens were wiped after it sat unused.
const ConnectionExpired = "expired"

// AuditExpire is the audit action of an idle expiry.
const AuditExpire = "expire"

// CountConnectionsByStatus returns how many connections there are per status.
func (s *Store) CountConnectionsByStatus(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT status, count(*) FROM mcp_connections
			WHERE ($1::text = '*' OR tenant_id = $1) GROUP BY status`, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				return err
			}
			out[status] = n
		}
		return rows.Err()
	})
	return out, wrapConnection("counting connections", err)
}

// IdleConnection names a connection that has not been used since a cutoff. It
// carries no ciphertext.
type IdleConnection struct {
	ID         int64
	TenantID   string
	UserID     string
	ServerNS   string
	ServerName string
}

// ListIdleConnections returns up to limit live connections (active or
// needs_reauth) whose last use, or creation when never used, is before cutoff.
func (s *Store) ListIdleConnections(ctx context.Context, cutoff time.Time, limit int) ([]IdleConnection, error) {
	out := []IdleConnection{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, tenant_id, user_id, server_ns, server_name FROM mcp_connections
			WHERE status IN ('active', 'needs_reauth') AND COALESCE(last_used_at, created_at) < $1
			  AND ($2::text = '*' OR tenant_id = $2)
			ORDER BY id LIMIT $3`, cutoff, scope, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c IdleConnection
			if err := rows.Scan(&c.ID, &c.TenantID, &c.UserID, &c.ServerNS, &c.ServerName); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, wrapConnection("listing idle connections", err)
}

// ExpireIdleConnection marks one connection expired and wipes its tokens, if it
// is still idle: the row is locked and its last use checked again, so a use that
// raced the sweep keeps the connection. It returns the row as it was before the
// wipe (the caller revokes the refresh token at the provider from it) and whether
// the connection was expired.
func (s *Store) ExpireIdleConnection(ctx context.Context, id int64, cutoff time.Time) (MCPConnection, bool, error) {
	var prev MCPConnection
	var expired bool
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanConnection(tx.QueryRowContext(ctx, `
			SELECT `+connCols+` FROM mcp_connections c
			WHERE c.id = $1 AND c.status IN ('active', 'needs_reauth') AND COALESCE(c.last_used_at, c.created_at) < $2
			  AND ($3::text = '*' OR c.tenant_id = $3)
			FOR UPDATE SKIP LOCKED`, id, cutoff, scope))
		if err != nil {
			return connNotFound(err) // used since, gone, or locked by a refresh: leave it
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE mcp_connections SET status = 'expired', refresh_token_enc = '\x'::bytea, access_token_enc = NULL,
				access_expires_at = NULL, updated_at = now()
			WHERE id = $1`, id); err != nil {
			return err
		}
		prev, expired = cur, true
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConnectionNotFound) {
			return MCPConnection{}, false, nil
		}
		return MCPConnection{}, false, wrapConnection("expiring the connection", err)
	}
	return prev, expired, nil
}

// ListConnectionsToRewrap returns up to limit ids of live connections after
// afterID whose key is not activeKeyID, oldest id first. Expired rows hold no
// tokens and are not listed.
func (s *Store) ListConnectionsToRewrap(ctx context.Context, activeKeyID string, afterID int64, limit int) ([]int64, error) {
	var ids []int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM mcp_connections
			WHERE key_id <> $1 AND id > $2 AND status IN ('active', 'needs_reauth') AND ($3::text = '*' OR tenant_id = $3)
			ORDER BY id LIMIT $4`, activeKeyID, afterID, scope, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, wrapConnection("listing connections to re-wrap", err)
}

// RewrapConnection runs fn on one connection whose key is still not
// activeKeyID, holding its row lock, and stores the row fn returns (only the
// token columns and key id change). A row that a refresh holds is skipped
// (SKIP LOCKED): the refresh seals under the active key anyway. It reports
// whether fn ran on the row, so a concurrent refresh, a reconnect or another
// replica that got there first is a no-op.
func (s *Store) RewrapConnection(ctx context.Context, id int64, activeKeyID string, fn func(MCPConnection) (*MCPConnection, error)) (bool, error) {
	var ran bool
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanConnection(tx.QueryRowContext(ctx, `
			SELECT `+connCols+` FROM mcp_connections c
			WHERE c.id = $1 AND c.key_id <> $2 AND c.status IN ('active', 'needs_reauth') AND ($3::text = '*' OR c.tenant_id = $3)
			FOR UPDATE SKIP LOCKED`, id, activeKeyID, scope))
		if err != nil {
			return connNotFound(err)
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		ran = true
		if next == nil {
			return nil
		}
		if next.KeyID == "" || len(next.RefreshTokenEnc) == 0 {
			return fmt.Errorf("%w: a re-wrapped row needs a key id and a refresh token", ErrInvalidConnection)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE mcp_connections SET refresh_token_enc = $2, access_token_enc = $3, access_expires_at = $4, key_id = $5
			WHERE id = $1`, id, next.RefreshTokenEnc, nullBytes(next.AccessTokenEnc), next.AccessExpiresAt, next.KeyID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrConnectionNotFound) {
			return false, nil
		}
		return false, wrapConnection("re-wrapping the connection", err)
	}
	return ran, nil
}

// ListOAuthAppsToRewrap returns up to limit ids of OAuth apps after afterID that
// hold a client secret under a key other than activeKeyID.
func (s *Store) ListOAuthAppsToRewrap(ctx context.Context, activeKeyID string, afterID int64, limit int) ([]int64, error) {
	var ids []int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM mcp_oauth_apps
			WHERE key_id <> $1 AND id > $2 AND client_secret_enc IS NOT NULL AND ($3::text = '*' OR tenant_id = $3)
			ORDER BY id LIMIT $4`, activeKeyID, afterID, scope, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, wrapConnection("listing OAuth apps to re-wrap", err)
}

// RewrapOAuthApp is RewrapConnection for an OAuth app's client secret.
func (s *Store) RewrapOAuthApp(ctx context.Context, id int64, activeKeyID string, fn func(MCPOAuthApp) (*MCPOAuthApp, error)) (bool, error) {
	var ran bool
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanApp(tx.QueryRowContext(ctx, `
			SELECT `+appCols+` FROM mcp_oauth_apps a
			WHERE a.id = $1 AND a.key_id <> $2 AND a.client_secret_enc IS NOT NULL AND ($3::text = '*' OR a.tenant_id = $3)
			FOR UPDATE SKIP LOCKED`, id, activeKeyID, scope))
		if err != nil {
			return connNotFound(err)
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		ran = true
		if next == nil {
			return nil
		}
		if next.KeyID == "" || len(next.ClientSecretEnc) == 0 {
			return fmt.Errorf("%w: a re-wrapped app needs a key id and a secret", ErrInvalidConnection)
		}
		_, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_apps SET client_secret_enc = $2, key_id = $3 WHERE id = $1`,
			id, next.ClientSecretEnc, next.KeyID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrConnectionNotFound) {
			return false, nil
		}
		return false, wrapConnection("re-wrapping the OAuth app", err)
	}
	return ran, nil
}
