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
	"math/rand/v2"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6g, want %.6g (±%.3g)", name, got, want, tol)
	}
}

func TestMcNemarKnownValues(t *testing.T) {
	for _, tc := range []struct {
		name         string
		imp, reg     int
		wantP        float64
		tol          float64
		exact        bool
		wantStatisic float64
	}{
		// Exact binomial arithmetic: 2 * (C(10,0)+C(10,1)) / 2^10 = 22/1024.
		// Matches R: binom.test(1, 10)$p.value = 0.02148.
		{"1 of 10", 1, 9, 22.0 / 1024, 1e-12, true, 0},
		// 2 * 1/32; R: binom.test(0, 5)$p.value = 0.0625.
		{"0 of 5", 0, 5, 0.0625, 1e-12, true, 0},
		// A perfect split cannot be unlikely; the doubled tail is capped at 1.
		{"3 and 3", 3, 3, 1, 1e-12, true, 0},
		{"no discordant pairs", 0, 0, 1, 0, true, 0},
		// Wikipedia, "McNemar's test", the worked example with discordant counts
		// 121 and 59: corrected statistic (|121-59|-1)^2/180 = 20.672, and the
		// chi-square(1) tail beyond it is 5.45e-6 (math.Erfc(sqrt(x/2))).
		{"121 and 59", 121, 59, 5.450094825e-6, 1e-9, false, 20.6722222},
	} {
		got, err := McNemar(tc.imp, tc.reg)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		near(t, tc.name+" p", got.P, tc.wantP, tc.tol)
		if got.Exact != tc.exact {
			t.Errorf("%s: Exact = %v, want %v", tc.name, got.Exact, tc.exact)
		}
		near(t, tc.name+" statistic", got.Statistic, tc.wantStatisic, 1e-6)
	}
}

func TestMcNemarSymmetricAndMonotone(t *testing.T) {
	for reg := 5; reg <= 40; reg += 5 { // an ever more lopsided split of 40 pairs
		a, _ := McNemar(40-reg, reg)
		b, _ := McNemar(reg, 40-reg)
		if a.P != b.P {
			t.Errorf("split %d/%d: swapping the arms changed p from %v to %v", 40-reg, reg, a.P, b.P)
		}
	}
	last := 1.0
	for imp := 20; imp <= 40; imp++ {
		r, _ := McNemar(imp, 40-imp)
		if r.P > last+1e-15 {
			t.Errorf("p rose from %v to %v as the split got more lopsided (%d of 40)", last, r.P, imp)
		}
		last = r.P
	}
	// The exact and chi-square tests agree closely where they meet.
	exact, _ := McNemar(40, 60)
	chi, _ := McNemar(41, 61)
	if !exact.Exact || chi.Exact {
		t.Fatalf("expected exact then chi-square, got %v and %v", exact.Exact, chi.Exact)
	}
	near(t, "exact against chi-square near the switch", exact.P, chi.P, 0.03)
}

func TestWilsonCI(t *testing.T) {
	// Newcombe 1998, Statistics in Medicine 17:857, the first of his worked
	// proportions: 81 of 263 gives 0.2553 to 0.3662; and the textbook values for
	// 0 of 10 (0 to 0.2775) and 5 of 10 (0.2366 to 0.7634).
	for _, tc := range []struct {
		k, n   int
		lo, hi float64
	}{{81, 263, 0.2553, 0.3662}, {0, 10, 0, 0.2775}, {5, 10, 0.2366, 0.7634}, {10, 10, 0.7225, 1}} {
		got, err := WilsonCI(tc.k, tc.n, 0.95)
		if err != nil {
			t.Fatal(err)
		}
		near(t, "lo", got.Lo, tc.lo, 5e-4)
		near(t, "hi", got.Hi, tc.hi, 5e-4)
	}
	if got, err := WilsonCI(0, 0, 0.95); err != nil || got.Lo != 0 || got.Hi != 1 {
		t.Errorf("no trials = %+v, %v; want the whole range", got, err)
	}
	for _, bad := range []struct {
		k, n int
		c    float64
	}{{-1, 5, 0.95}, {6, 5, 0.95}, {1, -1, 0.95}, {1, 5, 0}, {1, 5, 1}, {1, 5, math.NaN()}, {1, 5, math.Inf(1)}} {
		if _, err := WilsonCI(bad.k, bad.n, bad.c); !errors.Is(err, ErrInvalid) {
			t.Errorf("WilsonCI(%d, %d, %v) error = %v, want ErrInvalid", bad.k, bad.n, bad.c, err)
		}
	}
}

