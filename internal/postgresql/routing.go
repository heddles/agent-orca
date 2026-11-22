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
	"fmt"
	"log/slog"
	"strings"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// RoutingEvent is one LLM call the model-router made (table routing_events,
// migration 0003). It holds metadata only, never prompt or response content.
// The JSON tags are the wire format the model-router posts; the router keeps
// its own matching type so its image does not link this package.
type RoutingEvent struct {
	DecisionID       string `json:"decisionID"`
	ParentDecisionID string `json:"parentDecisionID,omitempty"`
	Tenant           string `json:"-"`
	// TenantID is the tenant's stable ID. Empty takes the call's tenant scope; a
	// platform-scoped event with none is stored unattributed.
	TenantID         string          `json:"-"`
	Namespace        string          `json:"-"`
	RunName          string          `json:"-"`
	Agent            string          `json:"agent,omitempty"`
	Selector         string          `json:"selector,omitempty"`
	Strategy         string          `json:"strategy,omitempty"`
	Algorithm        string          `json:"algorithm,omitempty"`
	Path             string          `json:"path,omitempty"`
	Stage            string          `json:"stage,omitempty"`
	StageConf        float64         `json:"stageConf,omitempty"`
	CategoryVersion  int             `json:"categoryVersion,omitempty"`
	SYLabel          string          `json:"syLabel,omitempty"`
	WorkflowStep     string          `json:"workflowStep,omitempty"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model,omitempty"`
	Candidates       []string        `json:"candidates,omitempty"`
	Signals          json.RawMessage `json:"signals,omitempty"`
	Confidence       float64         `json:"confidence,omitempty"`
	IsShadow         bool            `json:"isShadow,omitempty"`
	PromptTokens     int             `json:"promptTokens,omitempty"`
	CompletionTokens int             `json:"completionTokens,omitempty"`
	CostUSD          float64         `json:"costUSD,omitempty"`
	AuxCostUSD       float64         `json:"auxCostUSD,omitempty"`
	LatencyMS        int             `json:"latencyMS,omitempty"`
	// TTFTMS is nil when no token arrived (a failed call).
	TTFTMS           *int      `json:"ttftMS,omitempty"`
	FinishReason     string    `json:"finishReason,omitempty"`
	FallbackFrom     string    `json:"fallbackFrom,omitempty"`
	ErrorClass       string    `json:"errorClass,omitempty"`
	ToolCalls        int       `json:"toolCalls,omitempty"`
	ToolErrors       int       `json:"toolErrors,omitempty"`
	ToolParseErrors  int       `json:"toolParseErrors,omitempty"`
	GuardrailBlocked bool      `json:"guardrailBlocked,omitempty"`
	SafeguardTrip    string    `json:"safeguardTrip,omitempty"`
	TurnIndex        int       `json:"turnIndex,omitempty"`
	CreatedAt        time.Time `json:"createdAt,omitzero"`
}

// upsertRoutingEventSQL inserts an event, or refreshes the call-outcome columns
// of one already stored: a retried batch is idempotent, and an event sent early
// (before its tool results) can be completed by a later post.
const upsertRoutingEventSQL = `
INSERT INTO routing_events (
	decision_id, parent_decision_id, tenant, namespace, run_name, agent, selector,
	strategy, algorithm, path, stage, stage_conf, category_version, sy_label,
	workflow_step, provider, model, candidates, signals, confidence, is_shadow,
	prompt_tokens, completion_tokens, cost_usd, aux_cost_usd, latency_ms, ttft_ms,
	finish_reason, fallback_from, error_class, tool_calls, tool_errors,
	tool_parse_errors, guardrail_blocked, safeguard_trip, turn_index, created_at, tenant_id
) VALUES (
	$1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, NULLIF($6, ''), NULLIF($7, ''),
	$8, $9, $10, $11, $12, $13, $14,
	$15, $16, $17, $18, $19, $20, $21,
	$22, $23, $24, $25, $26, $27,
	$28, $29, $30, $31, $32,
	$33, $34, $35, $36, $37, NULLIF($38, '')
)
ON CONFLICT (decision_id) DO UPDATE SET
	prompt_tokens = EXCLUDED.prompt_tokens,
	completion_tokens = EXCLUDED.completion_tokens,
	cost_usd = EXCLUDED.cost_usd,
	aux_cost_usd = EXCLUDED.aux_cost_usd,
	latency_ms = EXCLUDED.latency_ms,
	ttft_ms = EXCLUDED.ttft_ms,
	finish_reason = EXCLUDED.finish_reason,
	error_class = EXCLUDED.error_class,
	tool_calls = EXCLUDED.tool_calls,
	tool_errors = EXCLUDED.tool_errors,
	tool_parse_errors = EXCLUDED.tool_parse_errors,
	guardrail_blocked = EXCLUDED.guardrail_blocked,
	safeguard_trip = EXCLUDED.safeguard_trip`

