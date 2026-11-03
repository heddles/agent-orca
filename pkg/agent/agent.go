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

// Package agent provides a Go SDK for writing agents that run on agent-orca.
//
// The SDK talks to the run's model-router (OPENAI_BASE_URL). The router keeps
// the conversation, injects the Agent's system prompt, and runs the Agent's
// tools and the built-in lifecycle tools (_done, _fail, _clarify, _handoff,
// _spawn, ...). Tools registered with [Agent.Tool] run in the agent process
// instead, e.g. a shell inside the agent's sandbox; they need
// spec.runtime.localTools on the Agent (docs/agent-run-tools.md).
//
// Example::
//
//	a := agent.New()
//	a.Tool("search", "Search the web", params, func(args map[string]any) (string, error) {
//	    return "results", nil
//	})
//	result, err := a.Run(context.Background(), "Find the latest news about LLMs")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Println(result.Phase, result.Output)
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// Run phases, matching the AgentRun phases they lead to.
const (
	PhaseSucceeded       = "Succeeded"
	PhaseFailed          = "Failed"
	PhaseHandedOff       = "HandedOff"
	PhaseWaitingForInput = "WaitingForInput"
)

// RunResult is the final result of an agent run.
type RunResult struct {
	// Output is the model's last text reply.
	Output string
	// Phase is one of the Phase* constants.
	Phase string
	// FailureReason explains a Failed phase.
	FailureReason string
}

// ToolHandler is the function signature for a custom tool handler.
// It receives the parsed arguments as a map and returns a string result.
type ToolHandler func(args map[string]any) (string, error)

// ToolSpec describes a tool callable by the LLM.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
	Handler     ToolHandler
}

// toOpenAI converts the ToolSpec to an OpenAI-compatible function definition.
func (t *ToolSpec) toOpenAI() map[string]any {
	params := t.Parameters
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  params,
		},
	}
}

// Agent is the Go SDK for writing agents that run on agent-orca.
type Agent struct {
	model        string
	baseURL      string
	apiKey       string
	systemPrompt string
	temperature  *float64
	maxTokens    *int
	httpClient   *http.Client
	tools        map[string]*ToolSpec
	toolOrder    []string
	onToolCall   func(ToolEvent)
}

// ToolEvent describes one tool call the agent ran.
type ToolEvent struct {
	Name      string
	Arguments string
	Result    string
	Duration  time.Duration
}

// WithToolObserver calls fn after each tool call the agent runs, e.g. to log
// progress.
func WithToolObserver(fn func(ToolEvent)) Option {
	return func(a *Agent) { a.onToolCall = fn }
}

// Option configures an Agent.
type Option func(*Agent)

// WithModel sets the model identifier sent with each request. The router
// chooses the provider, so this is usually left unset.
func WithModel(model string) Option {
	return func(a *Agent) { a.model = model }
}

// WithBaseURL overrides the model-router endpoint URL (default
// $OPENAI_BASE_URL, else http://localhost:8080/v1).
func WithBaseURL(url string) Option {
	return func(a *Agent) { a.baseURL = url }
}

// WithAPIKey sets the bearer token for the router (default $OPENAI_API_KEY).
func WithAPIKey(key string) Option {
	return func(a *Agent) { a.apiKey = key }
}

// WithSystemPrompt sets a system instruction sent with the first request, in
// addition to the Agent's spec.systemPrompt, which the router injects.
func WithSystemPrompt(prompt string) Option {
	return func(a *Agent) { a.systemPrompt = prompt }
}

// WithTemperature sets the sampling temperature.
func WithTemperature(t float64) Option {
	return func(a *Agent) { a.temperature = &t }
}

// WithMaxTokens sets the maximum tokens to generate per turn.
func WithMaxTokens(n int) Option {
	return func(a *Agent) { a.maxTokens = &n }
}

// WithHTTPClient sets a custom HTTP client. A turn streams for as long as the
// router's tool loop runs, so avoid a client-wide Timeout; use the context.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Agent) { a.httpClient = c }
}

// defaultBaseURL is the router address when neither an option nor
// OPENAI_BASE_URL sets one.
const defaultBaseURL = "http://localhost:8080/v1"

