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

	// DraftProvider names the ModelProvider that writes a draft from a
	// description. It is looked up in the caller's workspace first and then in the
	// platform catalog (the lookup a judge uses), so a tenant that defines a
	// provider of that name uses its own. Empty turns "Draft from a description" off.
	DraftProvider string
	// DraftPerUserPerHour is how many drafts one user may ask for an hour. Default 10.
	DraftPerUserPerHour int
	// DraftMaxCostUSD is the most one draft call may cost, estimated before the
	// call and refused above it. Default 0.05.
	DraftMaxCostUSD float64
	// DraftDailyUSDPerUser is the most one user's drafts may cost in 24 hours. Default 1.
	DraftDailyUSDPerUser float64
}

func (c BuilderConfig) checkBudget() float64 { return orFloat(c.CheckDefaultBudgetUSD, 0.25) }
func (c BuilderConfig) checkMaxBudget() float64 {
	return orFloat(c.CheckMaxBudgetUSD, 2)
}
func (c BuilderConfig) checkCases() int       { return orInt(c.CheckMaxCases, 25) }
func (c BuilderConfig) checkConcurrency() int { return orInt(c.CheckMaxConcurrency, 2) }
func (c BuilderConfig) checkPerHour() int     { return orInt(c.CheckPerUserPerHour, 12) }

func (c BuilderConfig) draftPerHour() int     { return orInt(c.DraftPerUserPerHour, 10) }
func (c BuilderConfig) draftMaxCost() float64 { return orFloat(c.DraftMaxCostUSD, 0.05) }
func (c BuilderConfig) draftDaily() float64   { return orFloat(c.DraftDailyUSDPerUser, 1) }

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

// spendLedger adds up what a key has spent in a window, to hold a per-user
// daily cap on the model calls the builder makes. Single-process, like windowLimiter.
type spendLedger struct {
	mu    sync.Mutex
	spent map[string][]spendEntry
}

type spendEntry struct {
	at  time.Time
	usd float64
}

// total is what key spent in the last window.
func (l *spendLedger) total(key string, window time.Duration, now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-window)
	var sum float64
	kept := l.spent[key][:0]
	for _, e := range l.spent[key] {
		if e.at.After(cut) {
			kept = append(kept, e)
			sum += e.usd
		}
	}
	if len(kept) == 0 {
		delete(l.spent, key)
	} else {
		l.spent[key] = kept
	}
	return sum
}

func (l *spendLedger) add(key string, usd float64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spent == nil {
		l.spent = map[string][]spendEntry{}
	}
	l.spent[key] = append(l.spent[key], spendEntry{now, usd})
}
