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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
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
	Phase    string `db:"phase"`
	PodName  string `db:"pod_name"`
	SpendUSD string `db:"spend_usd"`
	// CostCapture mirrors the run's CostCapture condition: "complete" when the
	// spend was read from the state store, "incomplete" when it could not be,
	// "" for rows archived before the column existed (spend unknown either way).
	CostCapture       string     `db:"cost_capture"`
	Output            string     `db:"output"`
	RawOutput         string     `db:"raw_output"`
	RestartCount      int        `db:"restart_count"`
	StartTime         *time.Time `db:"start_time"`
	CompletionTime    *time.Time `db:"completion_time"`
	ContextUsedTokens int        `db:"context_used_tokens"`
	MaxContextTokens  int        `db:"max_context_tokens"`
	// Tenant attribution (from agentorca.io/tenant label, empty if unattributed).
	Tenant string `db:"tenant"`
	// TenantID is the tenant's stable ID, from the run's agentorca.io/tenant-id
	// label; empty when the run is unattributed.
	TenantID string `db:"tenant_id"`
	// Routing decisions and child refs (JSON-serialised).
	RoutingDecisionsJSON []byte `db:"routing_decisions"`
	ChildRunRefsJSON     []byte `db:"child_run_refs"`
	// TraceEventsJSON is the archived execution trace — a JSON array of
	// {id, event, ts, childRunName} entries read from the Redis token stream
	// at archival time. NULL when no trace events were available (store
	// disabled, stream expired, or pre-feature run).
	TraceEventsJSON []byte `db:"trace_events"`
	// PayloadPrunedAt is when archived-run retention emptied the output and
	// trace of this run (ARCHIVED_RUNS_RETENTION_DAYS); nil when it is intact.
	PayloadPrunedAt *time.Time `db:"payload_pruned_at"`
	// Timestamps.
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// HistoryQuery holds filter/sort/pagination parameters for run history lookups.
type HistoryQuery struct {
	// TenantID narrows a platform-scoped query to one tenant. A tenant-scoped
	// call is always limited to its own tenant.
	TenantID string
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
	// IncludeEval lists the runs of evaluation experiments too. The zero value
	// leaves them out: they are not production traffic.
	IncludeEval bool
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
	// defaultScope is the scope of calls whose context carries none; empty
	// unless the store was made by AsPlatform.
	defaultScope string
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

	// A re-archive (the operator archives every finished run again when it
	// restarts) must not undo archived-run retention: a row the sweep reduced
	// keeps its empty payload and its payload_pruned_at, and a row that has not
	// changed keeps its updated_at, which the sweep goes by.
	//
	// UPSERT by composite key so re-archival updates the row. When upserting
	// with a nil traceEventsJSON (e.g. Redis stream expired during a back-fill),
	// the existing trace_events column is preserved rather than overwritten.
	const q = `
		INSERT INTO archived_runs (
			id, name, namespace, labels, agent_ref, input, parent_run_ref,
			prior_run_ref, timeout_sec, phase, pod_name, spend_usd, output,
			raw_output, restart_count, start_time, completion_time,
			context_used_tokens, max_context_tokens, tenant,
			routing_decisions, child_run_refs, trace_events, created_at, updated_at,
			cost_capture, tenant_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, NULLIF($27, '')
		)
		ON CONFLICT (id) DO UPDATE SET
			phase = EXCLUDED.phase,
			spend_usd = EXCLUDED.spend_usd,
			cost_capture = EXCLUDED.cost_capture,
			output = CASE WHEN archived_runs.payload_pruned_at IS NOT NULL
			              THEN archived_runs.output ELSE EXCLUDED.output END,
			raw_output = CASE WHEN archived_runs.payload_pruned_at IS NOT NULL
			                  THEN archived_runs.raw_output ELSE EXCLUDED.raw_output END,
			restart_count = EXCLUDED.restart_count,
			start_time = EXCLUDED.start_time,
			completion_time = EXCLUDED.completion_time,
			context_used_tokens = EXCLUDED.context_used_tokens,
			routing_decisions = EXCLUDED.routing_decisions,
			child_run_refs = EXCLUDED.child_run_refs,
			trace_events = CASE WHEN archived_runs.payload_pruned_at IS NOT NULL
			                    THEN archived_runs.trace_events
			                    WHEN EXCLUDED.trace_events IS NOT NULL
			                    THEN EXCLUDED.trace_events
			                    ELSE archived_runs.trace_events END,
			payload_pruned_at = archived_runs.payload_pruned_at,
			updated_at = CASE WHEN archived_runs.payload_pruned_at IS NOT NULL
			                    OR (archived_runs.phase IS NOT DISTINCT FROM EXCLUDED.phase
			                        AND archived_runs.spend_usd IS NOT DISTINCT FROM EXCLUDED.spend_usd
			                        AND archived_runs.cost_capture IS NOT DISTINCT FROM EXCLUDED.cost_capture
			                        AND archived_runs.output IS NOT DISTINCT FROM EXCLUDED.output
			                        AND archived_runs.raw_output IS NOT DISTINCT FROM EXCLUDED.raw_output
			                        AND archived_runs.restart_count IS NOT DISTINCT FROM EXCLUDED.restart_count
			                        AND archived_runs.completion_time IS NOT DISTINCT FROM EXCLUDED.completion_time
			                        AND archived_runs.context_used_tokens IS NOT DISTINCT FROM EXCLUDED.context_used_tokens)
			                  THEN archived_runs.updated_at ELSE EXCLUDED.updated_at END
	`
	now := time.Now().UTC()
	// The archive row and the run's outcome land together: an outcome without
	// its run (or the reverse) would skew the routing analysis that joins them.
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tid, err := rowTenant(scope, archived.TenantID)
		if err != nil {
			return err
		}
		archived.TenantID = tid
		return archiveRun(ctx, tx, q, run, archived, labelsJSON, now)
	})
	if err != nil {
		return fmt.Errorf("archiving run %s: %w", archived.ID, err)
	}
	slog.Debug("archived run to PostgreSQL", "run", archived.ID, "phase", archived.Phase)
	return nil
}

