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

// Versioned schema migrations. Each file in migrations/ is named
// NNNN_description.sql and runs once, in version order, in its own
// transaction; schema_migrations records what has run. Migrations are never
// edited after they ship: change the schema with a new file. New tables carry a
// tenant column so queries can be tenant-scoped.

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey serializes migrations across operator replicas that start
// together (pg_advisory_lock). Arbitrary but fixed.
const migrationLockKey = 7_413_602_118

// migration is one schema change.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// migrations returns the embedded migrations in version order.
func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		version, err := strconv.Atoi(num)
		if !ok || err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q is not named NNNN_description.sql", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(migrationFiles, path.Join("migrations", name))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{Version: version, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies the migrations db has not run yet and returns how many it
// applied. It holds an advisory lock for the duration, so replicas starting at
// once migrate one at a time.
func Migrate(ctx context.Context, db *sql.DB) (int, error) {
	all, err := migrations()
	if err != nil {
		return 0, err
	}
	// The advisory lock belongs to a session, so lock, migrate and unlock on
	// one connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrations: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return 0, fmt.Errorf("migrations: taking the migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`); err != nil {
		return 0, fmt.Errorf("migrations: creating schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("migrations: reading schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return 0, err
		}
		applied[v] = true
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	n := 0
	for _, m := range all {
		if applied[m.Version] {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
		return fmt.Errorf("migration %s: recording it: %w", m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	return nil
}