// New creates a new Agent with the given options.
func New(opts ...Option) *Agent {
	a := &Agent{
		httpClient: &http.Client{},
		tools:      make(map[string]*ToolSpec),
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.baseURL == "" {
		a.baseURL = os.Getenv("OPENAI_BASE_URL")
	}
	if a.baseURL == "" {
		a.baseURL = defaultBaseURL
	}
	a.baseURL = strings.TrimSuffix(a.baseURL, "/")
	if a.apiKey == "" {
		a.apiKey = os.Getenv("OPENAI_API_KEY")
	}
	return a
}

// Tool registers a tool the agent process runs. Its name must not start with
// "_" or match a router tool; the router rejects such requests.
func (a *Agent) Tool(name, description string, params map[string]any, handler ToolHandler) {
	if _, exists := a.tools[name]; !exists {
		a.toolOrder = append(a.toolOrder, name)
	}
	a.tools[name] = &ToolSpec{
		Name:        name,
		Description: description,
		Parameters:  params,
		Handler:     handler,
	}
}

// RunOption configures a Run call.
type RunOption func(*runConfig)

// runConfig holds options for a Run call.
type runConfig struct {
	maxTurns       int
	continuePrompt string
}

// WithMaxTurns sets the maximum number of requests to the router (default 200).
func WithMaxTurns(n int) RunOption {
	return func(c *runConfig) { c.maxTurns = n }
}

// WithContinuePrompt keeps the run going when the model replies with text
// instead of a tool call: prompt is sent as the next user message. The run then
// ends only when the model calls a lifecycle tool (_done, _fail, ...), which
// the router reports by closing the run. Without it, a text reply ends the run.
func WithContinuePrompt(prompt string) RunOption {
	return func(c *runConfig) { c.continuePrompt = prompt }
}

// ErrMaxTurns is returned in RunResult.FailureReason when the turn limit is hit.
var ErrMaxTurns = errors.New("max turns exceeded")

// Run executes the agent on the given input until the model ends the turn
// with text (see WithContinuePrompt) or the router closes the run.
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	cfg := &runConfig{maxTurns: 200}
	for _, opt := range opts {
		opt(cfg)
	}

	a.waitForRouter(ctx)

	// The router stores the conversation, so each request carries only what is
	// new: the input first, then tool results or a continue prompt.
	var pending []message
	if a.systemPrompt != "" {
		pending = append(pending, message{Role: "system", Content: a.systemPrompt})
	}
	pending = append(pending, message{Role: "user", Content: input})

	for turn := 0; turn < cfg.maxTurns; turn++ {
		reply, closed, err := a.send(ctx, pending)
		if err != nil {
			return nil, fmt.Errorf("turn %d: %w", turn, err)
		}
		if closed != nil {
			return closed, nil
		}
		if len(reply.toolCalls) == 0 {
			if cfg.continuePrompt == "" {
				return &RunResult{Phase: PhaseSucceeded, Output: reply.content}, nil
			}
			pending = []message{{Role: "user", Content: cfg.continuePrompt}}
			continue
		}
		pending = pending[:0]
		for _, tc := range reply.toolCalls {
			start := time.Now()
			result := a.runTool(tc)
			if a.onToolCall != nil {
				a.onToolCall(ToolEvent{
					Name: tc.Function.Name, Arguments: tc.Function.Arguments,
					Result: result, Duration: time.Since(start),
				})
			}
			pending = append(pending, message{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}
	return &RunResult{Phase: PhaseFailed, FailureReason: ErrMaxTurns.Error()}, nil
}

// runTool runs one call and returns its result for the model. Errors are
// results too: the router requires an answer for every call.
func (a *Agent) runTool(tc toolCall) string {
	tool, ok := a.tools[tc.Function.Name]
	if !ok || tool.Handler == nil {
		return fmt.Sprintf(`{"error": "unknown tool: %s"}`, tc.Function.Name)
	}
	args := map[string]any{}
	if strings.TrimSpace(tc.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf(`{"error": %q}`, "invalid JSON arguments: "+err.Error())
		}
	}
	result, err := tool.Handler(args)
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return result
}

// message is a chat message sent to the router.
type message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// toolCall is a tool call the router handed to the agent.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    int    `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// reply is one streamed turn: its text and the tool calls for the agent.
type reply struct {
	content      string
	toolCalls    []toolCall
	finishReason string
}

// send posts one request and reads the streamed reply. A non-nil RunResult
// means the router has closed the run (HTTP 410).
func (a *Agent) send(ctx context.Context, msgs []message) (*reply, *RunResult, error) {
	payload := map[string]any{"messages": msgs, "stream": true}
	if a.model != "" {
		payload["model"] = a.model
	}
	if a.temperature != nil {
		payload["temperature"] = *a.temperature
	}
	if a.maxTokens != nil {
		payload["max_tokens"] = *a.maxTokens
	}
	if len(a.toolOrder) > 0 {
		tools := make([]map[string]any, 0, len(a.toolOrder))
		for _, name := range a.toolOrder {
			tools = append(tools, a.tools[name].toOpenAI())
		}
		payload["tools"] = tools
	}
	resp, err := a.post(ctx, payload)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusGone:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, closedRun(strings.TrimSpace(string(body))), nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("model-router returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	r, err := readStream(resp.Body, nil)
	return r, nil, err
}

func (a *Agent) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// The router answers 409 while it is still finishing the previous turn,
	// which a client sending its next request on [DONE] can beat it to. Retry
	// briefly: that is what the router's 409 asks the client to do.
	delay := conflictRetryDelay
	connectDelay := connectRetryDelay
	started := time.Now()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		if a.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+a.apiKey)
		}
		resp, err := a.httpClient.Do(req)
		if err != nil && connectRetryable(err) && time.Since(started) < connectRetryWindow {
			// The router is not reachable yet. A sandboxed agent can start before
			// the network policy that admits its router is programmed, and its
			// first request then fails at once; the policy lands a few seconds on.
			select {
			case <-time.After(connectDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			connectDelay = min(connectDelay*2, 4*time.Second)
			attempt--
			continue
		}
		if err != nil || resp.StatusCode != http.StatusConflict || attempt == conflictRetries {
			return resp, err
		}
		_ = resp.Body.Close()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		delay = min(delay*2, time.Second)
	}
}

// waitForRouter probes the router until it answers. A sandboxed agent can start
// before the network path to its router is open, and its supervisor may then
// hold the first request without ever answering it, which a client with no
// timeout waits on forever. A short per-attempt timeout turns that into a retry.
// Giving up is not an error: the first real request reports what is wrong.
func (a *Agent) waitForRouter(ctx context.Context) {
	started := time.Now()
	delay := connectRetryDelay
	for time.Since(started) < connectRetryWindow && ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, a.baseURL+"/models", nil)
		if err != nil {
			cancel()
			return
		}
		if a.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+a.apiKey)
		}
		resp, err := a.httpClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, 4*time.Second)
	}
}

