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
	"os"
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
		resp.Body.Close()
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
			d := time.Duration(math.Ceil(secs)) * time.Second
			if d > 60*time.Second {
				d = 60 * time.Second
			}
			return d
		}
	}
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
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
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// forwardToProviderStream sends a streaming request to the provider and returns the
// raw *http.Response for SSE proxying. The caller is responsible for closing the body.
// Uses a detached context so the outgoing call survives agent-side disconnects.
func (r *Router) forwardToProviderStream(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest) (*http.Response, error) {
	// Use the run-scoped cancel context so the LLM call is detached from the
	// agent's HTTP connection (survives disconnects) but can still be cancelled
	// explicitly via Router.Cancel() when the run is stopped.
	llmCtx, cancel := context.WithTimeout(r.cancelCtx, llmRequestTimeout)

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
	// Keep full model identifier when custom BaseURL is set (e.g., Poolside proxy)
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
	apiKey, err := os.ReadFile(provider.APIKeyFile)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}

	key := strings.TrimSpace(string(apiKey))
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
		resp.Body.Close()
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
	apiKey, err := os.ReadFile(provider.APIKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}

	// Concatenate system messages (user prompt + builtin hints) rather than replacing.
	// Note: SystemPrompt is now always injected into the messages array in HandleChatCompletions,
	// so we don't need to add it from config separately.
	var systemParts []string
	var anthropicMessages []map[string]interface{}
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
			var content []map[string]interface{}
			if text, ok := msg.Content.(string); ok && text != "" {
				content = append(content, map[string]interface{}{"type": "text", "text": text})
			}
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				content = append(content, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": json.RawMessage(args),
				})
			}
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role":    "assistant",
				"content": content,
			})
			continue
		}

		// Convert tool result messages to Anthropic tool_result blocks.
		if msg.Role == "tool" {
			resultContent, _ := msg.Content.(string)
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     resultContent,
				}},
			})
			continue
		}

		anthropicMessages = append(anthropicMessages, map[string]interface{}{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}
	if len(anthropicMessages) == 0 {
		return nil, fmt.Errorf("anthropic: no user messages in request")
	}

	modelName := strings.TrimPrefix(provider.LiteLLMModel, "anthropic/")
	reqBody := map[string]interface{}{
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
		var anthropicTools []map[string]interface{}
		for _, t := range chatReq.Tools {
			fn, _ := t["function"].(map[string]interface{})
			if fn == nil {
				continue
			}
			at := map[string]interface{}{
				"name": fn["name"],
			}
			if desc, ok := fn["description"]; ok {
				at["description"] = desc
			}
			if params, ok := fn["parameters"]; ok {
				at["input_schema"] = params
			} else {
				at["input_schema"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			anthropicTools = append(anthropicTools, at)
		}
		reqBody["tools"] = anthropicTools
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshalling anthropic request: %w", err)
	}

	key := strings.TrimSpace(string(apiKey))
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
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, respBody)
	}

	// Wrap the Anthropic SSE body in a translator that emits OpenAI-format SSE.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		translateAnthropicSSE(resp.Body, pw)
		resp.Body.Close()
	}()

	// Return a synthetic response with the translated body.
	translated := *resp
	translated.Body = pr
	return &translated, nil
}

