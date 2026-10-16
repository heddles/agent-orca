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

// newTokenBucketRate is the test constructor: same bucket semantics but with
// an explicit refill rate so tests can observe pacing in milliseconds instead
// of waiting out real per-minute budgets.
func newTokenBucketRate(limitPerMinute int, perSecond float64) *tokenBucket {
	if limitPerMinute <= 0 {
		return nil
	}
	return &tokenBucket{
		tokens: float64(limitPerMinute),
		max:    float64(limitPerMinute),
		rate:   perSecond,
	}
}

// take blocks until n tokens are available or ctx is done.
// A nil bucket means "unlimited" — no pacing.
func (b *tokenBucket) take(ctx context.Context, n int64) error {
	if b == nil {
		return nil
	}
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
	if b.tokens > b.max {
		b.tokens = b.max
	}
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
	// tokenRatio calibrates the chars→tokens estimate: an EWMA of
	// (actual tokens charged) / (estimated tokens). Dense text (PDFs) runs
	// ~1.4× the naive chars/4 estimate; without calibration the client
	// overshoots the provider's TPM budget on the first pass.
	tokenRatio float64
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
		tokenRatio:  1.0,
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

	// Nil buckets mean "unlimited" — take() is nil-safe and skips pacing.
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
	// Floor: a fraction of the base budget so repeated penalties can always
	// shrink meaningfully, but never below a minimal useful request size.
	floor := max(1, min(minBatchCharBudget, l.base.MaxBatchChars/4))
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

// estimateTokens converts a character count into a token estimate using the
// calibrated chars→tokens ratio (see tokenRatio).
func (l *adaptiveLimiter) estimateTokens(chars int) int64 {
	l.mu.Lock()
	ratio := l.tokenRatio
	l.mu.Unlock()
	if chars <= 0 {
		return 1
	}
	return max(1, int64(float64(chars)/float64(charsPerTokenEstimate)*ratio))
}

// reconcileTokens refunds or debits the difference between the upfront token
// estimate and the API-reported usage, and folds the observed ratio into the
// calibration EWMA so future estimates track the provider's tokenizer.
func (l *adaptiveLimiter) reconcileTokens(estimate, actual int64) {
	l.tpm.adjust(estimate, actual)
	if estimate <= 0 || actual <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ratio := float64(actual) / float64(estimate)
	l.tokenRatio = min(4.0, max(0.5, 0.9*l.tokenRatio+0.1*ratio))
}
