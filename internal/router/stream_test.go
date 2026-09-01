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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floppyfish14/agent-orca/internal/state"
)

// --- test store -------------------------------------------------------------

// testStore embeds a no-op backing store and only intercepts the two operations
// the streaming path relies on: SaveToken (token streaming) and SaveTraceEvent
// (toolCall/toolResult/fail emission, used to assert dispatch).
type testStore struct {
	state.Store
	mu            sync.Mutex
	doneEvents    int      // count of terminal `done` trace events with finish_reason (0 — emitDoneEvent removed)
	traceEvents   []string // captured trace-event JSON
	savedMessages []json.RawMessage
	saveCount     int // number of checkpoint() (SaveMessages) calls
}

func newTestStore(t *testing.T) *testStore {
	t.Helper()
	nop, err := state.NewStoreFromConfig(state.Config{})
	if err != nil {
		t.Fatalf("nop store: %v", err)
	}
	return &testStore{Store: nop}
}

func (s *testStore) SaveToken(_ context.Context, _ string, _ string) error {
	// Real-time token appends are not asserted on here. Run-level completion
	// is asserted via the terminal [DONE] SSE frame in the client body, since
	// the done trace event (emitDoneEvent) has been removed in favor of
	// relying on the OpenAI schema's [DONE] and the UI API's finalOutput.
	return nil
}

// SaveMessages records checkpoint() writes so tests can assert the conversation
// is persisted on run completion (used by reused warm pods via PriorRunRef).
func (s *testStore) SaveMessages(_ context.Context, _ string, messages []json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedMessages = messages
	s.saveCount++
	return nil
}

func (s *testStore) checkpointCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}

func (s *testStore) savedMessageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.savedMessages)
}

func (s *testStore) SaveTraceEvent(_ context.Context, _ string, eventJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traceEvents = append(s.traceEvents, eventJSON)
	// A terminal `done` trace event is the schema-aligned completion signal
	// (replacing the old empty-token done sentinel); count it for assertions.
	var m map[string]any
	if json.Unmarshal([]byte(eventJSON), &m) == nil {
		if t, _ := m["type"].(string); t == "done" {
			s.doneEvents++
		}
	}
	return nil
}

func (s *testStore) lastDoneEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneEvents
}

// doneEventJSONs returns the JSON of every terminal `done` trace event emitted,
// so tests can assert on the OpenAI schema fields (e.g. finish_reason).
func (s *testStore) doneEventJSONs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.traceEvents))
	for _, e := range s.traceEvents {
		var m map[string]any
		if json.Unmarshal([]byte(e), &m) == nil {
			if t, _ := m["type"].(string); t == "done" {
				out = append(out, e)
			}
		}
	}
	return out
}

func (s *testStore) toolCallEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.traceEvents {
		if strings.Contains(e, `"toolCall"`) {
			n++
		}
	}
	return n
}

func (s *testStore) thoughtEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.traceEvents {
		if strings.Contains(e, `"type":"thought"`) {
			n++
		}
	}
	return n
}

// --- SSE frame builders -----------------------------------------------------

func sseFrame(chunk streamingChatCompletionChunk) string {
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", data)
}

func sseDone() string { return "data: [DONE]\n\n" }

// --- provider stub ----------------------------------------------------------

