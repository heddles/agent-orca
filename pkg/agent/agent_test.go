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

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewDefaults(t *testing.T) {
	a := New()
	if a.baseURL != "http://localhost:8080" {
		t.Fatalf("expected default baseURL, got %s", a.baseURL)
	}
	if a.temperature != 0.7 {
		t.Fatalf("expected default temperature 0.7, got %v", a.temperature)
	}
	if len(a.tools) == 0 {
		t.Fatal("expected builtin tools to be registered")
	}
	for _, name := range []string{"_done", "_fail", "_clarify", "_handoff", "_spawn"} {
		if _, ok := a.tools[name]; !ok {
			t.Fatalf("expected builtin tool %s to be registered", name)
		}
	}
}

func TestNewWithOptions(t *testing.T) {
	a := New(WithModel("gpt-4"), WithSystemPrompt("You are helpful."),
		WithTemperature(0.1), WithBaseURL("http://router:8080"), WithAPIKey("secret"))
	if a.model != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %s", a.model)
	}
	if a.systemPrompt != "You are helpful." {
		t.Fatalf("expected system prompt, got %s", a.systemPrompt)
	}
	if a.temperature != 0.1 {
		t.Fatalf("expected temperature 0.1, got %v", a.temperature)
	}
	if a.baseURL != "http://router:8080" {
		t.Fatalf("expected base URL, got %s", a.baseURL)
	}
	if a.apiKey != "secret" {
		t.Fatalf("expected API key, got %s", a.apiKey)
	}
}

func TestToolRegistration(t *testing.T) {
	a := New()
	a.Tool("search", "Search the web", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"required": []string{"query"},
	}, func(args map[string]any) (string, error) {
		return "results", nil
	})
	tool, ok := a.tools["search"]
	if !ok {
		t.Fatal("expected search tool to be registered")
	}
	if tool.Name != "search" {
		t.Fatalf("expected name 'search', got %s", tool.Name)
	}
	if tool.Description != "Search the web" {
		t.Fatalf("expected description, got %s", tool.Description)
	}
}

func TestDoneFailAskHandoffSpawn(t *testing.T) {
	a := New()

	a.Done("final answer")
	if !a.doneCalled {
		t.Fatal("expected doneCalled to be true")
	}

	a.Fail("bad input", true)
	if !a.failCalled {
		t.Fatal("expected failCalled to be true")
	}

	a.Ask("What do you mean?")
	if !a.clarifyCalled {
		t.Fatal("expected clarifyCalled to be true")
	}

	a.Handoff("other-agent", "take over")
	if !a.handoffCalled {
		t.Fatal("expected handoffCalled to be true")
	}

	a.Spawn("child", "do subtask", 120)
	if a.spawnResult == nil {
		t.Fatal("expected spawnResult to be set")
	}
}

func TestRunWithDoneTool(t *testing.T) { //nolint:dupl

	// Set up a mock model-router.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		msgs, _ := req["messages"].([]any)

		if len(msgs) == 1 {
			// First turn: LLM calls _done.
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_1",
									"type": "function",
									"function": map[string]any{
										"name":      "_done",
										"arguments": `{"output": "Final answer"}`,
									},
								},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	result, err := a.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "Succeeded" { //nolint:goconst

		t.Fatalf("expected phase Succeeded, got %s", result.Phase)
	}
	if result.Output != "Final answer" {
		t.Fatalf("expected output 'Final answer', got %s", result.Output)
	}
}

func TestRunWithFailTool(t *testing.T) { //nolint:dupl

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		msgs, _ := req["messages"].([]any)

		if len(msgs) == 1 {
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_1",
									"type": "function",
									"function": map[string]any{
										"name":      "_fail",
										"arguments": `{"reason": "bad input", "retryable": false}`,
									},
								},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	result, err := a.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "Failed" {
		t.Fatalf("expected phase Failed, got %s", result.Phase)
	}
	if result.FailureReason != "bad input" {
		t.Fatalf("expected failure reason 'bad input', got %s", result.FailureReason)
	}
}

func TestRunWithClarifyTool(t *testing.T) { //nolint:dupl

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "call_1",
								"type": "function",
								"function": map[string]any{
									"name":      "_clarify",
									"arguments": `{"question": "What do you mean?"}`,
								},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	result, err := a.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "WaitingForInput" {
		t.Fatalf("expected phase WaitingForInput, got %s", result.Phase)
	}
}

func TestRunWithHandoffTool(t *testing.T) { //nolint:dupl

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "call_1",
								"type": "function",
								"function": map[string]any{
									"name":      "_handoff",
									"arguments": `{"agentRef": "other-agent", "input": "take over"}`,
								},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	result, err := a.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "HandedOff" {
		t.Fatalf("expected phase HandedOff, got %s", result.Phase)
	}
}

func TestRunMaxTurnsExceeded(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": "thinking...",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	result, err := a.Run(context.Background(), "test", WithMaxTurns(3))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "Failed" {
		t.Fatalf("expected phase Failed, got %s", result.Phase)
	}
	if result.FailureReason != "max turns exceeded" {
		t.Fatalf("expected 'max turns exceeded', got %s", result.FailureReason)
	}
}

func TestRunWithCustomTool(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		msgs, _ := req["messages"].([]any)

		callCount++
		if callCount == 1 {
			// LLM calls echo tool.
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_1",
									"type": "function",
									"function": map[string]any{
										"name":      "echo",
										"arguments": `{"text": "hello"}`,
									},
								},
							},
						},
					},
				},
			}
			_ = msgs
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if callCount == 2 {
			// LLM sees tool result and calls _done.
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_2",
									"type": "function",
									"function": map[string]any{
										"name":      "_done",
										"arguments": `{"output": "echoed: hello"}`,
									},
								},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	a.Tool("echo", "Echo text", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required": []string{"text"},
	}, func(args map[string]any) (string, error) {
		return args["text"].(string), nil
	})

	result, err := a.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "Succeeded" {
		t.Fatalf("expected phase Succeeded, got %s", result.Phase)
	}
	if result.Output != "echoed: hello" {
		t.Fatalf("expected output 'echoed: hello', got %s", result.Output)
	}
}

func TestRunNoToolCallsContinues(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"role":    "assistant",
							"content": "Let me think...",
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Second call: _done
		resp := map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "call_1",
								"type": "function",
								"function": map[string]any{
									"name":      "_done",
									"arguments": `{"output": "done"}`,
								},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	a := New(WithBaseURL(srv.URL))
	result, err := a.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != "Succeeded" {
		t.Fatalf("expected phase Succeeded, got %s", result.Phase)
	}
	if result.Output != "done" {
		t.Fatalf("expected output 'done', got %s", result.Output)
	}
}

func TestCheckpointHelpers(t *testing.T) {
	a := New()
	msgs := a.LoadCheckpoint("test-session")
	if msgs != nil {
		t.Fatalf("expected nil, got %v", msgs)
	}
	ref := a.SaveCheckpoint("test-session", nil)
	if ref != "agentorc/runs/test-session/state" {
		t.Fatalf("unexpected checkpoint ref: %s", ref)
	}
}
