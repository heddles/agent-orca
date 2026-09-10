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
// The SDK wraps an OpenAI-compatible client pointed at the model-router sidecar
// (http://localhost:8080 by default) and provides Go-idiomatic helpers for the
// built-in lifecycle tools (_done, _fail, _clarify, _handoff, _spawn).
//
// Example::
//
//	agent := agent.New(agent.WithSystemPrompt("You are a helpful assistant."))
//
//	agent.Tool("search", "Search the web", func(args map[string]any) (string, error) {
//	    return "results", nil
//	})
//
//	result, err := agent.Run(context.Background(), "Find the latest news about LLMs")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Println(result.Output)
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RunResult is the final result of an agent run.
type RunResult struct {
	Output        string
	Phase         string
	SpendUSD      string
	FailureReason string
	CompletedAt   string
	Raw           map[string]any
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
func (t *ToolSpec) toOpenAI() map[string]any { //nolint:unused

	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		},
	}
}

// Agent is the Go SDK for writing agents that run on agent-orca.
// It wraps an OpenAI-compatible client pointed at the model-router sidecar
// and provides built-in lifecycle tool helpers.
type Agent struct {
	model         string
	baseURL       string
	apiKey        string
	systemPrompt  string
	temperature   float64
	maxTokens     *int
	httpClient    *http.Client
	tools         map[string]*ToolSpec
	doneCalled    bool
	failCalled    bool
	clarifyCalled bool
	handoffCalled bool
	spawnResult   map[string]any
}

// Option configures an Agent.
type Option func(*Agent)

// WithModel sets the default model identifier.
func WithModel(model string) Option {
	return func(a *Agent) { a.model = model }
}

// WithBaseURL overrides the model-router endpoint URL.
func WithBaseURL(url string) Option {
	return func(a *Agent) { a.baseURL = url }
}

// WithAPIKey sets the API key for the OpenAI-compatible endpoint.
func WithAPIKey(key string) Option {
	return func(a *Agent) { a.apiKey = key }
}

// WithSystemPrompt sets the system instruction.
func WithSystemPrompt(prompt string) Option {
	return func(a *Agent) { a.systemPrompt = prompt }
}

// WithTemperature sets the sampling temperature.
func WithTemperature(t float64) Option {
	return func(a *Agent) { a.temperature = t }
}

// WithMaxTokens sets the maximum tokens to generate per turn.
func WithMaxTokens(n int) Option {
	return func(a *Agent) { a.maxTokens = &n }
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Agent) { a.httpClient = c }
}

// New creates a new Agent with the given options.
// Defaults to http://localhost:8080 (the model-router sidecar).
func New(opts ...Option) *Agent {
	a := &Agent{
		baseURL:     "http://localhost:8080",
		temperature: 0.7,
		httpClient:  &http.Client{Timeout: 120 * time.Second},
		tools:       make(map[string]*ToolSpec),
	}
	for _, opt := range opts {
		opt(a)
	}
	// Allow env var override.
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" && a.baseURL == "http://localhost:8080" {
		a.baseURL = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" && a.apiKey == "" {
		a.apiKey = v
	}
	a.registerBuiltinTools()
	return a
}

// registerBuiltinTools registers the built-in lifecycle tools.
func (a *Agent) registerBuiltinTools() {
	a.tools["_done"] = &ToolSpec{
		Name:        "_done",
		Description: "Signal that the task is complete. Use this when you have a final answer or output to return.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"output": map[string]any{"type": "string", "description": "The final output/answer."},
			},
			"required": []string{"output"},
		},
		Handler: a.doneHandler,
	}
	a.tools["_fail"] = &ToolSpec{
		Name:        "_fail",
		Description: "Signal an unrecoverable failure with an explanation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason":    map[string]any{"type": "string", "description": "Why the task failed."},
				"retryable": map[string]any{"type": "boolean", "description": "Whether the failure is retryable."},
			},
			"required": []string{"reason"},
		},
		Handler: a.failHandler,
	}
	a.tools["_clarify"] = &ToolSpec{
		Name:        "_clarify",
		Description: "Ask the user for clarification on a specific point.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question to ask the user."},
			},
			"required": []string{"question"},
		},
		Handler: a.askHandler,
	}
	a.tools["_handoff"] = &ToolSpec{
		Name:        "_handoff",
		Description: "Hand off the task to another agent.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agentRef": map[string]any{"type": "string", "description": "Name of the agent to hand off to."},
				"input":    map[string]any{"type": "string", "description": "Input to pass to the handed-off agent."},
			},
			"required": []string{"agentRef", "input"},
		},
		Handler: a.handoffHandler,
	}
	a.tools["_spawn"] = &ToolSpec{
		Name:        "_spawn",
		Description: "Spawn a child agent to work on a sub-task. The parent waits for completion.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agentRef":       map[string]any{"type": "string", "description": "Name of the child agent."},
				"input":          map[string]any{"type": "string", "description": "Input for the child agent."},
				"timeoutSeconds": map[string]any{"type": "integer", "description": "Timeout in seconds (default 300)."},
			},
			"required": []string{"agentRef", "input"},
		},
		Handler: a.spawnHandler,
	}
}