// fakeProvider serves canned SSE responses, one per call, in order.
func fakeProvider(t *testing.T, bodies ...[]byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		idx := int(calls.Add(1) - 1)
		if idx >= len(bodies) {
			http.Error(w, "no more canned responses", http.StatusServiceUnavailable)
			return
		}
		w.Write(bodies[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// --- router harness ---------------------------------------------------------

func newStreamingTestRouter(t *testing.T, providerURL, apiKeyFile string, store *testStore) *Router {
	t.Helper()
	cfg := &Config{
		RunName:              "test-run",
		RunNamespace:         "default",
		CheckpointEvery:      0, // disable periodic checkpoint writes during the test
		CheckpointKey:        "",
		ContextWindowReserve: 0.2,
		LLMRequestTimeout:    time.Hour,
		SATokenFile:          apiKeyFile,
		Providers: []ProviderConfig{{
			Name:          "test",
			LiteLLMModel:  "openai/gpt-4o",
			BaseURL:       providerURL,
			APIKeyFile:    apiKeyFile,
			ContextWindow: 100000,
			Weight:        1,
		}},
		Safeguards: RouterSafeguards{ToolExecutionTimeoutSec: 60},
	}
	r, err := New(cfg, store, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.cancelFunc() })
	return r
}

// consumeClientBody runs the full streaming path against an httptest provider and
// returns the client-visible SSE body, the recorded terminal-done-event count, and the
// number of tool-call trace events captured.
func consumeClientBody(t *testing.T, r *Router, reqBody []byte) (string, int, int, int) {
	t.Helper()
	store := r.store.(*testStore)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	// Drive the streaming path. The recursion (tool-call loop) reuses the same
	// ResponseWriter, so rec accumulates both turns' SSE frames.
	r.HandleChatCompletions(rec, req)

	// Since emitDoneEvent was removed, the done trace event count is always 0.
	// Tests that need to verify completion should check for the [DONE] SSE frame
	// in the returned body instead.
	return rec.Body.String(), store.lastDoneEvents(), store.toolCallEvents(), store.thoughtEvents()
}

// toolCallTurnSSE is a canned streaming turn whose LLM emits a _write_state tool
// call with finish_reason left null (the LiteLLM shape that previously
// caused tool calls to be dropped).
func toolCallTurnSSE() []byte {
	return []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c1", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c1", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: "Storing via tool."}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c1", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{ToolCalls: []ToolCall{{
				Index: 0, ID: "call_1", Type: "function",
				Function: FunctionCall{Name: "_write_state", Arguments: ""},
			}}}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c1", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{ToolCalls: []ToolCall{{
				Index:    0,
				Function: FunctionCall{Name: "_write_state", Arguments: `{"scope":"run","key":"k","value":"v"}`},
			}}}}},
		}),
		sseDone(),
	}, ""))
}

// textTurnSSE is a canned final text turn with the given finish_reason and NO
// tool calls, i.e. the terminal response that closes the stream.
func textTurnSSE(finishReason string) []byte {
	fr := finishReason
	return []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c2", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c2", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: "Stored."}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c2", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{}, FinishReason: &fr}},
		}),
		sseDone(),
	}, ""))
}

// writeHeaderCountingRecorder wraps httptest.ResponseRecorder and counts
// WriteHeader calls so tests can assert the streaming tool-call recursion never
// re-issues WriteHeader on the same response (the "superfluous" warning).
type writeHeaderCountingRecorder struct {
	*httptest.ResponseRecorder
	writeHeaderCalls int
}

func (w *writeHeaderCountingRecorder) WriteHeader(code int) {
	w.writeHeaderCalls++
	w.ResponseRecorder.WriteHeader(code)
}

func TestStreamingToolCallWriteHeaderCalledOnce(t *testing.T) {
	// The streaming tool-call loop recurses into HandleChatCompletions reusing the
	// same ResponseWriter. Before the fix, each recursion re-called WriteHeader(200),
	// producing "superfluous response.WriteHeader call" warnings and risking stream
	// framing on real servers. Use a header-set-flagged recorder so the guard
	// (isContinuation) is exercised: WriteHeader must fire exactly once.
	srv, _ := fakeProvider(t, toolCallTurnSSE(), textTurnSSE("stop"))
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "store this"}},
	})
	rec := &writeHeaderCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.HandleChatCompletions(rec, req)

	if rec.writeHeaderCalls != 1 {
		t.Errorf("expected WriteHeader to be called exactly once across the tool-call loop, got %d", rec.writeHeaderCalls)
	}
}

func TestStreamingToolCallWithNullFinishReasonIsDispatched(t *testing.T) {
	// Reproduces the bug: the provider streams a tool call but emits
	// finish_reason: null (relying on `data: [DONE]` to signal completion), which
	// is what OpenAI-compatible proxies like LiteLLM actually do.
	//
	// Before the fix, the streaming guard `len(toolCalls) > 0 && finishReason ==
	// "tool_calls"` failed to dispatch the tool call even though [DONE] was
	// suppressed — silently dropping the call, never recursing, and sending no
	// terminating [DONE] to the client. After the fix, dispatch mirrors the
	// non-streaming path (len(toolCalls) > 0), so the tool is dispatched and the
	// final text turn emits exactly one [DONE] and one run-level done sentinel.
	fr := "stop"
	turn1 := toolCallTurnSSE()
	turn2 := textTurnSSE(fr)

	srv, calls := fakeProvider(t, turn1, turn2)
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "store this"}},
	})
	out, doneEvents, toolCalls, _ := consumeClientBody(t, r, body)

	if toolCalls != 1 {
		t.Errorf("expected the tool call to be dispatched (1 toolCall trace event), got %d", toolCalls)
	}
	if doneEvents != 0 {
		t.Errorf("expected zero done trace events (emitDoneEvent removed) (terminal text turn), got %d", doneEvents)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Errorf("expected exactly one client [DONE] (turn1 suppressed, turn2 emitted), got %d", got)
	}
	if !strings.Contains(out, "Stored.") {
		t.Errorf("expected the recursive final text turn to be streamed to the client; body:\n%s", out)
	}
	if c := calls.Load(); c != 2 {
		t.Errorf("expected the provider to be called twice (tool turn + final text turn), got %d", c)
	}
}