func TestWilsonNarrowsWithN(t *testing.T) {
	prev := math.Inf(1)
	for _, n := range []int{4, 8, 16, 32, 64, 128, 256} {
		ci, _ := WilsonCI(n/2, n, 0.95)
		if ci.Width() >= prev {
			t.Errorf("n=%d: width %v did not shrink from %v", n, ci.Width(), prev)
		}
		if !ci.Contains(0.5) || ci.Lo < 0 || ci.Hi > 1 {
			t.Errorf("n=%d: interval %+v should contain 0.5 and stay in [0,1]", n, ci)
		}
		prev = ci.Width()
	}
	// More confidence, wider interval.
	lo, _ := WilsonCI(30, 100, 0.8)
	hi, _ := WilsonCI(30, 100, 0.99)
	if hi.Width() <= lo.Width() {
		t.Errorf("99%% interval %+v is not wider than 80%% %+v", hi, lo)
	}
}

func TestComparePassRatesDegenerate(t *testing.T) {
	if _, err := ComparePassRates(PairedCounts{}, 0.95); !errors.Is(err, ErrNoPairs) {
		t.Errorf("n=0 error = %v, want ErrNoPairs", err)
	}
	if _, err := ComparePassRates(PairedCounts{Improved: -1}, 0.95); !errors.Is(err, ErrInvalid) {
		t.Errorf("negative count error = %v, want ErrInvalid", err)
	}
	for name, c := range map[string]PairedCounts{
		"one case that improved": {Improved: 1},
		"one tie":                {BothPass: 1},
		"all pass":               {BothPass: 50},
		"all fail":               {BothFail: 50},
		"all improved":           {Improved: 50},
		"all regressed":          {Regressed: 50},
		"all ties, mixed":        {BothPass: 25, BothFail: 25},
	} {
		d, err := ComparePassRates(c, 0.95)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, v := range []float64{d.Diff, d.CI.Lo, d.CI.Hi, d.McNemar.P, d.BaselineRate, d.CandidateRate} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Errorf("%s: %+v has a NaN or Inf", name, d)
			}
		}
		if d.CI.Lo > d.Diff || d.CI.Hi < d.Diff || d.CI.Lo < -1 || d.CI.Hi > 1 {
			t.Errorf("%s: interval %+v does not hold the difference %v inside [-1, 1]", name, d.CI, d.Diff)
		}
	}
	// One case can never be conclusive: the interval must be wide.
	d, _ := ComparePassRates(PairedCounts{Improved: 1}, 0.95)
	if d.CI.Lo > 0 {
		t.Errorf("one improved case gave a confident interval %+v", d.CI)
	}
	// Ties tell nothing apart: zero difference, p = 1, yet a nonzero interval.
	d, _ = ComparePassRates(PairedCounts{BothPass: 40}, 0.95)
	if d.Diff != 0 || d.McNemar.P != 1 || d.CI.Width() == 0 {
		t.Errorf("all ties = %+v", d)
	}
	// Everything one way: clearly positive.
	d, _ = ComparePassRates(PairedCounts{Improved: 40}, 0.95)
	if d.Diff != 1 || d.CI.Lo <= 0 {
		t.Errorf("all improved = %+v", d)
	}
}

