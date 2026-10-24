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
	"os"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// migration0001 returns the run-archive migration's SQL.
func migration0001(t *testing.T) string {
	t.Helper()
	all, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[0].Version != 1 {
		t.Fatalf("first migration = %+v, want version 1", all)
	}
	return all[0].SQL
}

func TestMigrationsAreOrderedAndNamed(t *testing.T) {
	all, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range all {
		if m.Version != i+1 {
			t.Errorf("migration %s has version %d at position %d: versions must be 1..n without gaps", m.Name, m.Version, i)
		}
		if m.SQL == "" {
			t.Errorf("migration %s is empty", m.Name)
		}
	}
}

func expectLockAndTable(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_lock($1)`)).WithArgs(migrationLockKey).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS schema_migrations`).WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestMigrateAppliesPendingMigrations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	all, _ := migrations()

	expectLockAndTable(mock)
	mock.ExpectQuery(`SELECT version FROM schema_migrations`).WillReturnRows(sqlmock.NewRows([]string{"version"}))
	for _, m := range all {
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta(m.SQL[:40])).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`INSERT INTO schema_migrations`).WithArgs(m.Version, m.Name).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
	}
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_unlock($1)`)).WithArgs(migrationLockKey).WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := Migrate(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(all) {
		t.Errorf("applied %d, want %d", n, len(all))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMigrateSkipsAppliedMigrations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	all, _ := migrations()

	expectLockAndTable(mock)
	rows := sqlmock.NewRows([]string{"version"})
	for _, m := range all {
		rows.AddRow(m.Version)
	}
	mock.ExpectQuery(`SELECT version FROM schema_migrations`).WillReturnRows(rows)
	mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_unlock($1)`)).WithArgs(migrationLockKey).WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := Migrate(context.Background(), db)
	if err != nil || n != 0 {
		t.Fatalf("Migrate = %d, %v; want nothing applied", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMigrateAgainstPostgres runs the migrations on a real database when
// AGENTORCA_TEST_POSTGRES_DSN names one (e.g. a throwaway container): from
// scratch, again (a no-op), and onto a database that predates versioned
// migrations (the old schema exists but schema_migrations does not).
// The test database is throwaway: each test drops its whole schema.
func TestMigrateAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("AGENTORCA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AGENTORCA_TEST_POSTGRES_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	reset := func() {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := migrations()

	reset()
	if n, err := Migrate(ctx, db); err != nil || n != len(all) {
		t.Fatalf("fresh: applied %d, %v; want %d", n, err, len(all))
	}
	if n, err := Migrate(ctx, db); err != nil || n != 0 {
		t.Fatalf("second run: applied %d, %v; want 0", n, err)
	}

	// A database from before versioned migrations: archived_runs exists.
	reset()
	if _, err := db.ExecContext(ctx, all[0].SQL); err != nil {
		t.Fatal(err)
	}
	if n, err := Migrate(ctx, db); err != nil || n != len(all) {
		t.Fatalf("adopting a pre-migration database: applied %d, %v", n, err)
	}
	var cols int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'archived_runs' AND column_name IN ('trace_events', 'cost_capture')`).Scan(&cols); err != nil || cols != 2 {
		t.Errorf("archived_runs columns = %d, %v", cols, err)
	}
}