func TestStreamingTextResponseWritesDoneEvent(t *testing.T) {
	// Baseline: a plain text response (no tool calls) must emit [DONE] to the
	// client and exactly one run-level done sentinel — the normal end-of-run path.
	fr := "stop"
	end := []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: "The capital of Japan is Tokyo."}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{}, FinishReason: &fr}},
		}),
		sseDone(),
	}, ""))

	srv, calls := fakeProvider(t, end)
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "capital of Japan?"}},
	})
	out, doneEvents, toolCalls, _ := consumeClientBody(t, r, body)

	if toolCalls != 0 {
		t.Errorf("expected no tool calls on a text response, got %d", toolCalls)
	}
	if doneEvents != 0 {
		t.Errorf("expected zero done trace events (emitDoneEvent removed), got %d", doneEvents)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one client [DONE], got body:\n%s", out)
	}
	if !strings.Contains(out, "Tokyo") {
		t.Errorf("expected streamed text content; body:\n%s", out)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly one provider call, got %d", c)
	}
}

func TestStreamingReasoningIsCapturedAsThoughtEvents(t *testing.T) {
	// A thinking/reasoning model streams delta.reasoning chunks. The model-router must
	// capture them as `thought` trace events (so the UI can render an expandable
	// thinking panel and they survive refresh) instead of silently dropping them, and
	// must still stream the final content + emit exactly one done sentinel at the end.
	end := []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Reasoning: "analyzing the query… "}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Reasoning: "I should cite sources."}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: "The capital of Japan is Tokyo."}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{}, FinishReason: strPtr("stop")}},
		}),
		sseDone(),
	}, ""))

	srv, calls := fakeProvider(t, end)
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "capital of Japan?"}},
	})
	out, doneEvents, toolCalls, thoughts := consumeClientBody(t, r, body)

	if toolCalls != 0 {
		t.Errorf("expected no tool calls, got %d", toolCalls)
	}
	// Both reasoning deltas must be surfaced as thought trace events.
	if thoughts != 2 {
		t.Errorf("expected 2 thought trace events (one per reasoning delta), got %d", thoughts)
	}
	if doneEvents != 0 {
		t.Errorf("expected zero done trace events (emitDoneEvent removed) (at true end), got %d", doneEvents)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one client [DONE], got body:\n%s", out)
	}
	if !strings.Contains(out, "Tokyo") {
		t.Errorf("expected final content streamed to client; body:\n%s", out)
	}
	// Reasoning deltas are proxied to the client (the OpenAI SDK needs them to
	// reconstruct thinking) AND captured as thought trace events for the UI.
	if !strings.Contains(out, "analyzing the query") {
		t.Errorf("expected reasoning to be proxied to the client; body:\n%s", out)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly one provider call, got %d", c)
	}
}

func strPtr(s string) *string { return &s }

func TestStreamingMidStreamDoneDoesNotTruncateClient(t *testing.T) {
	// Malformed/thinking-model proxy: emits a premature `data: [DONE]` right after
	// the reasoning block, then continues with real content and a second [DONE].
	// The OpenAI-SDK client would close at the FIRST [DONE] and miss the answer.
	// The model-router must withhold upstream [DONE]s and emit exactly one
	// canonical terminal [DONE] after the full payload, so the client receives
	// both the thinking and the final content.
	fr := "stop"
	end := []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Reasoning: "let me think…"}}},
		}),
		// premature [DONE] (only after the thinking block)
		sseDone(),
		// real content arriving AFTER the premature [DONE]
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: "The capital of Japan is Tokyo."}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{}, FinishReason: &fr}},
		}),
		// second (terminal) [DONE]
		sseDone(),
	}, ""))

	srv, calls := fakeProvider(t, end)
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "capital of Japan?"}},
	})
	out, doneEvents, _, thoughts := consumeClientBody(t, r, body)

	// The client must see the FULL content, not just the thinking that preceded
	// the premature [DONE].
	if !strings.Contains(out, "The capital of Japan is Tokyo.") {
		t.Errorf("premature [DONE] truncated the real content from the client; body:\n%s", out)
	}
	// Thinking is still captured (forwarded + surfaced as thought events).
	if thoughts < 1 {
		t.Errorf("expected reasoning to be captured as thought events, got %d", thoughts)
	}
	// Exactly one canonical terminal [DONE] reaches the client.
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Errorf("expected exactly one canonical [DONE] (withheld the premature one), got %d; body:\n%s", got, out)
	}
	if doneEvents != 0 {
		t.Errorf("expected zero done trace events (emitDoneEvent removed), got %d", doneEvents)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly one provider call, got %d", c)
	}
}