func archiveRun(ctx context.Context, tx dbtx, q string, run *agentorcav1alpha1.AgentRun, archived RunArchive, labelsJSON []byte, now time.Time) error {
	_, err := tx.ExecContext(ctx, q,
		archived.ID, archived.Name, archived.Namespace, labelsJSON,
		archived.AgentRef, archived.Input, archived.ParentRunRef,
		archived.PriorRunRef, archived.TimeoutSec, archived.Phase,
		archived.PodName, archived.SpendUSD, archived.Output, archived.RawOutput,
		archived.RestartCount, archived.StartTime, archived.CompletionTime,
		archived.ContextUsedTokens, archived.MaxContextTokens, archived.Tenant,
		archived.RoutingDecisionsJSON, archived.ChildRunRefsJSON,
		archived.TraceEventsJSON, now, now, archived.CostCapture, archived.TenantID,
	)
	if err != nil {
		return err
	}
	return upsertRunOutcome(ctx, tx, archived.TenantID, run, archived)
}

// QueryHistory returns a paginated, filtered list of archived runs.
func (s *Store) QueryHistory(ctx context.Context, q HistoryQuery) (*HistoryPage, error) {
	var page *HistoryPage
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		page, err = queryHistory(ctx, tx, scope, q)
		return err
	})
	return page, err
}

