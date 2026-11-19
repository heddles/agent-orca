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
	"fmt"
	"net/http"
	"testing"

	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// pairedRows is n cases in two arms: the candidate passes all, the baseline the
// first half.
func pairedRows(n int) []compare.Row {
	var rows []compare.Row
	for c := 1; c <= n; c++ {
		for _, arm := range []string{"base", "cheap"} {
			pass := arm == "cheap" || c <= n/2
			score := 0.0
			if pass {
				score = 1
			}
			lat := 100
			rows = append(rows, compare.Row{CaseID: int64(c), Arm: arm, Phase: compare.PhaseSucceeded, CostUSD: 0.01, CostComplete: true, LatencyMS: &lat,
				Grades: []compare.Grade{{GraderID: 1, Score: score, Pass: pass, Status: compare.GradeOK, Weight: 1, Required: true}}})
		}
	}
	return rows
}

func (f *expFixture) summary(role tenancy.Role, tenant string, id int64) (int, summaryView) {
	f.t.Helper()
	rec := f.do(idFor(role, tenant), http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d/summary", id), "")
	var v summaryView
	if rec.Code == http.StatusOK {
		v = evalDecode[summaryView](f.t, rec)
	}
	return rec.Code, v
}

func TestExperimentSummary_QueuedFailedLiveAndStored(t *testing.T) {
	f := newExpFixture(t)
	_, e := f.create()
	if code, _ := f.summary(tenancy.RoleViewer, isoA, e.ID); code != http.StatusConflict {
		t.Fatalf("queued summary = %d, want 409", code)
	}

	// Running: worked out from the results so far, marked live and partial.
	f.exps.rows = map[int64][]compare.Row{e.ID: pairedRows(40)}
	f.exps.exps[e.ID].Status = postgresql.ExperimentRunning
	code, v := f.summary(tenancy.RoleViewer, isoA, e.ID)
	if code != http.StatusOK || !v.Live || !v.Partial || v.Status != postgresql.ExperimentRunning {
		t.Fatalf("live summary = %d %+v", code, v)
	}
	var live struct {
		Partial    bool
		Results    int
		Comparison compare.Summary
	}
	if err := json.Unmarshal(v.Summary, &live); err != nil {
		t.Fatal(err)
	}
	c := live.Comparison
	if !live.Partial || live.Results != 80 || len(c.Arms) != 2 || len(c.Comparisons) != 1 {
		t.Fatalf("live = %+v", live)
	}
	cmp := c.Comparisons[0]
	if cmp.Pairs != 40 || cmp.PassRate == nil || cmp.PassRate.Counts.Improved != 20 || cmp.PassRate.Counts.Regressed != 0 ||
		cmp.PassRate.McNemar.P >= 0.05 || cmp.Cost == nil || cmp.Latency == nil || cmp.Verdict.Verdict != compare.VerdictBetter {
		t.Errorf("comparison = %+v", cmp)
	}
	if c.Arms[0].PassRateCI.Hi <= c.Arms[0].PassRate || c.Arms[0].Passed != 20 {
		t.Errorf("baseline arm = %+v", c.Arms[0])
	}

	// Ended: the stored summary, partial as stored.
	f.exps.exps[e.ID].Status = postgresql.ExperimentCancelled
	f.exps.exps[e.ID].Summary = json.RawMessage(`{"stoppedBy":"cancel","partial":true,"results":4}`)
	code, v = f.summary(tenancy.RoleViewer, isoA, e.ID)
	if code != http.StatusOK || v.Live || !v.Partial {
		t.Fatalf("stored summary = %d %+v", code, v)
	}
	f.exps.exps[e.ID].Summary = json.RawMessage(`{"results":80}`)
	if _, v = f.summary(tenancy.RoleViewer, isoA, e.ID); v.Partial {
		t.Errorf("a complete summary is not partial: %+v", v)
	}

	// Failed before it had a summary.
	f.exps.exps[e.ID].Status, f.exps.exps[e.ID].Summary, f.exps.exps[e.ID].Error = postgresql.ExperimentFailed, nil, "no budget"
	if code, _ := f.summary(tenancy.RoleViewer, isoA, e.ID); code != http.StatusConflict {
		t.Errorf("failed summary = %d, want 409", code)
	}
}

func TestExperimentSummary_ForeignTenantIsNotFound(t *testing.T) {
	f := newExpFixture(t)
	_, e := f.create()
	f.exps.exps[e.ID].Status = postgresql.ExperimentRunning
	if code, _ := f.summary(tenancy.RoleAdmin, isoB, e.ID); code != http.StatusNotFound {
		t.Errorf("foreign tenant = %d, want 404", code)
	}
}
