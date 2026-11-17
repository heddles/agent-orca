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
	"math"
	"reflect"
	"slices"
	"testing"
)

func decide(t *testing.T, c PairedCounts, o Options) VerdictResult {
	t.Helper()
	d, err := ComparePassRates(c, 0.95)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Decide(d, o)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    PairedCounts
		want Verdict
	}{
		{"too few cases, however lopsided", PairedCounts{Improved: 29}, VerdictNotEnoughData},
		{"clear improvement", PairedCounts{BothPass: 20, Improved: 15, Regressed: 1, BothFail: 14}, VerdictBetter},
		{"clear regression", PairedCounts{BothPass: 20, Improved: 1, Regressed: 15, BothFail: 14}, VerdictWorse},
		{"all one way", PairedCounts{Improved: 40}, VerdictBetter},
		{"all ties", PairedCounts{BothPass: 40}, VerdictNoClearDifference},
		{"a small lead is not enough", PairedCounts{BothPass: 20, Improved: 4, Regressed: 2, BothFail: 14}, VerdictNoClearDifference},
		{"thirty cases is enough to speak", PairedCounts{BothPass: 10, Improved: 15, BothFail: 5}, VerdictBetter},
	} {
		v := decide(t, tc.c, Options{})
		if v.Verdict != tc.want {
			t.Errorf("%s: verdict = %q (%s), want %q", tc.name, v.Verdict, v.Reason, tc.want)
		}
		if v.Conclusive != (v.Verdict == VerdictBetter || v.Verdict == VerdictWorse) {
			t.Errorf("%s: Conclusive = %v for %q", tc.name, v.Conclusive, v.Verdict)
		}
		if v.Reason == "" {
			t.Errorf("%s: no reason", tc.name)
		}
	}
}

func TestVerdictSwapFlips(t *testing.T) {
	flip := map[Verdict]Verdict{
		VerdictBetter: VerdictWorse, VerdictWorse: VerdictBetter,
		VerdictNoClearDifference: VerdictNoClearDifference, VerdictNotEnoughData: VerdictNotEnoughData,
	}
	for _, c := range []PairedCounts{
		{BothPass: 20, Improved: 15, Regressed: 1, BothFail: 14},
		{BothPass: 20, Improved: 4, Regressed: 2, BothFail: 14},
		{Improved: 29}, {BothPass: 40}, {BothFail: 10, Improved: 30},
	} {
		a, b := decide(t, c, Options{}), decide(t, c.Swap(), Options{})
		if b.Verdict != flip[a.Verdict] {
			t.Errorf("%+v: %q, swapped %q, want %q", c, a.Verdict, b.Verdict, flip[a.Verdict])
		}
	}
}

func TestNonInferiority(t *testing.T) {
	// 100 cases, 3 lost and 3 won: the lowest plausible difference is far below
	// -5 points, so no claim of non-inferiority.
	small := PairedCounts{BothPass: 47, Improved: 3, Regressed: 3, BothFail: 47}
	if v := decide(t, small, Options{}); v.NonInferior || v.Verdict != VerdictNoClearDifference {
		t.Errorf("100 cases with a 6%% discordance = %+v, want no non-inferiority", v)
	}
	// 1000 cases, 30 each way: the interval is within about 2.5 points.
	big := PairedCounts{BothPass: 470, Improved: 30, Regressed: 30, BothFail: 470}
	v := decide(t, big, Options{})
	if !v.NonInferior || v.Verdict != VerdictNoClearDifference || v.Conclusive {
		t.Errorf("1000 cases, even = %+v, want non-inferior with no clear difference", v)
	}
	// A tighter margin withholds the claim.
	if v := decide(t, big, Options{Margin: 0.01}); v.NonInferior {
		t.Errorf("margin 0.01 = %+v, want not non-inferior", v)
	}
	// Too few cases: never non-inferior.
	if v := decide(t, PairedCounts{BothPass: 20}, Options{}); v.NonInferior {
		t.Errorf("20 cases = %+v, want not non-inferior", v)
	}
	// A clear regression is not non-inferior.
	if v := decide(t, PairedCounts{BothPass: 20, Improved: 1, Regressed: 15, BothFail: 14}, Options{}); v.NonInferior {
		t.Errorf("a regression = %+v", v)
	}
}

