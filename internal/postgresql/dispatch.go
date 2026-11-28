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
	"encoding/json"
	"fmt"
	"time"
)

// DispatchCandidate is an agent a dispatch decision considered.
type DispatchCandidate struct {
	Namespace   string  `json:"namespace"`
	Agent       string  `json:"agent"`
	Probability float64 `json:"probability"`
}

// DispatchDecision is the record of one pick (or refusal). It never holds the
// request.
type DispatchDecision struct {
	ID       string
	TenantID string
	// Source is the entry point that asked.
	Source string
	// Method is only, decided, fallback or refused; Reason says more.
	Method, Reason        string
	AgentNamespace, Agent string
	Confidence            float64
	CandidateCount        int
	// Candidates are the most probable ones.
	Candidates      []DispatchCandidate
	Provider, Model string
	Escalated       bool
	InputTokens     int
	OutputTokens    int
	CostUSD         float64
	Latency         time.Duration
}

// InsertDispatchDecision stores a decision.
func (s *Store) InsertDispatchDecision(ctx context.Context, d DispatchDecision) error {
	candidates := d.Candidates
	if candidates == nil {
		candidates = []DispatchCandidate{}
	}
	encoded, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		if scope != PlatformScope && scope != d.TenantID {
			return fmt.Errorf("%w: the decision belongs to another tenant", ErrNoTenantScope)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO dispatch_decisions (id, tenant_id, source, method, reason, agent_namespace, agent, confidence,
				candidate_count, candidates, provider, model, escalated, input_tokens, output_tokens, cost_usd, latency_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14, $15, $16, $17)`,
			d.ID, d.TenantID, d.Source, d.Method, d.Reason, d.AgentNamespace, d.Agent, d.Confidence,
			d.CandidateCount, string(encoded), d.Provider, d.Model, d.Escalated, d.InputTokens, d.OutputTokens,
			d.CostUSD, d.Latency.Milliseconds())
		return err
	})
	if err != nil {
		return fmt.Errorf("storing a dispatch decision: %w", err)
	}
	return nil
}

// DispatchSpendSince is what the tenant's decision calls have cost since then.
func (s *Store) DispatchSpendSince(ctx context.Context, tenantID string, since time.Time) (float64, error) {
	var total float64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE(sum(cost_usd), 0)::float8 FROM dispatch_decisions
			WHERE tenant_id = $1 AND created_at >= $2 AND ($3::text = '*' OR tenant_id = $3)`,
			tenantID, since, scope).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("summing dispatch spend: %w", err)
	}
	return total, nil
}

// PruneDispatchDecisions deletes decisions made before the given time and
// returns how many.
func (s *Store) PruneDispatchDecisions(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM dispatch_decisions WHERE created_at < $1 AND ($2::text = '*' OR tenant_id = $2)`, before, scope)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("pruning dispatch decisions: %w", err)
	}
	return n, nil
}
