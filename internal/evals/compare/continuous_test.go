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

// series returns n baseline values and candidate values that are on average
// shift higher, from a seeded source.
func series(n int, shift float64, seed uint64) (base, cand []float64) {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	for range n {
		b := rng.Float64()
		base = append(base, b)
		cand = append(cand, b+shift+0.1*(rng.Float64()-0.5))
	}
	return base, cand
}

func TestPairedMeanDiffKnownValue(t *testing.T) {
	// Hand arithmetic: differences 1,2,3,4,5,6,7,8,9,10 have mean 5.5.
	var base, cand []float64
	for i := 1; i <= 10; i++ {
		base = append(base, 100)
		cand = append(cand, 100+float64(i))
	}
	m, err := PairedMeanDiff(base, cand, Options{})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "mean", m.Mean, 5.5, 1e-12)
	if !m.HasCI || m.Approximate || !m.CI.Contains(5.5) || m.CI.Lo < 1 || m.CI.Hi > 10 {
		t.Errorf("interval %+v should surround 5.5 and stay within the data", m)
	}
	// Bootstrap of a tight sample: its 95% interval is narrower than the data.
	if m.CI.Width() > 5 {
		t.Errorf("interval %+v is wider than half the data range", m.CI)
	}
}

func TestPairedMeanDiffIsReproducible(t *testing.T) {
	base, cand := series(40, 0.05, 1)
	a, _ := PairedMeanDiff(base, cand, Options{Seed: 42})
	b, _ := PairedMeanDiff(base, cand, Options{Seed: 42})
	c, _ := PairedMeanDiff(base, cand, Options{Seed: 43})
	if a != b {
		t.Errorf("the same seed gave %+v and %+v", a, b)
	}
	if a.CI == c.CI {
		t.Error("a different seed gave the identical interval")
	}
	d, _ := PairedMeanDiff(base, cand, Options{Seed: 42, Resamples: DefaultResamples})
	if a != d {
		t.Errorf("the default resamples is not %d", DefaultResamples)
	}
}

func TestPairedMeanDiffSymmetry(t *testing.T) {
	base, cand := series(30, 0.2, 3)
	ab, _ := PairedMeanDiff(base, cand, Options{Seed: 9})
	ba, _ := PairedMeanDiff(cand, base, Options{Seed: 9})
	near(t, "mean flips", ab.Mean, -ba.Mean, 1e-12)
	// The same resamples are drawn, so the interval is the mirror image.
	near(t, "lo flips", ab.CI.Lo, -ba.CI.Hi, 1e-9)
	near(t, "hi flips", ab.CI.Hi, -ba.CI.Lo, 1e-9)
}

func TestPairedMeanDiffNarrowsWithN(t *testing.T) {
	prev := math.Inf(1)
	for _, n := range []int{10, 40, 160, 640} {
		base, cand := series(n, 0.1, 5)
		m, _ := PairedMeanDiff(base, cand, Options{Seed: 1})
		if m.CI.Width() >= prev {
			t.Errorf("n=%d: width %v did not shrink from %v", n, m.CI.Width(), prev)
		}
		prev = m.CI.Width()
	}
}

func TestPairedMeanDiffTinySamples(t *testing.T) {
	if _, err := PairedMeanDiff(nil, nil, Options{}); !errors.Is(err, ErrNoPairs) {
		t.Errorf("n=0 error = %v, want ErrNoPairs", err)
	}
	one, err := PairedMeanDiff([]float64{1}, []float64{3}, Options{})
	if err != nil || one.HasCI || one.Mean != 2 {
		t.Errorf("n=1 = %+v, %v; want a mean and no interval", one, err)
	}
	// Two to seven pairs get the observed range, never a bootstrap.
	for n := 2; n < MinBootstrapPairs; n++ {
		base, cand := series(n, 0.3, 7)
		m, err := PairedMeanDiff(base, cand, Options{})
		if err != nil || !m.Approximate || !m.HasCI || !m.CI.Contains(m.Mean) {
			t.Errorf("n=%d = %+v, %v; want an approximate interval holding the mean", n, m, err)
		}
	}
	base, cand := series(MinBootstrapPairs, 0.3, 7)
	if m, err := PairedMeanDiff(base, cand, Options{}); err != nil || m.Approximate {
		t.Errorf("%d pairs = %+v, %v; want a bootstrap interval", MinBootstrapPairs, m, err)
	}
}

func TestPairedMeanDiffAllTies(t *testing.T) {
	base := []float64{0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5}
	m, err := PairedMeanDiff(base, base, Options{})
	if err != nil || m.Mean != 0 || m.CI.Lo != 0 || m.CI.Hi != 0 {
		t.Errorf("identical series = %+v, %v; want a zero difference", m, err)
	}
}