// connectRetryable reports whether err means the request never reached the
// router: the name did not resolve, the connection was refused or reset, or it
// closed before any response.
func connectRetryable(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || errors.As(err, &op) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

// connectRetryWindow and connectRetryDelay bound how long post waits for an
// unreachable router before giving up.
var (
	connectRetryWindow = 90 * time.Second
	probeTimeout       = 5 * time.Second
	connectRetryDelay  = 500 * time.Millisecond
)

// conflictRetries and conflictRetryDelay bound how long post waits for the
// router to finish the previous turn (about 2.5 s in all).
var (
	conflictRetries    = 8
	conflictRetryDelay = 25 * time.Millisecond
)

// closedRun maps the router's 410 reasons (HandleChatCompletions) to a result.
func closedRun(reason string) *RunResult {
	switch {
	case strings.Contains(reason, "completed by agent"):
		return &RunResult{Phase: PhaseSucceeded}
	case strings.Contains(reason, "handed off"):
		return &RunResult{Phase: PhaseHandedOff}
	case strings.Contains(reason, "waiting for human input"):
		return &RunResult{Phase: PhaseWaitingForInput}
	default:
		return &RunResult{Phase: PhaseFailed, FailureReason: reason}
	}
}

// readStream assembles an OpenAI SSE stream: text deltas, tool-call deltas
// merged by index, and the finish reason. onContent, when set, receives each
// text delta as it arrives.
func readStream(body io.Reader, onContent func(string)) (*reply, error) {
	var r reply
	var content strings.Builder
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string     `json:"content"`
					ToolCalls []toolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
				if onContent != nil {
					onContent(c.Delta.Content)
				}
			}
			for _, d := range c.Delta.ToolCalls {
				for len(r.toolCalls) <= d.Index {
					r.toolCalls = append(r.toolCalls, toolCall{})
				}
				tc := &r.toolCalls[d.Index]
				if d.ID != "" {
					tc.ID = d.ID
				}
				if d.Type != "" {
					tc.Type = d.Type
				}
				tc.Function.Name += d.Function.Name
				tc.Function.Arguments += d.Function.Arguments
			}
			if c.FinishReason != nil {
				r.finishReason = *c.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading stream: %w", err)
	}
	r.content = content.String()
	return &r, nil
}

// RunStream sends input as a single turn and streams the reply's text. It does
// not run tools; use Run for that.
func (a *Agent) RunStream(ctx context.Context, input string) (<-chan string, <-chan error) {
	contentChan := make(chan string)
	errChan := make(chan error, 1)

	go func() {
		defer close(contentChan)
		defer close(errChan)

		msgs := []message{{Role: "user", Content: input}}
		if a.systemPrompt != "" {
			msgs = append([]message{{Role: "system", Content: a.systemPrompt}}, msgs...)
		}
		payload := map[string]any{"messages": msgs, "stream": true}
		if a.model != "" {
			payload["model"] = a.model
		}
		resp, err := a.post(ctx, payload)
		if err != nil {
			errChan <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			errChan <- fmt.Errorf("model-router returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return
		}
		if _, err := readStream(resp.Body, func(s string) {
			select {
			case contentChan <- s:
			case <-ctx.Done():
			}
		}); err != nil {
			errChan <- err
		}
	}()

	return contentChan, errChan
}

// Input returns the run's input (AGENTORC_INPUT). In a sandbox, a multi-line
// input arrives base64-encoded in AGENTORC_INPUT_B64, because the sandbox
// gateway accepts no line breaks in environment values, while AGENTORC_INPUT
// holds a one-line version; Input prefers the exact one.
func Input() string {
	if enc := os.Getenv("AGENTORC_INPUT_B64"); enc != "" {
		if b, err := base64.StdEncoding.DecodeString(enc); err == nil {
			return string(b)
		}
	}
	return os.Getenv("AGENTORC_INPUT")
}