// UpsertRoutingEvents stores events in one transaction. Namespace, run name and
// decision ID are required on each; created_at defaults to now.
func (s *Store) UpsertRoutingEvents(ctx context.Context, events []RoutingEvent) error {
	if len(events) == 0 {
		return nil
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		return upsertRoutingEvents(ctx, tx, scope, events)
	})
	if err != nil {
		return fmt.Errorf("storing routing events: %w", err)
	}
	return nil
}

func upsertRoutingEvents(ctx context.Context, tx dbtx, scope string, events []RoutingEvent) error {
	now := time.Now().UTC()
	for i := range events {
		e := &events[i]
		tid, err := rowTenant(scope, e.TenantID)
		if err != nil {
			return err
		}
		if e.DecisionID == "" || e.Namespace == "" || e.RunName == "" || e.Provider == "" {
			return fmt.Errorf("storing routing events: event %d lacks decision ID, namespace, run or provider", i)
		}
		candidates, _ := json.Marshal(nonNil(e.Candidates))
		var signals any
		if len(e.Signals) > 0 {
			signals = []byte(e.Signals)
		}
		created := e.CreatedAt
		if created.IsZero() {
			created = now
		}
		var ttft any
		if e.TTFTMS != nil {
			ttft = *e.TTFTMS
		}
		if _, err := tx.ExecContext(ctx, upsertRoutingEventSQL,
			e.DecisionID, e.ParentDecisionID, e.Tenant, e.Namespace, e.RunName, e.Agent, e.Selector,
			e.Strategy, e.Algorithm, e.Path, e.Stage, e.StageConf, e.CategoryVersion, e.SYLabel,
			e.WorkflowStep, e.Provider, e.Model, candidates, signals, e.Confidence, e.IsShadow,
			e.PromptTokens, e.CompletionTokens, e.CostUSD, e.AuxCostUSD, e.LatencyMS, ttft,
			e.FinishReason, e.FallbackFrom, e.ErrorClass, e.ToolCalls, e.ToolErrors,
			e.ToolParseErrors, e.GuardrailBlocked, e.SafeguardTrip, e.TurnIndex, created, tid,
		); err != nil {
			return fmt.Errorf("storing routing event %s: %w", e.DecisionID, err)
		}
	}
	return nil
}