func TestDecideWithNoCases(t *testing.T) {
	v, err := Decide(PassRateDiff{}, Options{})
	if err != nil || v.Verdict != VerdictNotEnoughData || v.Conclusive {
		t.Errorf("no cases = %+v, %v", v, err)
	}
}

func TestDecideRejectsBadOptions(t *testing.T) {
	d, _ := ComparePassRates(PairedCounts{BothPass: 40}, 0.95)
	if _, err := Decide(d, Options{Confidence: math.NaN()}); !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v", err)
	}
}

// ---- Summarize ----

const (
	armBase = "base"
	armNew  = "new"
)

// gradeRows makes one succeeded result with one required grader.
func gradeRow(arm string, c int64, pass bool, score, cost float64, lat int) Row {
	return Row{
		CaseID: c, Arm: arm, Phase: PhaseSucceeded, CostUSD: cost, CostComplete: true, LatencyMS: new(lat),
		Grades: []Grade{{GraderID: 1, Score: score, Pass: pass, Status: GradeOK, Weight: 1, Required: true}},
	}
}

// fortyCases: the baseline passes cases 1 to 20; the candidate passes 1 to 20
// except 5 and 6, and also 21 to 34, so 2 regressed, 14 improved, 18 tied
// passing... 18 both-pass, 6 both-fail.
func fortyCases() []Row {
	var rows []Row
	for c := int64(1); c <= 40; c++ {
		bp := c <= 20
		cp := (c <= 20 && c != 5 && c != 6) || (c >= 21 && c <= 34)
		bs, cs := 0.2, 0.2
		if bp {
			bs = 0.9
		}
		if cp {
			cs = 0.95
		}
		rows = append(rows, gradeRow(armBase, c, bp, bs, 0.01, 1000+int(c)), gradeRow(armNew, c, cp, cs, 0.02, 800+int(c)))
	}
	return rows
}