// Tool registers a custom tool that the LLM can call.
func (a *Agent) Tool(name, description string, params map[string]any, handler ToolHandler) {
	a.tools[name] = &ToolSpec{
		Name:        name,
		Description: description,
		Parameters:  params,
		Handler:     handler,
	}
}

// Done signals that the task is complete.
func (a *Agent) Done(output string) map[string]any {
	a.doneCalled = true
	return map[string]any{"status": "done", "output": output}
}

// Fail signals an unrecoverable failure.
func (a *Agent) Fail(reason string, retryable bool) map[string]any {
	a.failCalled = true
	return map[string]any{"status": "failed", "reason": reason, "retryable": retryable}
}

// Ask asks the user for clarification.
func (a *Agent) Ask(question string) map[string]any {
	a.clarifyCalled = true
	return map[string]any{"status": "clarifying", "question": question}
}

// Handoff hands off the task to another agent.
func (a *Agent) Handoff(agentRef, input string) map[string]any {
	a.handoffCalled = true
	return map[string]any{"status": "handed_off", "agent": agentRef, "input": input}
}

// Spawn spawns a child agent.
func (a *Agent) Spawn(agentRef, input string, timeoutSeconds int) map[string]any {
	a.spawnResult = map[string]any{
		"agentRef":       agentRef,
		"input":          input,
		"timeoutSeconds": timeoutSeconds,
	}
	return map[string]any{"status": "spawning", "agent": agentRef}
}

// --- Internal tool handlers (called by the LLM via tool execution) ---

func (a *Agent) doneHandler(args map[string]any) (string, error) {
	output, _ := args["output"].(string)
	a.Done(output)
	return `{"status": "done"}`, nil
}

func (a *Agent) failHandler(args map[string]any) (string, error) {
	reason, _ := args["reason"].(string)
	retryable, _ := args["retryable"].(bool)
	a.Fail(reason, retryable)
	return `{"status": "failed"}`, nil
}

func (a *Agent) askHandler(args map[string]any) (string, error) {
	question, _ := args["question"].(string)
	a.Ask(question)
	return `{"status": "clarifying"}`, nil
}

func (a *Agent) handoffHandler(args map[string]any) (string, error) {
	agentRef, _ := args["agentRef"].(string)
	input, _ := args["input"].(string)
	a.Handoff(agentRef, input)
	return `{"status": "handed_off"}`, nil
}

func (a *Agent) spawnHandler(args map[string]any) (string, error) {
	agentRef, _ := args["agentRef"].(string)
	input, _ := args["input"].(string)
	timeout, _ := args["timeoutSeconds"].(float64)
	a.Spawn(agentRef, input, int(timeout))
	return `{"status": "spawning"}`, nil
}

// LoadCheckpoint loads conversation history from the checkpoint store.
// This is a stub — in production, the model-router handles checkpoint
// loading via PriorRunRef.
func (a *Agent) LoadCheckpoint(sessionID string) []map[string]string {
	return nil
}

// SaveCheckpoint saves conversation history to the checkpoint store.
// This is a stub — in production, the model-router handles checkpointing
// automatically every N turns.
func (a *Agent) SaveCheckpoint(sessionID string, messages []map[string]string) string {
	return fmt.Sprintf("agentorca/runs/%s/state", sessionID)
}

