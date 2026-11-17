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
	"errors"
	"fmt"
	"time"

	"github.com/heddles/agent-orca/internal/tenancy"
)

var _ tenancy.Registry = (*Store)(nil)

const tenantColumns = `id, name, k8s_namespace, k8s_uid, state, created_at, deleted_at`

func scanTenant(row interface{ Scan(...any) error }) (tenancy.Record, error) {
	var r tenancy.Record
	var state string
	var deleted sql.NullTime
	if err := row.Scan(&r.ID, &r.Name, &r.Namespace, &r.UID, &state, &r.CreatedAt, &deleted); err != nil {
		return r, err
	}
	r.State = tenancy.State(state)
	if deleted.Valid {
		t := deleted.Time
		r.DeletedAt = &t
	}
	return r, nil
}

// GetTenant implements tenancy.Registry.
func (s *Store) GetTenant(ctx context.Context, id string) (*tenancy.Record, error) {
	r, err := scanTenant(s.db.QueryRowContext(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading tenant %s: %w", id, err)
	}
	return &r, nil
}

// PutTenant implements tenancy.Registry. created_at is set on insert only.
func (s *Store) PutTenant(ctx context.Context, r tenancy.Record) error {
	var deleted sql.NullTime
	if r.DeletedAt != nil {
		deleted = sql.NullTime{Time: *r.DeletedAt, Valid: true}
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tenants (id, name, k8s_namespace, k8s_uid, state, created_at, deleted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, k8s_namespace = EXCLUDED.k8s_namespace, k8s_uid = EXCLUDED.k8s_uid,
    state = EXCLUDED.state, deleted_at = EXCLUDED.deleted_at`,
		r.ID, r.Name, r.Namespace, r.UID, string(r.State), created, deleted)
	if err != nil {
		return fmt.Errorf("writing tenant %s: %w", r.ID, err)
	}
	return nil
}

// LiveTenantsByName implements tenancy.Registry.
func (s *Store) LiveTenantsByName(ctx context.Context, namespace, name string) ([]tenancy.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tenantColumns+` FROM tenants
WHERE k8s_namespace = $1 AND name = $2 AND state <> 'deleted'`, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []tenancy.Record
	for rows.Next() {
		r, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
