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

package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRateLimitRetries = 3

// doWithRateLimitRetry executes an HTTP request, retrying on 429 responses
// with exponential backoff. The buildReq function is called for each attempt
// to produce a fresh request (the body reader is consumed on each attempt).
func doWithRateLimitRetry(ctx context.Context, buildReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; attempt <= maxRateLimitRetries; attempt++ {
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == maxRateLimitRetries {
			return resp, nil
		}
		// Read the error body to determine if this is a transient rate limit
		// or a permanent "request too large" error.
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodyStr := string(body)
		// "Request too large" 429s will never succeed with retries —
		// the request itself exceeds the model's token limit.
		if strings.Contains(bodyStr, "Request too large") ||
			strings.Contains(bodyStr, "input or output tokens must be reduced") {
			return &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(strings.NewReader(bodyStr)),
			}, nil
		}
		delay := retryAfterDelay(resp.Header, attempt)
		slog.Warn("rate limited (429), retrying",
			"attempt", attempt+1,
			"delay", delay,
			"body", string(body),
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	// unreachable, but satisfies the compiler
	return nil, fmt.Errorf("rate limit retries exhausted")
}

// retryAfterDelay parses the Retry-After header (seconds) and returns a delay.
// Falls back to exponential backoff (2^attempt seconds, capped at 30s).
func retryAfterDelay(headers http.Header, attempt int) time.Duration {
	if ra := headers.Get("Retry-After"); ra != "" {
		if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs > 0 {
			d := min(time.Duration(math.Ceil(secs))*time.Second, 60*time.Second)
			return d
		}
	}
	d := min(time.Duration(1<<uint(attempt))*time.Second, 30*time.Second)
	return d
}

// streamingChatCompletionChunk mirrors the OpenAI streaming chunk format.
type streamingChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Choices []streamChoice `json:"choices"`
	Usage   *TokenUsage    `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// Reasoning holds streaming extended-thinking tokens (OpenAI o-series models emit
	// these as delta.reasoning). Captured and forwarded to the UI as `thought` trace
	// events; without this field the thinking is silently dropped while still being
	// proxied raw to the client, which can leave the run appearing to terminate as
	// soon as the (content-less) thinking block ends.
	Reasoning string     `json:"reasoning,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// forwardToProviderStream sends a streaming request to the provider and returns the
// raw *http.Response for SSE proxying. The caller is responsible for closing the body.
// Uses a detached context so the outgoing call survives agent-side disconnects.
func (r *Router) forwardToProviderStream(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest) (*http.Response, error) { //nolint:unparam

	// Use the run-scoped cancel context so the LLM call is detached from the
	// agent's HTTP connection (survives disconnects) but can still be cancelled
	// explicitly via Router.Cancel() when the run is stopped. Timeout is
	// config-driven (default 1h).
	llmCtx, cancel := context.WithTimeout(r.cancelCtx, r.cfg.LLMRequestTimeout)

	if strings.HasPrefix(provider.LiteLLMModel, "anthropic/") && provider.BaseURL == "" {
		resp, err := r.forwardToAnthropicStream(llmCtx, provider, chatReq)
		if err != nil {
			cancel()
			return nil, err
		}
		// Caller owns resp.Body; cancel when body is closed.
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}

	modelName := provider.LiteLLMModel
	// Keep full model identifier when custom BaseURL is set
	// since the proxy expects the complete identifier including provider prefix.
	if provider.BaseURL == "" {
		if idx := strings.Index(modelName, "/"); idx != -1 {
			modelName = modelName[idx+1:]
		}
	}
	chatReq.Model = modelName
	chatReq.Stream = true

	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		cancel()
		return nil, err
	}

	endpoint := liteLLMEndpoint(provider)
	key, err := readAPIKey(provider.APIKeyFile)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}

	resp, err := doWithRateLimitRetry(llmCtx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(llmCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		return req, nil
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("provider %s: %w", provider.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("provider %s returned %d: %s", provider.Name, resp.StatusCode, body)
	}
	// Caller owns resp.Body; cancel the context when the body is closed.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose wraps an io.ReadCloser and calls a cancel function on Close.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// forwardToAnthropicStream sends a streaming request to Anthropic and returns a response
// whose body emits OpenAI-compatible SSE events (translated on the fly).
func (r *Router) forwardToAnthropicStream(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest) (*http.Response, error) {
	key, err := readAPIKey(provider.APIKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}

	// Concatenate system messages (user prompt + builtin hints) rather than replacing.
	// Note: SystemPrompt is now always injected into the messages array in HandleChatCompletions,
	// so we don't need to add it from config separately.
	var systemParts []string
	var anthropicMessages []map[string]any
	for _, msg := range chatReq.Messages {
		if msg.Role == "system" {
			if text, ok := msg.Content.(string); ok {
				if len(systemParts) == 0 {
					systemParts = []string{text}
				} else {
					systemParts = append(systemParts, text)
				}
			}
			continue
		}

		// Convert assistant messages with tool_calls to Anthropic format.
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			var content []map[string]any
			if text, ok := msg.Content.(string); ok && text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": json.RawMessage(args),
				})
			}
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    "assistant",
				"content": content,
			})
			continue
		}

		// Convert tool result messages to Anthropic tool_result blocks.
		if msg.Role == "tool" {
			resultContent, _ := msg.Content.(string)
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     resultContent,
				}},
			})
			continue
		}

		anthropicMessages = append(anthropicMessages, map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}
	if len(anthropicMessages) == 0 {
		return nil, fmt.Errorf("anthropic: no user messages in request")
	}

	modelName := strings.TrimPrefix(provider.LiteLLMModel, "anthropic/")
	reqBody := map[string]any{
		"model":      modelName,
		"messages":   anthropicMessages,
		"max_tokens": 4096,
		"stream":     true,
	}
	if len(systemParts) > 0 {
		reqBody["system"] = strings.Join(systemParts, "\n\n")
	}

	// Convert OpenAI tool definitions to Anthropic format.
	if len(chatReq.Tools) > 0 {
		var anthropicTools []map[string]any
		for _, t := range chatReq.Tools {
			fn, _ := t["function"].(map[string]any)
			if fn == nil {
				continue
			}
			at := map[string]any{
				"name": fn["name"],
			}
			if desc, ok := fn["description"]; ok {
				at["description"] = desc
			}
			if params, ok := fn["parameters"]; ok {
				at["input_schema"] = params
			} else {
				at["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			anthropicTools = append(anthropicTools, at)
		}
		reqBody["tools"] = anthropicTools
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshalling anthropic request: %w", err)
	}

	resp, err := doWithRateLimitRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, respBody)
	}

	// Wrap the Anthropic SSE body in a translator that emits OpenAI-format SSE.
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		translateAnthropicSSE(resp.Body, pw)
		_ = resp.Body.Close()
	}()

	// Return a synthetic response with the translated body.
	translated := *resp
	translated.Body = pr
	return &translated, nil
}

