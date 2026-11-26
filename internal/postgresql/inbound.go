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
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Limits of an inbound connection.
const (
	MaxInboundConnectionsPerAgent = 10
	maxInboundNameLen             = 80
	maxInboundConfigBytes         = 16 << 10
	inboundIDPrefix               = "inb_"

	// A message claimed but never launched (the process died in between) may be
	// claimed again after this long.
	inboundClaimStale = 5 * time.Minute
)

// Statuses of an inbound message.
const (
	InboundReceived = "received" // claimed, run not started yet
	InboundRunning  = "running"  // run started, reply pending
	InboundReplied  = "replied"
	InboundRejected = "rejected" // refused by policy
	InboundIgnored  = "ignored"  // a message nobody should answer (auto-reply, loop)
	InboundFailed   = "failed"
)

var (
	ErrInboundNotFound = errors.New("inbound connection not found")
	ErrInvalidInbound  = errors.New("invalid inbound connection")
	ErrInboundExists   = errors.New("the agent already has an inbound connection with that name")
	ErrTooManyInbound  = errors.New("the agent has the most inbound connections it may have")
)

var inboundSecretRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// InboundConnection is one channel into one agent (table inbound_connections).
type InboundConnection struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenantId,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Enabled   bool   `json:"enabled"`
	// Config is the adapter's NON-secret settings.
	Config json.RawMessage `json:"config"`
	// SecretRef names the Kubernetes Secret (in Namespace) with the credentials.
	SecretRef string    `json:"secretRef"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	LastPollAt    *time.Time `json:"lastPollAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	ReceivedCount int64      `json:"receivedCount"`
	RepliedCount  int64      `json:"repliedCount"`
	RejectedCount int64      `json:"rejectedCount"`
}

// NewInboundID makes the public id of a connection.
func NewInboundID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return inboundIDPrefix + hex.EncodeToString(b[:]), nil
}

// ValidInboundID reports whether id has the shape NewInboundID makes.
func ValidInboundID(id string) bool {
	rest, ok := strings.CutPrefix(id, inboundIDPrefix)
	if !ok || len(rest) != 32 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// HashKey is the digest the ledger stores in place of an identifier.
func HashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Validate reports why c cannot be stored.
func (c InboundConnection) Validate() error {
	switch {
	case c.Namespace == "" || !evalObjectNameRe.MatchString(c.Agent):
		return fmt.Errorf("%w: a connection belongs to a namespace and an agent", ErrInvalidInbound)
	case strings.TrimSpace(c.Name) == "" || len(c.Name) > maxInboundNameLen:
		return fmt.Errorf("%w: name is required and at most %d characters", ErrInvalidInbound, maxInboundNameLen)
	case c.Kind == "" || len(c.Kind) > 32:
		return fmt.Errorf("%w: kind is required", ErrInvalidInbound)
	case !ValidInboundID(c.ID):
		return fmt.Errorf("%w: id is required", ErrInvalidInbound)
	case c.SecretRef != "" && (len(c.SecretRef) > 253 || !inboundSecretRe.MatchString(c.SecretRef)):
		return fmt.Errorf("%w: secretRef is not a Secret name", ErrInvalidInbound)
	case len(c.Config) > maxInboundConfigBytes || (len(c.Config) > 0 && !json.Valid(c.Config)):
		return fmt.Errorf("%w: config must be JSON of at most %d bytes", ErrInvalidInbound, maxInboundConfigBytes)
	case c.CreatedBy == "":
		return fmt.Errorf("%w: created by is required", ErrInvalidInbound)
	}
	return nil
}

const inboundCols = `c.id, c.tenant_id, c.kind, c.name, c.namespace, c.agent, c.enabled, c.config, c.secret_ref, c.created_by,
	c.created_at, c.updated_at, c.last_poll_at, c.last_error, c.received_count, c.replied_count, c.rejected_count`

func scanInbound(row interface{ Scan(...any) error }) (InboundConnection, error) {
	var c InboundConnection
	var cfg []byte
	var polled sql.NullTime
	err := row.Scan(&c.ID, &c.TenantID, &c.Kind, &c.Name, &c.Namespace, &c.Agent, &c.Enabled, &cfg, &c.SecretRef, &c.CreatedBy,
		&c.CreatedAt, &c.UpdatedAt, &polled, &c.LastError, &c.ReceivedCount, &c.RepliedCount, &c.RejectedCount)
	c.Config = json.RawMessage(cfg)
	if polled.Valid {
		v := polled.Time
		c.LastPollAt = &v
	}
	return c, err
}

func wrapInbound(what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInboundNotFound
	}
	for _, sentinel := range []error{ErrInboundNotFound, ErrInvalidInbound, ErrInboundExists, ErrTooManyInbound, ErrNoTenantScope} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func configOrEmpty(c json.RawMessage) []byte {
	if len(c) == 0 {
		return []byte("{}")
	}
	return c
}

// CreateInboundConnection stores a connection under the call's tenant.
func (s *Store) CreateInboundConnection(ctx context.Context, c InboundConnection) (InboundConnection, error) {
	if err := c.Validate(); err != nil {
		return InboundConnection{}, err
	}
	var out InboundConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, c.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM inbound_connections WHERE tenant_id = $1 AND namespace = $2 AND agent = $3`,
			tenant, c.Namespace, c.Agent).Scan(&n); err != nil {
			return err
		}
		if n >= MaxInboundConnectionsPerAgent {
			return ErrTooManyInbound
		}
		var dup bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM inbound_connections WHERE tenant_id = $1 AND namespace = $2 AND agent = $3 AND name = $4)`,
			tenant, c.Namespace, c.Agent, c.Name).Scan(&dup); err != nil {
			return err
		}
		if dup {
			return ErrInboundExists
		}
		out, err = scanInbound(tx.QueryRowContext(ctx, `
			INSERT INTO inbound_connections AS c (id, tenant_id, kind, name, namespace, agent, config, secret_ref, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
			RETURNING `+inboundCols,
			c.ID, tenant, c.Kind, c.Name, c.Namespace, c.Agent, string(configOrEmpty(c.Config)), c.SecretRef, c.CreatedBy))
		return err
	})
	if err != nil {
		return InboundConnection{}, wrapInbound("creating the inbound connection", err)
	}
	return out, nil
}