// Run executes the agent on the given input until a terminal state.
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	cfg := &runConfig{
		maxTurns: 50,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	conversation := make([]map[string]any, 0)
	if a.systemPrompt != "" {
		conversation = append(conversation, map[string]any{"role": "system", "content": a.systemPrompt})
	}
	conversation = append(conversation, map[string]any{"role": "user", "content": input})

	for turn := 0; turn < cfg.maxTurns; turn++ {
		resp, err := a.chatCompletion(ctx, conversation)
		if err != nil {
			return nil, fmt.Errorf("chat completion (turn %d): %w", turn, err)
		}
		if resp == nil {
			break
		}

		// Check for tool calls.
		choices, ok := resp["choices"].([]any)
		if !ok || len(choices) == 0 {
			break
		}
		msg, ok := choices[0].(map[string]any)["message"].(map[string]any)
		if !ok {
			break
		}

		toolCalls, ok := msg["tool_calls"].([]any)
		if !ok || len(toolCalls) == 0 {
			// No tool calls — add the assistant's text response.
			conversation = append(conversation, msg)
			continue
		}

		// Execute tool calls.
		assistantMsg := map[string]any{
			"role":       "assistant",
			"tool_calls": toolCalls,
		}
		if content, ok := msg["content"].(string); ok && content != "" {
			assistantMsg["content"] = content
		}
		conversation = append(conversation, assistantMsg)

		for _, tc := range toolCalls {
			tcMap, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tcMap["function"].(map[string]any)
			if !ok {
				continue
			}
			toolName, _ := fn["name"].(string)
			argsStr, _ := fn["arguments"].(string)

			var args map[string]any
			if argsStr != "" {
				if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
					args = make(map[string]any)
				}
			}

			tool, exists := a.tools[toolName]
			var toolResult string
			if exists && tool.Handler != nil {
				result, err := tool.Handler(args)
				if err != nil {
					toolResult = fmt.Sprintf(`{"error": %q}`, err.Error())
				} else {
					toolResult = result
				}
			} else {
				toolResult = fmt.Sprintf(`{"error": "unknown tool: %s"}`, toolName)
			}

			id, _ := tcMap["id"].(string)
			conversation = append(conversation, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"name":         toolName,
				"content":      toolResult,
			})
		}

		// Check terminal states.
		if a.doneCalled {
			return &RunResult{Phase: "Succeeded", Output: a.extractDoneOutput(conversation)}, nil
		}
		if a.failCalled {
			return &RunResult{Phase: "Failed", FailureReason: a.extractFailReason(conversation)}, nil
		}
		if a.handoffCalled {
			return &RunResult{Phase: "HandedOff"}, nil
		}
		if a.clarifyCalled {
			return &RunResult{Phase: "WaitingForInput"}, nil
		}
	}

	return &RunResult{Phase: "Failed", FailureReason: "max turns exceeded"}, nil
}

// chatCompletion sends a chat completion request to the model-router.
func (a *Agent) chatCompletion(ctx context.Context, messages []map[string]any) (map[string]any, error) {
	body := map[string]any{
		"messages":    messages,
		"temperature": a.temperature,
	}
	if a.model != "" {
		body["model"] = a.model
	}
	if a.maxTokens != nil {
		body["max_tokens"] = *a.maxTokens
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return result, nil
}

// extractDoneOutput finds the most recent _done tool call and returns its output.
func (a *Agent) extractDoneOutput(conversation []map[string]any) string {
	for i := len(conversation) - 1; i >= 0; i-- {
		msg := conversation[i]
		if msg["role"] != "assistant" {
			continue
		}
		toolCalls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tc := range toolCalls {
			tcMap, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tcMap["function"].(map[string]any)
			if !ok {
				continue
			}
			if name, _ := fn["name"].(string); name == "_done" {
				if argsStr, ok := fn["arguments"].(string); ok && argsStr != "" {
					var args map[string]any
					if json.Unmarshal([]byte(argsStr), &args) == nil {
						if output, ok := args["output"].(string); ok && output != "" {
							return output
						}
						if summary, ok := args["summary"].(string); ok && summary != "" {
							return summary
						}
					}
				}
			}
		}
	}
	return ""
}

// extractFailReason finds the most recent _fail tool call and returns its reason.
func (a *Agent) extractFailReason(conversation []map[string]any) string {
	for i := len(conversation) - 1; i >= 0; i-- {
		msg := conversation[i]
		if msg["role"] != "assistant" {
			continue
		}
		toolCalls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tc := range toolCalls {
			tcMap, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tcMap["function"].(map[string]any)
			if !ok {
				continue
			}
			if name, _ := fn["name"].(string); name == "_fail" {
				if argsStr, ok := fn["arguments"].(string); ok && argsStr != "" {
					var args map[string]any
					if json.Unmarshal([]byte(argsStr), &args) == nil {
						if reason, ok := args["reason"].(string); ok {
							return reason
						}
					}
				}
			}
		}
	}
	return ""
}

// RunOption configures a Run call.
type RunOption func(*runConfig)

// runConfig holds options for a Run call.
type runConfig struct {
	maxTurns int
}

// WithMaxTurns sets the maximum number of LLM turns.
func WithMaxTurns(n int) RunOption {
	return func(c *runConfig) { c.maxTurns = n }
}

// ErrNoTool is returned when a tool is not found.
var ErrNoTool = errors.New("tool not found")
