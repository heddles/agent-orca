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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingProvider serves canned SSE responses in order and records each
// request body it received.
type recordingProvider struct {
	mu     sync.Mutex
	bodies [][]byte
	reqs   []ChatCompletionRequest
}

func newRecordingProvider(t *testing.T, bodies ...[]byte) (*httptest.Server, *recordingProvider) {
	t.Helper()
	p := &recordingProvider{bodies: bodies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req ChatCompletionRequest
		_ = json.Unmarshal(raw, &req)
		p.mu.Lock()
		idx := len(p.reqs)
		p.reqs = append(p.reqs, req)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx >= len(p.bodies) {
			http.Error(w, "no more canned responses", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(p.bodies[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

func (p *recordingProvider) requests() []ChatCompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ChatCompletionRequest(nil), p.reqs...)
}

// toolCallsTurnSSE is a turn in which the model calls the given tools, streamed
// the way OpenAI does: one delta with id and name, then the arguments, then
// finish_reason "tool_calls".
func toolCallsTurnSSE(calls ...ToolCall) []byte {
	frames := make([]string, 0, 1+2*len(calls)+2)
	frames = append(frames, sseFrame(streamingChatCompletionChunk{
		ID: "c", Object: "chat.completion.chunk",
		Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
	}))
	for i, tc := range calls {
		frames = append(frames,
			sseFrame(streamingChatCompletionChunk{
				ID: "c", Object: "chat.completion.chunk",
				Choices: []streamChoice{{Index: 0, Delta: streamDelta{ToolCalls: []ToolCall{{
					Index: i, ID: tc.ID, Type: "function", Function: FunctionCall{Name: tc.Function.Name},
				}}}}},
			}),
			sseFrame(streamingChatCompletionChunk{
				ID: "c", Object: "chat.completion.chunk",
				Choices: []streamChoice{{Index: 0, Delta: streamDelta{ToolCalls: []ToolCall{{
					Index: i, Function: FunctionCall{Arguments: tc.Function.Arguments},
				}}}}},
			}))
	}
	fr := finishReasonToolCalls
	frames = append(frames,
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, FinishReason: &fr}},
		}),
		sseDone())
	return []byte(strings.Join(frames, ""))
}

func call(id, name, args string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
}

// shellCallID is the ID of the agent shell call in the canned turns.
const shellCallID = "call_sh"

// shellToolName is the agent tool the canned turns call.
const shellToolName = "shell"

// sseDoneData is the payload of the terminating SSE event.
const sseDoneData = "[DONE]"

var shellTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":       shellToolName,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}},
	},
}

func newLocalToolsRouter(t *testing.T, providerURL string) (*Router, *testStore) {
	t.Helper()
	store := newTestStore(t)
	r := newStreamingTestRouter(t, providerURL, writeKeyFile(t), store)
	r.cfg.LocalTools = true
	return r, store
}

func postChat(t *testing.T, r *Router, req ChatCompletionRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	return rec
}

// clientToolCalls parses the SSE body the agent received the way an OpenAI
// client accumulator does: tool-call deltas merged by index.
func clientToolCalls(t *testing.T, body string) ([]ToolCall, []string) {
	t.Helper()
	var calls []ToolCall
	var finishes []string
	for line := range strings.SplitSeq(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == sseDoneData {
			continue
		}
		var chunk streamingChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		for _, c := range chunk.Choices {
			for _, d := range c.Delta.ToolCalls {
				for len(calls) <= d.Index {
					calls = append(calls, ToolCall{})
				}
				tc := &calls[d.Index]
				if d.ID != "" {
					tc.ID = d.ID
				}
				tc.Function.Name += d.Function.Name
				tc.Function.Arguments += d.Function.Arguments
			}
			if c.FinishReason != nil {
				finishes = append(finishes, *c.FinishReason)
			}
		}
	}
	return calls, finishes
}

