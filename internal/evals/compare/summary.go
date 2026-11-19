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

package compare

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/heddles/agent-orca/internal/evals/graders"
)

// Result phases, as stored in eval_results.phase.
const (
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
	PhaseTimedOut  = "TimedOut"
	PhaseCancelled = "Cancelled"
	PhaseSkipped   = "Skipped"
)

// Grade statuses, as stored in eval_grades.status.
const (
	GradeOK            = "ok"
	GradeError         = "error"
	GradeInvalid       = "invalid"
	GradePendingReview = "pending_review"
)

// ErrNoBaseline means no row belongs to the baseline arm.
var ErrNoBaseline = errors.New("compare: no results for the baseline arm")

// Grade is one eval_grades row joined to its grader's weight and required flag.
type Grade struct {
	GraderID int64   `json:"graderId"`
	Score    float64 `json:"score"` // 0 to 1
	Pass     bool    `json:"pass"`
	Status   string  `json:"status"`
	// Weight is the grader's weight; zero means 1.
	Weight   float64 `json:"weight"`
	Required bool    `json:"required"`
}

// Row is one eval_results row (one case, arm and repeat) with its grades.
type Row struct {
	CaseID       int64   `json:"caseId"`
	Arm          string  `json:"arm"`
	Repeat       int     `json:"repeat"`
	Phase        string  `json:"phase"`
	CostUSD      float64 `json:"costUsd"`
	CostComplete bool    `json:"costComplete"`
	LatencyMS    *int    `json:"latencyMs,omitempty"`
	Grades       []Grade `json:"grades,omitempty"`
}

// ArmSummary is what one arm did.
type ArmSummary struct {
	Label string `json:"label"`
	// Cases is the cases with a usable result in this arm; Skipped those with
	// none (cancelled, skipped, or not yet graded).
	Cases   int `json:"cases"`
	Skipped int `json:"skipped"`
	Passed  int `json:"passed"`
	// ExcludedError and ExcludedPending count the results (not cases) left out
	// of every figure because a grade could not be produced (error) or is
	// waiting for a person (pending review). Skipped counts the cases that this
	// leaves with no usable result at all.
	ExcludedError   int `json:"excludedError"`
	ExcludedPending int `json:"excludedPending"`
	// PassRate is Passed over Cases, with the Wilson interval for it.
	PassRate   float64  `json:"passRate"`
	PassRateCI Interval `json:"passRateCi"`
	MeanScore  float64  `json:"meanScore"`
	// FailureRate is the share of finished runs that failed or timed out.
	FailureRate float64 `json:"failureRate"`
	// CostTotalUSD is every run's cost, repeats and skipped cases included.
	CostTotalUSD float64 `json:"costTotalUsd"`
	// CostMedianUSD is the median over cases of a case's mean cost.
	CostMedianUSD float64 `json:"costMedianUsd"`
	// CostIncomplete is true when some run's cost was not fully captured, so the
	// cost figures are a floor.
	CostIncomplete bool `json:"costIncomplete"`
	// HasLatency is false when no run recorded a latency.
	HasLatency   bool    `json:"hasLatency"`
	LatencyP50MS float64 `json:"latencyP50Ms"`
	LatencyP95MS float64 `json:"latencyP95Ms"`
	// RepeatedCases is the cases with two or more usable repeats, and
	// FlakyCases those among them whose repeats did not all agree on pass or
	// fail. Stability is 1 - FlakyCases/RepeatedCases, or 1 with no repeats.
	RepeatedCases int     `json:"repeatedCases"`
	FlakyCases    int     `json:"flakyCases"`
	Stability     float64 `json:"stability"`
}

// Comparison is a candidate arm against the baseline.
type Comparison struct {
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
	// Pairs is the cases usable in both arms; Skipped the rest.
	Pairs   int `json:"pairs"`
	Skipped int `json:"skipped"`
	// PassRate is nil when there are no pairs.
	PassRate *PassRateDiff `json:"passRate,omitempty"`
	// Score, Cost and Latency are nil when no pair has the quantity.
	Score   *MeanDiff     `json:"score,omitempty"`
	Cost    *MeanDiff     `json:"cost,omitempty"`
	Latency *MeanDiff     `json:"latency,omitempty"`
	CostMed *MedianDiff   `json:"costMedian,omitempty"`
	LatMed  *MedianDiff   `json:"latencyMedian,omitempty"`
	Verdict VerdictResult `json:"verdict"`
	// Case IDs in ascending order.
	RegressedCases []int64 `json:"regressedCases"`
	ImprovedCases  []int64 `json:"improvedCases"`
	SkippedCases   []int64 `json:"skippedCases"`
}