// PruneRoutingEvents deletes events older than cutoff and returns how many.
func (s *Store) PruneRoutingEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		// Retention spans every tenant, so it is a platform-only operation.
		if scope != PlatformScope {
			return fmt.Errorf("%w: pruning needs the platform scope", ErrNoTenantScope)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM routing_events WHERE created_at < $1`, cutoff)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("pruning routing events: %w", err)
	}
	return n, nil
}

// maxRunRoutingEvents bounds one run's event listing.
const maxRunRoutingEvents = 5000

// RoutingEventsForRun returns a run's events oldest first, up to limit
// (clamped to maxRunRoutingEvents).
func (s *Store) RoutingEventsForRun(ctx context.Context, namespace, run string, limit int) ([]RoutingEvent, error) {
	if limit < 1 || limit > maxRunRoutingEvents {
		limit = maxRunRoutingEvents
	}
	out := []RoutingEvent{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
		SELECT decision_id, COALESCE(parent_decision_id, ''), COALESCE(agent, ''), COALESCE(selector, ''),
		       strategy, path, stage, stage_conf, provider, model, confidence,
		       prompt_tokens, completion_tokens, cost_usd::float8, aux_cost_usd::float8,
		       latency_ms, ttft_ms, finish_reason, fallback_from, error_class,
		       tool_calls, tool_errors, tool_parse_errors, turn_index, created_at
		FROM routing_events
		WHERE namespace = $1 AND run_name = $2 AND ($4::text = '*' OR tenant_id = $4)
		ORDER BY created_at, decision_id
		LIMIT $3`, namespace, run, limit, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			e := RoutingEvent{Namespace: namespace, RunName: run}
			var ttft sql.NullInt64
			if err := rows.Scan(&e.DecisionID, &e.ParentDecisionID, &e.Agent, &e.Selector,
				&e.Strategy, &e.Path, &e.Stage, &e.StageConf, &e.Provider, &e.Model, &e.Confidence,
				&e.PromptTokens, &e.CompletionTokens, &e.CostUSD, &e.AuxCostUSD,
				&e.LatencyMS, &ttft, &e.FinishReason, &e.FallbackFrom, &e.ErrorClass,
				&e.ToolCalls, &e.ToolErrors, &e.ToolParseErrors, &e.TurnIndex, &e.CreatedAt); err != nil {
				return fmt.Errorf("scanning routing event: %w", err)
			}
			if ttft.Valid {
				v := int(ttft.Int64)
				e.TTFTMS = &v
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("querying routing events: %w", err)
	}
	return out, nil
}

// Routing event outcome filters for RoutingEventQuery.Outcome.
const (
	// RoutingOutcomeOK is a served call that neither failed nor fell back.
	RoutingOutcomeOK = "ok"
	// RoutingOutcomeError is a call with an error class.
	RoutingOutcomeError = "error"
	// RoutingOutcomeFallback is a call served by a fallback after another failed.
	RoutingOutcomeFallback = "fallback"
)

// maxRoutingEventsList bounds one page of ListRoutingEvents.
const maxRoutingEventsList = 500

// RoutingEventQuery filters ListRoutingEvents. Empty fields match everything.
type RoutingEventQuery struct {
	// Namespaces limits the query to these workspaces (a tenant's namespaces).
	// Empty means every namespace the call's tenant scope can see.
	Namespaces []string
	Agent      string
	Selector   string
	// Outcome is one of the RoutingOutcome constants.
	Outcome string
	// IncludeShadow adds counterfactual shadow calls, which are left out by default.
	IncludeShadow bool
	// Since and Until bound created_at (Since inclusive, Until exclusive).
	Since, Until time.Time
	// Limit is clamped to 1..500, default 100.
	Limit int
}

// ErrInvalidRoutingQuery is returned for a filter value ListRoutingEvents does not know.
var ErrInvalidRoutingQuery = errors.New("invalid routing event query")