func TestLocalToolCallIsReturnedToAgent(t *testing.T) {
	srv, provider := newRecordingProvider(t,
		toolCallsTurnSSE(call(shellCallID, shellToolName, `{"command":"go build ./..."}`)),
		textTurnSSE(finishReasonStop),
	)
	r, store := newLocalToolsRouter(t, srv.URL)

	rec := postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "build it"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	calls, finishes := clientToolCalls(t, body)
	if len(calls) != 1 || calls[0].ID != shellCallID || calls[0].Function.Name != shellToolName ||
		calls[0].Function.Arguments != `{"command":"go build ./..."}` {
		t.Fatalf("agent got tool calls %+v, want the one shell call", calls)
	}
	if len(finishes) != 1 || finishes[0] != finishReasonToolCalls {
		t.Errorf("finish reasons %v, want exactly [tool_calls]", finishes)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("stream does not end with [DONE]: %q", body)
	}
	if n := len(provider.requests()); n != 1 {
		t.Fatalf("provider called %d times, want 1: the router must not run or continue past an agent tool", n)
	}
	if store.toolCallEvents() != 1 {
		t.Errorf("toolCall trace events = %d, want 1", store.toolCallEvents())
	}

	// The agent runs the tool and sends the result.
	rec = postChat(t, r, ChatCompletionRequest{
		Messages: []Message{
			{Role: "user", Content: "build it"},
			{Role: "assistant", ToolCalls: []ToolCall{call(shellCallID, shellToolName, `{"command":"go build ./..."}`)}},
			{Role: "tool", ToolCallID: shellCallID, Content: "ok\n[exit 0]"},
		},
		Tools:  []map[string]any{shellTool},
		Stream: true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("result request status %d: %s", rec.Code, rec.Body)
	}
	reqs := provider.requests()
	if len(reqs) != 2 {
		t.Fatalf("provider called %d times, want 2", len(reqs))
	}
	var roles []string
	for _, m := range reqs[1].Messages {
		if m.Role != "system" {
			roles = append(roles, m.Role)
		}
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool" {
		t.Fatalf("second LLM request roles = %s, want user,assistant,tool (history stored once)", got)
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if last.ToolCallID != shellCallID || messageText(last) != "ok\n[exit 0]" {
		t.Errorf("tool result sent to the LLM = %+v", last)
	}
	if !strings.Contains(strings.Join(store.traceEvents, "\n"), `"backendType":"agent"`) {
		t.Error("agent tool calls and results are not traced with backendType agent")
	}
}

func TestLocalToolsMixedWithRouterTools(t *testing.T) {
	// One turn calls a router tool and an agent tool; the next LLM call (after the
	// agent's result) must see both results.
	srv, provider := newRecordingProvider(t,
		toolCallsTurnSSE(
			call("call_ws", "_write_state", `{"scope":"run","key":"k","value":"v"}`),
			call(shellCallID, shellToolName, `{"command":"ls"}`),
		),
		textTurnSSE(finishReasonStop),
	)
	r, _ := newLocalToolsRouter(t, srv.URL)

	rec := postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "go"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	calls, _ := clientToolCalls(t, rec.Body.String())
	if len(calls) != 1 || calls[0].Function.Name != shellToolName {
		t.Fatalf("agent got %+v, want only its shell call (the router runs _write_state)", calls)
	}

	rec = postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "tool", ToolCallID: shellCallID, Content: "README.md"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	answered := map[string]bool{}
	for _, m := range provider.requests()[1].Messages {
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	if !answered["call_ws"] || !answered[shellCallID] {
		t.Errorf("second LLM request answered %v, want both call_ws and call_sh", answered)
	}
}

func TestLocalToolAfterRouterToolLoop(t *testing.T) {
	// The router runs a tool and continues; the model then calls an agent tool.
	// Both LLM calls index their tool calls from 0, so the agent must see only
	// its own call, or an OpenAI accumulator merges the two.
	srv, provider := newRecordingProvider(t,
		toolCallsTurnSSE(call("call_ws", "_write_state", `{"scope":"run","key":"k","value":"v"}`)),
		toolCallsTurnSSE(call(shellCallID, shellToolName, `{"command":"make test"}`)),
	)
	r, _ := newLocalToolsRouter(t, srv.URL)

	rec := postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "test it"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	calls, finishes := clientToolCalls(t, rec.Body.String())
	if len(calls) != 1 || calls[0].ID != shellCallID || calls[0].Function.Name != shellToolName ||
		calls[0].Function.Arguments != `{"command":"make test"}` {
		t.Fatalf("agent got %+v, want exactly the shell call", calls)
	}
	if len(finishes) != 1 {
		t.Errorf("finish reasons %v, want one", finishes)
	}
	if n := len(provider.requests()); n != 2 {
		t.Errorf("provider called %d times, want 2", n)
	}
}

func TestLocalToolResultsAreValidated(t *testing.T) {
	cases := []struct {
		name string
		msgs []Message
	}{
		{"missing result", []Message{{Role: "user", Content: "and now?"}}},
		{"unknown id", []Message{{Role: "tool", ToolCallID: "call_other", Content: "x"}}},
		{"one of two missing", []Message{{Role: "tool", ToolCallID: "call_a", Content: "x"}}},
		{"duplicate", []Message{
			{Role: "tool", ToolCallID: "call_a", Content: "x"},
			{Role: "tool", ToolCallID: "call_a", Content: "x"},
			{Role: "tool", ToolCallID: "call_b", Content: "y"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, provider := newRecordingProvider(t,
				toolCallsTurnSSE(call("call_a", shellToolName, `{"command":"a"}`), call("call_b", shellToolName, `{"command":"b"}`)),
			)
			r, _ := newLocalToolsRouter(t, srv.URL)
			postChat(t, r, ChatCompletionRequest{
				Messages: []Message{{Role: "user", Content: "go"}},
				Tools:    []map[string]any{shellTool},
				Stream:   true,
			})
			rec := postChat(t, r, ChatCompletionRequest{Messages: tc.msgs, Tools: []map[string]any{shellTool}, Stream: true})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
			if n := len(provider.requests()); n != 1 {
				t.Errorf("provider called %d times, want 1 (a rejected request must not reach the LLM)", n)
			}
		})
	}
}

func TestLocalToolNameMayNotShadowRouterTools(t *testing.T) {
	for _, name := range []string{"_done", "done", "fs_read_text_file", "jira_search"} {
		t.Run(name, func(t *testing.T) {
			srv, provider := newRecordingProvider(t, textTurnSSE(finishReasonStop))
			r, _ := newLocalToolsRouter(t, srv.URL)
			r.cfg.ToolDefinitions = []ToolDefinition{{Name: "jira_search", BackendType: backendTypeMCP}}
			rec := postChat(t, r, ChatCompletionRequest{
				Messages: []Message{{Role: "user", Content: "hi"}},
				Tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": name}}},
				Stream:   true,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			if len(provider.requests()) != 0 {
				t.Error("rejected request reached the LLM")
			}
		})
	}
}

func TestLocalToolCallsCountTowardSafeguards(t *testing.T) {
	srv, _ := newRecordingProvider(t,
		toolCallsTurnSSE(call("c1", shellToolName, `{"command":"ls"}`)),
		toolCallsTurnSSE(call("c2", shellToolName, `{"command":"ls"}`)),
	)
	r, _ := newLocalToolsRouter(t, srv.URL)
	r.cfg.Safeguards.MaxRepeatedToolCalls = 2

	postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "go"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	rec := postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "tool", ToolCallID: "c1", Content: "a"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	calls, _ := clientToolCalls(t, rec.Body.String())
	if len(calls) != 0 {
		t.Errorf("agent got %+v after the safeguard tripped", calls)
	}
	r.mu.Lock()
	tripped := r.loopDetected
	r.mu.Unlock()
	if !tripped {
		t.Error("repeated agent tool calls did not trip the safeguard")
	}
}

func TestLocalToolsDisabledKeepsRouterBehavior(t *testing.T) {
	// Without LocalTools a client tool named "done" is not rejected, and its
	// call is dispatched (and fails) in the router as before.
	srv, provider := newRecordingProvider(t,
		toolCallsTurnSSE(call("c1", shellToolName, `{"command":"ls"}`)),
		textTurnSSE(finishReasonStop),
	)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), store)
	rec := postChat(t, r, ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "go"}},
		Tools:    []map[string]any{shellTool},
		Stream:   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if n := len(provider.requests()); n != 2 {
		t.Fatalf("provider called %d times, want 2 (router dispatches and continues)", n)
	}
	if !strings.Contains(messageText(provider.requests()[1].Messages[len(provider.requests()[1].Messages)-1]), "unknown tool") {
		t.Error("expected the router's unknown-tool result")
	}
}

func TestUnansweredToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "x"},
		{Role: "assistant", ToolCalls: []ToolCall{call("a", shellToolName, ""), call("b", "_write_state", ""), call("c", shellToolName, "")}},
		{Role: "tool", ToolCallID: "b", Content: "ok"},
	}
	got := unansweredToolCalls(msgs)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("pending = %+v, want a and c", got)
	}
	if got := unansweredToolCalls(append(msgs, Message{Role: "assistant", Content: "done"})); len(got) != 0 {
		t.Errorf("a text reply after the calls leaves %+v pending", got)
	}
}

