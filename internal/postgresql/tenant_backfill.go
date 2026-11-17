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
	"fmt"
	"log/slog"
)

// tenantTables are the tables that carry tenant_id (migration 0009).
var tenantTables = []string{
	"archived_runs", "approvals", "routing_events", "run_outcomes",
	"routing_feedback", "shadow_results", "shadow_leases",
}

// BackfillTenantIDs attributes rows that migration 0009 could not, using the
// namespace to tenant binding the registry does not hold (a TenantConfig's
// allowedNamespaces). resolve returns the tenant ID that owns a namespace, or
// "" when none or several do; those rows stay unattributed, visible only to the
// platform scope, and are logged. It runs as the platform and is safe to repeat.
// It returns the number of rows still unattributed per table.
func (s *Store) BackfillTenantIDs(ctx context.Context, resolve func(namespace string) string) (map[string]int64, error) {
	left := map[string]int64{}
	err := s.inScopeValue(ctx, PlatformScope, func(tx dbtx, _ string) error {
		for _, table := range tenantTables {
			if err := backfillTable(ctx, tx, table, resolve); err != nil {
				return fmt.Errorf("backfilling %s: %w", table, err)
			}
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id IS NULL`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				left[table] = n
				slog.Warn("tenant_id backfill: rows cannot be attributed to a tenant", "table", table, "rows", n)
			}
		}
		return nil
	})
	return left, err
}

// backfillTable assigns each namespace's unattributed rows to its tenant.
func backfillTable(ctx context.Context, tx dbtx, table string, resolve func(string) string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT namespace FROM `+table+` WHERE tenant_id IS NULL`)
	if err != nil {
		return err
	}
	var namespaces []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			_ = rows.Close()
			return err
		}
		namespaces = append(namespaces, ns)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	for _, ns := range namespaces {
		id := resolve(ns)
		if id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET tenant_id = $1 WHERE namespace = $2 AND tenant_id IS NULL`, id, ns); err != nil {
			return err
		}
	}
	return nil
}