// ListInboundConnections returns the connections of the namespaces (all when
// empty), optionally of one agent, oldest first. The platform scope sees every
// tenant's: the operator's pollers use it.
func (s *Store) ListInboundConnections(ctx context.Context, namespaces []string, agent string) ([]InboundConnection, error) {
	out := []InboundConnection{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+inboundCols+` FROM inbound_connections c
			WHERE ($1::text = '*' OR c.tenant_id = $1)
			  AND (cardinality($2::text[]) = 0 OR c.namespace = ANY($2))
			  AND ($3 = '' OR c.agent = $3)
			ORDER BY c.created_at, c.id`, scope, pgTextArray(namespaces), agent)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			c, err := scanInbound(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapInbound("listing inbound connections", err)
	}
	return out, nil
}

// GetInboundConnection returns one connection of the call's scope.
func (s *Store) GetInboundConnection(ctx context.Context, id string) (InboundConnection, error) {
	var c InboundConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		c, err = scanInbound(tx.QueryRowContext(ctx,
			`SELECT `+inboundCols+` FROM inbound_connections c WHERE c.id = $1 AND ($2::text = '*' OR c.tenant_id = $2)`, id, scope))
		return err
	})
	if err != nil {
		return InboundConnection{}, wrapInbound("reading the inbound connection", err)
	}
	return c, nil
}

// InboundPatch is the editable part of a connection; nil fields stay.
type InboundPatch struct {
	Enabled   *bool
	Config    *json.RawMessage
	SecretRef *string
}

// UpdateInboundConnection changes the editable fields and returns the row.
func (s *Store) UpdateInboundConnection(ctx context.Context, id string, p InboundPatch) (InboundConnection, error) {
	var c InboundConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanInbound(tx.QueryRowContext(ctx,
			`SELECT `+inboundCols+` FROM inbound_connections c WHERE c.id = $1 AND ($2::text = '*' OR c.tenant_id = $2) FOR UPDATE`, id, scope))
		if err != nil {
			return err
		}
		if p.Enabled != nil {
			cur.Enabled = *p.Enabled
		}
		if p.Config != nil {
			cur.Config = *p.Config
		}
		if p.SecretRef != nil {
			cur.SecretRef = *p.SecretRef
		}
		if err := cur.Validate(); err != nil {
			return err
		}
		c, err = scanInbound(tx.QueryRowContext(ctx, `
			UPDATE inbound_connections c SET enabled = $3, config = $4::jsonb, secret_ref = $5, updated_at = now()
			WHERE c.id = $1 AND ($2::text = '*' OR c.tenant_id = $2) RETURNING `+inboundCols,
			id, scope, cur.Enabled, string(configOrEmpty(cur.Config)), cur.SecretRef))
		return err
	})
	if err != nil {
		return InboundConnection{}, wrapInbound("updating the inbound connection", err)
	}
	return c, nil
}

