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
)

// ScheduleDestination is the Slack channel an administrator chose in the UI for
// the reports of a schedule (table schedule_destinations).
type ScheduleDestination struct {
	TenantID    string
	Namespace   string
	Name        string
	App         string
	ChannelID   string
	ChannelName string
	SetBy       string
	SetAt       time.Time
}

const scheduleDestinationCols = `tenant_id, namespace, name, app, channel_id, channel_name, set_by, set_at`

func scanScheduleDestination(row interface{ Scan(...any) error }) (ScheduleDestination, error) {
	var d ScheduleDestination
	err := row.Scan(&d.TenantID, &d.Namespace, &d.Name, &d.App, &d.ChannelID, &d.ChannelName, &d.SetBy, &d.SetAt)
	return d, err
}

// SaveScheduleDestination stores the choice, replacing an earlier one.
func (s *Store) SaveScheduleDestination(ctx context.Context, d ScheduleDestination) error {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, d.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO schedule_destinations (tenant_id, namespace, name, app, channel_id, channel_name, set_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, namespace, name) DO UPDATE SET
			  app = EXCLUDED.app, channel_id = EXCLUDED.channel_id, channel_name = EXCLUDED.channel_name,
			  set_by = EXCLUDED.set_by, set_at = now()`,
			tenant, d.Namespace, d.Name, d.App, d.ChannelID, d.ChannelName, d.SetBy)
		return err
	})
	if err != nil {
		return fmt.Errorf("saving the schedule's destination: %w", err)
	}
	return nil
}

// GetScheduleDestination reads one tenant's choice for a schedule; found is
// false when there is none.
func (s *Store) GetScheduleDestination(ctx context.Context, tenantID, namespace, name string) (d ScheduleDestination, found bool, err error) {
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		var serr error
		d, serr = scanScheduleDestination(tx.QueryRowContext(ctx, `
			SELECT `+scheduleDestinationCols+` FROM schedule_destinations
			WHERE ($1::text = '*' OR tenant_id = $1) AND tenant_id = $2 AND namespace = $3 AND name = $4`,
			scope, tenantID, namespace, name))
		if errors.Is(serr, sql.ErrNoRows) {
			return nil
		}
		found = serr == nil
		return serr
	})
	if err != nil {
		return ScheduleDestination{}, false, fmt.Errorf("reading the schedule's destination: %w", err)
	}
	return d, found, nil
}

// ListScheduleDestinations returns the choices the call's scope can see.
func (s *Store) ListScheduleDestinations(ctx context.Context) ([]ScheduleDestination, error) {
	var out []ScheduleDestination
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+scheduleDestinationCols+` FROM schedule_destinations
			WHERE ($1::text = '*' OR tenant_id = $1)`, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			d, err := scanScheduleDestination(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing schedule destinations: %w", err)
	}
	return out, nil
}

// DeleteScheduleDestination removes one tenant's choice and reports whether
// there was one.
func (s *Store) DeleteScheduleDestination(ctx context.Context, tenantID, namespace, name string) (bool, error) {
	var n int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM schedule_destinations
			WHERE ($1::text = '*' OR tenant_id = $1) AND tenant_id = $2 AND namespace = $3 AND name = $4`,
			scope, tenantID, namespace, name)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, fmt.Errorf("removing the schedule's destination: %w", err)
	}
	return n > 0, nil
}

// DeleteScheduleDestinationsOf removes every choice for a schedule that was
// deleted, so that a later schedule of the same name does not inherit one.
func (s *Store) DeleteScheduleDestinationsOf(ctx context.Context, namespace, name string) error {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM schedule_destinations
			WHERE ($1::text = '*' OR tenant_id = $1) AND namespace = $2 AND name = $3`, scope, namespace, name)
		return err
	})
	if err != nil {
		return fmt.Errorf("removing a deleted schedule's destinations: %w", err)
	}
	return nil
}
