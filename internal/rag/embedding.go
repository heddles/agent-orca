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
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/ratelimit"
)

const (
	// maxBatchCharacterLimit caps total characters per embedding request
	// (used by the local/self-hosted and unknown-remote presets) so payload
	// and response sizes stay predictable on memory-constrained runners.
	maxBatchCharacterLimit = 250000
	// maxBatchChunks caps the number of texts per request. Smaller batches keep
	// each request well under the HTTP timeout on slow CPU-only backends and
	// let parallel workers balance load evenly.
	maxBatchChunks = 32
	// defaultEmbedConcurrency is the number of embedding requests sent in
	// parallel for local backends. It matches OLLAMA_NUM_PARALLEL=4 in the
	// sample Ollama deployment; Ollama continuous-batches parallel requests
	// so cores stay saturated instead of idling between round-trips.
	defaultEmbedConcurrency = 4
	// concurrencyEnvVar lets operators tune embedding concurrency without a
	// rebuild (e.g. set to 2 under tight memory pressure). It overrides the
	// provider preset's concurrency for all endpoint types.
	concurrencyEnvVar = "EMBEDDING_CONCURRENCY"
	// maxEmbedAttempts bounds retries for transient embeddings API failures
	// (429 rate limit / 503 overloaded) before the batch fails.
	maxEmbedAttempts = 5
	// initialRetryDelay/maxRetryDelay bound the exponential backoff between
	// retry attempts when the provider declares no delay of its own.
	initialRetryDelay = 1 * time.Second
	maxRetryDelay     = 30 * time.Second
	// serverRetryDelayCap bounds a provider-declared retry delay (Retry-After
	// header or google.rpc.RetryInfo retryDelay). Longer values usually mean
	// a per-day quota (RPD) is exhausted — retrying after that is the
	// caller's decision, not the embedding client's.
	serverRetryDelayCap = 60 * time.Second
	// charsPerTokenEstimate is the rough characters-to-tokens ratio used to
	// keep embedding requests inside a provider's per-minute token budget.
	// It errs high (fewer tokens assumed per char would undershoot the
	// budget) and is reconciled against the API's reported usage.
	charsPerTokenEstimate = 4
)

// EmbeddingClient calls an OpenAI-compatible /v1/embeddings endpoint.
//
// Requests are shaped by a per-provider rate-limit policy (see Limits):
// batches are sized to stay inside the provider's per-minute token budget,
// RPM/TPM buckets pace dispatch, and a shared AIMD limiter shrinks the batch
// budget and concurrency whenever the provider answers 429.
type EmbeddingClient struct {
	endpoint    string // base URL (e.g. "https://api.openai.com")
	apiKeyFile  string // path to file containing the API key (used by agent pods)
	apiKey      string // API key value (used by operator, read from k8s Secret)
	model       string // model identifier (e.g. "text-embedding-3-small")
	docPrompt   string // optional prefix prepended to document chunk texts before embedding
	queryPrompt string // optional prefix prepended to query texts before embedding
	httpClient  *http.Client

	// limits is the base (preset + overrides) shaping policy.
	limits Limits
	// lim holds the adaptive runtime state; it is process-wide (not per
	// client instance) so short-lived clients don't re-learn throttle state.
	lim *adaptiveLimiter
}

// newEmbeddingClient builds a client with the prompt and credential variants
// resolved; the rate-limit policy is derived from the endpoint host, with the
// EMBEDDING_CONCURRENCY env var overriding the preset concurrency.
func newEmbeddingClient(endpoint, apiKeyFile, apiKey, model, docPrompt, queryPrompt string) *EmbeddingClient {
	limits := ResolveLimits(endpoint)
	if v := os.Getenv(concurrencyEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limits.Concurrency = n
		}
	}
	return &EmbeddingClient{
		endpoint:    strings.TrimRight(endpoint, "/"),
		apiKeyFile:  apiKeyFile,
		apiKey:      apiKey,
		model:       model,
		docPrompt:   docPrompt,
		queryPrompt: queryPrompt,
		httpClient:  &http.Client{Timeout: 5 * time.Minute},
		limits:      limits,
	}
}