// ListRoutingEvents returns routing events across runs, newest first, inside the
// call's tenant scope (tenant_id filter, with row-level security underneath).
func (s *Store) ListRoutingEvents(ctx context.Context, q RoutingEventQuery) ([]RoutingEvent, error) {
	var outcomeCond string
	switch q.Outcome {
	case "":
	case RoutingOutcomeOK:
		outcomeCond = " AND error_class = '' AND fallback_from = ''"
	case RoutingOutcomeError:
		outcomeCond = " AND error_class <> ''"
	case RoutingOutcomeFallback:
		outcomeCond = " AND fallback_from <> ''"
	default:
		return nil, fmt.Errorf("%w: outcome %q", ErrInvalidRoutingQuery, q.Outcome)
	}
	limit := q.Limit
	if limit < 1 {
		limit = 100
	}
	limit = min(limit, maxRoutingEventsList)
	out := []RoutingEvent{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var since, until any
		if !q.Since.IsZero() {
			since = q.Since
		}
		if !q.Until.IsZero() {
			until = q.Until
		}
		rows, err := tx.QueryContext(ctx, `
		SELECT decision_id, COALESCE(parent_decision_id, ''), namespace, run_name, COALESCE(agent, ''), COALESCE(selector, ''),
		       strategy, path, stage, stage_conf, provider, model, confidence, is_shadow,
		       prompt_tokens, completion_tokens, cost_usd::float8, aux_cost_usd::float8,
		       latency_ms, ttft_ms, finish_reason, fallback_from, error_class,
		       tool_calls, tool_errors, tool_parse_errors, turn_index, created_at
		FROM routing_events
		WHERE ($1::text = '*' OR tenant_id = $1)
		  AND (cardinality($2::text[]) = 0 OR namespace = ANY($2::text[]))
		  AND ($3::text = '' OR agent = $3)
		  AND ($4::text = '' OR selector = $4)
		  AND ($5::boolean OR NOT is_shadow)
		  AND ($6::timestamptz IS NULL OR created_at >= $6)
		  AND ($7::timestamptz IS NULL OR created_at < $7)`+outcomeCond+`
		ORDER BY created_at DESC, decision_id DESC
		LIMIT $8`, scope, pgTextArray(q.Namespaces), q.Agent, q.Selector, q.IncludeShadow, since, until, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var e RoutingEvent
			var ttft sql.NullInt64
			if err := rows.Scan(&e.DecisionID, &e.ParentDecisionID, &e.Namespace, &e.RunName, &e.Agent, &e.Selector,
				&e.Strategy, &e.Path, &e.Stage, &e.StageConf, &e.Provider, &e.Model, &e.Confidence, &e.IsShadow,
				&e.PromptTokens, &e.CompletionTokens, &e.CostUSD, &e.AuxCostUSD,
				&e.LatencyMS, &ttft, &e.FinishReason, &e.FallbackFrom, &e.ErrorClass,
				&e.ToolCalls, &e.ToolErrors, &e.ToolParseErrors, &e.TurnIndex, &e.CreatedAt); err != nil {
				return fmt.Errorf("scanning routing event: %w", err)
			}
			if ttft.Valid {
				v := int(ttft.Int64)
				e.TTFTMS = &v
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing routing events: %w", err)
	}
	return out, nil
}

// ModelCost is the spend routing telemetry attributes to one provider/model.
type ModelCost struct {
	Provider string
	Model    string
	Calls    int
	CostUSD  float64
	// AuxCostUSD is what selectors spent deciding (meta-router calls); it is
	// not included in CostUSD.
	AuxCostUSD float64
}

// CostByModel sums served (non-shadow) calls by provider and model since the
// given time. Unlike archived-run cost rows, which credit a whole run to its
// last model, this attributes each call's own cost to the model that served it.
func (s *Store) CostByModel(ctx context.Context, namespaces []string, since time.Time) ([]ModelCost, error) {
	out := []ModelCost{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
		SELECT provider, model, COUNT(*), COALESCE(SUM(cost_usd), 0)::float8, COALESCE(SUM(aux_cost_usd), 0)::float8
		FROM routing_events
		WHERE NOT is_shadow AND created_at >= $1
		  AND (cardinality($2::text[]) = 0 OR namespace = ANY($2::text[]))
		  AND ($3::text = '*' OR tenant_id = $3)
		GROUP BY provider, model
		ORDER BY SUM(cost_usd) DESC`, since, pgTextArray(namespaces), scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c ModelCost
			if err := rows.Scan(&c.Provider, &c.Model, &c.Calls, &c.CostUSD, &c.AuxCostUSD); err != nil {
				return fmt.Errorf("scanning model cost: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("querying cost by model: %w", err)
	}
	return out, nil
}

// pgTextArray renders ss as a Postgres text[] literal.
func pgTextArray(ss []string) string {
	b := []byte{'{'}
	for i, s := range ss {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		for _, r := range []byte(s) {
			if r == '"' || r == '\\' {
				b = append(b, '\\')
			}
			b = append(b, r)
		}
		b = append(b, '"')
	}
	return string(append(b, '}'))
}

// Failure classes recorded in run_outcomes.fail_reason_class.
const (
	failClassBudget    = "budget"
	failClassSafeguard = "safeguard"
	failClassTimeout   = "timeout"
	failClassError     = "error"
)

// failReasonClass groups how a run ended into a small set that can be compared
// across runs. Runs that did not fail have no class.
func failReasonClass(run *agentorcav1alpha1.AgentRun) string {
	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		return ""
	}
	switch {
	case run.Status.Budget != nil && run.Status.Budget.State == agentorcav1alpha1.RunBudgetExceeded:
		return failClassBudget
	case run.Status.LoopDetected != nil:
		return failClassSafeguard
	case strings.Contains(strings.ToLower(run.Status.FailureReason), "timeout"),
		strings.Contains(strings.ToLower(run.Status.FailureReason), "timed out"):
		return failClassTimeout
	}
	return failClassError
}

// upsertRunOutcome records how a run ended. The turn count, selector and
// tool-approval rejections come from rows already in the database, so the
// archive path needs no extra plumbing.
func upsertRunOutcome(ctx context.Context, tx dbtx, tenantID string, run *agentorcav1alpha1.AgentRun, archived RunArchive) error {
	const q = `
		INSERT INTO run_outcomes (
			namespace, run_name, tenant, selector, phase, fail_reason_class, turns,
			total_cost_usd, tool_approval_rejections, budget_exceeded, restart_count, archived_at, tenant_id
		) VALUES (
			$1, $2, NULLIF($3, ''),
			(SELECT selector FROM routing_events WHERE namespace = $1 AND run_name = $2 AND selector IS NOT NULL
				AND tenant_id IS NOT DISTINCT FROM NULLIF($10, '') LIMIT 1),
			$4, $5,
			(SELECT COUNT(*) FROM routing_events WHERE namespace = $1 AND run_name = $2 AND NOT is_shadow
				AND tenant_id IS NOT DISTINCT FROM NULLIF($10, '')),
			$6,
			(SELECT COUNT(*) FROM approvals WHERE namespace = $1 AND run_name = $2 AND kind = 'tool' AND status = 'rejected'
				AND tenant_id IS NOT DISTINCT FROM NULLIF($10, '')),
			$7, $8, $9, NULLIF($10, '')
		)
		ON CONFLICT (namespace, run_name) DO UPDATE SET
			selector = EXCLUDED.selector,
			phase = EXCLUDED.phase,
			fail_reason_class = EXCLUDED.fail_reason_class,
			turns = EXCLUDED.turns,
			total_cost_usd = EXCLUDED.total_cost_usd,
			tool_approval_rejections = EXCLUDED.tool_approval_rejections,
			budget_exceeded = EXCLUDED.budget_exceeded,
			restart_count = EXCLUDED.restart_count,
			archived_at = EXCLUDED.archived_at`
	cost := parseUSD(archived.SpendUSD)
	exceeded := run.Status.Budget != nil && run.Status.Budget.State == agentorcav1alpha1.RunBudgetExceeded
	if _, err := tx.ExecContext(ctx, q,
		archived.Namespace, archived.Name, archived.Tenant,
		archived.Phase, failReasonClass(run), cost, exceeded, archived.RestartCount, time.Now().UTC(), tenantID,
	); err != nil {
		return fmt.Errorf("recording outcome of run %s: %w", archived.ID, err)
	}
	return nil
}

// parseUSD reads a decimal USD string; an empty or malformed one is 0.
func parseUSD(s string) float64 {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &f); err != nil {
		return 0
	}
	return f
}

// RoutingEventsRetentionInterval is how often old routing events are pruned.
const RoutingEventsRetentionInterval = 6 * time.Hour

// RunRoutingEventsRetention deletes routing events older than retention, once
// now and then every interval, until ctx ends. A retention of 0 or less keeps
// events forever and returns immediately.
func (s *Store) RunRoutingEventsRetention(ctx context.Context, retention, interval time.Duration) error {
	return runRetention(ctx, s.PruneRoutingEvents, time.Now, retention, interval)
}

// runRetention is RunRoutingEventsRetention with its prune function and clock
// injected. A failed prune is logged and retried on the next tick.
func runRetention(ctx context.Context, prune func(context.Context, time.Time) (int64, error), now func() time.Time, retention, interval time.Duration) error {
	if retention <= 0 {
		return nil
	}
	run := func() {
		n, err := prune(ctx, now().Add(-retention))
		switch {
		case err != nil:
			slog.Error("pruning routing events", "err", err)
		case n > 0:
			slog.Info("pruned routing events", "deleted", n, "retention", retention)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			run()
		}
	}
}