// With uncorrelated outcomes the paired interval is Newcombe's independent one
// (method 10 sets the correlation term to zero when the correction removes it).
func TestPairedIntervalMatchesIndependentWhenUncorrelated(t *testing.T) {
	c := PairedCounts{BothPass: 10, Improved: 15, Regressed: 15, BothFail: 23} // ad-bc = 5, inside the zeroed band 0 to n/2
	d, err := ComparePassRates(c, 0.95)
	if err != nil {
		t.Fatal(err)
	}
	_, want := NewcombeDiff(25, 63, 25, 63, 0.95)
	near(t, "lo", d.CI.Lo, want.Lo, 1e-12)
	near(t, "hi", d.CI.Hi, want.Hi, 1e-12)
	// Agreement between the arms shrinks the interval below the independent one.
	agree := PairedCounts{BothPass: 22, Improved: 4, Regressed: 1, BothFail: 23}
	pd, _ := ComparePassRates(agree, 0.95)
	_, ind := NewcombeDiff(26, 50, 23, 50, 0.95)
	if pd.CI.Width() >= ind.Width() {
		t.Errorf("paired width %v should be below the independent width %v when the arms agree", pd.CI.Width(), ind.Width())
	}
}

func TestPassRateSymmetryProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	for range 500 {
		c := PairedCounts{BothPass: rng.IntN(60), Improved: rng.IntN(40), Regressed: rng.IntN(40), BothFail: rng.IntN(60)}
		if c.N() == 0 {
			continue
		}
		a, err1 := ComparePassRates(c, 0.95)
		b, err2 := ComparePassRates(c.Swap(), 0.95)
		if err1 != nil || err2 != nil {
			t.Fatalf("%+v: %v %v", c, err1, err2)
		}
		near(t, "diff flips", a.Diff, -b.Diff, 1e-12)
		near(t, "lo flips", a.CI.Lo, -b.CI.Hi, 1e-12)
		near(t, "hi flips", a.CI.Hi, -b.CI.Lo, 1e-12)
		if a.McNemar.P != b.McNemar.P {
			t.Errorf("%+v: p changed on swapping the arms", c)
		}
		if a.N != c.N() || a.Counts.Tied() != c.Tied() {
			t.Errorf("%+v: counts not preserved", c)
		}
		// Counts add up and the rates agree with the table.
		near(t, "diff", a.Diff, float64(c.Improved-c.Regressed)/float64(c.N()), 1e-12)
	}
}

func TestPassRateIntervalNarrowsWithN(t *testing.T) {
	prev := math.Inf(1)
	for _, scale := range []int{1, 2, 4, 8, 16, 32} {
		c := PairedCounts{BothPass: 6 * scale, Improved: 3 * scale, Regressed: 1 * scale, BothFail: 4 * scale}
		d, _ := ComparePassRates(c, 0.95)
		if d.CI.Width() >= prev {
			t.Errorf("scale %d: width %v did not shrink from %v", scale, d.CI.Width(), prev)
		}
		prev = d.CI.Width()
	}
}

// With the seed fixed the true difference is known; the interval should cover
// it about as often as it claims. Newcombe's paired interval is conservative,
// so the lower bound is the one that matters.
func TestPassRateCoverage(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	const trials, n = 2000, 60
	covered := 0
	// Joint probabilities: both pass .4, improved .2, regressed .1, both fail .3.
	for range trials {
		var c PairedCounts
		for range n {
			switch u := rng.Float64(); {
			case u < 0.4:
				c.BothPass++
			case u < 0.6:
				c.Improved++
			case u < 0.7:
				c.Regressed++
			default:
				c.BothFail++
			}
		}
		d, _ := ComparePassRates(c, 0.95)
		if d.CI.Contains(0.1) {
			covered++
		}
	}
	if rate := float64(covered) / trials; rate < 0.93 {
		t.Errorf("coverage = %.3f, want at least about 0.95", rate)
	}
}

func TestCountPairs(t *testing.T) {
	c, err := CountPairs([]bool{true, true, false, false, true}, []bool{true, false, true, false, true})
	if err != nil {
		t.Fatal(err)
	}
	if want := (PairedCounts{BothPass: 2, Improved: 1, Regressed: 1, BothFail: 1}); c != want {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
	if _, err := CountPairs([]bool{true}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("length mismatch error = %v", err)
	}
}