// Summary is what an experiment's endpoints return.
type Summary struct {
	Baseline    string       `json:"baseline"`
	Options     Options      `json:"options"`
	Arms        []ArmSummary `json:"arms"`
	Comparisons []Comparison `json:"comparisons"`
	// ExcludedError and ExcludedPending total the arms' results left out of every
	// figure for a grade in error or waiting for a person.
	ExcludedError   int `json:"excludedError"`
	ExcludedPending int `json:"excludedPending"`
}

// caseOutcome is one arm's outcome on one case, with repeats folded in.
type caseOutcome struct {
	usable  bool
	pass    bool
	score   float64
	flaky   bool
	repeats int
	cost    float64 // mean over all the case's rows
	latency float64 // mean over rows that have one
	hasLat  bool
	// errored and pending count the case's results left out for a grade in
	// error or waiting for a person.
	errored int
	pending int
}

// exclusion says why a succeeded result is left out of a comparison.
type exclusion int

const (
	notExcluded exclusion = iota
	// excludedPlatform is a cancelled or skipped run, or one with no grades yet.
	excludedPlatform
	// excludedError is a result with a grade whose grader could not run.
	excludedError
	// excludedPending is a result with a grade still waiting for a person.
	excludedPending
)

// rowOutcome classifies one result: usable or not, and if so pass and score.
// A succeeded result passes by graders.CasePass, the rule stored in
// eval_results.case_pass: every required grader passes and the weighted mean
// score of the others is at least graders.CasePassMean. An invalid judge reply
// is a failed grade with score 0. A grade in error or pending review leaves the
// whole result out (the reason is returned), so a result is judged on all of its
// graders or not at all, and the exclusion is counted in the summary.
func rowOutcome(r Row) (usable, pass bool, score float64, ex exclusion, err error) {
	switch r.Phase {
	case PhaseCancelled, PhaseSkipped:
		return false, false, 0, excludedPlatform, nil
	case PhaseFailed, PhaseTimedOut:
		// The agent did not answer: that is a failed case, not a missing one.
		return true, false, 0, notExcluded, nil
	case PhaseSucceeded:
	default:
		return false, false, 0, notExcluded, invalidf("case %d arm %q has unknown phase %q", r.CaseID, r.Arm, r.Phase)
	}
	if len(r.Grades) == 0 {
		return false, false, 0, excludedPlatform, nil
	}
	var wsum, wscore float64
	graded := make([]graders.Graded, 0, len(r.Grades))
	for _, g := range r.Grades {
		if !finite(g.Score) || g.Score < 0 || g.Score > 1 {
			return false, false, 0, notExcluded, invalidf("case %d arm %q has score %v outside 0 to 1", r.CaseID, r.Arm, g.Score)
		}
		if !finite(g.Weight) || g.Weight < 0 {
			return false, false, 0, notExcluded, invalidf("case %d arm %q has grader weight %v", r.CaseID, r.Arm, g.Weight)
		}
		switch g.Status {
		case GradeOK, GradeInvalid:
		case GradeError, GradePendingReview:
		default:
			return false, false, 0, notExcluded, invalidf("case %d arm %q has unknown grade status %q", r.CaseID, r.Arm, g.Status)
		}
	}
	for _, g := range r.Grades {
		switch g.Status {
		case GradeError:
			return false, false, 0, excludedError, nil
		case GradePendingReview:
			ex = excludedPending
		}
	}
	if ex != notExcluded {
		return false, false, 0, ex, nil
	}
	for _, g := range r.Grades {
		w := g.Weight
		if w == 0 {
			w = 1
		}
		score := g.Score
		if g.Status == GradeInvalid {
			score, g.Pass = 0, false
		}
		wsum += w
		wscore += w * score
		graded = append(graded, graders.Graded{Result: graders.Result{Pass: g.Pass, Score: score}, Weight: g.Weight, Required: g.Required})
	}
	return true, graders.CasePass(graded), wscore / wsum, notExcluded, nil
}

// foldCase folds the rows of one case in one arm.
func foldCase(rows []Row) (caseOutcome, error) {
	var o caseOutcome
	var passes int
	var score, cost, lat float64
	var nLat int
	for _, r := range rows {
		cost += r.CostUSD
		if r.LatencyMS != nil && r.Phase != PhaseSkipped && r.Phase != PhaseCancelled {
			lat += float64(*r.LatencyMS)
			nLat++
		}
		usable, pass, s, ex, err := rowOutcome(r)
		if err != nil {
			return o, err
		}
		if !usable {
			switch ex {
			case excludedError:
				o.errored++
			case excludedPending:
				o.pending++
			case notExcluded, excludedPlatform:
			}
			continue
		}
		o.repeats++
		score += s
		if pass {
			passes++
		}
	}
	o.cost = cost / float64(len(rows))
	if nLat > 0 {
		o.latency, o.hasLat = lat/float64(nLat), true
	}
	if o.repeats == 0 {
		return o, nil
	}
	o.usable = true
	o.pass = passes*2 > o.repeats // a majority of repeats; a tie fails
	o.score = score / float64(o.repeats)
	o.flaky = o.repeats > 1 && passes > 0 && passes < o.repeats
	return o, nil
}