func TestStripToolCallDeltas(t *testing.T) {
	fr := finishReasonToolCalls
	stop := finishReasonStop
	content := streamingChatCompletionChunk{Choices: []streamChoice{{Delta: streamDelta{Content: "hi"}}}}
	if _, changed := stripToolCallDeltas(content); changed {
		t.Error("content chunk changed")
	}
	if out, changed := stripToolCallDeltas(streamingChatCompletionChunk{
		Choices: []streamChoice{{Delta: streamDelta{ToolCalls: []ToolCall{call("a", shellToolName, "")}}}},
	}); !changed || out != nil {
		t.Errorf("tool-call-only chunk: changed=%v out=%s, want dropped", changed, out)
	}
	if out, changed := stripToolCallDeltas(streamingChatCompletionChunk{
		Choices: []streamChoice{{FinishReason: &fr}},
	}); !changed || out != nil {
		t.Errorf("tool_calls finish chunk: changed=%v out=%s, want dropped", changed, out)
	}
	out, changed := stripToolCallDeltas(streamingChatCompletionChunk{
		Choices: []streamChoice{{Delta: streamDelta{Content: "x", ToolCalls: []ToolCall{call("a", shellToolName, "")}}}},
	})
	if !changed || !strings.Contains(string(out), `"content":"x"`) || strings.Contains(string(out), "tool_calls") {
		t.Errorf("mixed chunk = %s, want content kept and tool calls removed", out)
	}
	if _, changed := stripToolCallDeltas(streamingChatCompletionChunk{
		Choices: []streamChoice{{FinishReason: &stop}},
	}); changed {
		t.Error("stop finish chunk changed")
	}
}