// handleStreamingResponse proxies SSE chunks from the provider to the client,
// accumulating the full message for conversation history tracking.
func (r *Router) handleStreamingResponse(w http.ResponseWriter, req *http.Request, provider *ProviderConfig, resp *http.Response, chatReq ChatCompletionRequest) {
	defer resp.Body.Close()

	tokenStreamKey := "tokens:" + r.cfg.RunNamespace + ":" + r.cfg.RunName
	slog.Info("streaming response started", "provider", provider.Name, "tokenStreamKey", tokenStreamKey, "storeType", fmt.Sprintf("%T", r.store))

	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("ResponseWriter does not support flushing, falling back to buffered")
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Accumulate the full response for conversation history.
	var contentBuilder strings.Builder
	var toolCalls []ToolCall
	var usage TokenUsage
	var finishReason string

	scanner := bufio.NewScanner(resp.Body)
	// Allow large SSE lines (up to 1MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// pendingToolCalls tracks whether we have accumulated tool calls that need
	// to be resolved before emitting [DONE] to the client.  We suppress the
	// upstream [DONE] and re-emit it only after the full tool-call loop finishes.
	var suppressDone bool

	for scanner.Scan() {
		line := scanner.Text()

		// Parse "data: " lines first so we can decide whether to proxy them.
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if data == "[DONE]" {
				// Suppress [DONE] if there are pending tool calls — the recursive
				// call will eventually emit its own [DONE] to close the stream.
				suppressDone = len(toolCalls) > 0
				if !suppressDone {
					fmt.Fprintf(w, "%s\n", line)
					flusher.Flush()
				}
				continue
			}

			var chunk streamingChatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				for _, choice := range chunk.Choices {
					if choice.Delta.Content != "" {
						contentBuilder.WriteString(choice.Delta.Content)
						// Broadcast to external subscribers (operator SSE stream).
						r.tokens.Send(choice.Delta.Content)
						// Write to Redis Stream for UI token streaming.
						if r.store != nil {
							if err := r.store.SaveToken(req.Context(), tokenStreamKey, choice.Delta.Content); err != nil {
								slog.Warn("failed to save token to Redis stream", "key", tokenStreamKey, "err", err)
							}
						}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolCalls = mergeToolCallDeltas(toolCalls, choice.Delta.ToolCalls)
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
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("error reading SSE stream", "err", err)
	}

	// If the run was cancelled, write the done sentinel so UI's TailTokens stops
	// blocking, then return early — no point checkpointing or continuing.
	if r.cancelCtx.Err() != nil {
		slog.Info("run cancelled, writing done sentinel", "run", r.cfg.RunName)
		if r.store != nil {
			_ = r.store.SaveToken(context.Background(), tokenStreamKey, "")
		}
		return
	}

	// NOTE: Done sentinel is deferred until after the clarify safety net check.
	// If we trigger _clarify, executeClarify sends its own clarify event + done sentinel.

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
	r.mu.Unlock()



	// Checkpoint if needed.
	if r.cfg.CheckpointEvery > 0 && r.ruleRouter.TurnCount()%r.cfg.CheckpointEvery == 0 {
		go r.checkpoint(context.Background())
	}

	// Safety net: if the LLM output a question as text instead of calling _clarify,
	// detect it and synthetically trigger _clarify. The SSE stream has already been
	// proxied to the agent, but we can still intercept here: trigger clarify, which
	// sets WaitingForInput on the run and causes the next agent request to get 410 Gone.
	if len(toolCalls) == 0 && (finishReason == "stop" || finishReason == "") {
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

	// Signal end-of-stream to Redis so the UI handler stops blocking.
	// Deferred to here so the clarify safety net can send its own events first.
	if r.store != nil && len(toolCalls) == 0 {
		slog.Info("sending done sentinel to Redis token stream", "key", tokenStreamKey)
		sentinelCtx, sentinelCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.store.SaveToken(sentinelCtx, tokenStreamKey, ""); err != nil {
			slog.Warn("failed to write done sentinel to Redis stream", "key", tokenStreamKey, "err", err)
		}
		sentinelCancel()
	}

	// Handle tool calls if the LLM requested them.
	// NOTE: r.messages already contains chatReq.Messages + assistantMsg (added above),
	// so we only need to dispatch, append results, and recurse — not re-append the history.
	if len(toolCalls) > 0 && finishReason == "tool_calls" {
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

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "message_start":
			if msg, ok := event["message"].(map[string]interface{}); ok {
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
			if cb, ok := event["content_block"].(map[string]interface{}); ok {
				if cbType, _ := cb["type"].(string); cbType == "tool_use" {
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
			if delta, ok := event["delta"].(map[string]interface{}); ok {
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
			if delta, ok := event["delta"].(map[string]interface{}); ok {
				stopReason, _ := delta["stop_reason"].(string)
				if stopReason == "end_turn" {
					stopReason = "stop"
				} else if stopReason == "tool_use" {
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
				if usage, ok := event["usage"].(map[string]interface{}); ok {
					outputTokens, _ := usage["output_tokens"].(float64)
					chunk.Usage = &TokenUsage{
						CompletionTokens: int(outputTokens),
					}
				}
				writeSSEChunk(w, chunk)
			}

		case "message_stop":
			fmt.Fprintf(w, "data: [DONE]\n\n")
		}
	}
}

func writeSSEChunk(w io.Writer, chunk streamingChatCompletionChunk) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}
