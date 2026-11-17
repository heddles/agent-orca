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

package routingbench

import "github.com/heddles/agent-orca/internal/evals/compare"

// The statistics live in internal/evals/compare, which the evaluation
// comparison shares; these are the names this package has always used.

// zFor returns the two-sided normal quantile for a confidence level such as 0.95.
func zFor(confidence float64) float64 { return compare.ZFor(confidence) }

// Interval is a confidence interval.
type Interval = compare.Interval

// Wilson returns the Wilson score interval for k successes in n trials.
func Wilson(k, n int, confidence float64) Interval { return compare.Wilson(k, n, confidence) }

// NewcombeDiff returns the interval for the difference of two independent
// proportions by Newcombe's hybrid score method.
func NewcombeDiff(k1, n1, k2, n2 int, confidence float64) (diff float64, ci Interval) {
	return compare.NewcombeDiff(k1, n1, k2, n2, confidence)
}

// bootstrap returns the seeded percentile interval of stat over resamples.
func bootstrap(n, resamples int, confidence float64, seed uint64, stat func(idx []int) (float64, bool)) Interval {
	return compare.Bootstrap(n, resamples, confidence, seed, stat)
}
