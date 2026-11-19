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

package gate

import (
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/evals/compare"
)

// rows are n cases: the baseline passes the first basePass, the candidate the
// first candPass.
func rows(n, basePass, candPass int) []compare.Row {
	var out []compare.Row
	for c := 1; c <= n; c++ {
		for _, arm := range []struct {
			label string
			pass  int
		}{{"baseline", basePass}, {"candidate", candPass}} {
			ok := c <= arm.pass
			score := 0.0
			if ok {
				score = 1
			}
			out = append(out, compare.Row{CaseID: int64(c), Arm: arm.label, Phase: compare.PhaseSucceeded, CostComplete: true,
				Grades: []compare.Grade{{GraderID: 1, Score: score, Pass: ok, Status: compare.GradeOK, Weight: 1, Required: true}}})
		}
	}
	return out
}

func input(r []compare.Row) Input {
	return Input{
		Config:         Config{Enabled: true, DatasetID: 1, Margin: 0.05, MinCases: 30, BaselineRevision: 1},
		DatasetVersion: 7, DatasetExists: true, Hash: "h1",
		Experiment: &Experiment{ID: 42, DatasetID: 1, DatasetVersion: 7, CandidateHash: "h1", Rows: r, Baseline: "baseline", Candidate: "candidate"},
	}
}

func TestCheckGate(t *testing.T) {
	floor, none := 0.9, 0
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		rows   []compare.Row
		want   string
		reason string
	}{
		{"no experiment", func(in *Input) { in.Experiment = nil }, nil, StatusMissing, "not been evaluated"},
		{"a better candidate passes", nil, rows(60, 30, 60), StatusPassed, "passes 100%"},
		{"an equal candidate passes", nil, rows(200, 150, 150), StatusPassed, ""},
		{"a worse candidate fails", nil, rows(60, 60, 30), StatusFailed, "worse by more than 5 points"},
		{"too few cases", func(in *Input) { in.Config.MinCases = 100 }, rows(60, 30, 60), StatusFailed, "at least 100"},
		{"below the pass floor", func(in *Input) { in.Config.MinPassRate = &floor }, rows(60, 30, 50), StatusFailed, "needs 90%"},
		{"a regression is too many", func(in *Input) { in.Config.MaxRegressions, in.Config.Margin = &none, 0.5 }, regressionRows(), StatusFailed, "went from pass to fail"},
		{"another dataset", func(in *Input) { in.Config.DatasetID = 2 }, rows(60, 30, 60), StatusStale, "different dataset"},
		{"the dataset was deleted", func(in *Input) { in.DatasetExists = false }, rows(60, 30, 60), StatusStale, "no longer exists"},
		{"the dataset was edited", func(in *Input) { in.DatasetVersion = 8 }, rows(60, 30, 60), StatusStale, "dataset changed"},
		{"another revision hash", func(in *Input) { in.Hash = "h2" }, rows(60, 30, 60), StatusStale, "different version of the agent"},
		{"graders changed", func(in *Input) { in.Experiment.GradersChanged = true }, rows(60, 30, 60), StatusStale, "graders changed"},
		{"results still in review", func(in *Input) {}, pendingRows(), StatusFailed, "waiting for a review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(tc.rows)
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			d := CheckGate(in)
			if d.Status != tc.want || !strings.Contains(d.Reason, tc.reason) || d.Passed() != (tc.want == StatusPassed) {
				t.Errorf("decision = %+v, want %s with %q", d, tc.want, tc.reason)
			}
			if tc.want != StatusMissing && d.ExperimentID != 42 {
				t.Errorf("experiment = %d", d.ExperimentID)
			}
			if tc.want == StatusPassed || tc.want == StatusFailed {
				if d.Best == nil || d.Required.MinCases != in.Config.MinCases {
					t.Errorf("best/required = %+v %+v", d.Best, d.Required)
				}
			}
		})
	}
}

// regressionRows: the candidate passes the cases 11 to 60, the baseline 1 to 50;
// so 10 cases regress and 10 improve.
func regressionRows() []compare.Row {
	var out []compare.Row
	for c := 1; c <= 60; c++ {
		for _, arm := range []string{"baseline", "candidate"} {
			ok := (arm == "baseline" && c <= 50) || (arm == "candidate" && c > 10)
			score := 0.0
			if ok {
				score = 1
			}
			out = append(out, compare.Row{CaseID: int64(c), Arm: arm, Phase: compare.PhaseSucceeded, CostComplete: true,
				Grades: []compare.Grade{{GraderID: 1, Score: score, Pass: ok, Status: compare.GradeOK, Weight: 1, Required: true}}})
		}
	}
	return out
}

func pendingRows() []compare.Row {
	r := rows(60, 30, 60)
	for i := range r {
		if r[i].Arm == "candidate" && r[i].CaseID == 1 {
			r[i].Grades[0].Status = compare.GradePendingReview
		}
	}
	return r
}

// The margin moves the bar: the same experiment passes a wide margin and fails
// a narrow one.
func TestCheckGateMarginMovesTheBar(t *testing.T) {
	r := rows(60, 54, 48) // six cases worse, no improvement
	narrow, wide := input(r), input(r)
	narrow.Config.Margin, wide.Config.Margin = 0.02, 0.5
	if CheckGate(narrow).Passed() {
		t.Error("a narrow margin should fail a candidate that lost 6 of 60")
	}
	if !CheckGate(wide).Passed() {
		t.Errorf("a wide margin should pass: %+v", CheckGate(wide))
	}
}
