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
	endpoint   string // base URL (e.g. "https://api.openai.com")
	apiKeyFile string // path to file containing the API key (used by agent pods)
	apiKey     string // API key value (used by operator, read from k8s Secret)
	model      string // model identifier (e.g. "text-embedding-3-small")
	httpClient *http.Client
}

// NewEmbeddingClient creates an embedding client that reads the API key from a file.
func NewEmbeddingClient(endpoint, apiKeyFile, model string) *EmbeddingClient {
	return &EmbeddingClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		apiKeyFile: apiKeyFile,
		model:      model,
		httpClient: &http.Client{Timeout: 5 * time.Minute},
	}
}

// NewEmbeddingClientWithKey creates an embedding client with an explicit API key.
func NewEmbeddingClientWithKey(endpoint, apiKey, model string) *EmbeddingClient {
	return &EmbeddingClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 5 * time.Minute},
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
func (e *EmbeddingClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
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
		end := start + maxEmbeddingBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]

		vectors, err := e.embedBatch(ctx, batch, apiKey)
		if err != nil {
			return nil, fmt.Errorf("embedding batch [%d:%d]: %w", start, end, err)
		}
		for i, v := range vectors {
			results[start+i] = v
		}
	}

	return results, nil
}

func (e *EmbeddingClient) embedBatch(ctx context.Context, texts []string, apiKey string) ([][]float32, error) {
	reqBody, err := json.Marshal(embeddingRequest{
		Model: e.model,
		Input: texts,
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
	defer resp.Body.Close()

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
// requiring users to specify it manually.
func (e *EmbeddingClient) ProbeDimension(ctx context.Context) (int, error) {
	vecs, err := e.Embed(ctx, []string{"probe"})
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
