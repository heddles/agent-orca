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

// Package gate decides whether an agent may be published: it checks the latest
// finished gate experiment against the gate's bar (docs/evaluations.md, "The
// publish gate"). It is pure, so every status can be tested without a database.
//
// An evaluation counts only when it is about what is being published and what
// the bar is now: it ran the revision hash being published, on the dataset at
// its current version, graded by the graders as they are now. The latest
// experiment counts, not the best one, so a pass cannot be had by rerunning
// until lucky.
package gate

import (
	"fmt"

	"github.com/heddles/agent-orca/internal/evals/compare"
)

// Statuses of a gate check.
const (
	// StatusPassed: the evaluation meets the bar. Publish may go on.
	StatusPassed = "passed"
	// StatusFailed: the evaluation is about this version and dataset and does not
	// meet the bar.
	StatusFailed = "failed"
	// StatusMissing: the agent has never been evaluated under a gate.
	StatusMissing = "missing"
	// StatusStale: the latest evaluation no longer applies: the agent, the
	// dataset or its graders changed since.
	StatusStale = "stale"
)

// Config is the bar, from the agent's gate row.
type Config struct {
	Enabled          bool
	DatasetID        int64
	Margin           float64
	MinCases         int
	MinPassRate      *float64
	MaxRegressions   *int
	BaselineRevision int
}

// Experiment is the latest finished gate experiment of the agent.
type Experiment struct {
	ID             int64
	DatasetID      int64
	DatasetVersion int
	// CandidateHash is the hash of the spec the candidate arm ran.
	CandidateHash string
	// GradersChanged is true when the dataset's graders changed since it ran.
	GradersChanged bool
	// Rows are its results with grades, for compare.
	Rows []compare.Row
	// Baseline and Candidate are the arm labels.
	Baseline, Candidate string
}

// Input is everything a check reads.
type Input struct {
	Config Config
	// DatasetVersion is the dataset's version now; DatasetExists is false when
	// it was deleted.
	DatasetVersion int
	DatasetExists  bool
	// Hash is the hash of the spec being published.
	Hash string
	// Experiment is nil when the agent has no finished gate experiment.
	Experiment *Experiment
}

// Required is the bar, as numbers a person can read.
type Required struct {
	Margin         float64  `json:"margin"`
	MinCases       int      `json:"minCases"`
	MinPassRate    *float64 `json:"minPassRate,omitempty"`
	MaxRegressions *int     `json:"maxRegressions,omitempty"`
}

// Best is what the experiment found. (The name is the proposal's; it is the
// latest experiment, not the best one.)
type Best struct {
	Cases int `json:"cases"`
	// PassRate is the candidate's; BaselinePassRate the baseline's.
	PassRate         float64 `json:"passRate"`
	BaselinePassRate float64 `json:"baselinePassRate"`
	// DiffLo is the low end of the interval for candidate minus baseline.
	DiffLo      float64 `json:"diffLo"`
	Regressions int     `json:"regressions"`
	NonInferior bool    `json:"nonInferior"`
	// Unreviewed is the candidate results left out for a grade still pending.
	Unreviewed int `json:"unreviewed,omitempty"`
}

// Decision is the outcome of a check.
type Decision struct {
	Status       string   `json:"status"`
	Required     Required `json:"required"`
	Best         *Best    `json:"best,omitempty"`
	ExperimentID int64    `json:"experimentId,omitempty"`
	Reason       string   `json:"reason"`
}

// Passed reports whether publish may go on.
func (d Decision) Passed() bool { return d.Status == StatusPassed }

