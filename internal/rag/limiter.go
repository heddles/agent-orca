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

package rag

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	// minBatchCharBudget is the floor the AIMD loop shrinks the batch char
	// budget to. Below this, requests are single-chunk anyway and shrinking
	// further only changes the text size, which the provider already accepted
	// as one request's worth of work.
	minBatchCharBudget = 1_000
	// rewardsPerGrowth: after this many clean (first-try) successes the AIMD
	// loop grows batch budget and concurrency by one step.
	rewardsPerGrowth = 8
	// acquirePollInterval bounds how often a blocked worker re-checks the
	// adaptive concurrency gate (sync.Cond cannot select on ctx).
	acquirePollInterval = 25 * time.Millisecond
	// maxWaitChunk bounds a single bucket wait so context cancellation stays
	// responsive during long TPM refills (e.g. 30k tokens at 500/s).
	maxWaitChunk = 5 * time.Second
)

// tokenBucket is a replenishing bucket for a per-minute budget (requests or
// tokens). Capacity equals the full per-minute allowance and refills linearly
// (limit/60 per second), so a fresh client may spend its whole budget at once
// but sustained throughput never exceeds the limit.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // tokens per second
	last   time.Time
}

// newTokenBucket returns nil for limit <= 0 ("unlimited" marker) so callers
// skip pacing entirely.
func newTokenBucket(limitPerMinute int) *tokenBucket {
	if limitPerMinute <= 0 {
		return nil
	}
	return &tokenBucket{
		tokens: float64(limitPerMinute),
		max:    float64(limitPerMinute),
		rate:   float64(limitPerMinute) / 60,
	}
}

// take blocks until n tokens are available or ctx is done.
func (b *tokenBucket) take(ctx context.Context, n int64) error {
	for {
		b.mu.Lock()
		b.refill()
		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return nil
		}
		deficit := time.Duration((float64(n) - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		if deficit > maxWaitChunk {
			deficit = maxWaitChunk
		}
		if deficit < acquirePollInterval {
			deficit = acquirePollInterval
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(deficit):
		}
	}
}

func (b *tokenBucket) refill() {
	now := time.Now()
	if b.rate <= 0 {
		return
	}
	if b.last.IsZero() {
		b.last = now
		return
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		b.last = now
		if b.tokens > b.max {
			b.tokens = b.max
		}
	}
}

// adjust reconciles an upfront estimate with the usage the API actually
// charged: underestimates go into "debt" (future takes wait longer) and
// overestimates are refunded.
func (b *tokenBucket) adjust(estimate, actual int64) {
	if b == nil || actual <= 0 || estimate == actual {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	b.tokens += float64(estimate - actual)
}

// adaptiveLimiter shapes embedding traffic against a provider's published
// per-minute budgets and adapts downward when the provider says otherwise.
//
// AIMD (additive increase / multiplicative decrease) semantics:
//   - 429 responses halve the batch char budget and the allowed concurrency;
//   - clean successes grow both back toward the preset one step at a time.
//
// This lets a client converge on whatever budget the caller's key actually
// has (free tier, paid tier, shared key) without anyone configuring it.
type adaptiveLimiter struct {
	base Limits // immutable preset the AIMD loop recovers toward

	rpm, tpm *tokenBucket

	mu          sync.Mutex
	inflight    int
	maxInflight int // current allowed concurrency (>=1)
	charBudget  int // current per-request char budget
	successes   int
}

var limiters sync.Map // limits key -> *adaptiveLimiter, shared per process

// limiterFor returns the process-wide limiter for a base policy. Sharing is
// deliberate: the operator creates a fresh EmbeddingClient on every reconcile,
// and AIMD state must survive across reconciles instead of re-learning (and
// re-receiving 429s) on each pass.
func limiterFor(base Limits) *adaptiveLimiter {
	key := fmt.Sprintf("%+v", base)
	if v, ok := limiters.Load(key); ok {
		return v.(*adaptiveLimiter)
	}
	l := &adaptiveLimiter{
		base:        base,
		rpm:         newTokenBucket(base.RPM),
		tpm:         newTokenBucket(base.TPM),
		maxInflight: max(1, base.Concurrency),
		charBudget:  max(1, base.MaxBatchChars),
	}
	actual, loaded := limiters.LoadOrStore(key, l)
	if loaded {
		return actual.(*adaptiveLimiter)
	}
	return l
}

// acquire reserves one request slot, one request-token, and an estimated
// token spend, blocking as needed. The returned func releases the slot.
func (l *adaptiveLimiter) acquire(ctx context.Context, estTokens int64) (func(), error) {
	for {
		l.mu.Lock()
		if l.inflight < l.maxInflight {
			l.inflight++
			l.mu.Unlock()
			break
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(acquirePollInterval):
		}
	}
	release := func() {
		l.mu.Lock()
		l.inflight--
		l.mu.Unlock()
	}

	if err := l.rpm.take(ctx, 1); err != nil {
		release()
		return nil, err
	}
	// A single request that is bigger than the whole bucket (one text exceeds
	// the provider's entire per-minute budget) can never be paced — cap the
	// estimate at capacity so it still goes out rather than blocking forever.
	if l.tpm != nil && estTokens > int64(l.tpm.max) {
		estTokens = int64(l.tpm.max)
	}
	if err := l.tpm.take(ctx, estTokens); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// penalize records a 429: halve batch budget and concurrency.
func (l *adaptiveLimiter) penalize() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.successes = 0
	floor := min(minBatchCharBudget, l.base.MaxBatchChars)
	if next := l.charBudget / 2; next >= floor {
		l.charBudget = next
	} else {
		l.charBudget = floor
	}
	if next := l.maxInflight / 2; next >= 1 {
		l.maxInflight = next
	}
	slog.Warn("embedding provider throttled us; shrinking batch budget",
		"char_budget", l.charBudget,
		"max_inflight", l.maxInflight,
	)
}

// reward records a clean (no-retry) success; after enough of them the AIMD
// loop grows back one step toward the preset.
func (l *adaptiveLimiter) reward() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.successes++
	if l.successes < rewardsPerGrowth {
		return
	}
	l.successes = 0
	if l.charBudget < l.base.MaxBatchChars {
		l.charBudget = min(l.base.MaxBatchChars, l.charBudget*2)
	}
	if l.maxInflight < l.base.Concurrency {
		l.maxInflight++
	}
	slog.Info("embedding provider healthy; growing batch budget",
		"char_budget", l.charBudget,
		"max_inflight", l.maxInflight,
	)
}

// batchCharBudget returns the current per-request character budget.
func (l *adaptiveLimiter) batchCharBudget() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.charBudget
}

// reconcileTokens refunds or debits the difference between the upfront token
// estimate and the API-reported usage.
func (l *adaptiveLimiter) reconcileTokens(estimate, actual int64) {
	l.tpm.adjust(estimate, actual)
}
