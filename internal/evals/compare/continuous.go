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
	"math"
	"sort"
)

// Defaults for Options.
const (
	DefaultResamples = 2000
	DefaultMinCases  = 30
	DefaultMargin    = 0.05
	// MinBootstrapPairs is the fewest pairs for which the bootstrap interval is
	// trusted. Below it a resample can take only a handful of distinct values
	// and the percentile interval is far too narrow.
	MinBootstrapPairs = 8
)

// Options set the confidence, the randomness and the evidence bar. The zero
// value means the defaults.
type Options struct {
	// Confidence of every interval. Default 0.95.
	Confidence float64 `json:"confidence,omitempty"`
	// Resamples is the bootstrap size. Default 2000.
	Resamples int `json:"resamples,omitempty"`
	// Seed makes the bootstrap reproducible; the runner passes the experiment ID.
	// The same seed and input always give the same interval.
	Seed uint64 `json:"seed,omitempty"`
	// MinCases is the fewest paired cases a verdict needs. Default 30.
	MinCases int `json:"minCases,omitempty"`
	// Margin is the non-inferiority margin, as a pass-rate difference. Default 0.05.
	Margin float64 `json:"margin,omitempty"`
}

// WithDefaults fills the zero fields and rejects values that are not usable.
func (o Options) WithDefaults() (Options, error) {
	if o.Confidence == 0 {
		o.Confidence = DefaultConfidence
	}
	if err := checkConfidence(o.Confidence); err != nil {
		return o, err
	}
	if o.Resamples < 0 {
		return o, invalidf("resamples %d", o.Resamples)
	}
	if o.Resamples == 0 {
		o.Resamples = DefaultResamples
	}
	if o.MinCases < 0 {
		return o, invalidf("minimum cases %d", o.MinCases)
	}
	if o.MinCases == 0 {
		o.MinCases = DefaultMinCases
	}
	if math.IsNaN(o.Margin) || math.IsInf(o.Margin, 0) || o.Margin < 0 || o.Margin >= 1 {
		return o, invalidf("margin %v is not in [0, 1)", o.Margin)
	}
	if o.Margin == 0 {
		o.Margin = DefaultMargin
	}
	return o, nil
}

// MeanDiff is the mean of candidate minus baseline over the paired cases, with
// a bootstrap interval.
type MeanDiff struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	// HasCI is false when there is a single pair and no interval can be given.
	HasCI bool     `json:"hasCi"`
	CI    Interval `json:"ci"`
	// Approximate is true when there were fewer than MinBootstrapPairs pairs: CI
	// is then the range of the observed differences, which always contains the
	// mean's true interval and is deliberately wide.
	Approximate bool `json:"approximate,omitempty"`
}

// PairedMeanDiff returns the mean paired difference (candidate minus baseline)
// and its percentile bootstrap interval. baseline[i] and candidate[i] are the
// same case. With fewer than MinBootstrapPairs pairs the interval is the range
// of the differences, flagged Approximate; with one pair there is none.
func PairedMeanDiff(baseline, candidate []float64, opts Options) (MeanDiff, error) {
	o, err := opts.WithDefaults()
	if err != nil {
		return MeanDiff{}, err
	}
	d, err := pairedDiffs(baseline, candidate)
	if err != nil {
		return MeanDiff{}, err
	}
	n := len(d)
	if n == 0 {
		return MeanDiff{}, ErrNoPairs
	}
	m := MeanDiff{N: n, Mean: mean(d)}
	switch {
	case n == 1:
		return m, nil
	case n < MinBootstrapPairs:
		sorted := append([]float64(nil), d...)
		sort.Float64s(sorted)
		m.HasCI, m.Approximate = true, true
		m.CI = Interval{sorted[0], sorted[n-1]}
		return m, nil
	}
	m.HasCI = true
	m.CI = Bootstrap(n, o.Resamples, o.Confidence, o.Seed, func(idx []int) (float64, bool) {
		s := 0.0
		for _, i := range idx {
			s += d[i]
		}
		return s / float64(len(idx)), true
	})
	return m, nil
}

// MedianDiff is the median of candidate minus baseline over the paired cases,
// for quantities such as cost and latency whose mean a few outliers dominate.
type MedianDiff struct {
	N              int     `json:"n"`
	Median         float64 `json:"median"`
	BaselineMedian float64 `json:"baselineMedian"`
	// CandidateMedian is the median of the candidate's own values.
	CandidateMedian float64 `json:"candidateMedian"`
	// Rel is Median relative to BaselineMedian (0.1 is ten percent more); only
	// meaningful when HasRel, which is false when the baseline median is zero.
	Rel    float64 `json:"rel"`
	HasRel bool    `json:"hasRel"`
}

// PairedMedianDiff returns the median paired difference (candidate minus
// baseline) and the change relative to the baseline's median.
func PairedMedianDiff(baseline, candidate []float64) (MedianDiff, error) {
	d, err := pairedDiffs(baseline, candidate)
	if err != nil {
		return MedianDiff{}, err
	}
	if len(d) == 0 {
		return MedianDiff{}, ErrNoPairs
	}
	m := MedianDiff{N: len(d), Median: median(d), BaselineMedian: median(baseline), CandidateMedian: median(candidate)}
	if m.BaselineMedian != 0 {
		m.Rel, m.HasRel = m.Median/m.BaselineMedian, true
	}
	return m, nil
}

// pairedDiffs checks both slices and returns candidate[i] - baseline[i].
func pairedDiffs(baseline, candidate []float64) ([]float64, error) {
	if len(baseline) != len(candidate) {
		return nil, invalidf("%d baseline values against %d candidate values", len(baseline), len(candidate))
	}
	d := make([]float64, len(baseline))
	for i := range baseline {
		if !finite(baseline[i]) || !finite(candidate[i]) {
			return nil, invalidf("pair %d is not a finite number (%v, %v)", i, baseline[i], candidate[i])
		}
		d[i] = candidate[i] - baseline[i]
		if !finite(d[i]) {
			return nil, invalidf("pair %d overflows (%v, %v)", i, baseline[i], candidate[i])
		}
	}
	return d, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// median of a non-empty slice, which it does not modify.
func median(v []float64) float64 { return quantile(v, 0.5) }

func quantile(v []float64, q float64) float64 {
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	return Percentile(sorted, q)
}