func TestSummarizeTwoArms(t *testing.T) {
	s, err := Summarize(fortyCases(), armBase, Options{Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Arms) != 2 || s.Arms[0].Label != armBase || s.Arms[1].Label != armNew || len(s.Comparisons) != 1 {
		t.Fatalf("shape = %+v", s)
	}
	b, n := s.Arms[0], s.Arms[1]
	if b.Cases != 40 || b.Passed != 20 || b.PassRate != 0.5 || !b.PassRateCI.Contains(0.5) {
		t.Errorf("baseline = %+v", b)
	}
	if n.Passed != 32 || n.PassRate != 0.8 {
		t.Errorf("candidate = %+v", n)
	}
	near(t, "baseline total cost", b.CostTotalUSD, 0.40, 1e-9)
	near(t, "candidate median cost", n.CostMedianUSD, 0.02, 1e-9)
	near(t, "baseline mean score", b.MeanScore, (20*0.9+20*0.2)/40, 1e-9)
	if !b.HasLatency || b.LatencyP50MS < 1000 || b.LatencyP95MS < b.LatencyP50MS || b.LatencyP95MS > 1040 {
		t.Errorf("latency = %+v", b)
	}
	if b.CostIncomplete || b.Stability != 1 || b.FailureRate != 0 {
		t.Errorf("baseline marks = %+v", b)
	}

}

func TestSummarizeTwoArmsComparison(t *testing.T) {
	s, err := Summarize(fortyCases(), armBase, Options{Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Comparisons[0]
	if c.Pairs != 40 || c.Skipped != 0 {
		t.Errorf("pairs = %d skipped = %d", c.Pairs, c.Skipped)
	}
	counts := c.PassRate.Counts
	if want := (PairedCounts{BothPass: 18, Improved: 14, Regressed: 2, BothFail: 6}); counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	if !reflect.DeepEqual(c.RegressedCases, []int64{5, 6}) || len(c.ImprovedCases) != 14 || c.ImprovedCases[0] != 21 || c.ImprovedCases[13] != 34 {
		t.Errorf("regressed %v improved %v", c.RegressedCases, c.ImprovedCases)
	}
	near(t, "diff", c.PassRate.Diff, 0.3, 1e-12)
	if c.Verdict.Verdict != VerdictBetter {
		t.Errorf("verdict = %+v", c.Verdict)
	}
	if c.Score == nil || c.Score.Mean <= 0 || c.Cost == nil || c.Cost.Mean <= 0 || c.Latency == nil || c.Latency.Mean >= 0 {
		t.Errorf("score %+v cost %+v latency %+v", c.Score, c.Cost, c.Latency)
	}
	near(t, "median cost diff", c.CostMed.Median, 0.01, 1e-9)
	near(t, "relative cost", c.CostMed.Rel, 1, 1e-9)
}

func TestSummarizeIsReproducible(t *testing.T) {
	a, _ := Summarize(fortyCases(), armBase, Options{Seed: 7})
	b, _ := Summarize(fortyCases(), armBase, Options{Seed: 7})
	if !reflect.DeepEqual(a, b) {
		t.Error("the same rows and seed gave different summaries")
	}
	// Row order must not matter.
	rows := fortyCases()
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	c, _ := Summarize(rows, armBase, Options{Seed: 7})
	if !reflect.DeepEqual(a.Comparisons, c.Comparisons) {
		t.Error("reordering the rows changed the comparison")
	}
}

func TestSummarizeSwappingTheBaseline(t *testing.T) {
	a, _ := Summarize(fortyCases(), armBase, Options{})
	b, _ := Summarize(fortyCases(), armNew, Options{})
	ca, cb := a.Comparisons[0], b.Comparisons[0]
	if ca.Verdict.Verdict != VerdictBetter || cb.Verdict.Verdict != VerdictWorse {
		t.Errorf("verdicts %q and %q", ca.Verdict.Verdict, cb.Verdict.Verdict)
	}
	if !reflect.DeepEqual(ca.ImprovedCases, cb.RegressedCases) || !reflect.DeepEqual(ca.RegressedCases, cb.ImprovedCases) {
		t.Error("improved and regressed lists did not swap")
	}
	near(t, "diff flips", ca.PassRate.Diff, -cb.PassRate.Diff, 1e-12)
}

func TestSummarizeSkipsAndFailures(t *testing.T) {
	rows := fortyCases()
	for i := range rows {
		switch {
		case rows[i].Arm == armNew && rows[i].CaseID == 1:
			rows[i].Phase = PhaseCancelled // skipped
		case rows[i].Arm == armNew && rows[i].CaseID == 2:
			rows[i].Grades[0].Status = GradePendingReview // not yet judged: skipped
		case rows[i].Arm == armNew && rows[i].CaseID == 3:
			rows[i].Phase, rows[i].Grades = PhaseTimedOut, nil // a failure, not a skip
		case rows[i].Arm == armBase && rows[i].CaseID == 40:
			rows[i].Phase, rows[i].Grades = PhaseSkipped, nil
		case rows[i].Arm == armNew && rows[i].CaseID == 4:
			rows[i].CostComplete = false
		}
	}
	s, err := Summarize(rows, armBase, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Comparisons[0]
	if c.Skipped != 3 || !reflect.DeepEqual(c.SkippedCases, []int64{1, 2, 40}) || c.Pairs != 37 {
		t.Errorf("skipped %d %v pairs %d", c.Skipped, c.SkippedCases, c.Pairs)
	}
	// Case 3: baseline passes, the timed-out candidate fails: a regression.
	if !slices.Contains(c.RegressedCases, 3) {
		t.Errorf("a timeout should regress case 3: %v", c.RegressedCases)
	}
	n := s.Arms[1]
	if n.Skipped != 2 || n.Cases != 38 || !n.CostIncomplete {
		t.Errorf("candidate = %+v", n)
	}
	near(t, "failure rate", n.FailureRate, 1.0/39, 1e-9)
	if s.Arms[0].Skipped != 1 {
		t.Errorf("baseline skipped = %d", s.Arms[0].Skipped)
	}
}

func TestSummarizeGradersAndRepeats(t *testing.T) {
	// Two optional graders of weight 1 and 3: the heavy one decides. A required
	// grader failing fails the case whatever else says.
	mk := func(req, a, b bool) Row {
		return Row{CaseID: 1, Arm: "x", Phase: PhaseSucceeded, CostComplete: true, Grades: []Grade{
			{GraderID: 1, Score: 1, Pass: req, Status: GradeOK, Required: true},
			{GraderID: 2, Score: 1, Pass: a, Status: GradeOK, Weight: 1},
			{GraderID: 3, Score: 0, Pass: b, Status: GradeOK, Weight: 3},
		}}
	}
	for _, tc := range []struct {
		req, a, b, want bool
	}{{true, true, true, true}, {true, false, true, true}, {true, true, false, false}, {false, true, true, false}} {
		_, pass, _, err := rowOutcome(mk(tc.req, tc.a, tc.b))
		if err != nil || pass != tc.want {
			t.Errorf("required %v a %v b %v: pass = %v, %v; want %v", tc.req, tc.a, tc.b, pass, err, tc.want)
		}
	}
	_, _, score, _ := rowOutcome(mk(true, true, true))
	near(t, "weighted score", score, (1*1+1*1+3*0)/5.0, 1e-12) // required has weight 0, counted as 1

	// Repeats: a majority decides; a flaky case is flagged.
	var rows []Row
	add := func(arm string, c int64, passes ...bool) {
		for i, p := range passes {
			r := gradeRow(arm, c, p, map[bool]float64{true: 1, false: 0}[p], 0.01, 100)
			r.Repeat = i
			rows = append(rows, r)
		}
	}
	add(armBase, 1, true, true, true)
	add(armNew, 1, true, false, true) // majority passes, flaky
	add(armBase, 2, false, false, false)
	add(armNew, 2, true, false, false) // majority fails, flaky
	add(armBase, 3, true)
	add(armNew, 3, false, false)
	s, err := Summarize(rows, armBase, Options{MinCases: 1})
	if err != nil {
		t.Fatal(err)
	}
	n := s.Arms[1]
	if n.Passed != 1 || n.FlakyCases != 2 || n.RepeatedCases != 3 {
		t.Errorf("candidate = %+v", n)
	}
	near(t, "stability", n.Stability, 1.0/3, 1e-12)
	near(t, "total cost counts every repeat", n.CostTotalUSD, 0.08, 1e-12)
	if c := s.Comparisons[0]; !reflect.DeepEqual(c.RegressedCases, []int64{3}) || len(c.ImprovedCases) != 0 {
		t.Errorf("comparison = %+v", c)
	}
}

func TestSummarizeDegenerate(t *testing.T) {
	if _, err := Summarize(nil, armBase, Options{}); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("no rows error = %v", err)
	}
	// Only the baseline: arms but no comparison.
	s, err := Summarize([]Row{gradeRow(armBase, 1, true, 1, 0, 5)}, armBase, Options{})
	if err != nil || len(s.Comparisons) != 0 || s.Arms[0].Cases != 1 {
		t.Errorf("baseline only = %+v, %v", s, err)
	}
	// Arms with no case in common: no pairs, not enough data, no NaN anywhere.
	s, err = Summarize([]Row{gradeRow(armBase, 1, true, 1, 0, 5), gradeRow(armNew, 2, true, 1, 0, 5)}, armBase, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Comparisons[0]
	if c.Pairs != 0 || c.PassRate != nil || c.Score != nil || c.Verdict.Verdict != VerdictNotEnoughData || len(c.SkippedCases) != 2 {
		t.Errorf("disjoint arms = %+v", c)
	}
	// Everything cancelled in one arm.
	r := gradeRow(armNew, 1, true, 1, 0, 5)
	r.Phase, r.Grades = PhaseCancelled, nil
	s, err = Summarize([]Row{gradeRow(armBase, 1, true, 1, 0, 5), r}, armBase, Options{})
	if err != nil || s.Arms[1].Cases != 0 || s.Arms[1].PassRateCI != (Interval{0, 1}) || s.Comparisons[0].Pairs != 0 {
		t.Errorf("all cancelled = %+v, %v", s, err)
	}
	// No latency recorded.
	r = gradeRow(armBase, 1, true, 1, 0, 0)
	r.LatencyMS = nil
	s, _ = Summarize([]Row{r}, armBase, Options{})
	if s.Arms[0].HasLatency {
		t.Error("latency reported with none recorded")
	}
}

func TestSummarizeRejectsBadRows(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	neg := -1
	for name, mod := range map[string]func(*Row){
		"NaN cost":           func(r *Row) { r.CostUSD = nan },
		"Inf cost":           func(r *Row) { r.CostUSD = inf },
		"negative cost":      func(r *Row) { r.CostUSD = -1 },
		"NaN score":          func(r *Row) { r.Grades[0].Score = nan },
		"Inf score":          func(r *Row) { r.Grades[0].Score = inf },
		"score above 1":      func(r *Row) { r.Grades[0].Score = 1.5 },
		"NaN weight":         func(r *Row) { r.Grades[0].Weight = nan },
		"negative latency":   func(r *Row) { r.LatencyMS = &neg },
		"unknown phase":      func(r *Row) { r.Phase = "Exploded" },
		"unknown grade":      func(r *Row) { r.Grades[0].Status = "maybe" },
		"empty arm":          func(r *Row) { r.Arm = "" },
		"duplicate (case,.)": func(r *Row) {},
	} {
		rows := []Row{gradeRow(armBase, 1, true, 1, 0.1, 5), gradeRow(armBase, 2, true, 1, 0.1, 5)}
		if name == "duplicate (case,.)" {
			rows[1].CaseID = 1
		} else {
			mod(&rows[1])
		}
		if s, err := Summarize(rows, armBase, Options{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v (%+v), want ErrInvalid", name, err, s.Arms)
		}
	}
	if _, err := Summarize([]Row{gradeRow(armBase, 1, true, 1, 0, 5)}, armBase, Options{Confidence: nan}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad options error = %v", err)
	}
}

func FuzzSummarizeNoNaN(f *testing.F) {
	f.Add(0.5, 0.01, 100, 3, uint64(1))
	f.Add(math.NaN(), math.Inf(1), -5, 0, uint64(2))
	f.Fuzz(func(t *testing.T, score, cost float64, lat, n int, seed uint64) {
		n = max(n%50+1, 1)
		var rows []Row
		for c := 0; c < n; c++ {
			rows = append(rows, gradeRow("a", int64(c), c%2 == 0, score, cost, lat), gradeRow("b", int64(c), c%3 == 0, score, cost, lat))
		}
		s, err := Summarize(rows, "a", Options{Seed: seed, Resamples: 20, MinCases: 1})
		if err != nil {
			return
		}
		check := func(vs ...float64) {
			for _, v := range vs {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("no error but a non-finite number in %+v", s)
				}
			}
		}
		for _, a := range s.Arms {
			check(a.PassRate, a.PassRateCI.Lo, a.PassRateCI.Hi, a.MeanScore, a.CostTotalUSD, a.CostMedianUSD, a.LatencyP50MS, a.LatencyP95MS, a.Stability, a.FailureRate)
		}
		for _, c := range s.Comparisons {
			if c.PassRate != nil {
				check(c.PassRate.Diff, c.PassRate.CI.Lo, c.PassRate.CI.Hi, c.PassRate.McNemar.P)
			}
			for _, m := range []*MeanDiff{c.Score, c.Cost, c.Latency} {
				if m != nil {
					check(m.Mean, m.CI.Lo, m.CI.Hi)
				}
			}
		}
	})
}
