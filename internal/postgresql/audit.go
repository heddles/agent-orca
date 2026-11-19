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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Audit actions.
const (
	AuditCreate  = "create"
	AuditUpdate  = "update"
	AuditDelete  = "delete"
	AuditCancel  = "cancel"
	AuditRequest = "request"
	AuditApprove = "approve"
	AuditReject  = "reject"
	// AuditOverride is a publish that went ahead past a failing evaluation gate
	// (docs/evaluations.md). The normal publish row is written too.
	AuditOverride = "override"
	// AuditExport is a dataset's cases leaving as a JSONL download.
	AuditExport = "export"
)

// MaxAuditSummaryBytes bounds the summary of an audit event.
const MaxAuditSummaryBytes = 1024

// ErrInvalidAudit is returned for an audit event the table would not take.
var ErrInvalidAudit = errors.New("invalid audit event")

// AuditEvent is one change made through the UI API, or one approval decision
// (table audit_events, migration 0011).
type AuditEvent struct {
	ID int64 `json:"id"`
	// TenantID is the tenant's stable ID. On an append, empty takes the call's
	// tenant scope; a platform-scoped append with none is stored unattributed.
	TenantID        string    `json:"tenantId,omitempty"`
	UserID          string    `json:"userId"`
	Action          string    `json:"action"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace,omitempty"`
	Name            string    `json:"name,omitempty"`
	ResourceVersion string    `json:"resourceVersion,omitempty"`
	Summary         string    `json:"summary,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

// AuditQuery filters ListAudit. Empty fields match everything.
type AuditQuery struct {
	// TenantID narrows a platform-scoped query to one tenant. A tenant-scoped call
	// is always limited to its own tenant, whatever this says.
	TenantID string
	// Namespaces limits the query to a tenant's namespaces.
	Namespaces []string
	Kind       string
	Namespace  string
	Name       string
	UserID     string
	Action     string
	// Before is a cursor: only events with an ID below it. Zero starts at the newest.
	Before int64
	Limit  int
}

// AppendAudit stores an event and returns it with its ID and time.
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) (AuditEvent, error) {
	if e.Action == "" || e.Kind == "" {
		return AuditEvent{}, fmt.Errorf("%w: action and kind are required", ErrInvalidAudit)
	}
	if len(e.Summary) > MaxAuditSummaryBytes {
		e.Summary = strings.ToValidUTF8(e.Summary[:MaxAuditSummaryBytes], "")
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tid, err := rowTenant(scope, e.TenantID)
		if err != nil {
			return err
		}
		e.TenantID = tid
		return tx.QueryRowContext(ctx, `
		INSERT INTO audit_events (tenant_id, user_id, action, kind, namespace, name, resource_version, summary)
		VALUES (NULLIF($1, ''), $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`,
			tid, e.UserID, e.Action, e.Kind, e.Namespace, e.Name, e.ResourceVersion, e.Summary,
		).Scan(&e.ID, &e.CreatedAt)
	})
	if err != nil {
		return AuditEvent{}, fmt.Errorf("storing audit event: %w", err)
	}
	return e, nil
}

// ListAudit returns events, newest first.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditEvent, error) {
	out := []AuditEvent{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var where []string
		var args []any
		arg := func(v any) string {
			args = append(args, v)
			return fmt.Sprintf("$%d", len(args))
		}
		if cond, ok := tenantFilter(scope, q.TenantID, arg); ok {
			where = append(where, cond)
		}
		if len(q.Namespaces) > 0 {
			ph := make([]string, len(q.Namespaces))
			for i, ns := range q.Namespaces {
				ph[i] = arg(ns)
			}
			where = append(where, "namespace IN ("+strings.Join(ph, ", ")+")")
		}
		filters := map[string]string{"kind": q.Kind, "namespace": q.Namespace, "name": q.Name, "user_id": q.UserID, "action": q.Action}
		cols := make([]string, 0, len(filters))
		for col := range filters {
			cols = append(cols, col)
		}
		sort.Strings(cols)
		for _, col := range cols {
			if v := filters[col]; v != "" {
				where = append(where, col+" = "+arg(v))
			}
		}
		if q.Before > 0 {
			where = append(where, "id < "+arg(q.Before))
		}
		limit := q.Limit
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		query := `SELECT id, COALESCE(tenant_id, ''), user_id, action, kind, namespace, name, resource_version, summary, created_at FROM audit_events`
		if len(where) > 0 {
			query += " WHERE " + strings.Join(where, " AND ")
		}
		query += " ORDER BY id DESC LIMIT " + arg(limit)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var e AuditEvent
			if err := rows.Scan(&e.ID, &e.TenantID, &e.UserID, &e.Action, &e.Kind, &e.Namespace, &e.Name, &e.ResourceVersion, &e.Summary, &e.CreatedAt); err != nil {
				return fmt.Errorf("scanning audit event: %w", err)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing audit events: %w", err)
	}
	return out, nil
}