func queryHistory(ctx context.Context, tx dbtx, scope string, q HistoryQuery) (*HistoryPage, error) {
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
	if tid, ok := tenantArg(scope, q.TenantID); ok {
		where = append(where, fmt.Sprintf("tenant_id = $%d", paramN))
		args = append(args, tid)
		paramN++
	}

	if !q.IncludeEval {
		where = append(where, notEvalRunSQL)
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
	if err := tx.QueryRowContext(ctx, countQ, args...).Scan(&count); err != nil {
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

	rows, err := tx.QueryContext(ctx, dataQ, args...)
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

// CostQuery filters the archived runs that feed the cost dashboard.
type CostQuery struct {
	// TenantID narrows a platform-scoped query to one tenant. A tenant-scoped
	// call is always limited to its own tenant.
	TenantID string
	// Namespaces restricts results to these namespaces. Empty = all.
	Namespaces []string
	// Deployment restricts to runs labelled agentorca.io/deployment=<name>.
	Deployment string
	// Run restricts to a single run name.
	Run string
	// Since drops runs that finished (or, if unfinished, started) before it.
	Since *time.Time
	// IncludeEval counts the runs of evaluation experiments too. The zero value
	// leaves them out: their spend is reported on its own (QueryEvalSpend).
	IncludeEval bool
}

// notEvalRunSQL keeps out the archived runs that carry an evaluation label.
const notEvalRunSQL = `NOT jsonb_exists_any(COALESCE(labels, '{}'::jsonb), ARRAY['agentorca.io/eval', 'agentorca.io/eval-experiment'])`

// CostRow is the slice of an archived run that cost aggregation needs.
type CostRow struct {
	Name      string
	Namespace string
	AgentRef  string
	Phase     string
	SpendUSD  string
	// CostIncomplete is true when the run's spend could not be captured.
	CostIncomplete bool
	// Model is the model of the run's last routing decision ("" if none).
	Model string
	// When is the completion time, falling back to start then creation time.
	When time.Time
	// Started and Completed are the run's start and completion times when the
	// archive has them. Both are needed to know how long the run took.
	Started   *time.Time
	Completed *time.Time
}

// maxCostRows bounds one cost query so a very large archive cannot exhaust
// memory; the newest rows win.
const maxCostRows = 100000

// QueryCostRows returns the archived runs matching q, newest first, in the
// narrow shape cost aggregation needs (no output, input or trace payloads).
func (s *Store) QueryCostRows(ctx context.Context, q CostQuery) ([]CostRow, error) {
	var out []CostRow
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		out, err = queryCostRows(ctx, tx, scope, q)
		return err
	})
	return out, err
}

func queryCostRows(ctx context.Context, tx dbtx, scope string, q CostQuery) ([]CostRow, error) {
	var (
		where  []string
		args   []any
		paramN = 1
	)
	if tid, ok := tenantArg(scope, q.TenantID); ok {
		where = append(where, fmt.Sprintf("tenant_id = $%d", paramN))
		args = append(args, tid)
		paramN++
	}
	if !q.IncludeEval {
		where = append(where, notEvalRunSQL)
	}
	if len(q.Namespaces) > 0 {
		ph := make([]string, len(q.Namespaces))
		for i, n := range q.Namespaces {
			ph[i] = fmt.Sprintf("$%d", paramN)
			args = append(args, n)
			paramN++
		}
		where = append(where, fmt.Sprintf("namespace IN (%s)", strings.Join(ph, ", ")))
	}
	if q.Deployment != "" {
		where = append(where, fmt.Sprintf("labels ->> 'agentorca.io/deployment' = $%d", paramN))
		args = append(args, q.Deployment)
		paramN++
	}
	if q.Run != "" {
		where = append(where, fmt.Sprintf("name = $%d", paramN))
		args = append(args, q.Run)
		paramN++
	}
	if q.Since != nil {
		where = append(where, fmt.Sprintf("COALESCE(completion_time, start_time, created_at) >= $%d", paramN))
		args = append(args, *q.Since)
		paramN++
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}
	dataQ := fmt.Sprintf(`
		SELECT name, namespace, agent_ref, phase, COALESCE(spend_usd, ''),
		       COALESCE(cost_capture, ''),
		       COALESCE(routing_decisions -> -1 ->> 'model', ''),
		       COALESCE(completion_time, start_time, created_at),
		       start_time, completion_time
		FROM archived_runs %s
		ORDER BY COALESCE(completion_time, start_time, created_at) DESC
		LIMIT $%d
	`, whereClause, paramN)
	args = append(args, maxCostRows)

	rows, err := tx.QueryContext(ctx, dataQ, args...)
	if err != nil {
		return nil, fmt.Errorf("querying archived run costs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []CostRow{}
	for rows.Next() {
		var r CostRow
		var capture string
		if err := rows.Scan(&r.Name, &r.Namespace, &r.AgentRef, &r.Phase, &r.SpendUSD, &capture, &r.Model, &r.When, &r.Started, &r.Completed); err != nil {
			return nil, fmt.Errorf("scanning cost row: %w", err)
		}
		r.CostIncomplete = capture == "incomplete"
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating cost rows: %w", err)
	}
	return out, nil
}

// GetRun retrieves a single archived run by namespace/name.
func (s *Store) GetRun(ctx context.Context, namespace, name string) (*RunArchive, error) {
	const q = `SELECT id, name, namespace, labels, agent_ref, input, parent_run_ref,
		prior_run_ref, timeout_sec, phase, pod_name, spend_usd, output, raw_output,
		restart_count, start_time, completion_time, context_used_tokens, max_context_tokens,
		tenant, routing_decisions, child_run_refs, trace_events, created_at, updated_at,
		COALESCE(tenant_id, ''), payload_pruned_at
		FROM archived_runs WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`
	id := fmt.Sprintf("%s/%s", namespace, name)
	var r RunArchive
	var labelsJSON []byte
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		return tx.QueryRowContext(ctx, q, id, scope).Scan(
			&r.ID, &r.Name, &r.Namespace, &labelsJSON,
			&r.AgentRef, &r.Input, &r.ParentRunRef, &r.PriorRunRef,
			&r.TimeoutSec, &r.Phase, &r.PodName, &r.SpendUSD, &r.Output, &r.RawOutput,
			&r.RestartCount, &r.StartTime, &r.CompletionTime,
			&r.ContextUsedTokens, &r.MaxContextTokens, &r.Tenant,
			&r.RoutingDecisionsJSON, &r.ChildRunRefsJSON, &r.TraceEventsJSON, &r.CreatedAt, &r.UpdatedAt,
			&r.TenantID, &r.PayloadPrunedAt,
		)
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("loading archived run %s: %w", id, err)
	}
	_ = json.Unmarshal(labelsJSON, &r.Labels)
	return &r, nil
}

// ApplyMigration brings the schema up to date (see migrate.go).
func (s *Store) ApplyMigration(ctx context.Context) error {
	applied, err := Migrate(ctx, s.db)
	if err != nil {
		return err
	}
	slog.Info("PostgreSQL schema up to date", "applied", applied)
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
		TenantID:          run.Labels[tenancy.LabelTenantID],
		ContextUsedTokens: run.Status.ContextUsedTokens,
		MaxContextTokens:  run.Status.MaxContextTokens,
		Labels:            run.Labels,
	}
	if c := meta.FindStatusCondition(run.Status.Conditions, "CostCapture"); c != nil {
		switch c.Status {
		case metav1.ConditionFalse:
			r.CostCapture = "incomplete"
		case metav1.ConditionTrue:
			r.CostCapture = "complete"
		}
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
