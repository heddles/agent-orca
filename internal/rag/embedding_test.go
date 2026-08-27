package rag

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
	var captured []embeddingRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req embeddingRequest
		_ = json.Unmarshal(body, &req)
		captured = append(captured, req)

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
	return srv.URL, func() []embeddingRequest { return captured }
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