// DeleteInboundConnection removes a connection and its ledger, and returns it.
func (s *Store) DeleteInboundConnection(ctx context.Context, id string) (InboundConnection, error) {
	var c InboundConnection
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		c, err = scanInbound(tx.QueryRowContext(ctx,
			`DELETE FROM inbound_connections c WHERE c.id = $1 AND ($2::text = '*' OR c.tenant_id = $2) RETURNING `+inboundCols, id, scope))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM inbound_messages WHERE tenant_id = $1 AND connection_id = $2`, c.TenantID, id)
		return err
	})
	if err != nil {
		return InboundConnection{}, wrapInbound("deleting the inbound connection", err)
	}
	return c, nil
}

// InboundStatus is what a poll or a message changes on the connection's row.
type InboundStatus struct {
	PolledAt *time.Time
	// SetError replaces last_error (empty clears it) when true.
	SetError  bool
	LastError string
	Received  int
	Replied   int
	Rejected  int
}

// RecordInboundStatus updates the status fields of a connection.
func (s *Store) RecordInboundStatus(ctx context.Context, id string, st InboundStatus) error {
	return wrapInbound("recording the inbound status", s.scoped(ctx, func(tx dbtx, scope string) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE inbound_connections SET
			  last_poll_at   = COALESCE($3, last_poll_at),
			  last_error     = CASE WHEN $4 THEN $5 ELSE last_error END,
			  received_count = received_count + $6,
			  replied_count  = replied_count + $7,
			  rejected_count = rejected_count + $8
			WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`,
			id, scope, st.PolledAt, st.SetError, st.LastError, st.Received, st.Replied, st.Rejected)
		return err
	}))
}

// InboundMessage is a ledger row (table inbound_messages). It holds digests and
// sizes, never the message.
type InboundMessage struct {
	ID           int64
	TenantID     string
	ConnectionID string
	MessageKey   string
	ThreadKey    string
	SenderHash   string
	SizeBytes    int
	Status       string
	Reason       string
	RunNamespace string
	RunName      string
	// ReplyTarget is set only while a reply is pending.
	ReplyTarget json.RawMessage
	ReceivedAt  time.Time
	UpdatedAt   time.Time
}

const inboundMsgCols = `m.id, m.tenant_id, m.connection_id, m.message_key, m.thread_key, m.sender_hash, m.size_bytes, m.status,
	m.reason, m.run_namespace, m.run_name, m.reply_target, m.received_at, m.updated_at`

func scanInboundMsg(row interface{ Scan(...any) error }) (InboundMessage, error) {
	var m InboundMessage
	var rt []byte
	err := row.Scan(&m.ID, &m.TenantID, &m.ConnectionID, &m.MessageKey, &m.ThreadKey, &m.SenderHash, &m.SizeBytes, &m.Status,
		&m.Reason, &m.RunNamespace, &m.RunName, &rt, &m.ReceivedAt, &m.UpdatedAt)
	m.ReplyTarget = json.RawMessage(rt)
	return m, err
}

// ClaimInboundMessage records a message as received. It reports false when the
// connection already has that message (a redelivery), unless the earlier claim
// never got as far as starting a run and is older than five minutes.
func (s *Store) ClaimInboundMessage(ctx context.Context, m InboundMessage) (bool, error) {
	claimed := false
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, m.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		var id int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO inbound_messages AS m (tenant_id, connection_id, message_key, thread_key, sender_hash, size_bytes, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'received')
			ON CONFLICT (tenant_id, connection_id, message_key) DO UPDATE SET updated_at = now()
			  WHERE m.status = 'received' AND m.updated_at < now() - make_interval(secs => $7)
			RETURNING m.id`,
			tenant, m.ConnectionID, m.MessageKey, m.ThreadKey, m.SenderHash, m.SizeBytes, inboundClaimStale.Seconds()).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, wrapInbound("claiming an inbound message", err)
	}
	return claimed, nil
}

// InboundUpdate sets the outcome of a ledger row. A nil ReplyTarget clears it
// only when ClearTarget is true.
type InboundUpdate struct {
	Status       string
	Reason       string
	RunNamespace string
	RunName      string
	ReplyTarget  json.RawMessage
	ClearTarget  bool
}

// UpdateInboundMessage sets the outcome of a message.
func (s *Store) UpdateInboundMessage(ctx context.Context, connectionID, messageKey string, u InboundUpdate) error {
	return wrapInbound("updating an inbound message", s.scoped(ctx, func(tx dbtx, scope string) error {
		var target any
		if len(u.ReplyTarget) > 0 {
			target = string(u.ReplyTarget)
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE inbound_messages SET status = $4, reason = $5,
			  run_namespace = CASE WHEN $6 <> '' THEN $6 ELSE run_namespace END,
			  run_name = CASE WHEN $7 <> '' THEN $7 ELSE run_name END,
			  reply_target = CASE WHEN $9 THEN NULL WHEN $8::jsonb IS NOT NULL THEN $8::jsonb ELSE reply_target END,
			  updated_at = now()
			WHERE connection_id = $1 AND message_key = $2 AND ($3::text = '*' OR tenant_id = $3)`,
			connectionID, messageKey, scope, u.Status, u.Reason, u.RunNamespace, u.RunName, target, u.ClearTarget)
		return err
	}))
}