// NewEmbeddingClient creates an embedding client that reads the API key from a file.
// docPrompt is prepended to document texts; queryPrompt is prepended to query texts.
// Leave either empty to send raw text (suitable for OpenAI-hosted models).
func NewEmbeddingClient(endpoint, apiKeyFile, model, docPrompt, queryPrompt string) *EmbeddingClient {
	return newEmbeddingClient(endpoint, apiKeyFile, "", model, docPrompt, queryPrompt)
}

// NewEmbeddingClientWithKey creates an embedding client with an explicit API key.
// docPrompt is prepended to document texts; queryPrompt is prepended to query texts.
// Leave either empty to send raw text (suitable for OpenAI-hosted models).
func NewEmbeddingClientWithKey(endpoint, apiKey, model, docPrompt, queryPrompt string) *EmbeddingClient {
	return newEmbeddingClient(endpoint, "", apiKey, model, docPrompt, queryPrompt)
}

// WithRateLimits layers explicit per-minute budgets (typically sourced from
// ModelProvider.spec.constraints.rpm/tpm) over the endpoint-derived preset.
// Non-positive values leave the preset untouched. Chainable; call before the
// first Embed.
func (e *EmbeddingClient) WithRateLimits(rpm, tpm int) *EmbeddingClient {
	if rpm <= 0 && tpm <= 0 {
		return e
	}
	e.limits = ApplyOverrides(e.limits, rpm, tpm)
	return e
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
	Usage struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage"`
}

// Embed converts texts into embedding vectors. Handles batching automatically.
// When forQuery is true, queryPrompt is prepended to each text (used for
// _rag_search); when false, docPrompt is prepended (used for _rag_ingest).
// When the relevant prompt is empty, texts are sent as-is — this preserves
// backward compatibility for models that do not need a task prefix (e.g.
// OpenAI's text-embedding-3-*).
//
// Batching is adaptive: a pool of workers pulls texts off a shared cursor,
// assembling each request under the limiter's current character budget and
// pacing dispatch through the provider's RPM/TPM buckets. After a 429 the
// budget and concurrency shrink (multiplicative decrease) and grow back
// toward the preset on sustained success. The returned vectors always align
// 1:1 with the input texts, in input order.
func (e *EmbeddingClient) Embed(ctx context.Context, texts []string, forQuery bool) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	apiKey, err := e.readAPIKey()
	if err != nil {
		return nil, err
	}

	if e.lim == nil {
		e.lim = limiterFor(e.limits)
	}

	results := make([][]float32, len(texts))
	var mu sync.Mutex
	var firstErr error
	cursor := 0

	// batchCtx lets the first failing batch cancel all in-flight and queued
	// batches instead of grinding through the rest of the workload.
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	worker := func() {
		for {
			mu.Lock()
			if firstErr != nil || cursor >= len(texts) {
				mu.Unlock()
				return
			}
			start := cursor
			charBudget := e.lim.batchCharBudget()
			chars := 0
			end := start
			for end < len(texts) && end-start < e.limits.MaxBatchChunks {
				// A text always gets its own request when the budget is
				// exhausted, but never overflow the budget once the batch is
				// non-empty — providers enforce per-request and per-minute
				// token caps hard.
				if chars > 0 && chars+len(texts[end]) > charBudget {
					break
				}
				chars += len(texts[end])
				end++
			}
			if end == start {
				end = start + 1
			}
			cursor = end
			mu.Unlock()

			// ~charsPerTokenEstimate chars per token; min 1 so empty/binary
			// texts still occupy request slots.
			estTokens := int64(chars/charsPerTokenEstimate + 1)

			release, err := e.lim.acquire(batchCtx, estTokens)
			if err != nil {
				// Cancellation — surface ctx.Err() below, not as a batch error.
				return
			}

			started := time.Now()
			vectors, usageTokens, firstTry, err := e.embedBatch(batchCtx, texts[start:end], apiKey, forQuery, estTokens)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("embedding batch [%d:%d]: %w", start, end, err)
					cancel()
				}
				mu.Unlock()
				release()
				return
			}

			copy(results[start:], vectors)

			// Reconcile the upfront estimate with what the API actually
			// charged, and grow the budget back after clean successes.
			e.lim.reconcileTokens(estTokens, usageTokens)
			if firstTry {
				e.lim.reward()
			}
			release()

			slog.Info("embedded batch",
				"chunks", end-start,
				"chars", chars,
				"span", fmt.Sprintf("[%d:%d]", start, end),
				"duration", time.Since(started).Round(time.Millisecond),
			)
		}
	}

	workers := e.limits.Concurrency
	if workers > len(texts) {
		workers = len(texts)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	// Surface caller cancellation: some batches may have been skipped without
	// recording an error of their own.
	return results, ctx.Err()
}

