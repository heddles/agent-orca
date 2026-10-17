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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// traceNotifications decodes every JSON-RPC notification written to the
// session-update stdout buffer into its session-update params.
func traceNotifications(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("notification line is not valid JSON: %q", line)
		}
		if msg.Method != acpMethodSessionUpdate {
			t.Fatalf("unexpected notification method %q", msg.Method)
		}
		raw, err := json.Marshal(msg.Params)
		if err != nil {
			t.Fatalf("re-marshal params: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decoding params: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// traceTestBridge builds a bridge whose notifications land in an in-memory
// buffer (notify() only writes; the transport loop is not needed for traces).
// A session "zed-1" is pre-registered so trace tool-call id sequencing (which
// lives on the session record) is exercised.
func traceTestBridge(t *testing.T) (*acpBridge, *openAIStreamState) {
	t.Helper()
	srv := newACPStdioServer(nil, strings.NewReader(""), &bytes.Buffer{}, discardWriter{})
	bridge := newACPBridge(srv, nil, "test-agent")
	bridge.mu.Lock()
	bridge.sessions["zed-1"] = &acpSession{SessionID: "zed-1"}
	bridge.mu.Unlock()
	st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}, traceSeen: map[string]bool{}}
	return bridge, st
}

// discardWriter is an io.Writer sink.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// traceToolCallEvent / traceToolResultEvent build trace SSE payloads matching
// the toolCall / toolResult events the model-router emits.
func traceToolCallEvent() string {
	return `{"type":"toolCall","name":"issue_read",` +
		`"arguments":"{\"issue_number\":34}","backendType":"mcp","backendRef":"github-mcp"}`
}

func traceToolResultEvent() string {
	return `{"type":"toolResult","name":"issue_read",` +
		`"result":"{\"number\":34}","backendType":"mcp","backendRef":"github-mcp","durationMs":667}`
}

// TestACPTraceToolCallAndResult verifies a toolCall trace event surfaces as an
// ACP tool_call update (in_progress) and its toolResult as the matching
// tool_call_update (completed) on the SAME toolCallId, so editors render one
// call→result pair — and that the result content uses the ACP v1
// ToolCallContent wrapper ({"type":"content","content":{…}}); a bare text
// block is dropped by strict clients as an unknown variant.
func TestACPTraceToolCallAndResult(t *testing.T) {
	bridge, _ := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	bridge.handleTraceEvent(parse(t, traceToolCallEvent()), "zed-1")
	bridge.handleTraceEvent(parse(t, traceToolResultEvent()), "zed-1")

	notes := traceNotifications(t, buf)
	if len(notes) != 2 {
		t.Fatalf("expected 2 session updates (call + result), got %d: %v", len(notes), notes)
	}

	callUpdate, ok := notes[0]["update"].(map[string]any)
	if !ok || callUpdate["sessionUpdate"] != acpUpdateToolCall {
		t.Fatalf("first update not a tool_call: %v", notes[0])
	}
	callID, _ := callUpdate["toolCallId"].(string)
	if !strings.HasPrefix(callID, "trace_issue_read_github-mcp_") {
		t.Fatalf("toolCallId not trace-prefixed: %q", callID)
	}
	if callUpdate["status"] != acpToolStatusInProgress {
		t.Fatalf("call status = %v, want in_progress", callUpdate["status"])
	}
	if title, _ := callUpdate["title"].(string); !strings.Contains(title, "github-mcp") ||
		!strings.Contains(title, "issue_read") {
		t.Fatalf("title not MCP-prefixed: %v", callUpdate["title"])
	}

	resUpdate, ok := notes[1]["update"].(map[string]any)
	if !ok || resUpdate["sessionUpdate"] != acpUpdateToolCallUpd {
		t.Fatalf("second update not a tool_call_update: %v", notes[1])
	}
	if resUpdate["toolCallId"] != callID {
		t.Fatalf("result toolCallId %v does not match call %v", resUpdate["toolCallId"], callID)
	}
	if resUpdate["status"] != acpToolStatusCompleted {
		t.Fatalf("result status = %v, want completed", resUpdate["status"])
	}
	// Content must be the wrapped ToolCallContent shape.
	content, _ := resUpdate["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("result update content = %v, want one wrapped entry", resUpdate["content"])
	}
	entry, _ := content[0].(map[string]any)
	if entry["type"] != "content" {
		t.Fatalf("content entry type = %v, want \"content\" (bare blocks are dropped by clients)", entry["type"])
	}
	inner, _ := entry["content"].(map[string]any)
	if text, _ := inner["text"].(string); !strings.Contains(text, "issue_read") || !strings.Contains(text, "34") {
		t.Fatalf("wrapped content text missing result detail: %v", inner)
	}
}