// ListInboundMessagesByStatus returns the oldest messages in a status, for the
// reply worker. The platform scope sees every tenant's.
func (s *Store) ListInboundMessagesByStatus(ctx context.Context, status string, limit int) ([]InboundMessage, error) {
	var out []InboundMessage
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+inboundMsgCols+` FROM inbound_messages m
			WHERE m.status = $1 AND ($2::text = '*' OR m.tenant_id = $2)
			ORDER BY m.updated_at LIMIT $3`, status, scope, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			m, err := scanInboundMsg(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, wrapInbound("listing inbound messages", err)
}

// LatestInboundRunInThread returns the run of the newest answered message of a
// thread, for continuing the conversation.
func (s *Store) LatestInboundRunInThread(ctx context.Context, connectionID, threadKey string) (ns, name string, err error) {
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		return tx.QueryRowContext(ctx, `
			SELECT m.run_namespace, m.run_name FROM inbound_messages m
			WHERE m.connection_id = $1 AND m.thread_key = $2 AND m.thread_key <> '' AND m.run_name <> ''
			  AND m.status IN ('running', 'replied') AND ($3::text = '*' OR m.tenant_id = $3)
			ORDER BY m.received_at DESC LIMIT 1`, connectionID, threadKey, scope).Scan(&ns, &name)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return ns, name, wrapInbound("finding the thread", err)
}

// FindRunningInboundMessage returns the message whose answer a run will be, with
// the reply target the adapter needs: for telling the thread something while the
// run is still working. found is false once the message is answered.
func (s *Store) FindRunningInboundMessage(ctx context.Context, namespace, runName string) (m InboundMessage, found bool, err error) {
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		row := tx.QueryRowContext(ctx, `
			SELECT `+inboundMsgCols+` FROM inbound_messages m
			WHERE m.run_namespace = $1 AND m.run_name = $2 AND m.status = 'running'
			  AND ($3::text = '*' OR m.tenant_id = $3)
			ORDER BY m.received_at DESC LIMIT 1`, namespace, runName, scope)
		var serr error
		m, serr = scanInboundMsg(row)
		return serr
	})
	if errors.Is(err, sql.ErrNoRows) {
		return InboundMessage{}, false, nil
	}
	return m, err == nil, wrapInbound("finding the message of a run", err)
}

// CountInboundBySender counts the messages of one sender that started a run
// since the given time: the basis of the per-sender cap.
func (s *Store) CountInboundBySender(ctx context.Context, connectionID, senderHash string, since time.Time) (int, error) {
	var n int
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		return tx.QueryRowContext(ctx, `
			SELECT count(*) FROM inbound_messages m
			WHERE m.connection_id = $1 AND m.sender_hash = $2 AND m.received_at >= $3
			  AND m.status IN ('running', 'replied', 'failed') AND ($4::text = '*' OR m.tenant_id = $4)`,
			connectionID, senderHash, since, scope).Scan(&n)
	})
	return n, wrapInbound("counting inbound messages", err)
}

// InboundRefusals counts, per connection and reason, the messages refused since
// the given time: those policy rejected, and those from another workspace, which
// is what a wrong workspace id looks like. It holds counts only.
func (s *Store) InboundRefusals(ctx context.Context, connectionIDs []string, since time.Time) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	if len(connectionIDs) == 0 {
		return out, nil
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT m.connection_id, m.reason, count(*) FROM inbound_messages m
			WHERE m.connection_id = ANY($1) AND m.received_at >= $2
			  AND (m.status = 'rejected' OR (m.status = 'ignored' AND m.reason = 'workspace_mismatch'))
			  AND ($3::text = '*' OR m.tenant_id = $3)
			GROUP BY m.connection_id, m.reason`, pgTextArray(connectionIDs), since, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, reason string
			var n int
			if err := rows.Scan(&id, &reason, &n); err != nil {
				return err
			}
			if out[id] == nil {
				out[id] = map[string]int{}
			}
			out[id][reason] = n
		}
		return rows.Err()
	})
	return out, wrapInbound("counting refused messages", err)
}

// DeleteInboundMessagesBefore removes ledger rows older than cutoff that are no
// longer pending, and returns how many. Run on the platform scope.
func (s *Store) DeleteInboundMessagesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM inbound_messages WHERE received_at < $1 AND status NOT IN ('received', 'running')
			  AND ($2::text = '*' OR tenant_id = $2)`, cutoff, scope)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, wrapInbound("pruning the inbound ledger", err)
}