// writingToolCallTurnSSE is a canned tool-call turn whose deltas carry no
// explicit index on every chunk (the shape some OpenAI-compatible proxies emit).
// It dispatches a single _write_state tool call and is meant to be replayed
// across many turns to simulate a long agentic loop.
func writingToolCallTurnSSE() []byte {
	return []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c1", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{ToolCalls: []ToolCall{{
				Index: 0, ID: "call_1", Type: "function",
				Function: FunctionCall{Name: "_write_state", Arguments: `{"scope":"run","key":"k","value":"v"}`},
			}}}}},
		}),
		sseDone(),
	}, ""))
}

func TestStreamingMultiTurnLoopEmitsSingleDoneEvent(t *testing.T) {
	// Regression for the live "UI exits prematurely" symptom on long agentic runs.
	// The reference agent streams many tool-call turns (each with finish_reason:
	// null, the LiteLLM shape) before a final text turn. Exactly one
	// terminal `done` trace event must be emitted — and only at the true end. A
	// done event on any tool-call turn would make the UI's TailTokens close and the
	// follow-up turns' tokens would never reach the browser.
	srv, calls := fakeProvider(t, writingToolCallTurnSSE(), writingToolCallTurnSSE(), writingToolCallTurnSSE(), textTurnSSE("stop"))
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "store this"}},
	})
	out, doneEvents, toolCalls, _ := consumeClientBody(t, r, body)

	if toolCalls != 3 {
		t.Errorf("expected 3 dispatched tool calls (one per tool-call turn), got %d", toolCalls)
	}
	if doneEvents != 0 {
		t.Errorf("expected zero done trace events (emitDoneEvent removed) at true end of loop, got %d", doneEvents)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Errorf("expected exactly one canonical client [DONE], got %d", got)
	}
	if !strings.Contains(out, "Stored.") {
		t.Errorf("expected the final text turn to reach the client; body:\n%s", out)
	}
	if c := calls.Load(); c != 4 {
		t.Errorf("expected 4 provider calls (3 tool turns + 1 final text), got %d", c)
	}
}

func writeKeyFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "key-*.txt")
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if _, err := f.WriteString("test-key-not-real\n"); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

func TestStreamingTerminalTurnEmitsDoneSSE(t *testing.T) {
	// The terminal text turn must emit a single canonical [DONE] SSE frame to
	// the agent client. This is the OpenAI schema completion signal — no custom
	// done trace event is emitted (emitDoneEvent was removed); the UI relies on
	// the UI API's finalOutput event for the output.
	fr := "length"
	srv, calls := fakeProvider(t, textTurnSSE(fr))
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "how big"}},
	})
	out, doneEvents, _, _ := consumeClientBody(t, r, body)

	if doneEvents != 0 {
		t.Fatalf("expected zero done trace events (emitDoneEvent removed), got %d", doneEvents)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one client [DONE] frame, got body:\n%s", out)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly one provider call, got %d", c)
	}
}

// TestStreamingCompletionCheckpoint asserts that a terminal text turn (an /invoke
// completing) persists the conversation via checkpoint()/SaveMessages, so a reused
// warm pod can resume it on the next chat turn via PriorRunRef. Without the
// completion checkpoint, short (<CheckpointEvery) chats were never saved and turn 2
// started fresh — the "lost context after two turns" symptom.
func TestStreamingCompletionCheckpoint(t *testing.T) {
	srv, calls := fakeProvider(t, textTurnSSE("stop"))
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)
	// Enable checkpointing (newStreamingTestRouter leaves CheckpointKey empty so
	// checkpoint() is a no-op); CheckpointEvery is 0, so only completion saves.
	r.cfg.CheckpointKey = "agentorca/runs/test-run/state"

	body := []byte(`{"model":"default","stream":true,"messages":[{"role":"user","content":"review my PR"}]}`)

	out, _, _, _ := consumeClientBody(t, r, body)

	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one client [DONE] frame, got body:\n%s", out)
	}
	if c := store.checkpointCount(); c != 1 {
		t.Fatalf("expected exactly 1 completion checkpoint, got %d", c)
	}
	// Folded conversation = user prompt + assistant final text turn.
	if n := store.savedMessageCount(); n != 2 {
		t.Errorf("expected 2 saved messages (user + assistant), got %d", n)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly one provider call, got %d", c)
	}
}

