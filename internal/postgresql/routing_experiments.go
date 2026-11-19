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
	"slices"
	"strconv"

	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/routingbench"
)

// untaggedCategory is the category of a case with no tag.
const untaggedCategory = "tag:untagged"

// loadExperimentSamples projects the finished model-comparison experiments of
// the Agents in q into the sample shape routingbench reads
// (docs/evaluations.md, "Feeding routing benchmarks"). It is a read path: no row
// is copied.
//
// An experiment counts when it is complete, finished inside the window, and an
// arm of it runs one of the Agents on a ModelProvider of its own: the baseline
// arm (the first) plays "served" and each other arm of the same Agent, with its
// own provider, is the "shadow". The category is the case's tag ("tag:<tag>",
// one sample per tag, "tag:untagged" without any), so a finding drawn from it
// reports a category but, not being a routing stage, proposes no patch. The
// winner comes from the paired case outcomes (compare.Pairs): the arm that
// passed alone, a tie when both did the same. An experiment whose baseline names
// no provider cannot say which model "served", and is left out.
func loadExperimentSamples(ctx context.Context, tx dbtx, scope string, q routingbench.Query) ([]routingbench.Sample, error) {
	exps, err := tx.QueryContext(ctx, `
		SELECT id, arms FROM eval_experiments
		WHERE namespace = $1 AND status = 'complete' AND finished_at >= $2 AND finished_at <= $3
		  AND ($4::text = '*' OR tenant_id = $4)
		ORDER BY id`, q.Namespace, q.Since, q.Now, scope)
	if err != nil {
		return nil, fmt.Errorf("loading experiments: %w", err)
	}
	type found struct {
		id   int64
		arms []EvalArm
	}
	var list []found
	for exps.Next() {
		var f found
		var raw []byte
		if err := exps.Scan(&f.id, &raw); err != nil {
			_ = exps.Close()
			return nil, fmt.Errorf("scanning experiment: %w", err)
		}
		if err := json.Unmarshal(raw, &f.arms); err != nil || len(f.arms) < 2 {
			continue
		}
		list = append(list, f)
	}
	if err := closeRows(exps); err != nil {
		return nil, fmt.Errorf("loading experiments: %w", err)
	}

	var out []routingbench.Sample
	for _, e := range list {
		base := e.arms[0]
		if base.ModelProvider == "" || !slices.Contains(q.Agents, base.Agent) {
			continue
		}
		var cands []EvalArm
		for _, a := range e.arms[1:] {
			if a.Agent == base.Agent && a.ModelProvider != "" && a.ModelProvider != base.ModelProvider {
				cands = append(cands, a)
			}
		}
		if len(cands) == 0 {
			continue
		}
		rows, err := evalSummaryRows(ctx, tx, scope, e.id)
		if err != nil {
			return nil, fmt.Errorf("reading experiment %d: %w", e.id, err)
		}
		tags, err := caseTags(ctx, tx, scope, rows)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			pairs, err := compare.Pairs(rows, base.Label, c.Label)
			if err != nil {
				continue // an experiment with nothing usable adds nothing
			}
			for _, p := range pairs {
				cats := tags[p.CaseID]
				if len(cats) == 0 {
					cats = []string{untaggedCategory}
				}
				for _, cat := range cats {
					out = append(out, routingbench.Sample{
						Agent: base.Agent, Category: cat,
						ServedProvider: base.ModelProvider, ShadowProvider: c.ModelProvider,
						Winner:         experimentWinner(p.Winner),
						ServedCostUSD:  p.BaselineCostUSD,
						ShadowCostUSD:  p.CandidateCostUSD,
						FromExperiment: true,
					})
				}
			}
		}
	}
	return out, nil
}

func experimentWinner(w compare.Winner) string {
	switch w {
	case compare.WinnerBaseline:
		return routingbench.WinnerServed
	case compare.WinnerCandidate:
		return routingbench.WinnerShadow
	}
	return routingbench.WinnerTie
}

// caseTags reads the tags of the cases the rows name, as categories.
func caseTags(ctx context.Context, tx dbtx, scope string, rows []compare.Row) (map[int64][]string, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, r := range rows {
		if !seen[r.CaseID] {
			seen[r.CaseID] = true
			ids = append(ids, r.CaseID)
		}
	}
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	res, err := tx.QueryContext(ctx, `
		SELECT id, to_jsonb(tags) FROM eval_cases WHERE id = ANY($1::bigint[]) AND ($2::text = '*' OR tenant_id = $2)`,
		int64Array(ids), scope)
	if err != nil {
		return nil, fmt.Errorf("loading case tags: %w", err)
	}
	for res.Next() {
		var id int64
		var raw []byte
		if err := res.Scan(&id, &raw); err != nil {
			_ = res.Close()
			return nil, fmt.Errorf("scanning case tags: %w", err)
		}
		var tags []string
		if err := json.Unmarshal(raw, &tags); err != nil {
			continue
		}
		for _, t := range tags {
			out[id] = append(out[id], "tag:"+t)
		}
		slices.Sort(out[id])
	}
	if err := closeRows(res); err != nil {
		return nil, fmt.Errorf("loading case tags: %w", err)
	}
	return out, nil
}

// int64Array is a PostgreSQL array literal of ids.
func int64Array(ids []int64) string {
	b := []byte{'{'}
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, id, 10)
	}
	return string(append(b, '}'))
}