// CheckGate checks the gate. A disabled gate is the caller's to skip: this
// function assumes the gate is on.
func CheckGate(in Input) Decision {
	c := in.Config
	d := Decision{Required: Required{Margin: c.Margin, MinCases: c.MinCases, MinPassRate: c.MinPassRate, MaxRegressions: c.MaxRegressions}}
	e := in.Experiment
	if e == nil {
		d.Status, d.Reason = StatusMissing, "this agent has not been evaluated yet: run the evaluation"
		return d
	}
	d.ExperimentID = e.ID
	switch {
	case !in.DatasetExists:
		d.Status, d.Reason = StatusStale, "the dataset the gate rests on no longer exists"
		return d
	case e.DatasetID != c.DatasetID:
		d.Status, d.Reason = StatusStale, "the latest evaluation ran on a different dataset than the gate uses now: run the evaluation again"
		return d
	case e.CandidateHash != in.Hash:
		d.Status, d.Reason = StatusStale, fmt.Sprintf("the latest evaluation (experiment %d) ran a different version of the agent than the one being published: run the evaluation again", e.ID)
		return d
	case e.DatasetVersion != in.DatasetVersion:
		d.Status, d.Reason = StatusStale, fmt.Sprintf("the dataset changed (version %d, evaluated on %d) since experiment %d: run the evaluation again", in.DatasetVersion, e.DatasetVersion, e.ID)
		return d
	case e.GradersChanged:
		d.Status, d.Reason = StatusStale, fmt.Sprintf("the dataset's graders changed since experiment %d: run the evaluation again", e.ID)
		return d
	}
	sum, err := compare.Summarize(e.Rows, e.Baseline, compare.Options{Seed: uint64(e.ID), MinCases: c.MinCases, Margin: c.Margin})
	if err != nil {
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("experiment %d cannot be read: %v", e.ID, err)
		return d
	}
	var cmp *compare.Comparison
	for i := range sum.Comparisons {
		if sum.Comparisons[i].Candidate == e.Candidate {
			cmp = &sum.Comparisons[i]
		}
	}
	var arm *compare.ArmSummary
	for i := range sum.Arms {
		if sum.Arms[i].Label == e.Candidate {
			arm = &sum.Arms[i]
		}
	}
	if cmp == nil || arm == nil || cmp.PassRate == nil {
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("experiment %d has no paired results to judge", e.ID)
		return d
	}
	b := &Best{
		Cases: cmp.Pairs, PassRate: arm.PassRate, BaselinePassRate: sum.Arms[0].PassRate,
		DiffLo: cmp.PassRate.CI.Lo, Regressions: len(cmp.RegressedCases), NonInferior: cmp.Verdict.NonInferior,
		Unreviewed: arm.ExcludedPending,
	}
	d.Best = b
	switch {
	case arm.ExcludedPending > 0:
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("%d results of experiment %d are still waiting for a review", arm.ExcludedPending, e.ID)
	case cmp.Pairs < c.MinCases:
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("experiment %d compared %d cases; the gate needs at least %d", e.ID, cmp.Pairs, c.MinCases)
	case !cmp.Verdict.NonInferior:
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("experiment %d: the candidate passes %.0f%% against the baseline's %.0f%%, and may be worse by more than %.0f points (low end %+.1f points)",
			e.ID, b.PassRate*100, b.BaselinePassRate*100, c.Margin*100, b.DiffLo*100)
	case c.MinPassRate != nil && b.PassRate < *c.MinPassRate:
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("experiment %d: the candidate passes %.0f%%; the gate needs %.0f%%", e.ID, b.PassRate*100, *c.MinPassRate*100)
	case c.MaxRegressions != nil && b.Regressions > *c.MaxRegressions:
		d.Status, d.Reason = StatusFailed, fmt.Sprintf("experiment %d: %d cases went from pass to fail; the gate allows %d", e.ID, b.Regressions, *c.MaxRegressions)
	default:
		d.Status, d.Reason = StatusPassed, fmt.Sprintf("experiment %d: the candidate passes %.0f%% (baseline %.0f%%) over %d cases and is not worse by more than %.0f points", e.ID, b.PassRate*100, b.BaselinePassRate*100, b.Cases, c.Margin*100)
	}
	return d
}
