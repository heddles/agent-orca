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

import "testing"

// TestPendingChunks verifies the resume filter: chunks with a matching stored
// content hash are skipped; missing points, stale hashes (changed content),
// and pre-hashing points (empty stored hash) are re-embedded.
func TestPendingChunks(t *testing.T) {
	chunks := []Chunk{
		{Index: 0, DocID: "doc-a", Text: "alpha"},
		{Index: 1, DocID: "doc-a", Text: "beta"},
		{Index: 2, DocID: "doc-a", Text: "gamma"},
	}

	existing := map[string]string{
		pointID("doc-a", 0): contentHash("alpha"),   // done — skip
		pointID("doc-a", 1): contentHash("CHANGED"), // content changed — re-embed
		pointID("doc-a", 2): "",                     // pre-hashing point — re-embed
		// chunk 3 (if it existed) would be missing entirely — re-embed
	}

	todo := pendingChunks(chunks, existing)
	if len(todo) != 2 {
		t.Fatalf("expected 2 pending chunks, got %d", len(todo))
	}
	if todo[0].Text != "beta" || todo[1].Text != "gamma" {
		t.Errorf("wrong chunks pending: %v", todo)
	}
}

func TestPendingChunksAllDone(t *testing.T) {
	chunks := []Chunk{
		{Index: 0, DocID: "doc-a", Text: "alpha"},
		{Index: 1, DocID: "doc-a", Text: "beta"},
	}
	existing := map[string]string{
		pointID("doc-a", 0): contentHash("alpha"),
		pointID("doc-a", 1): contentHash("beta"),
	}
	if todo := pendingChunks(chunks, existing); len(todo) != 0 {
		t.Errorf("expected all chunks skipped, got %d pending", len(todo))
	}
}

// TestTokenEstimateCalibration verifies the EWMA tracks dense text: after the
// API reports ~1.44× the naive estimate (the observed PDF overshoot), future
// estimates scale up accordingly.
func TestTokenEstimateCalibration(t *testing.T) {
	base := Limits{RPM: 0, TPM: 0, MaxBatchChars: 250000, MaxBatchChunks: 32, Concurrency: 1}
	l := limiterFor(base)

	naive := l.estimateTokens(40_000) // chars/4 = 10_000 tokens
	if naive != 10_000 {
		t.Fatalf("initial estimate = %d, want 10000", naive)
	}

	// Provider charges 14_400 tokens for what we estimated at 10_000.
	l.reconcileTokens(10_000, 14_400)
	calibrated := l.estimateTokens(40_000)
	// EWMA: 0.9*1.0 + 0.1*1.44 = 1.044 → ~10_440. Must be strictly above naive.
	if calibrated <= naive {
		t.Errorf("calibration did not increase estimate: %d (naive %d)", calibrated, naive)
	}

	// Repeated observations converge toward the true ratio.
	for i := 0; i < 50; i++ {
		l.reconcileTokens(10_000, 14_400)
	}
	converged := l.estimateTokens(40_000)
	if converged < 14_000 || converged > 14_800 {
		t.Errorf("estimate after convergence = %d, want ~14400", converged)
	}
}
