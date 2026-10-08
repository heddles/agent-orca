package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// embeddingMockData is the anonymous struct element type used by embeddingResponse.
type embeddingMockData = struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

// startEmbeddingMock starts a test HTTP server that serves /v1/embeddings.
// It captures each request body and returns a deterministic 1-d embedding
// vector (value = index+1) for every input.
func startEmbeddingMock(t *testing.T) (serverURL string, getRequests func() []embeddingRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []embeddingRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req embeddingRequest
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		captured = append(captured, req)
		mu.Unlock()

		data := make([]embeddingMockData, len(req.Input))
		for i := range req.Input {
			data[i] = embeddingMockData{
				Embedding: []float32{float32(i + 1)},
				Index:     i,
			}
		}
		resp := embeddingResponse{Data: data}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	t.Cleanup(srv.Close)
	return srv.URL, func() []embeddingRequest {
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
}

func TestEmbedPrependsDocPrompt(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	client := NewEmbeddingClientWithKey(
		serverURL, "test-key", "nomic-embed-text",
		"search_document: ", "search_query: ",
	)

	_, err := client.Embed(context.Background(), []string{"chunk one", "chunk two"}, false)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	reqs := getRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	input := reqs[0].Input
	if len(input) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(input))
	}
	if input[0] != "search_document: chunk one" {
		t.Errorf("docPrompt not prepended: got %q, want %q", input[0], "search_document: chunk one")
	}
	if input[1] != "search_document: chunk two" {
		t.Errorf("docPrompt not prepended: got %q, want %q", input[1], "search_document: chunk two")
	}
}

func TestEmbedPrependsQueryPrompt(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	client := NewEmbeddingClientWithKey(
		serverURL, "test-key", "nomic-embed-text",
		"search_document: ", "search_query: ",
	)

	_, err := client.Embed(context.Background(), []string{"how does RAG work?"}, true)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	reqs := getRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	input := reqs[0].Input
	if len(input) != 1 {
		t.Fatalf("expected 1 input, got %d", len(input))
	}
	if input[0] != "search_query: how does RAG work?" {
		t.Errorf("queryPrompt not prepended: got %q, want %q", input[0], "search_query: how does RAG work?")
	}
}

func TestEmbedNoPromptBackwardCompatible(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	// Empty prompts → raw text, no prefix (backward-compatible with OpenAI models).
	client := NewEmbeddingClientWithKey(
		serverURL, "test-key", "text-embedding-3-small",
		"", "",
	)

	tests := []struct {
		name     string
		texts    []string
		forQuery bool
	}{
		{"doc no prompt", []string{"hello", "world"}, false},
		{"query no prompt", []string{"what is RAG?"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Embed(context.Background(), tc.texts, tc.forQuery)
			if err != nil {
				t.Fatalf("Embed failed: %v", err)
			}
		})
	}

	reqs := getRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}

	// First request: doc, no prompt → raw text
	for i, input := range reqs[0].Input {
		if input != tests[0].texts[i] {
			t.Errorf("request 0 input %d: got %q, want %q (no prefix expected)", i, input, tests[0].texts[i])
		}
	}

	// Second request: query, no prompt → raw text
	for i, input := range reqs[1].Input {
		if input != tests[1].texts[i] {
			t.Errorf("request 1 input %d: got %q, want %q (no prefix expected)", i, input, tests[1].texts[i])
		}
	}
}

func TestEmbedOnlyRelevantPromptApplied(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	// Only docPrompt is set; queryPrompt is empty.
	// When forQuery=true, queryPrompt (empty) should be used → no prefix.
	// When forQuery=false, docPrompt should be prepended.
	client := NewEmbeddingClientWithKey(
		serverURL, "test-key", "nomic-embed-text",
		"search_document: ", "",
	)

	// Document embedding → should have docPrompt
	_, err := client.Embed(context.Background(), []string{"doc text"}, false)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	// Query embedding → should have no prompt (queryPrompt is empty)
	_, err = client.Embed(context.Background(), []string{"query text"}, true)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	reqs := getRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}

	// First request: docPrompt prepended
	if reqs[0].Input[0] != "search_document: doc text" {
		t.Errorf("doc path: got %q, want %q", reqs[0].Input[0], "search_document: doc text")
	}

	// Second request: no prompt (queryPrompt is empty)
	if reqs[1].Input[0] != "query text" {
		t.Errorf("query path with empty queryPrompt: got %q, want %q", reqs[1].Input[0], "query text")
	}
}

