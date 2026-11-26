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

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SetInboundCredentials stores a connection's sealed credentials and the config
// completed with them, and forgets the Secret it named: it uses one or the other.
func (s *Store) SetInboundCredentials(ctx context.Context, id, keyID string, sealed []byte, config json.RawMessage) (InboundConnection, error) {
	var c InboundConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		c, err = scanInbound(tx.QueryRowContext(ctx, `
			UPDATE inbound_connections c SET credentials_enc = $3, credentials_key_id = $4, config = $5::jsonb, secret_ref = '',
			  last_auth_error = '', last_auth_error_at = NULL, updated_at = now()
			WHERE c.id = $1 AND ($2::text = '*' OR c.tenant_id = $2) RETURNING `+inboundCols,
			id, scope, sealed, keyID, string(configOrEmpty(config))))
		return err
	})
	if err != nil {
		return InboundConnection{}, wrapInbound("storing the credentials", err)
	}
	return c, nil
}

// RecordInboundRequest notes that the channel called: authError empty for a
// request that passed the adapter's check, else why it was refused.
func (s *Store) RecordInboundRequest(ctx context.Context, id, authError string) error {
	return wrapInbound("recording the request", s.scoped(ctx, func(tx dbtx, scope string) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE inbound_connections SET
			  last_request_at    = CASE WHEN $3 = '' THEN now() ELSE last_request_at END,
			  last_auth_error    = $3,
			  last_auth_error_at = CASE WHEN $3 = '' THEN NULL ELSE now() END
			WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`, id, scope, authError)
		return err
	}))
}

// ListInboundCredentialsToRewrap returns the connections whose stored
// credentials are under a key other than activeKeyID.
func (s *Store) ListInboundCredentialsToRewrap(ctx context.Context, activeKeyID string) ([]string, error) {
	var ids []string
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM inbound_connections
			WHERE credentials_enc IS NOT NULL AND credentials_key_id <> $1 AND ($2::text = '*' OR tenant_id = $2)
			ORDER BY id`, activeKeyID, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, wrapInbound("listing credentials to re-wrap", err)
}

// RewrapInboundCredentials replaces one connection's sealed credentials with what
// fn makes of them, holding the row's lock. It reports false when the row is busy
// or no longer needs it.
func (s *Store) RewrapInboundCredentials(
	ctx context.Context, id, activeKeyID string, fn func(InboundConnection) (string, []byte, error),
) (bool, error) {
	ran := false
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanInbound(tx.QueryRowContext(ctx, `
			SELECT `+inboundCols+` FROM inbound_connections c
			WHERE c.id = $1 AND c.credentials_enc IS NOT NULL AND c.credentials_key_id <> $2 AND ($3::text = '*' OR c.tenant_id = $3)
			FOR UPDATE SKIP LOCKED`, id, activeKeyID, scope))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		keyID, sealed, err := fn(cur)
		if err != nil {
			return err
		}
		if keyID == "" || len(sealed) == 0 {
			return ErrInvalidInbound
		}
		ran = true
		_, err = tx.ExecContext(ctx, `UPDATE inbound_connections SET credentials_enc = $2, credentials_key_id = $3 WHERE id = $1`,
			id, sealed, keyID)
		return err
	})
	if err != nil {
		return false, wrapInbound("re-wrapping credentials", err)
	}
	return ran, nil
}

// InboundPairing is a one-time code waiting for, or holding, the person who sent it.
type InboundPairing struct {
	ID           string
	TenantID     string
	ConnectionID string
	CodeHash     string
	CreatedBy    string
	ExpiresAt    time.Time
	// Namespace and Agent: on a shared chat app, the route the sender joins.
	Namespace string
	Agent     string
	// The rest is empty until a message with the code arrives.
	ReceivedAt  *time.Time
	SenderID    string
	SenderName  string
	ChannelID   string
	Mode        string
	ReplyTarget json.RawMessage
}

// ErrPairingNotFound: no such pairing, or it has expired.
var ErrPairingNotFound = errors.New("the pairing was not found or has expired")

const pairingCols = `p.id, p.tenant_id, p.connection_id, p.created_by, p.expires_at, p.received_at, p.sender_id, p.sender_name,
	p.channel_id, p.mode, p.reply_target, p.namespace, p.agent`