func validateRow(r Row) error {
	if !finite(r.CostUSD) || r.CostUSD < 0 {
		return invalidf("case %d arm %q has cost %v", r.CaseID, r.Arm, r.CostUSD)
	}
	if r.LatencyMS != nil && *r.LatencyMS < 0 {
		return invalidf("case %d arm %q has latency %d", r.CaseID, r.Arm, *r.LatencyMS)
	}
	if r.Arm == "" {
		return invalidf("case %d has a result with no arm", r.CaseID)
	}
	return nil
}

// Summarize turns the paired result rows of an experiment into its summary:
// each arm on its own, and each other arm against the baseline. Arms appear in
// order of first appearance in rows, the baseline first.
//
// How rows become outcomes, stated so the numbers can be checked by hand. A run
// that failed or timed out is a failed case with score 0. A run that was
// cancelled or skipped, or with a grade in error or pending review, is unusable. A succeeded run passes
// by graders.CasePass, the rule stored in eval_results.case_pass: every required
// grader passes and the weighted mean score of the others is at least 0.5; its
// score is the weight-averaged score. An invalid judge reply is a failed grade
// of score 0; a grade in error or pending review leaves the result out, and
// ArmSummary counts those results. With repeats, a
// case passes in an arm when more than half of its usable repeats pass, and
// its score, cost and latency are the means over repeats. A case with no usable
// result in an arm is skipped for every comparison that involves the arm.
func Summarize(rows []Row, baseline string, opts Options) (Summary, error) {
	o, err := opts.WithDefaults()
	if err != nil {
		return Summary{}, err
	}
	type key struct {
		arm string
		c   int64
		rep int
	}
	seen := map[key]bool{}
	byArm := map[string]map[int64][]Row{}
	var arms []string
	for _, r := range rows {
		if err := validateRow(r); err != nil {
			return Summary{}, err
		}
		k := key{r.Arm, r.CaseID, r.Repeat}
		if seen[k] {
			return Summary{}, invalidf("duplicate result for case %d arm %q repeat %d", r.CaseID, r.Arm, r.Repeat)
		}
		seen[k] = true
		if byArm[r.Arm] == nil {
			byArm[r.Arm] = map[int64][]Row{}
			arms = append(arms, r.Arm)
		}
		byArm[r.Arm][r.CaseID] = append(byArm[r.Arm][r.CaseID], r)
	}
	if byArm[baseline] == nil {
		return Summary{}, fmt.Errorf("%w %q", ErrNoBaseline, baseline)
	}
	sort.SliceStable(arms, func(i, j int) bool { return arms[i] == baseline && arms[j] != baseline })

	outcomes := map[string]map[int64]caseOutcome{}
	s := Summary{Baseline: baseline, Options: o, Comparisons: []Comparison{}}
	for _, arm := range arms {
		outcomes[arm] = map[int64]caseOutcome{}
		for id, rs := range byArm[arm] {
			oc, err := foldCase(rs)
			if err != nil {
				return Summary{}, err
			}
			outcomes[arm][id] = oc
		}
		as, err := summarizeArm(arm, byArm[arm], outcomes[arm], o)
		if err != nil {
			return Summary{}, err
		}
		s.Arms = append(s.Arms, as)
		s.ExcludedError += as.ExcludedError
		s.ExcludedPending += as.ExcludedPending
	}
	for _, arm := range arms {
		if arm == baseline {
			continue
		}
		c, err := compareArms(baseline, arm, outcomes, o)
		if err != nil {
			return Summary{}, err
		}
		s.Comparisons = append(s.Comparisons, c)
	}
	return s, nil
}

