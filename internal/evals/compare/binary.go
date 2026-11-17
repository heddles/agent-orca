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
	"math"
)

// Errors returned for input the statistics cannot work with. Callers can test
// for them with errors.Is.
var (
	// ErrNoPairs means there were no paired cases to compare.
	ErrNoPairs = errors.New("compare: no paired cases")
	// ErrInvalid means an input was NaN, infinite, negative where it cannot be,
	// out of range or inconsistent.
	ErrInvalid = errors.New("compare: invalid input")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// maxExactDiscordant is the largest number of discordant pairs for which
// McNemar's p-value is the exact binomial one. The exact test is conservative
// and cheap at this size; above it the chi-square approximation is accurate.
const maxExactDiscordant = 100

// WilsonCI is Wilson with its input checked: k must lie in 0..n, n must not be
// negative, and confidence must be strictly between 0 and 1. With n = 0 it
// returns the whole range [0, 1], which says nothing.
func WilsonCI(k, n int, confidence float64) (Interval, error) {
	if err := checkConfidence(confidence); err != nil {
		return Interval{}, err
	}
	if n < 0 || k < 0 || k > n {
		return Interval{}, invalidf("%d successes of %d trials", k, n)
	}
	return Wilson(k, n, confidence), nil
}

func checkConfidence(c float64) error {
	if math.IsNaN(c) || c <= 0 || c >= 1 {
		return invalidf("confidence %v is not between 0 and 1", c)
	}
	return nil
}

// PairedCounts is the two by two table of the same cases passed or failed by a
// baseline and a candidate.
type PairedCounts struct {
	BothPass  int `json:"bothPass"`
	Improved  int `json:"improved"`  // baseline fails, candidate passes
	Regressed int `json:"regressed"` // baseline passes, candidate fails
	BothFail  int `json:"bothFail"`
}

// N is the number of paired cases.
func (c PairedCounts) N() int { return c.BothPass + c.Improved + c.Regressed + c.BothFail }

// Tied is the cases both arms treated alike.
func (c PairedCounts) Tied() int { return c.BothPass + c.BothFail }

// Swap returns the counts with the arms exchanged.
func (c PairedCounts) Swap() PairedCounts {
	return PairedCounts{BothPass: c.BothPass, Improved: c.Regressed, Regressed: c.Improved, BothFail: c.BothFail}
}

func (c PairedCounts) validate() error {
	if c.BothPass < 0 || c.Improved < 0 || c.Regressed < 0 || c.BothFail < 0 {
		return invalidf("negative count in %+v", c)
	}
	return nil
}

// CountPairs tallies baseline[i] against candidate[i], which are the outcomes
// of the same case i.
func CountPairs(baseline, candidate []bool) (PairedCounts, error) {
	if len(baseline) != len(candidate) {
		return PairedCounts{}, invalidf("%d baseline outcomes against %d candidate outcomes", len(baseline), len(candidate))
	}
	var c PairedCounts
	for i := range baseline {
		switch {
		case baseline[i] && candidate[i]:
			c.BothPass++
		case !baseline[i] && candidate[i]:
			c.Improved++
		case baseline[i] && !candidate[i]:
			c.Regressed++
		default:
			c.BothFail++
		}
	}
	return c, nil
}

// McNemarResult is McNemar's test on the discordant pairs.
type McNemarResult struct {
	Improved  int `json:"improved"`
	Regressed int `json:"regressed"`
	// Exact is true when P is the exact binomial p-value, false when it comes
	// from the continuity-corrected chi-square statistic.
	Exact bool `json:"exact"`
	// Statistic is the corrected chi-square value; zero for the exact test.
	Statistic float64 `json:"statistic"`
	// P is the two-sided p-value: how often a split at least this uneven would
	// arise if the two arms were equally good. 1 when no pair differs.
	P float64 `json:"p"`
}

// McNemar tests whether two arms that passed or failed the same cases differ,
// from the number of cases the candidate alone passed (improved) and the
// baseline alone passed (regressed). Only those discordant cases carry
// information. With at most 100 of them the p-value is the exact two-sided
// binomial one (twice the lower tail of Binomial(n, 1/2), capped at 1); with
// more it is the chi-square test with continuity correction, (|b-c|-1)^2/(b+c)
// on one degree of freedom.
func McNemar(improved, regressed int) (McNemarResult, error) {
	if improved < 0 || regressed < 0 {
		return McNemarResult{}, invalidf("discordant counts %d and %d", improved, regressed)
	}
	r := McNemarResult{Improved: improved, Regressed: regressed, P: 1}
	n := improved + regressed
	if n == 0 {
		r.Exact = true
		return r, nil
	}
	if n <= maxExactDiscordant {
		r.Exact = true
		r.P = exactBinomialTwoSided(min(improved, regressed), n)
		return r, nil
	}
	d := math.Abs(float64(improved-regressed)) - 1
	if d < 0 {
		d = 0
	}
	r.Statistic = d * d / float64(n)
	r.P = math.Min(1, math.Erfc(math.Sqrt(r.Statistic/2)))
	return r, nil
}

// exactBinomialTwoSided returns 2 * P(X <= k) for X ~ Binomial(n, 1/2), capped
// at 1.
func exactBinomialTwoSided(k, n int) float64 {
	lgn, _ := math.Lgamma(float64(n) + 1)
	sum := 0.0
	for i := 0; i <= k; i++ {
		lgi, _ := math.Lgamma(float64(i) + 1)
		lgni, _ := math.Lgamma(float64(n-i) + 1)
		sum += math.Exp(lgn - lgi - lgni - float64(n)*math.Ln2)
	}
	return math.Min(1, 2*sum)
}

// PassRateDiff is the paired comparison of two arms' pass rates.
type PassRateDiff struct {
	Counts        PairedCounts `json:"counts"`
	N             int          `json:"n"`
	BaselineRate  float64      `json:"baselineRate"`
	CandidateRate float64      `json:"candidateRate"`
	// Diff is the candidate's rate minus the baseline's.
	Diff float64 `json:"diff"`
	// CI is the interval for Diff at the confidence asked for.
	CI      Interval      `json:"ci"`
	McNemar McNemarResult `json:"mcnemar"`
}

// ComparePassRates computes the paired difference in pass rate (candidate minus
// baseline), its confidence interval and McNemar's test. The interval is
// Newcombe's method 10 for paired proportions (Newcombe 1998, Statistics in
// Medicine 17:2635), built from the Wilson interval of each arm and corrected
// for how strongly the arms' outcomes agree. It is chosen over the simple
// normal interval because that one collapses to zero width when the arms never
// disagree and is too narrow on small samples.
func ComparePassRates(c PairedCounts, confidence float64) (PassRateDiff, error) {
	if err := checkConfidence(confidence); err != nil {
		return PassRateDiff{}, err
	}
	if err := c.validate(); err != nil {
		return PassRateDiff{}, err
	}
	n := c.N()
	if n == 0 {
		return PassRateDiff{}, ErrNoPairs
	}
	mc, err := McNemar(c.Improved, c.Regressed)
	if err != nil {
		return PassRateDiff{}, err
	}
	nf := float64(n)
	candPass, basePass := c.BothPass+c.Improved, c.BothPass+c.Regressed
	p1, p2 := float64(candPass)/nf, float64(basePass)/nf
	w1, w2 := Wilson(candPass, n, confidence), Wilson(basePass, n, confidence)

	a, b, cc, d := float64(c.BothPass), float64(c.Improved), float64(c.Regressed), float64(c.BothFail)
	phi := 0.0
	if prod := (a + b) * (cc + d) * (a + cc) * (b + d); prod > 0 {
		corr := a*d - b*cc
		switch {
		case corr > nf/2:
			corr -= nf / 2
		case corr >= 0:
			corr = 0
		}
		phi = corr / math.Sqrt(prod)
	}
	diff := p1 - p2
	lo := diff - math.Sqrt(math.Max(0, (p1-w1.Lo)*(p1-w1.Lo)+(w2.Hi-p2)*(w2.Hi-p2)-2*phi*(p1-w1.Lo)*(w2.Hi-p2)))
	hi := diff + math.Sqrt(math.Max(0, (w1.Hi-p1)*(w1.Hi-p1)+(p2-w2.Lo)*(p2-w2.Lo)-2*phi*(w1.Hi-p1)*(p2-w2.Lo)))
	return PassRateDiff{
		Counts: c, N: n, BaselineRate: p2, CandidateRate: p1, Diff: diff,
		CI:      Interval{math.Max(-1, lo), math.Min(1, hi)},
		McNemar: mc,
	}, nil
}