func scanPairing(row interface{ Scan(...any) error }) (InboundPairing, error) {
	var p InboundPairing
	var received sql.NullTime
	var target []byte
	err := row.Scan(&p.ID, &p.TenantID, &p.ConnectionID, &p.CreatedBy, &p.ExpiresAt, &received, &p.SenderID, &p.SenderName,
		&p.ChannelID, &p.Mode, &target, &p.Namespace, &p.Agent)
	if received.Valid {
		v := received.Time
		p.ReceivedAt = &v
	}
	p.ReplyTarget = target
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrPairingNotFound
	}
	return p, err
}

// CreateInboundPairing stores a new code. A connection has one at a time: an
// earlier one, and every expired one of the tenant, is removed.
func (s *Store) CreateInboundPairing(ctx context.Context, p InboundPairing) error {
	return wrapInbound("creating a pairing", s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, p.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM inbound_pairings WHERE tenant_id = $1 AND (connection_id = $2 OR expires_at < now())`,
			tenant, p.ConnectionID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO inbound_pairings (id, tenant_id, connection_id, code_hash, created_by, expires_at, namespace, agent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.ID, tenant, p.ConnectionID, p.CodeHash, p.CreatedBy, p.ExpiresAt, p.Namespace, p.Agent)
		return err
	}))
}

// ReceiveInboundPairing records who sent a code. It reports false unless the
// connection has an unused, unexpired pairing with that code; a code works once.
func (s *Store) ReceiveInboundPairing(ctx context.Context, p InboundPairing) (bool, error) {
	matched := false
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var target any
		if len(p.ReplyTarget) > 0 {
			target = string(p.ReplyTarget)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE inbound_pairings SET received_at = now(), sender_id = $4, sender_name = $5, channel_id = $6, mode = $7, reply_target = $8::jsonb
			WHERE connection_id = $1 AND code_hash = $2 AND received_at IS NULL AND expires_at > now()
			  AND ($3::text = '*' OR tenant_id = $3)`,
			p.ConnectionID, p.CodeHash, scope, p.SenderID, p.SenderName, p.ChannelID, p.Mode, target)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		matched = n == 1
		return err
	})
	return matched, wrapInbound("receiving a pairing", err)
}

// GetInboundPairing returns an unexpired pairing of a connection.
func (s *Store) GetInboundPairing(ctx context.Context, connectionID, id string) (InboundPairing, error) {
	var p InboundPairing
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		p, err = scanPairing(tx.QueryRowContext(ctx, `SELECT `+pairingCols+` FROM inbound_pairings p
			WHERE p.connection_id = $1 AND p.id = $2 AND p.expires_at > now() AND ($3::text = '*' OR p.tenant_id = $3)`,
			connectionID, id, scope))
		return err
	})
	if errors.Is(err, ErrPairingNotFound) {
		return p, ErrPairingNotFound
	}
	return p, wrapInbound("reading a pairing", err)
}

// TakeInboundPairing deletes a pairing and returns what it held, so that only one
// caller can act on it and the sender's id is not kept.
func (s *Store) TakeInboundPairing(ctx context.Context, connectionID, id string) (InboundPairing, error) {
	var p InboundPairing
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		p, err = scanPairing(tx.QueryRowContext(ctx, `DELETE FROM inbound_pairings p
			WHERE p.connection_id = $1 AND p.id = $2 AND p.expires_at > now() AND ($3::text = '*' OR p.tenant_id = $3)
			RETURNING `+pairingCols, connectionID, id, scope))
		return err
	})
	if errors.Is(err, ErrPairingNotFound) {
		return p, ErrPairingNotFound
	}
	return p, wrapInbound("taking a pairing", err)
}

// DeleteExpiredInboundPairings removes expired pairings, and with them the ids of
// senders nobody approved. Run on the platform scope.
func (s *Store) DeleteExpiredInboundPairings(ctx context.Context) error {
	return wrapInbound("pruning pairings", s.scoped(ctx, func(tx dbtx, scope string) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM inbound_pairings WHERE expires_at < now() AND ($1::text = '*' OR tenant_id = $1)`, scope)
		return err
	}))
}