// embedBatch embeds one batch of texts, retrying transient throttling and
// overload responses. It returns the vectors (in input order), the token
// usage the API actually charged (0 when unavailable), and whether the batch
// succeeded on its first attempt (retry-free successes drive the adaptive
// limiter's growth).
func (e *EmbeddingClient) embedBatch(ctx context.Context, texts []string, apiKey string, forQuery bool, tokenEstimate int64) (vectors [][]float32, usageTokens int64, firstTry bool, err error) {
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
		return nil, 0, false, fmt.Errorf("marshaling request: %w", err)
	}

	url := e.endpoint + "/v1/embeddings"
	var resp *http.Response
	var body []byte
	firstTry = true
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			firstTry = false
			// Back off exponentially before retrying a throttled or overloaded
			// response. A provider-declared delay (RetryInfo in the error
			// body, or Retry-After) is authoritative and takes precedence.
			delay := initialRetryDelay << uint(attempt-1)
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}
			if sd := ratelimit.ParseRetryDelay(resp.Header, body); sd > 0 {
				delay = min(sd, serverRetryDelayCap)
			}
			slog.Warn("embeddings API throttled, retrying",
				"status", resp.StatusCode,
				"attempt", attempt,
				"max_attempts", maxEmbedAttempts,
				"retry_in", delay.Round(time.Millisecond),
			)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, 0, false, fmt.Errorf("calling embeddings API: %w", ctx.Err())
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, 0, false, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err = e.httpClient.Do(req)
		if err != nil {
			return nil, 0, false, fmt.Errorf("calling embeddings API: %w", err)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, 0, false, fmt.Errorf("reading response: %w", err)
		}

		// A 429 means our current budget exceeds the provider's — shrink it
		// before the retries (and any sibling batches) fire again.
		if resp.StatusCode == http.StatusTooManyRequests && e.lim != nil {
			e.lim.penalize()
		}

		// Retry transient throttling/overload responses; anything else falls
		// through to the terminal-error handling below.
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
			break
		}
		if attempt == maxEmbedAttempts-1 {
			break
		}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, 0, false, fmt.Errorf("embeddings API returned %d: %s", resp.StatusCode, string(body))
	}

	var embResp embeddingResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, 0, false, fmt.Errorf("parsing embedding response: %w", err)
	}

	// Order by index.
	vectors = make([][]float32, len(texts))
	for _, d := range embResp.Data {
		if d.Index < len(vectors) {
			vectors[d.Index] = d.Embedding
		}
	}

	// Verify all vectors present.
	for i, v := range vectors {
		if v == nil {
			return nil, 0, false, fmt.Errorf("missing embedding for input index %d", i)
		}
	}

	return vectors, embResp.Usage.TotalTokens, firstTry, nil
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