// handleStreamingResponse proxies SSE chunks from the provider to the client,
// accumulating the full message for conversation history tracking.
func (r *Router) handleStreamingResponse(w http.ResponseWriter, req *http.Request, provider *ProviderConfig, resp *http.Response, chatReq ChatCompletionRequest) { //nolint:gocyclo

	defer func() { _ = resp.Body.Close() }()

	tokenStreamKey := "tokens:" + r.cfg.RunNamespace + ":" + r.cfg.RunName
	slog.Info("streaming response started", "provider", provider.Name, "tokenStreamKey", tokenStreamKey, "storeType", fmt.Sprintf("%T", r.store))

	// A recursive (tool-call-loop) invocation reuses the controller's ResponseWriter:
	// the outer call has already written the 200 + SSE headers. Re-calling
	// WriteHeader here is logged as "superfluous response.WriteHeader call" and, on
	// some servers, resets framing — so skip it for continuations.
	isContinuation := req.Context().Value(continuationKey{}) != nil

	flusher, ok := w.(http.Flusher)
	if !ok {
		if !isContinuation {
			slog.Error("ResponseWriter does not support flushing, falling back to buffered")
			body, _ := io.ReadAll(resp.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
		return
	}

	if !isContinuation {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	// Accumulate the full response for conversation history.
	var contentBuilder strings.Builder
	var toolCalls []ToolCall
	var usage TokenUsage
	var finishReason string

	// sawToolCall is set whenever ANY tool-call delta is observed on this turn,
	// independent of mergeToolCallDeltas' index-based merging. The terminal
	// guards — the canonical client [DONE] (below) and the run-level done sentinel
	// written to Redis — are keyed off sawToolCall rather than len(toolCalls)==0.
	// This decouples "is this a terminal text turn?" from the fragility of
	// mergeToolCallDeltas: if a proxy streams tool-call deltas with a missing or
	// colliding `index` field, the merge could undercount and make a tool-call turn
	// look terminal (len==0), which would emit [DONE] and the done sentinel
	// mid-loop and cause the UI to exit prematurely ("done sentinel too soon").
	// sawToolCall can only go true when real tool-call content was streamed, so it
	// is a faithful, merge-independent terminal detector.
	var sawToolCall bool

	scanner := bufio.NewScanner(resp.Body)
	// Allow large SSE lines (up to 1MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// We withhold EVERY upstream `data: [DONE]` and emit a single canonical
	// terminal [DONE] ourselves once the scan completes (see below). The OpenAI
	// streaming spec permits exactly one terminating [DONE]; some providers
	// (e.g. thinking-model proxies) emit a premature [DONE] right after the
	// reasoning block and then keep streaming content. Forwarding it verbatim
	// makes the OpenAI-SDK client close the stream early ("exit too soon") and
	// discard the trailing content. Withholding normalizes the stream to exactly
	// one terminal [DONE]. This also covers the tool-call case (the recursion
	// emits its own [DONE] at the true end of the loop), so the per-turn
	// suppression logic that used to live here is no longer needed.
	for scanner.Scan() {
		line := scanner.Text()

		// Parse "data: " lines first so we can decide whether to proxy them.
		if after, ok0 := strings.CutPrefix(line, "data: "); ok0 {
			data := after

			if data == "[DONE]" {
				// Withhold; a single canonical [DONE] is emitted after the scan.
				continue
			}

			var chunk streamingChatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				for _, choice := range chunk.Choices {
					if choice.Delta.Content != "" {
						contentBuilder.WriteString(choice.Delta.Content)
						// Broadcast to external subscribers (operator SSE stream).
						r.tokens.Send(choice.Delta.Content)
						// Record model-router token output for :9091 metrics.
						r.metrics.AddTokens(len(choice.Delta.Content))
						// Write to Redis Stream for UI token streaming.
						if r.store != nil {
							if err := r.store.SaveToken(req.Context(), tokenStreamKey, choice.Delta.Content); err != nil {
								slog.Warn("failed to save token to Redis stream", "key", tokenStreamKey, "err", err)
							}
						}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						sawToolCall = true
						toolCalls = mergeToolCallDeltas(toolCalls, choice.Delta.ToolCalls)
					}
					// Surface extended-thinking (model "reasoning") progressively as
					// `thought` trace events so it is captured in the UI (rendered as an
					// expandable thinking panel) and replayable on refresh, rather than
					// being silently dropped. This keeps the agentic chat flow faithful
					// to the provider's full output instead of terminating on a
					// content-less thinking block.
					if choice.Delta.Reasoning != "" {
						ev, _ := json.Marshal(map[string]string{
							"type":    "thought",
							"content": choice.Delta.Reasoning,
						})
						r.emitTraceEvent(string(ev))
					}
					if choice.FinishReason != nil {
						finishReason = *choice.FinishReason
					}
				}
				if chunk.Usage != nil {
					usage = *chunk.Usage
				}
			}
		}

		// Proxy every line (including empty lines for SSE framing) to the client,
		// except [DONE] which is handled above.
		_, _ = fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("error reading SSE stream", "err", err)
	}

	// If the run was cancelled, the OpenAI turn loop never saw a terminal finish
	// reason. Emit a `fail` trace event so TailTokens closes promptly (the poller
	// would also catch this, but the fail event gives an immediate, authoritative
	// signal). No magic done sentinel — completion follows the OpenAI schema's
	// [DONE] to the agent client; the UI relies on finalOutput (with output) not
	// a done event (without output).
	if r.cancelCtx.Err() != nil {
		slog.Info("run cancelled, emitting terminal fail event", "run", r.cfg.RunName)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		if r.store != nil {
			failEv, _ := json.Marshal(map[string]string{"type": "fail", "reason": "router cancel"})
			_ = r.store.SaveTraceEvent(context.Background(), tokenStreamKey, string(failEv))
		}
		return
	}

	// NOTE: the terminal `done` trace event is emitted below (after this clarify
	// safety-net check) only on the genuine terminal text turn. If _clarify is
	// auto-triggered, executeClarify emits its own `clarify` trace event — a terminal
	// event — and the run phase moves to WaitingForInput, so TailTokens closes via
	// that event (and/or the CRD terminal-state poller). No empty-token sentinel is
	// written here either.

	// Build the complete message for conversation history.
	assistantMsg := Message{
		Role:      "assistant",
		Content:   contentBuilder.String(),
		ToolCalls: toolCalls,
	}

	// Update conversation history.
	// For continuation requests (recursive tool-call loops), chatReq.Messages was
	// built from r.messages and would duplicate the entire conversation if re-appended.
	streamIsContinuation := req.Context().Value(continuationKey{}) != nil
	r.mu.Lock()
	if !streamIsContinuation {
		r.messages = append(r.messages, chatReq.Messages...)
	}
	r.messages = append(r.messages, assistantMsg)
	r.updateSpend(usage, provider)
	r.ruleRouter.IncrementTurn()
	// Fold the finished turn into priorMessages and cap the live buffer. Gated to the
	// final (non-tool) response so the tool-call loop (which appends toolResults and
	// recurses with r.messages) doesn't lose in-flight context.
	if len(toolCalls) == 0 {
		r.concludeTurn()
	}
	r.mu.Unlock()

	// Checkpoint if needed.
	if r.cfg.CheckpointEvery > 0 && r.ruleRouter.TurnCount()%r.cfg.CheckpointEvery == 0 {
		go r.checkpoint(context.Background())
	}

	// Safety net: if the LLM output a question as text instead of calling _clarify,
	// detect it and synthetically trigger _clarify. The SSE stream has already been
	// proxied to the agent, but we can still intercept here: trigger clarify, which
	// sets WaitingForInput on the run and causes the next agent request to get 410 Gone.
	if len(toolCalls) == 0 && (finishReason == "stop" || finishReason == "") { //nolint:goconst

		fullText := contentBuilder.String()
		syntheticResp := ChatCompletionResponse{
			Choices: []Choice{{
				Message:      Message{Role: "assistant", Content: fullText},
				FinishReason: "stop",
			}},
		}
		if r.shouldAutoTriggerClarify(syntheticResp) {
			slog.Info("auto-triggering _clarify from streaming response", "run", r.cfg.RunName)
			// Checkpoint conversation so the pod can resume with the answer.
			r.checkpoint(context.Background())
			// Notify the operator to set WaitingForInput — do NOT write to the
			// token stream here because the LLM's text tokens have already been
			// streamed. The UI API's Phase 3 polling will emit the clarify SSE event.
			if err := r.notifyOperatorClarify(fullText); err != nil {
				slog.Error("failed to notify operator for clarify", "err", err)
			}

			// Fall through to the done sentinel below so the UI SSE handler exits.
		}
	}

	// Emit a single canonical terminal `data: [DONE]` to the client at the true end
	// of a non-tool response. Upstream [DONE]s were withheld in the scan loop (above)
	// so a provider's premature [DONE] cannot close the client stream early. When tool
	// calls are present, this block is skipped and the recursion emits [DONE] itself.
	//
	// Keyed off sawToolCall (set the first time any tool-call delta is observed) so a
	// mergeToolCallDeltas undercount can never make a tool-call turn look terminal.
	if !sawToolCall {
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// Completion is signalled solely by the OpenAI schema's [DONE] to the agent
	// framework. No custom done trace event is emitted here — the UI relies on the
	// UI API's finalOutput event (which carries the accumulated output) rather than a
	// done event (which carried only finish_reason, no output). This prevents the
	// done/finalOutput race where done closed the SSE connection before finalOutput
	// arrived. The UI API's poller detects the terminal CRD phase and emits
	// finalOutput after TailTokens exits.

	// Handle tool calls if the LLM requested them.
	// NOTE: r.messages already contains chatReq.Messages + assistantMsg (added above),
	// so we only need to dispatch, append results, and recurse — not re-append the history.
	//
	// Dispatch on len(toolCalls) > 0 ALONE — the same predicate the non-streaming path
	// uses (HandleChatCompletions, router.go:879). Do NOT additionally require
	// finishReason == "tool_calls": OpenAI-compatible proxies (LiteLLM)
	// frequently stream finish_reason: null and signal completion solely via
	// `data: [DONE]`. Gating dispatch on finish_reason there desyncs the [DONE]
	// suppression from the dispatch — [DONE] is suppressed (tool calls present) but the
	// tool call is never dispatched, so it is silently dropped and the client never
	// receives a terminating [DONE]. Mirroring the non-streaming path fixes this.
	//
	// The terminal guards above key off sawToolCall (any tool-call delta observed),
	// which is true whenever len(toolCalls) > 0 after a successful merge — so the
	// dispatch predicate and the terminal gate stay in sync even if a future merge
	// change alters index handling.
	if len(toolCalls) > 0 {

		toolResults := make([]Message, len(toolCalls))
		for i, tc := range toolCalls {
			result := r.dispatchToolCall(req.Context(), tc)
			toolResults[i] = Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			}
		}
		r.mu.Lock()
		r.messages = append(r.messages, toolResults...)
		continueReq := ChatCompletionRequest{
			Model:    chatReq.Model,
			Messages: r.messages,
			Tools:    chatReq.Tools,
			Stream:   true,
		}
		r.mu.Unlock()
		// Recursive call with the full conversation as the new request body.
		newBody, _ := json.Marshal(continueReq)
		ctx := context.WithValue(req.Context(), continuationKey{}, true)
		newReq := req.Clone(ctx)
		newReq.Body = io.NopCloser(bytes.NewReader(newBody))
		newReq.ContentLength = int64(len(newBody))
		r.HandleChatCompletions(w, newReq)
		return
	}

	// Notify operator of context usage for UI display (streaming path).
	go r.notifyOperatorContext()
}

// tryFallbackStream attempts streaming fallback to providers in the fallback chain.
func (r *Router) tryFallbackStream(ctx context.Context, chatReq ChatCompletionRequest, failedProvider string) (*http.Response, *ProviderConfig, error) {
	for _, name := range r.cfg.FallbackChain {
		if name == failedProvider {
			continue
		}
		for i := range r.cfg.Providers {
			if r.cfg.Providers[i].Name == name {
				p := &r.cfg.Providers[i]
				slog.Info("trying fallback provider (streaming)", "provider", name)
				resp, err := r.forwardToProviderStream(ctx, p, chatReq)
				if err == nil {
					return resp, p, nil
				}
				slog.Warn("fallback provider failed (streaming)", "provider", name, "err", err)
			}
		}
	}
	return nil, nil, fmt.Errorf("all fallback providers exhausted")
}

// explicitlyTerminal reports whether an explicit terminal tool (_done/_fail/
// _handoff/_clarify) has already fired for this run. The auto terminal signal
// (emitted on the genuine terminal text turn via [DONE] to the agent framework)
// is suppressed when this is true,
// because the explicit path already emitted its own terminal trace event and the
// streaming recursion short-circuits to 410 on the next call.
func (r *Router) explicitlyTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doneExplicit || r.failedExplicit || r.handedOff || r.waitingForInput
}

// mergeToolCallDeltas merges incremental tool call deltas into a running list.
// OpenAI streams tool calls as incremental chunks identified by their Index field.
// Each parallel tool call has a unique index; chunks with the same index are merged.
func mergeToolCallDeltas(existing []ToolCall, deltas []ToolCall) []ToolCall {
	for _, d := range deltas {
		// Find existing entry by index.
		found := false
		for i := range existing {
			if existing[i].Index == d.Index {
				existing[i].Function.Arguments += d.Function.Arguments
				if d.ID != "" {
					existing[i].ID = d.ID
				}
				if d.Function.Name != "" {
					existing[i].Function.Name = d.Function.Name
				}
				if d.Type != "" {
					existing[i].Type = d.Type
				}
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, d)
		}
	}
	return existing
}

// translateAnthropicSSE reads Anthropic SSE events and writes OpenAI-format SSE events to w.
func translateAnthropicSSE(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var msgID string
	// Track the current content block index → tool call index mapping.
	// Anthropic uses content block index; OpenAI uses tool call index.
	blockIndexToToolIndex := map[int]int{}
	toolCallCount := 0

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "message_start":
			if msg, ok := event["message"].(map[string]any); ok {
				msgID, _ = msg["id"].(string)
			}
			// Emit initial role chunk.
			chunk := streamingChatCompletionChunk{
				ID:     msgID,
				Object: "chat.completion.chunk",
				Choices: []streamChoice{{
					Index: 0,
					Delta: streamDelta{Role: "assistant"},
				}},
			}
			writeSSEChunk(w, chunk)

		case "content_block_start":
			// Detect tool_use blocks and register their index mapping.
			blockIdx, _ := event["index"].(float64)
			if cb, ok := event["content_block"].(map[string]any); ok {
				if cbType, _ := cb["type"].(string); cbType == "tool_use" { //nolint:goconst

					toolIdx := toolCallCount
					toolCallCount++
					blockIndexToToolIndex[int(blockIdx)] = toolIdx
					toolID, _ := cb["id"].(string)
					toolName, _ := cb["name"].(string)
					toolIdxCopy := toolIdx
					chunk := streamingChatCompletionChunk{
						ID:     msgID,
						Object: "chat.completion.chunk",
						Choices: []streamChoice{{
							Index: 0,
							Delta: streamDelta{
								ToolCalls: []ToolCall{{
									Index: toolIdxCopy,
									ID:    toolID,
									Type:  "function",
									Function: FunctionCall{
										Name:      toolName,
										Arguments: "",
									},
								}},
							},
						}},
					}
					writeSSEChunk(w, chunk)
				}
			}

		case "content_block_delta":
			blockIdx, _ := event["index"].(float64)
			if delta, ok := event["delta"].(map[string]any); ok {
				deltaType, _ := delta["type"].(string)
				switch deltaType {
				case "text_delta":
					if text, ok := delta["text"].(string); ok {
						chunk := streamingChatCompletionChunk{
							ID:     msgID,
							Object: "chat.completion.chunk",
							Choices: []streamChoice{{
								Index: 0,
								Delta: streamDelta{Content: text},
							}},
						}
						writeSSEChunk(w, chunk)
					}
				case "input_json_delta":
					// Streaming tool call argument chunk.
					if partial, ok := delta["partial_json"].(string); ok {
						if toolIdx, mapped := blockIndexToToolIndex[int(blockIdx)]; mapped {
							toolIdxCopy := toolIdx
							chunk := streamingChatCompletionChunk{
								ID:     msgID,
								Object: "chat.completion.chunk",
								Choices: []streamChoice{{
									Index: 0,
									Delta: streamDelta{
										ToolCalls: []ToolCall{{
											Index: toolIdxCopy,
											Function: FunctionCall{
												Arguments: partial,
											},
										}},
									},
								}},
							}
							writeSSEChunk(w, chunk)
						}
					}
				}
			}

		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				stopReason, _ := delta["stop_reason"].(string)
				switch stopReason {
				case "end_turn": //nolint:goconst

					stopReason = "stop"
				case "tool_use":
					stopReason = "tool_calls"
				}
				chunk := streamingChatCompletionChunk{
					ID:     msgID,
					Object: "chat.completion.chunk",
					Choices: []streamChoice{{
						Index:        0,
						Delta:        streamDelta{},
						FinishReason: &stopReason,
					}},
				}
				// Include usage if present.
				if usage, ok := event["usage"].(map[string]any); ok {
					outputTokens, _ := usage["output_tokens"].(float64)
					chunk.Usage = &TokenUsage{
						CompletionTokens: int(outputTokens),
					}
				}
				writeSSEChunk(w, chunk)
			}

		case "message_stop":
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		}
	}
}

func writeSSEChunk(w io.Writer, chunk streamingChatCompletionChunk) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}