func TestPairedMedianDiff(t *testing.T) {
	// Latency in ms. A single huge outlier moves the mean but not the median.
	base := []float64{100, 110, 120, 130, 140}
	cand := []float64{90, 100, 110, 120, 5000}
	m, err := PairedMedianDiff(base, cand)
	if err != nil {
		t.Fatal(err)
	}
	// Differences: -10, -10, -10, -10, 4860; median -10. Baseline median 120.
	near(t, "median diff", m.Median, -10, 1e-12)
	near(t, "baseline median", m.BaselineMedian, 120, 1e-12)
	near(t, "relative", m.Rel, -10.0/120, 1e-12)
	if !m.HasRel {
		t.Error("a nonzero baseline median should give a relative change")
	}
	zero, _ := PairedMedianDiff([]float64{0, 0, 0}, []float64{1, 1, 1})
	if zero.HasRel || zero.Median != 1 {
		t.Errorf("zero baseline = %+v, want no relative change", zero)
	}
	if _, err := PairedMedianDiff(nil, nil); !errors.Is(err, ErrNoPairs) {
		t.Errorf("n=0 error = %v", err)
	}
	flip, _ := PairedMedianDiff(cand, base)
	near(t, "median flips", flip.Median, 10, 1e-12)
}

func TestNonFiniteAndMismatchedInputsAreErrors(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for name, tc := range map[string]struct{ b, c []float64 }{
		"NaN baseline":     {[]float64{1, nan}, []float64{1, 2}},
		"NaN candidate":    {[]float64{1, 2}, []float64{nan, 2}},
		"Inf baseline":     {[]float64{inf, 2}, []float64{1, 2}},
		"-Inf candidate":   {[]float64{1, 2}, []float64{1, -inf}},
		"overflowing diff": {[]float64{-math.MaxFloat64}, []float64{math.MaxFloat64}},
		"length mismatch":  {[]float64{1, 2}, []float64{1}},
	} {
		if m, err := PairedMeanDiff(tc.b, tc.c, Options{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: PairedMeanDiff = %+v, %v; want ErrInvalid", name, m, err)
		}
		if m, err := PairedMedianDiff(tc.b, tc.c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: PairedMedianDiff = %+v, %v; want ErrInvalid", name, m, err)
		}
	}
	for name, o := range map[string]Options{
		"NaN confidence":      {Confidence: nan},
		"confidence 1":        {Confidence: 1},
		"negative confidence": {Confidence: -0.5},
		"Inf confidence":      {Confidence: inf},
		"NaN margin":          {Margin: nan},
		"Inf margin":          {Margin: inf},
		"negative margin":     {Margin: -0.1},
		"margin 1":            {Margin: 1},
		"negative resamples":  {Resamples: -1},
		"negative min cases":  {MinCases: -1},
	} {
		if _, err := o.WithDefaults(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: WithDefaults error = %v, want ErrInvalid", name, err)
		}
		if _, err := PairedMeanDiff([]float64{1}, []float64{2}, o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: PairedMeanDiff error = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := ComparePassRates(PairedCounts{BothPass: 1}, nan); !errors.Is(err, ErrInvalid) {
		t.Errorf("NaN confidence for pass rates: %v", err)
	}
}

func TestOptionsDefaults(t *testing.T) {
	o, err := Options{}.WithDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if o.Confidence != 0.95 || o.Resamples != 2000 || o.MinCases != 30 || o.Margin != 0.05 {
		t.Errorf("defaults = %+v", o)
	}
}

func FuzzPairedMeanDiff(f *testing.F) {
	f.Add(1.0, 2.0, 3.0, 4.0, uint64(1), 0.95)
	f.Add(math.NaN(), 0.0, 0.0, 0.0, uint64(0), 0.95)
	f.Add(math.Inf(1), 0.0, 0.0, math.Inf(-1), uint64(2), 0.5)
	f.Add(1e308, -1e308, 1.0, 1.0, uint64(3), math.NaN())
	f.Fuzz(func(t *testing.T, b1, b2, c1, c2 float64, seed uint64, conf float64) {
		o := Options{Seed: seed, Confidence: conf, Resamples: 50}
		base, cand := []float64{b1, b2, 0, 1, 2, 3, 4, 5, 6}, []float64{c1, c2, 1, 2, 3, 4, 5, 6, 7}
		m, err := PairedMeanDiff(base, cand, o)
		if err != nil {
			return
		}
		for _, v := range []float64{m.Mean, m.CI.Lo, m.CI.Hi} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("no error but %+v for %v %v", m, base, cand)
			}
		}
		md, err := PairedMedianDiff(base, cand)
		if err == nil && (math.IsNaN(md.Median) || math.IsInf(md.Median, 0)) {
			t.Fatalf("no error but %+v", md)
		}
	})
}

func FuzzComparePassRates(f *testing.F) {
	f.Add(10, 3, 1, 5, 0.95)
	f.Add(0, 0, 0, 0, 0.95)
	f.Add(-1, 2, 3, 4, 0.95)
	f.Add(1000000, 1000000, 1000000, 1000000, 0.999999)
	f.Fuzz(func(t *testing.T, a, b, c, d int, conf float64) {
		// Keep the counts where float conversion is exact; larger ones are a
		// different kind of input and the database cannot hold them.
		for _, v := range []int{a, b, c, d} {
			if v > 1<<24 {
				return
			}
		}
		r, err := ComparePassRates(PairedCounts{BothPass: a, Improved: b, Regressed: c, BothFail: d}, conf)
		if err != nil {
			return
		}
		for _, v := range []float64{r.Diff, r.CI.Lo, r.CI.Hi, r.McNemar.P, r.BaselineRate, r.CandidateRate} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("no error but %+v", r)
			}
		}
		if r.CI.Lo > r.CI.Hi || r.McNemar.P < 0 || r.McNemar.P > 1 {
			t.Fatalf("invalid result %+v", r)
		}
	})
}
