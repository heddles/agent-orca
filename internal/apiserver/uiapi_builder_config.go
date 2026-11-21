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
	"sync"
	"time"
)

// BuilderConfig holds the operator-level limits of the agent builder's helpers
// (chart values operator.builder.*): "Check with my cases", "Draft from a
// description" and document upload for knowledge. The zero value means the
// defaults below.
type BuilderConfig struct {
	// CheckDefaultBudgetUSD is the budget of a check that names none. Default 0.25.
	CheckDefaultBudgetUSD float64
	// CheckMaxBudgetUSD is the most a check may be given. Default 2.
	CheckMaxBudgetUSD float64
	// CheckMaxCases is the most cases one check runs; a larger dataset is cut to
	// its first cases. Default 25.
	CheckMaxCases int
	// CheckMaxConcurrency caps the runs a check has in flight. Default 2.
	CheckMaxConcurrency int
	// CheckPerUserPerHour is how many checks one user may start an hour. Default 12.
	CheckPerUserPerHour int
}

func (c BuilderConfig) checkBudget() float64 { return orFloat(c.CheckDefaultBudgetUSD, 0.25) }
func (c BuilderConfig) checkMaxBudget() float64 {
	return orFloat(c.CheckMaxBudgetUSD, 2)
}
func (c BuilderConfig) checkCases() int       { return orInt(c.CheckMaxCases, 25) }
func (c BuilderConfig) checkConcurrency() int { return orInt(c.CheckMaxConcurrency, 2) }
func (c BuilderConfig) checkPerHour() int     { return orInt(c.CheckPerUserPerHour, 12) }

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

// SetBuilder sets the limits of the builder's helpers.
func (s *UIServer) SetBuilder(c BuilderConfig) { s.builderCfg = c }

// windowLimiter allows at most n events per key in a sliding window. It is
// single-process, like RateLimiter: with several replicas each enforces alone.
type windowLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// limiterWindow is the window of the per-user limits: an hour.
const limiterWindow = time.Hour

// allow records an event for key and reports whether it fits the limit; when it
// does not, nothing is recorded and the wait until the oldest event expires is
// returned.
func (l *windowLimiter) allow(key string, n int, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	cut := now.Add(-limiterWindow)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= n {
		l.hits[key] = kept
		return kept[0].Add(limiterWindow).Sub(now), false
	}
	l.hits[key] = append(kept, now)
	return 0, true
}