func sortedCaseIDs(m map[int64]caseOutcome) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func summarizeArm(label string, rows map[int64][]Row, out map[int64]caseOutcome, o Options) (ArmSummary, error) {
	a := ArmSummary{Label: label, Stability: 1}
	var scoreSum float64
	caseCosts := make([]float64, 0, len(out))
	var lats []float64
	for _, id := range sortedCaseIDs(out) {
		oc := out[id]
		a.ExcludedError += oc.errored
		a.ExcludedPending += oc.pending
		caseCosts = append(caseCosts, oc.cost)
		if !oc.usable {
			a.Skipped++
		} else {
			a.Cases++
			scoreSum += oc.score
			if oc.pass {
				a.Passed++
			}
			if oc.repeats > 1 {
				a.RepeatedCases++
				if oc.flaky {
					a.FlakyCases++
				}
			}
		}
		for _, r := range rows[id] {
			a.CostTotalUSD += r.CostUSD
			a.CostIncomplete = a.CostIncomplete || !r.CostComplete
			if r.LatencyMS != nil && r.Phase != PhaseSkipped && r.Phase != PhaseCancelled {
				lats = append(lats, float64(*r.LatencyMS))
			}
		}
	}
	var finished, failed int
	for _, rs := range rows {
		for _, r := range rs {
			switch r.Phase {
			case PhaseSucceeded:
				finished++
			case PhaseFailed, PhaseTimedOut:
				finished++
				failed++
			}
		}
	}
	if finished > 0 {
		a.FailureRate = float64(failed) / float64(finished)
	}
	ci, err := WilsonCI(a.Passed, a.Cases, o.Confidence)
	if err != nil {
		return a, err
	}
	a.PassRateCI = ci
	if a.Cases > 0 {
		a.PassRate = float64(a.Passed) / float64(a.Cases)
		a.MeanScore = scoreSum / float64(a.Cases)
	}
	if len(caseCosts) > 0 {
		a.CostMedianUSD = median(caseCosts)
	}
	if len(lats) > 0 {
		a.HasLatency = true
		a.LatencyP50MS = quantile(lats, 0.5)
		a.LatencyP95MS = quantile(lats, 0.95)
	}
	if a.RepeatedCases > 0 {
		a.Stability = 1 - float64(a.FlakyCases)/float64(a.RepeatedCases)
	}
	return a, nil
}

func compareArms(baseline, candidate string, outcomes map[string]map[int64]caseOutcome, o Options) (Comparison, error) {
	base, cand := outcomes[baseline], outcomes[candidate]
	c := Comparison{Baseline: baseline, Candidate: candidate, RegressedCases: []int64{}, ImprovedCases: []int64{}, SkippedCases: []int64{}}
	ids := map[int64]bool{}
	for id := range base {
		ids[id] = true
	}
	for id := range cand {
		ids[id] = true
	}
	order := make([]int64, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	slices.Sort(order)

	var counts PairedCounts
	var bScore, cScore, bCost, cCost, bLat, cLat []float64
	for _, id := range order {
		b, bok := base[id]
		d, dok := cand[id]
		if !bok || !dok || !b.usable || !d.usable {
			c.Skipped++
			c.SkippedCases = append(c.SkippedCases, id)
			continue
		}
		switch {
		case b.pass && d.pass:
			counts.BothPass++
		case !b.pass && d.pass:
			counts.Improved++
			c.ImprovedCases = append(c.ImprovedCases, id)
		case b.pass && !d.pass:
			counts.Regressed++
			c.RegressedCases = append(c.RegressedCases, id)
		default:
			counts.BothFail++
		}
		bScore, cScore = append(bScore, b.score), append(cScore, d.score)
		bCost, cCost = append(bCost, b.cost), append(cCost, d.cost)
		if b.hasLat && d.hasLat {
			bLat, cLat = append(bLat, b.latency), append(cLat, d.latency)
		}
	}
	c.Pairs = counts.N()
	if c.Pairs > 0 {
		pr, err := ComparePassRates(counts, o.Confidence)
		if err != nil {
			return c, err
		}
		c.PassRate = &pr
		if c.Score, err = ptrMean(bScore, cScore, o, 0); err != nil {
			return c, err
		}
		if c.Cost, err = ptrMean(bCost, cCost, o, 1); err != nil {
			return c, err
		}
		if c.CostMed, err = ptrMedian(bCost, cCost); err != nil {
			return c, err
		}
		if len(bLat) > 0 {
			if c.Latency, err = ptrMean(bLat, cLat, o, 2); err != nil {
				return c, err
			}
			if c.LatMed, err = ptrMedian(bLat, cLat); err != nil {
				return c, err
			}
		}
	}
	var pr PassRateDiff
	if c.PassRate != nil {
		pr = *c.PassRate
	}
	v, err := Decide(pr, o)
	if err != nil {
		return c, err
	}
	c.Verdict = v
	return c, nil
}

// ptrMean runs PairedMeanDiff with a seed offset, so the three quantities
// resample independently but reproducibly.
func ptrMean(b, c []float64, o Options, offset uint64) (*MeanDiff, error) {
	o.Seed += offset
	m, err := PairedMeanDiff(b, c, o)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func ptrMedian(b, c []float64) (*MedianDiff, error) {
	m, err := PairedMedianDiff(b, c)
	if err != nil {
		return nil, err
	}
	return &m, nil
}
