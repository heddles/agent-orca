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

// Package postgresql provides a PostgreSQL-backed archival store for completed
// AgentRun resources. The operator's AgentRunReconciler snapshots terminal-phase
// runs here so the UI can display historical runs beyond the lifetime of the
// live Kubernetes CRDs (which may be garbage-collected by the controller's
// retention policy).
package postgresql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx as database/sql driver
)

// RunArchive is the relational representation of an archived AgentRun.
// Fields are flattened from the CRD spec and status for SQL querying.
type RunArchive struct {
	// ID is the composite primary key: "namespace/name".
	ID string `db:"id"`
	// Kubernetes metadata.
	Name      string            `db:"name"`
	Namespace string            `db:"namespace"`
	Labels    map[string]string `db:"labels"`
	// Spec-level fields.
	AgentRef     string        `db:"agent_ref"`
	Input        string        `db:"input"`
	ParentRunRef string        `db:"parent_run_ref"`
	PriorRunRef  string        `db:"prior_run_ref"`
	TimeoutSec   sql.NullInt64 `db:"timeout_sec"`
	// Status-level fields (captured at archival time).
	Phase             string     `db:"phase"`
	PodName           string     `db:"pod_name"`
	SpendUSD          string     `db:"spend_usd"`
	Output            string     `db:"output"`
	RawOutput         string     `db:"raw_output"`
	RestartCount      int        `db:"restart_count"`
	StartTime         *time.Time `db:"start_time"`
	CompletionTime    *time.Time `db:"completion_time"`
	ContextUsedTokens int        `db:"context_used_tokens"`
	MaxContextTokens  int        `db:"max_context_tokens"`
	// Tenant attribution (from agentorca.io/tenant label, empty if unattributed).
	Tenant string `db:"tenant"`
	// Routing decisions and child refs (JSON-serialised).
	RoutingDecisionsJSON []byte `db:"routing_decisions"`
	ChildRunRefsJSON     []byte `db:"child_run_refs"`
	// TraceEventsJSON is the archived execution trace — a JSON array of
	// {id, event, ts, childRunName} entries read from the Redis token stream
	// at archival time. NULL when no trace events were available (store
	// disabled, stream expired, or pre-feature run).
	TraceEventsJSON []byte `db:"trace_events"`
	// Timestamps.
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// HistoryQuery holds filter/sort/pagination parameters for run history lookups.
type HistoryQuery struct {
	// Limit caps the number of results (default 50, clamped to [1, 200]).
	Limit int
	// Offset is the number of rows to skip (for pagination).
	Offset int
	// Phase filters by AgentRun phase (e.g. "Succeeded", "Failed"). Empty = all.
	Phase string
	// AgentRef filters by the agent name referenced by the run.
	AgentRef string
	// Namespaces restricts results to the given namespaces (typically a tenant's
	// authorized set). Empty = all namespaces (only meaningful when the caller has
	// cluster-wide access).
	Namespaces []string
	// Search matches against the run name case-insensitively.
	Search string
}

// HistoryResult is a single row in a history listing.
type HistoryResult struct {
	Name           string     `json:"name"`
	Namespace      string     `json:"namespace"`
	AgentRef       string     `json:"agentRef"`
	Phase          string     `json:"phase"`
	SpendUSD       string     `json:"spendUSD"`
	StartTime      *time.Time `json:"startTime,omitempty"`
	CompletionTime *time.Time `json:"completionTime,omitempty"`
	Tenant         string     `json:"tenant,omitempty"`
	// ContextUsedTokens / MaxContextTokens are surfaced so the UI can show a
	// context-utilization preview without a full detail round-trip.
	ContextUsedTokens int `json:"contextUsedTokens,omitempty"`
	MaxContextTokens  int `json:"maxContextTokens,omitempty"`
}

// HistoryPage is the paginated response for run history queries.
type HistoryPage struct {
	Runs   []HistoryResult `json:"runs"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// Store is the PostgreSQL archival store.
type Store struct {
	db *sql.DB
}

// NewStore creates a new PostgreSQL store from a connection DSN.
// The DSN format follows the pgx connection string syntax:
//
//	postgresql://user:password@host:5432/dbname?sslmode=require
func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	slog.Info("PostgreSQL archival store connected", "maxOpenConns", 25)
	return &Store{db: db}, nil
}

// Close releases the connection pool.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping verifies connectivity.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store not initialized")
	}
	return s.db.PingContext(ctx)
}

// ArchiveRun upserts a completed AgentRun into the archival table.
// traceEventsJSON is a pre-marshalled JSON array of trace entries (or nil to
// leave the column untouched on conflict). It is passed in by the controller
// after reading the Redis token stream, so the full execution trace can be
// rendered in the history view.
func (s *Store) ArchiveRun(ctx context.Context, run *agentorcav1alpha1.AgentRun, traceEventsJSON []byte) error {
	archived := fromAgentRun(run)
	archived.TraceEventsJSON = traceEventsJSON

	labelsJSON, _ := json.Marshal(archived.Labels)

	// UPSERT by composite key so re-archival updates the row. When upserting
	// with a nil traceEventsJSON (e.g. Redis stream expired during a back-fill),
	// the existing trace_events column is preserved rather than overwritten.
	const q = `
		INSERT INTO archived_runs (
			id, name, namespace, labels, agent_ref, input, parent_run_ref,
			prior_run_ref, timeout_sec, phase, pod_name, spend_usd, output,
			raw_output, restart_count, start_time, completion_time,
			context_used_tokens, max_context_tokens, tenant,
			routing_decisions, child_run_refs, trace_events, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25
		)
		ON CONFLICT (id) DO UPDATE SET
			phase = EXCLUDED.phase,
			spend_usd = EXCLUDED.spend_usd,
			output = EXCLUDED.output,
			raw_output = EXCLUDED.raw_output,
			restart_count = EXCLUDED.restart_count,
			start_time = EXCLUDED.start_time,
			completion_time = EXCLUDED.completion_time,
			context_used_tokens = EXCLUDED.context_used_tokens,
			routing_decisions = EXCLUDED.routing_decisions,
			child_run_refs = EXCLUDED.child_run_refs,
			trace_events = CASE WHEN EXCLUDED.trace_events IS NOT NULL
			                    THEN EXCLUDED.trace_events
			                    ELSE archived_runs.trace_events END,
			updated_at = EXCLUDED.updated_at
	`
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, q,
		archived.ID, archived.Name, archived.Namespace, labelsJSON,
		archived.AgentRef, archived.Input, archived.ParentRunRef,
		archived.PriorRunRef, archived.TimeoutSec, archived.Phase,
		archived.PodName, archived.SpendUSD, archived.Output, archived.RawOutput,
		archived.RestartCount, archived.StartTime, archived.CompletionTime,
		archived.ContextUsedTokens, archived.MaxContextTokens, archived.Tenant,
		archived.RoutingDecisionsJSON, archived.ChildRunRefsJSON,
		archived.TraceEventsJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("archiving run %s: %w", archived.ID, err)
	}
	slog.Debug("archived run to PostgreSQL", "run", archived.ID, "phase", archived.Phase)
	return nil
}

// QueryHistory returns a paginated, filtered list of archived runs.
func (s *Store) QueryHistory(ctx context.Context, q HistoryQuery) (*HistoryPage, error) {
	// Clamp limit.
	limit := q.Limit
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := max(q.Offset, 0)

	var (
		where  []string
		args   []any
		count  int
		paramN = 1
	)

	add := func(cond string, val any) {
		where = append(where, fmt.Sprintf("(%s)", cond))
		args = append(args, val)
		paramN++
	}

	if q.Phase != "" {
		add(fmt.Sprintf("$%d::text = ANY(string_to_array(phase, ','))", paramN), q.Phase)
	}
	if q.AgentRef != "" {
		where = append(where, fmt.Sprintf("agent_ref = $%d", paramN))
		args = append(args, q.AgentRef)
		paramN++
	}
	if len(q.Namespaces) > 0 {
		ph := make([]string, len(q.Namespaces))
		for i, n := range q.Namespaces {
			ph[i] = fmt.Sprintf("$%d", paramN+i)
			args = append(args, n)
		}
		paramN += len(q.Namespaces)
		where = append(where, fmt.Sprintf("namespace IN (%s)", strings.Join(ph, ", ")))
	}
	if q.Search != "" {
		where = append(where, fmt.Sprintf("name ILIKE $%d", paramN))
		args = append(args, "%"+strings.ReplaceAll(q.Search, "%", "\\%")+"%")
		paramN++
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Total count (ignoring LIMIT/OFFSET).
	countQ := fmt.Sprintf("SELECT COUNT(*) FROM archived_runs %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&count); err != nil {
		return nil, fmt.Errorf("counting archived runs: %w", err)
	}

	// Paginated results, newest first.
	dataQ := fmt.Sprintf(`
		SELECT name, namespace, agent_ref, phase, spend_usd, start_time, completion_time, tenant,
		       context_used_tokens, max_context_tokens
		FROM archived_runs %s
		ORDER BY COALESCE(start_time, created_at) DESC, name ASC
		LIMIT $%d OFFSET $%d
	`, whereClause, paramN, paramN+1)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, dataQ, args...)
	if err != nil {
		return nil, fmt.Errorf("querying archived runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Non-nil empty slice so JSON serializes to [] (not null). A null `runs`
	// array previously crashed the UI with "can't access property
	// Symbol.iterator, t.runs is null" on every empty history response, which
	// unmounted the whole React root and left a stale `tab: "history"` in
	// sessionStorage — forcing users to clear the cache to recover.
	results := []HistoryResult{}
	for rows.Next() {
		var r HistoryResult
		if err := rows.Scan(&r.Name, &r.Namespace, &r.AgentRef, &r.Phase,
			&r.SpendUSD, &r.StartTime, &r.CompletionTime, &r.Tenant,
			&r.ContextUsedTokens, &r.MaxContextTokens); err != nil {
			return nil, fmt.Errorf("scanning run row: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating run rows: %w", err)
	}

	return &HistoryPage{
		Runs:   results,
		Total:  count,
		Limit:  limit,
		Offset: offset,
	}, nil
}

// GetRun retrieves a single archived run by namespace/name.
func (s *Store) GetRun(ctx context.Context, namespace, name string) (*RunArchive, error) {
	const q = `SELECT id, name, namespace, labels, agent_ref, input, parent_run_ref,
		prior_run_ref, timeout_sec, phase, pod_name, spend_usd, output, raw_output,
		restart_count, start_time, completion_time, context_used_tokens, max_context_tokens,
		tenant, routing_decisions, child_run_refs, trace_events, created_at, updated_at
		FROM archived_runs WHERE id = $1`
	id := fmt.Sprintf("%s/%s", namespace, name)
	var r RunArchive
	var labelsJSON []byte
	if err := s.db.QueryRowContext(ctx, q, id).Scan(
		&r.ID, &r.Name, &r.Namespace, &labelsJSON,
		&r.AgentRef, &r.Input, &r.ParentRunRef, &r.PriorRunRef,
		&r.TimeoutSec, &r.Phase, &r.PodName, &r.SpendUSD, &r.Output, &r.RawOutput,
		&r.RestartCount, &r.StartTime, &r.CompletionTime,
		&r.ContextUsedTokens, &r.MaxContextTokens, &r.Tenant,
		&r.RoutingDecisionsJSON, &r.ChildRunRefsJSON, &r.TraceEventsJSON, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("loading archived run %s: %w", id, err)
	}
	_ = json.Unmarshal(labelsJSON, &r.Labels)
	return &r, nil
}

// MigrationSQL is the SQL used to create/alter the archival schema.
// Applied at operator startup.
const MigrationSQL = `
CREATE TABLE IF NOT EXISTS archived_runs (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    labels          JSONB DEFAULT '{}',
    agent_ref       TEXT NOT NULL,
    input           TEXT,
    parent_run_ref  TEXT,
    prior_run_ref   TEXT,
    timeout_sec     BIGINT,
    phase           TEXT NOT NULL,
    pod_name        TEXT,
    spend_usd       TEXT,
    output          TEXT,
    raw_output      TEXT,
    restart_count   INTEGER DEFAULT 0,
    start_time      TIMESTAMPTZ,
    completion_time TIMESTAMPTZ,
    context_used_tokens  INTEGER DEFAULT 0,
    max_context_tokens   INTEGER DEFAULT 0,
    tenant          TEXT,
    routing_decisions JSONB,
    child_run_refs  JSONB,
    trace_events     JSONB,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_archived_runs_tenant    ON archived_runs(tenant);
CREATE INDEX IF NOT EXISTS idx_archived_runs_namespace ON archived_runs(namespace);
CREATE INDEX IF NOT EXISTS idx_archived_runs_phase     ON archived_runs(phase);
CREATE INDEX IF NOT EXISTS idx_archived_runs_created   ON archived_runs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_archived_runs_name      ON archived_runs(name);

-- Back-fill: add the trace_events column to tables created before the full
-- execution-trace archival feature shipped. ADD COLUMN IF NOT EXISTS is a no-op
-- for fresh tables (which already declare the column above).
ALTER TABLE archived_runs ADD COLUMN IF NOT EXISTS trace_events JSONB;

-- Enable lz4 TOAST compression on columns that can hold large values
-- (trace events, routing decisions, output). PostgreSQL automatically TOASTs
-- values > 2KB; SET COMPRESSION controls the algorithm. lz4 is fast and gives
-- a good ratio. A VACUUM (FULL if possible) is needed in production to
-- retroactively compress existing rows; new inserts use lz4 automatically.
ALTER TABLE archived_runs ALTER COLUMN trace_events SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN routing_decisions SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN output SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN raw_output SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN input SET COMPRESSION lz4;
`

// ApplyMigration runs the schema migration SQL against the store.
func (s *Store) ApplyMigration(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, MigrationSQL); err != nil {
		return fmt.Errorf("applying postgresql migration: %w", err)
	}
	slog.Info("PostgreSQL migration applied")
	return nil
}

// fromAgentRun converts a CRD AgentRun into the flat archival struct.
func fromAgentRun(run *agentorcav1alpha1.AgentRun) RunArchive {
	r := RunArchive{
		ID:                fmt.Sprintf("%s/%s", run.Namespace, run.Name),
		Name:              run.Name,
		Namespace:         run.Namespace,
		AgentRef:          run.Spec.AgentRef,
		Input:             run.Spec.Input,
		ParentRunRef:      run.Spec.ParentRunRef,
		PriorRunRef:       run.Spec.PriorRunRef,
		PodName:           run.Status.PodName,
		Phase:             string(run.Status.Phase),
		SpendUSD:          run.Status.SpendUSD,
		Output:            run.Status.Output,
		RawOutput:         run.Status.RawOutput,
		RestartCount:      run.Status.RestartCount,
		Tenant:            run.Labels["agentorca.io/tenant"],
		ContextUsedTokens: run.Status.ContextUsedTokens,
		MaxContextTokens:  run.Status.MaxContextTokens,
		Labels:            run.Labels,
	}
	if run.Spec.Timeout != nil {
		r.TimeoutSec = sql.NullInt64{Int64: int64(run.Spec.Timeout.Seconds()), Valid: true}
	}
	if run.Status.StartTime != nil {
		t := run.Status.StartTime.Time
		r.StartTime = &t
	}
	if run.Status.CompletionTime != nil {
		t := run.Status.CompletionTime.Time
		r.CompletionTime = &t
	}
	// Pre-serialise routing decisions and child refs as compact JSON.
	if len(run.Status.RoutingDecisions) > 0 {
		r.RoutingDecisionsJSON, _ = json.Marshal(run.Status.RoutingDecisions)
	}
	if len(run.Status.ChildRunRefs) > 0 {
		r.ChildRunRefsJSON, _ = json.Marshal(run.Status.ChildRunRefs)
	}
	return r
}

// IsTerminalPhase reports whether the given AgentRun phase is terminal.
func IsTerminalPhase(phase agentorcav1alpha1.AgentRunPhase) bool {
	switch phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded,
		agentorcav1alpha1.AgentRunPhaseFailed,
		agentorcav1alpha1.AgentRunPhaseHandedOff:
		return true
	default:
		return false
	}
}