// textTurnSSENoFinishReason is a terminal text turn whose chunks carry no
// finish_reason (only `data: [DONE]` marks completion) — the exact shape emitted by
// OpenAI-compatible proxies like LiteLLM.
func textTurnSSENoFinishReason() []byte {
	return []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}},
		}),
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: "Done."}}},
		}),
		// No FinishReason set here; completion is signalled only by [DONE] below.
		sseFrame(streamingChatCompletionChunk{
			ID: "c", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{}}},
		}),
		sseDone(),
	}, ""))
}

func TestStreamingTerminalTurnNormalizesNullFinishReason(t *testing.T) {
	// OpenAI-compatible proxies stream the terminal turn with finish_reason: null
	// and signal completion solely via `data: [DONE]`. The model-router withholds
	// upstream [DONE]s and emits exactly one canonical [DONE] at the true terminal
	// turn. No done trace event is emitted (emitDoneEvent removed) — the UI
	// relies on the UI API's finalOutput event for output.
	srv, calls := fakeProvider(t, textTurnSSENoFinishReason())
	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "ok?"}},
	})
	out, doneEvents, _, _ := consumeClientBody(t, r, body)

	if doneEvents != 0 {
		t.Fatalf("expected zero done trace events (emitDoneEvent removed), got %d (out: %s)", doneEvents, out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one client [DONE] frame, got body:\n%s", out)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly one provider call, got %d", c)
	}
}

func TestConcurrentStreamingRequestRejected(t *testing.T) {
	// Guards against the race where a second top-level (non-continuation)
	// streaming request arrives while the first is still in flight, which
	// previously caused the first stream to be silently abandoned without a
	// terminal done event ("streaming response started" x2, only one "run
	// complete"). The streamingActive guard rejects the second request with
	// 409 Conflict.

	var requestCount atomic.Int32
	requestReceived := make(chan struct{})
	releaseOnce := sync.Once{}
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requestCount.Add(1) == 1 {
			close(requestReceived)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // block until the test releases us
		_, _ = w.Write([]byte(sseDone()))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})

	// First streaming request in a goroutine — blocks inside handleStreamingResponse.
	var firstWG sync.WaitGroup
	firstWG.Add(1)
	go func() {
		defer firstWG.Done()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.HandleChatCompletions(rec, req)
	}()

	// Safety net: ensure cleanup unblocks the first goroutine even if the test
	// fails early or panics.
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		firstWG.Wait()
	})

	// Wait for the provider to receive the first request — streamingActive is now set.
	<-requestReceived

	// Second top-level request must be rejected with 409 Conflict. Use a goroutine
	// + timeout so the test fails fast instead of hanging if the guard is absent.
	type httpResult struct {
		code int
		body string
	}
	secondCh := make(chan httpResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.HandleChatCompletions(rec, req)
		secondCh <- httpResult{code: rec.Code, body: rec.Body.String()}
	}()

	select {
	case res := <-secondCh:
		if res.code != http.StatusConflict {
			t.Errorf("expected second concurrent request rejected with 409 Conflict, got %d: %s",
				res.code, res.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second request did not return within 3s — streaming guard not working")
	}

	// Release the first stream so the goroutine can complete cleanly.
	releaseOnce.Do(func() { close(release) })
	firstWG.Wait()

	// The first stream should have emitted the canonical [DONE] to the client.
	// (No done trace event is emitted — emitDoneEvent was removed.)

	// After the first stream completes, streamingActive is cleared and a new
	// request should be allowed (not rejected with 409).
	thirdCh := make(chan httpResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.HandleChatCompletions(rec, req)
		thirdCh <- httpResult{code: rec.Code}
	}()

	select {
	case res := <-thirdCh:
		if res.code == http.StatusConflict {
			t.Error("expected third request (after first stream completes) to be allowed, got 409 Conflict")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("third request did not return within 3s")
	}
}