// TestACPTraceSameToolCallsGetDistinctIDs is the regression for the editor
// showing one entry for multiple calls of the same tool: the two issue_read
// calls in a live run collapsed onto one deterministic toolCallId, so Zed
// rendered a single cycling entry instead of two call→result pairs.
func TestACPTraceSameToolCallsGetDistinctIDs(t *testing.T) {
	bridge, _ := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	// Two calls of the SAME tool (the live failure mode), each with a result.
	bridge.handleTraceEvent(parse(t, traceToolCallEvent()), "zed-1")
	bridge.handleTraceEvent(parse(t, traceToolResultEvent()), "zed-1")
	bridge.handleTraceEvent(parse(t, traceToolCallEvent()), "zed-1")
	bridge.handleTraceEvent(parse(t, traceToolResultEvent()), "zed-1")

	notes := traceNotifications(t, buf)
	if len(notes) != 4 {
		t.Fatalf("expected 4 session updates, got %d: %v", len(notes), notes)
	}
	ids := make([]string, 0, 4)
	for _, n := range notes {
		if u, ok := n["update"].(map[string]any); ok {
			ids = append(ids, u["toolCallId"].(string))
		}
	}
	// Pairing: call1→result1 share an id, call2→result2 share another, and the
	// two pairs are distinct.
	if ids[0] == ids[2] {
		t.Fatalf("same-tool calls collapsed onto one id %q", ids[0])
	}
	if ids[0] != ids[1] || ids[2] != ids[3] {
		t.Fatalf("call/result pairing broken: %v", ids)
	}
}

// parse decodes a trace payload for direct handler tests.
func parse(t *testing.T, data string) acpTraceEvent {
	t.Helper()
	ev, ok := parseACPTraceEvent(data)
	if !ok {
		t.Fatalf("trace payload did not parse: %q", data)
	}
	return ev
}

// TestACPTraceEventsViaStream verifies the full consume path: `event: trace`
// SSE frames are intercepted, translated, and DEDUPED across stream
// reconnects (TailTokens replays prior Redis entries on each attempt).
func TestACPTraceEventsViaStream(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	ch := make(chan Event, 4)
	ch <- Event{Type: acpEventTypeTrace, Data: traceToolCallEvent()}
	ch <- Event{Type: acpEventTypeTrace, Data: traceToolCallEvent()} // replay — must dedup
	ch <- Event{Type: acpEventTypeTrace, Data: traceToolResultEvent()}
	close(ch)
	bridge.consumeAndStreamHTTP(context.Background(), ch, "zed-1", "msg_1", st)

	notes := traceNotifications(t, buf)
	if len(notes) != 2 {
		t.Fatalf("expected 2 updates after dedup (call + result), got %d: %v", len(notes), notes)
	}

	// A second stream attempt (fresh channel, same state) replays the events
	// again — still nothing new.
	ch2 := make(chan Event, 4)
	ch2 <- Event{Type: acpEventTypeTrace, Data: traceToolCallEvent()}
	ch2 <- Event{Type: acpEventTypeTrace, Data: traceToolResultEvent()}
	close(ch2)
	bridge.consumeAndStreamHTTP(context.Background(), ch2, "zed-1", "msg_1", st)
	if notes = traceNotifications(t, buf); len(notes) != 2 {
		t.Fatalf("replay after reconnect must be fully deduped, got %d updates", len(notes))
	}
}

// TestACPTraceNonToolEvents verifies guardrail / providerFallback / discovery
// trace events render as thought-chunk notes and terminal events (fail /
// finalOutput) are consumed silently (the run lifecycle ends the turn).
func TestACPTraceNonToolEvents(t *testing.T) {
	bridge, _ := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	bridge.handleTraceEvent(parse(t, `{"type":"guardrail","action":"blocked","reason":"PII"}`), "zed-1")
	bridge.handleTraceEvent(parse(t, `{"type":"providerFallback","from":"gpt4","to":"claude","reason":"429"}`), "zed-1")
	bridge.handleTraceEvent(parse(t, `{"type":"mcpDiscovery","server":"github-mcp","tools":91}`), "zed-1")
	bridge.handleTraceEvent(parse(t, `{"type":"fail","reason":"boom"}`), "zed-1")
	bridge.handleTraceEvent(parse(t, `{"type":"finalOutput","output":"done"}`), "zed-1")

	notes := traceNotifications(t, buf)
	if len(notes) != 3 {
		t.Fatalf("expected 3 thought-chunk notes (terminal events silent), got %d: %v", len(notes), notes)
	}
	for i, want := range []string{"[guardrail blocked] PII", "provider fallback", "[MCP discovery]"} {
		update, _ := notes[i]["update"].(map[string]any)
		content, _ := update["content"].(map[string]any)
		if text, _ := content["text"].(string); !strings.Contains(text, want) {
			t.Fatalf("note %d = %q, want something containing %q", i, text, want)
		}
	}
}
