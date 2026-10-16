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
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTokenBucketPacesAfterDepletion verifies the bucket empties instantly
// (no cold-start wait) then refills at the configured rate.
func TestTokenBucketPacesAfterDepletion(t *testing.T) {
	// Capacity 100, refill 10/s: draining is instant, the next take waits
	// ~1s for 10 tokens to accrue.
	b := newTokenBucketRate(100, 10)
	if b == nil {
		t.Fatal("nil bucket for positive limit")
	}
	if err := b.take(context.Background(), 100); err != nil { // drain in one take
		t.Fatalf("drain take failed: %v", err)
	}
	start := time.Now()
	if err := b.take(context.Background(), 10); err != nil {
		t.Fatalf("paced take failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Millisecond {
		t.Errorf("expected bucket to pace the post-drain take (~1s at 10/s), took %v", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("paced take waited far too long: %v", elapsed)
	}
}

// TestTokenBucketAdjustReconciles verifies usage reconciliation: refunding
// overestimates and debiting underestimates.
func TestTokenBucketAdjustReconciles(t *testing.T) {
	b := newTokenBucketRate(1000, 1000)                        // 1s to refill fully — fast test
	if err := b.take(context.Background(), 1000); err != nil { // drain
		t.Fatal(err)
	}
	// API charged only 400 of the estimated 1000 → 600 refunded.
	b.adjust(1000, 400)
	if err := b.take(context.Background(), 600); err != nil {
		t.Fatalf("expected refund to make tokens available: %v", err)
	}
	// API charged more than estimated → debt; further takes must pace.
	b.adjust(600, 700)
	start := time.Now()
	if err := b.take(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Errorf("expected debt to force a wait, took %v", time.Since(start))
	}
}

func TestTokenBucketUnlimitedIsNil(t *testing.T) {
	if newTokenBucket(0) != nil || newTokenBucket(-1) != nil {
		t.Error("zero/negative limits must disable the bucket (nil)")
	}
}

// TestAIMDHalvesAndGrows verifies the penalize/reward cycle drives batch
// budget and concurrency back toward the preset after shrinking.
func TestAIMDHalvesAndGrows(t *testing.T) {
	// Distinct base values so %+v cache keys don't collide with other tests.
	base := Limits{RPM: 0, TPM: 0, MaxBatchChars: 120, MaxBatchChunks: 32, Concurrency: 4}
	l := limiterFor(base)

	if got := l.batchCharBudget(); got != 120 {
		t.Fatalf("initial budget = %d, want 120", got)
	}

	l.penalize()
	if got := l.batchCharBudget(); got != 60 {
		t.Errorf("after one penalize budget = %d, want 60", got)
	}
	l.penalize()
	if got := l.batchCharBudget(); got != 30 {
		t.Errorf("after two penalizes budget = %d, want floor 30", got)
	}
	if got := l.adaptiveConcurrency(); got != 1 {
		t.Errorf("after two penalizes concurrency = %d, want 1", got)
	}

	// 8 clean successes grow one step: budget 30→60 and concurrency 1→2.
	for range 8 {
		l.reward()
	}
	if got := l.batchCharBudget(); got != 60 {
		t.Errorf("after growth budget = %d, want 60", got)
	}
	if got := l.adaptiveConcurrency(); got != 2 {
		t.Errorf("after growth concurrency = %d, want 2", got)
	}
}

// adaptiveConcurrency exposes the allowed in-flight count for assertions.
func (l *adaptiveLimiter) adaptiveConcurrency() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxInflight
}

// TestAcquireCapsEstimateAtCapacity verifies a token estimate larger than the
// whole TPM bucket doesn't block forever (one text can exceed a tiny provider
// budget — the request must still go out).
func TestAcquireCapsEstimateAtCapacity(t *testing.T) {
	base := Limits{RPM: 0, TPM: 1, MaxBatchChars: 250000, MaxBatchChunks: 32, Concurrency: 1}
	l := limiterFor(base)

	done := make(chan error, 1)
	go func() {
		release, err := l.acquire(context.Background(), 10_000) // est >> capacity 1
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acquire failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire blocked on estimate exceeding bucket capacity")
	}
}

// throttlingMock serves /v1/embeddings, 429ing the first multi-input request
// with a short Retry-After, and captures every request body.
type throttlingMock struct {
	requests []embeddingRequest
	four29s  int32 // actual 429 responses served
	mu       sync.Mutex
}

// mockEmbeddingValue derives a deterministic, globally-unique-per-content
// value so the test can verify each result landed at its original index.
func mockEmbeddingValue(input string) float32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(input))
	return float32(h.Sum32()%9999 + 1)
}

// TestEmbedShrinksBatchesAfter429 is the end-to-end AIMD test: a single 429
// from the provider must shrink subsequent batch sizes without failing Embed,
// and effects must remain invisible to Embed's contract (full coverage,
// order preserved).
func TestEmbedShrinksBatchesAfter429(t *testing.T) {
	mock := &throttlingMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req embeddingRequest
		_ = json.Unmarshal(body, &req)
		mock.mu.Lock()
		mock.requests = append(mock.requests, req)
		mock.mu.Unlock()

		if len(req.Input) > 1 && atomic.CompareAndSwapInt32(&mock.four29s, 0, 1) {
			// Throttle exactly once, with a short server-declared delay.
			w.Header().Set("Retry-After", "0.1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`))
			return
		}

		data := make([]embeddingMockData, len(req.Input))
		for i := range req.Input {
			data[i] = embeddingMockData{Embedding: []float32{mockEmbeddingValue(req.Input[i])}, Index: i}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse{Data: data})
	}))
	defer srv.Close()

	client := NewEmbeddingClientWithKey(srv.URL, "test-key", "test-model", "", "")
	// Distinct limits → isolated limiter instance (cache is keyed by %+v).
	// 120-char budget with 60-char texts = 2 texts per request initially.
	client.limits = Limits{RPM: 0, TPM: 0, MaxBatchChars: 120, MaxBatchChunks: 32, Concurrency: 2}

	texts := make([]string, 30)
	for i := range texts {
		texts[i] = pad60(i)
	}

	vectors, err := client.Embed(context.Background(), texts, false)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	for i, v := range vectors {
		if want := mockEmbeddingValue(texts[i]); len(v) == 0 || v[0] != want {
			t.Errorf("result %d: got %v, want [%v] (order not preserved)", i, v, want)
		}
	}

	if n := atomic.LoadInt32(&mock.four29s); n != 1 {
		t.Errorf("expected exactly 1 429 response, got %d", n)
	}

	// Coverage: every batch is within the (initial) char budget and every
	// text was embedded exactly once — verified by order preservation above.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	singleInput := 0
	for i, req := range mock.requests {
		total := 0
		for _, in := range req.Input {
			total += len(in)
		}
		if total > 120 {
			t.Errorf("request %d sent %d chars, above the initial 120-char budget", i, total)
		}
		if len(req.Input) == 1 {
			singleInput++
		}
	}
	// Shrink to a 60-char budget must have produced single-text requests
	// (before AIMD growth recovers the 120-char budget).
	if singleInput < 2 {
		t.Errorf("expected at least 2 shrunken single-input requests, got %d across %d requests",
			singleInput, len(mock.requests))
	}
}

// pad60 returns a unique 60-character string for index i.
func pad60(i int) string {
	s := make([]byte, 60)
	for j := range s {
		s[j] = byte('a' + (i*7+j*3)%26)
	}
	return string(s)
}
