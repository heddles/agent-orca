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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const maxEmbeddingBatchSize = 10 // Smaller batches for local models like Ollama

// EmbeddingClient calls an OpenAI-compatible /v1/embeddings endpoint.
type EmbeddingClient struct {
	endpoint    string // base URL (e.g. "https://api.openai.com")
	apiKeyFile  string // path to file containing the API key (used by agent pods)
	apiKey      string // API key value (used by operator, read from k8s Secret)
	model       string // model identifier (e.g. "text-embedding-3-small")
	docPrompt   string // optional prefix prepended to document chunk texts before embedding
	queryPrompt string // optional prefix prepended to query texts before embedding
	httpClient  *http.Client
}

// NewEmbeddingClient creates an embedding client that reads the API key from a file.
// docPrompt is prepended to document texts; queryPrompt is prepended to query texts.
// Leave either empty to send raw text (suitable for OpenAI-hosted models).
func NewEmbeddingClient(endpoint, apiKeyFile, model, docPrompt, queryPrompt string) *EmbeddingClient {
	return &EmbeddingClient{
		endpoint:    strings.TrimRight(endpoint, "/"),
		apiKeyFile:  apiKeyFile,
		model:       model,
		docPrompt:   docPrompt,
		queryPrompt: queryPrompt,
		httpClient:  &http.Client{Timeout: 5 * time.Minute},
	}
}

// NewEmbeddingClientWithKey creates an embedding client with an explicit API key.
// docPrompt is prepended to document texts; queryPrompt is prepended to query texts.
// Leave either empty to send raw text (suitable for OpenAI-hosted models).
func NewEmbeddingClientWithKey(endpoint, apiKey, model, docPrompt, queryPrompt string) *EmbeddingClient {
	return &EmbeddingClient{
		endpoint:    strings.TrimRight(endpoint, "/"),
		apiKey:      apiKey,
		model:       model,
		docPrompt:   docPrompt,
		queryPrompt: queryPrompt,
		httpClient:  &http.Client{Timeout: 5 * time.Minute},
	}
}

type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed converts texts into embedding vectors. Handles batching automatically.
// When forQuery is true, queryPrompt is prepended to each text (used for
// _rag_search); when false, docPrompt is prepended (used for _rag_ingest).
// When the relevant prompt is empty, texts are sent as-is — this preserves
// backward compatibility for models that do not need a task prefix (e.g.
// OpenAI's text-embedding-3-*).
func (e *EmbeddingClient) Embed(ctx context.Context, texts []string, forQuery bool) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	apiKey, err := e.readAPIKey()
	if err != nil {
		return nil, err
	}

	results := make([][]float32, len(texts))

	// Process in batches.
	for start := 0; start < len(texts); start += maxEmbeddingBatchSize {
		end := min(start+maxEmbeddingBatchSize, len(texts))
		batch := texts[start:end]

		vectors, err := e.embedBatch(ctx, batch, apiKey, forQuery)
		if err != nil {
			return nil, fmt.Errorf("embedding batch [%d:%d]: %w", start, end, err)
		}
		for i, v := range vectors {
			results[start+i] = v
		}
	}

	return results, nil
}

func (e *EmbeddingClient) embedBatch(ctx context.Context, texts []string, apiKey string, forQuery bool) ([][]float32, error) {
	// Select the appropriate prompt prefix. forQuery=true → queryPrompt
	// (for _rag_search); false → docPrompt (for _rag_ingest).
	prompt := e.docPrompt
	if forQuery {
		prompt = e.queryPrompt
	}

	// Prepend the prompt to each text if one is configured. This is needed for
	// open-source embedding models that require task-specific prefixes (e.g.
	// nomic-embed-text expects "search_query: " / "search_document: ").
	inputTexts := make([]string, len(texts))
	for i, t := range texts {
		if prompt != "" {
			inputTexts[i] = prompt + t
		} else {
			inputTexts[i] = t
		}
	}

	reqBody, err := json.Marshal(embeddingRequest{
		Model: e.model,
		Input: inputTexts,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	url := e.endpoint + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling embeddings API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings API returned %d: %s", resp.StatusCode, string(body))
	}

	var embResp embeddingResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, fmt.Errorf("parsing embedding response: %w", err)
	}

	// Order by index.
	vectors := make([][]float32, len(texts))
	for _, d := range embResp.Data {
		if d.Index < len(vectors) {
			vectors[d.Index] = d.Embedding
		}
	}

	// Verify all vectors present.
	for i, v := range vectors {
		if v == nil {
			return nil, fmt.Errorf("missing embedding for input index %d", i)
		}
	}

	return vectors, nil
}

// ProbeDimension embeds a single placeholder string and returns the output vector length.
// Call once on first use to discover the model's actual embedding dimension rather than
// requiring users to specify it manually. Uses docPrompt for consistency; the dimension
// is prompt-independent.
func (e *EmbeddingClient) ProbeDimension(ctx context.Context) (int, error) {
	vecs, err := e.Embed(ctx, []string{"probe"}, false)
	if err != nil {
		return 0, fmt.Errorf("probing embedding dimension: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return 0, fmt.Errorf("embedding probe returned empty vector")
	}
	return len(vecs[0]), nil
}

func (e *EmbeddingClient) readAPIKey() (string, error) {
	if e.apiKey != "" {
		return e.apiKey, nil
	}
	data, err := os.ReadFile(e.apiKeyFile)
	if err != nil {
		return "", fmt.Errorf("reading API key from %s: %w", e.apiKeyFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}