// TestEmbedConcurrentPreservesOrder verifies that concurrent batch dispatch
// keeps results aligned 1:1 with input texts, and that batches actually run
// in parallel.
func TestEmbedConcurrentPreservesOrder(t *testing.T) {
	var inflight, maxInflight int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		defer atomic.AddInt32(&inflight, -1)
		for {
			m := atomic.LoadInt32(&maxInflight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInflight, m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)

		body, _ := io.ReadAll(io.LimitReader(r.Body, 5<<20))
		var req embeddingRequest
		_ = json.Unmarshal(body, &req)

		// Encode the input length into the embedding so the test can verify
		// each result landed at its original index.
		data := make([]embeddingMockData, len(req.Input))
		for i := range req.Input {
			data[i] = embeddingMockData{Embedding: []float32{float32(len(req.Input[i]))}, Index: i}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse{Data: data})
	}))
	defer srv.Close()

	client := NewEmbeddingClientWithKey(srv.URL, "test-key", "test-model", "", "")

	// 100 texts with unique lengths → 4 batches (32+32+32+4) at maxBatchChunks=32.
	texts := make([]string, 100)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}

	vectors, err := client.Embed(context.Background(), texts, false)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	for i, v := range vectors {
		if len(v) == 0 || v[0] != float32(i+1) {
			t.Errorf("result %d: got %v, want [%d] (order not preserved)", i, v, i+1)
		}
	}

	if got := atomic.LoadInt32(&maxInflight); got < 2 {
		t.Errorf("expected batches to run in parallel, max in-flight requests = %d, want >= 2", got)
	}
}

// TestEmbedBatchingCapsRequestSize verifies texts are split into requests of
// at most maxBatchChunks inputs.
func TestEmbedBatchingCapsRequestSize(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	client := NewEmbeddingClientWithKey(serverURL, "test-key", "test-model", "", "")

	texts := make([]string, 100) // tiny texts → char limit never binds, chunk cap does
	for i := range texts {
		texts[i] = fmt.Sprintf("text %d", i)
	}

	if _, err := client.Embed(context.Background(), texts, false); err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	reqs := getRequests()
	total := 0
	for i, req := range reqs {
		if len(req.Input) > maxBatchChunks {
			t.Errorf("request %d has %d inputs, want <= %d", i, len(req.Input), maxBatchChunks)
		}
		total += len(req.Input)
	}
	if total != len(texts) {
		t.Errorf("expected %d total inputs across requests, got %d", len(texts), total)
	}
	if len(reqs) < 2 {
		t.Errorf("expected multiple requests for %d texts, got %d", len(texts), len(reqs))
	}
}

// TestEmbedOversizedSingleText verifies a single text larger than
// maxBatchCharacterLimit is sent alone instead of stalling the batch loop.
// Request order is nondeterministic under concurrency, so the test matches
// requests by content rather than position.
func TestEmbedOversizedSingleText(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	client := NewEmbeddingClientWithKey(serverURL, "test-key", "test-model", "", "")

	big := strings.Repeat("a", maxBatchCharacterLimit+1000)
	vectors, err := client.Embed(context.Background(), []string{big, "small"}, false)
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(vectors) != 2 || vectors[0] == nil || vectors[1] == nil {
		t.Fatalf("expected 2 vectors, got %d", len(vectors))
	}

	reqs := getRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests (oversized text alone, then the rest), got %d", len(reqs))
	}
	var bigReq, smallReq *embeddingRequest
	for i := range reqs {
		switch {
		case len(reqs[i].Input) == 1 && len(reqs[i].Input[0]) == len(big):
			bigReq = &reqs[i]
		case len(reqs[i].Input) == 1 && reqs[i].Input[0] == "small":
			smallReq = &reqs[i]
		}
	}
	if bigReq == nil || smallReq == nil {
		t.Fatalf("expected one request with only the oversized text and one with 'small', got %v", reqs)
	}
}

func TestProbeDimensionUsesDocPrompt(t *testing.T) {
	serverURL, getRequests := startEmbeddingMock(t)

	client := NewEmbeddingClientWithKey(
		serverURL, "test-key", "nomic-embed-text",
		"search_document: ", "search_query: ",
	)

	dims, err := client.ProbeDimension(context.Background())
	if err != nil {
		t.Fatalf("ProbeDimension failed: %v", err)
	}
	if dims != 1 {
		t.Errorf("expected dimension 1, got %d", dims)
	}

	reqs := getRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	// ProbeDimension uses forQuery=false, which selects docPrompt.
	if reqs[0].Input[0] != "search_document: probe" {
		t.Errorf("ProbeDimension should use docPrompt: got %q, want %q",
			reqs[0].Input[0], "search_document: probe")
	}
}
