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
func traceTestBridge(t *testing.T) (*acpBridge, *openAIStreamState) {
	t.Helper()
	srv := newACPStdioServer(nil, strings.NewReader(""), &bytes.Buffer{}, discardWriter{})
	bridge := newACPBridge(srv, nil, "test-agent")
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
// tool_call_update (completed, content attached) on the SAME toolCallId, so
// editors render one call→result pair.
func TestACPTraceToolCallAndResult(t *testing.T) {
	// The relayed call/result pair must land on the SAME toolCallId so clients
	// render one call → result entry instead of two unrelated ones.
	toolCallID := traceToolCallID("issue_read", "github-mcp")
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
	if callUpdate["toolCallId"] != toolCallID {
		t.Fatalf("toolCallId mismatch: %v", callUpdate["toolCallId"])
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
	if resUpdate["toolCallId"] != callUpdate["toolCallId"] {
		t.Fatalf("result toolCallId %v does not match call %v", resUpdate["toolCallId"], callUpdate["toolCallId"])
	}
	if resUpdate["status"] != acpToolStatusCompleted {
		t.Fatalf("result status = %v, want completed", resUpdate["status"])
	}
	content, _ := resUpdate["content"].([]any)
	if len(content) == 0 {
		t.Fatal("result update carries no content")
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
