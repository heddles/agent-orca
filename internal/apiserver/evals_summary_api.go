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

package apiserver

import (
	"encoding/json"
	"net/http"

	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/evals/runner"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// summaryView is the body of GET /api/evals/experiments/{id}/summary.
type summaryView struct {
	ExperimentID int64  `json:"experimentId"`
	Status       string `json:"status"`
	// Live is true when the summary was worked out now from the results so far
	// (the experiment has not ended), false when it is the one stored at the end.
	Live bool `json:"live"`
	// Partial is true when the comparison does not cover every case in every
	// arm: the experiment is still running, or stopped early (cancel, budget).
	Partial bool            `json:"partial"`
	Summary json.RawMessage `json:"summary"`
}

// getEvalExperimentSummary serves the paired comparison of an experiment:
// per-arm pass rates with Wilson intervals, the paired deltas with McNemar's
// test, cost and latency differences and the verdict (internal/evals/compare).
// Once the experiment has ended it is the stored summary; before that it is
// computed from the results so far, marked live and partial.
func (s *UIServer) getEvalExperimentSummary(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.experimentPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	e, ok := s.loadExperiment(w, r, store, id)
	if !ok {
		return
	}
	view := summaryView{ExperimentID: e.ID, Status: e.Status}
	if len(e.Summary) > 0 {
		var head struct {
			Partial bool `json:"partial"`
		}
		_ = json.Unmarshal(e.Summary, &head)
		view.Partial, view.Summary = head.Partial, e.Summary
		jsonResponse(w, view)
		return
	}
	switch e.Status {
	case postgresql.ExperimentFailed:
		jsonError(w, "the experiment failed before it had a summary: "+e.Error, http.StatusConflict)
		return
	case postgresql.ExperimentQueued:
		jsonError(w, "the experiment has not started", http.StatusConflict)
		return
	}
	rows, err := store.EvalSummaryRows(pgScope(r.Context()), id)
	if err != nil {
		evalFail(w, "reading the results", err)
		return
	}
	sum := runner.Summary{Partial: true, Results: len(rows), SpentUSD: e.SpentUSD}
	for _, row := range rows {
		if row.Phase == compare.PhaseSkipped {
			sum.Skipped++
		}
	}
	if len(e.Arms) > 0 && len(rows) > 0 {
		cmp, err := compare.Summarize(rows, e.Arms[0].Label, compare.Options{Seed: uint64(e.ID)})
		if err != nil {
			sum.ComparisonError = err.Error()
		} else {
			sum.Comparison = &cmp
		}
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		evalFail(w, "encoding the summary", err)
		return
	}
	view.Live, view.Partial, view.Summary = true, true, raw
	jsonResponse(w, view)
}
