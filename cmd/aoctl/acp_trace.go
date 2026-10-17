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
	"encoding/json"
	"fmt"
	"strings"
)

// This file translates agent-orca trace events (the structured events the
// model-router interleaves into the run's Redis token stream — toolCall,
// toolResult, guardrail, providerFallback, mcpDiscovery, clarify, etc.) into
// ACP session/update notifications so editors like Zed can render live
// activity during a prompt turn instead of showing only a spinner until the
// final response arrives.
//
// Event schema mirrors ui/src/api/traceStream.ts (the UI's TraceEvent union):
//
//	toolCall:   {"type":"toolCall","name":"issue_read","arguments":"{...}","backendType":"mcp","backendRef":"github-mcp"}
//	toolResult: {"type":"toolResult","name":"issue_read","result":"…","durationMs":667}
//	{"type":"guardrail","action":"blocked","reason":"..."}
//	{"type":"providerFallback","from":"gpt4","to":"claude","reason":"..."}
//	{"type":"mcpDiscovery","server":"github-mcp","tools":42}
//	{"type":"thought","content":"..."}            (also streamed as thought chunks)
//	{"type":"fail","reason":"..."}                 (terminal)
//	{"type":"finalOutput","output":"..."}           (terminal)
//
// Terminal trace events (fail/finalOutput) are consumed by the bridge but NOT
// re-emitted as session updates — the ACP run lifecycle events already end the
// turn, and a duplicate "fail" message would confuse clients.

// acpEventTypeTrace is the SSE event type the ACP API uses for forwarded
// model-router trace events (internal/apiserver/acp_trace.go: writeTraceEvent).
const acpEventTypeTrace = "trace"

// acpTraceEvent is the generic envelope of a model-router trace event payload.
type acpTraceEvent struct {
	Type string `json:"type"`
	// toolResult / guardrail / providerFallback fields.
	Name        string `json:"name,omitempty"`
	Result      string `json:"result,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
	BackendType string `json:"backendType,omitempty"`
	BackendRef  string `json:"backendRef,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	Server      string `json:"server,omitempty"`
	Tools       int    `json:"tools,omitempty"`
	// toolResult duration, milliseconds.
	DurationMs int64 `json:"durationMs,omitempty"`
}

// parseACPTraceEvent decodes one \x00-prefixed trace event payload.
// ok=false when the JSON doesn't parse or has no recognizable type — callers
// silently skip unparseable events (they were written for the UI, which is
// more lenient than this strict decoder).
func parseACPTraceEvent(data string) (acpTraceEvent, bool) {
	var ev acpTraceEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil || ev.Type == "" {
		return acpTraceEvent{}, false
	}
	return ev, true
}

// handleTraceEvent translates one trace event into ACP session/update
// notifications on the given session. Terminal trace events (fail /
// finalOutput) are consumed silently — the ACP run lifecycle events already
// end the turn, and a duplicate "fail" message would confuse clients.
func (b *acpBridge) handleTraceEvent(ev acpTraceEvent, sessionID string) {
	switch ev.Type {
	case "toolCall":
		b.emitTraceToolCall(sessionID, ev)
	case "toolResult":
		b.emitTraceToolResult(sessionID, ev)
	case "guardrail":
		b.emitTraceNote(sessionID, ev.guardrailTitle(), ev.guardrailText())
	case "providerFallback":
		b.emitTraceNote(sessionID, "provider fallback", ev.providerFallbackText())
	case "mcpDiscovery":
		b.emitTraceNote(sessionID, "MCP discovery",
			fmt.Sprintf("discovered %d tools from MCP server %q", ev.Tools, ev.Server))
	case "clarify":
		// The clarify question is surfaced via the elicitation/create flow
		// (handleAwaitingRun); emitting it as a note would duplicate it.
	case "thought", "token":
		// Streaming deltas already surface as agent_thought_chunk /
		// agent_message_chunk from the OpenAI chunk path; skip to avoid
		// duplicates.
	}
}

// emitTraceToolCall emits a tool call as an ACP tool_call update in the
// in_progress state. ToolCallID is derived deterministically from name+args
// so the matching toolResult (same name+args) maps onto the same tool_call id
// and clients render one call → result pair instead of two unrelated entries.
func (b *acpBridge) emitTraceToolCall(sessionID string, ev acpTraceEvent) {
	b.emitToolCallUpdate(sessionID, openaiToolCall{
		Index:     0,
		ID:        traceToolCallID(ev.Name, ev.BackendRef),
		Name:      traceToolTitle(ev),
		Arguments: "",
	}, acpUpdateToolCall, acpToolStatusInProgress, nil)
}

// emitTraceToolResult completes the matching tool_call with the result text
// (truncated — tool results can be very large; the full result is for the
// model, not the user).
func (b *acpBridge) emitTraceToolResult(sessionID string, ev acpTraceEvent) {
	const maxResult = 2000
	result := ev.Result
	if len(result) > maxResult {
		result = result[:maxResult] + "…"
	}
	title := traceToolTitle(ev)
	detail := fmt.Sprintf("%s → %s", title, result)
	b.emitToolCallUpdate(sessionID, openaiToolCall{
		Index:     0,
		ID:        traceToolCallID(ev.Name, ev.BackendRef),
		Name:      title,
		Arguments: "",
	}, acpUpdateToolCallUpd, acpToolStatusCompleted,
		[]acpContentBlock{{Type: "text", Text: detail}})
}

// emitTraceNote surfaces non-tool trace events (guardrail, fallback,
// discovery) as agent_thought_chunk text — ACP has no generic "note" update
// type, and thought chunks render inline without claiming to be assistant
// output.
func (b *acpBridge) emitTraceNote(sessionID, title, text string) {
	if text == "" {
		return
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpUpdate{
			SessionUpdate: acpUpdateThoughtChunk,
			Content:       &acpContentBlock{Type: "text", Text: "[" + title + "] " + text},
		},
	})
}

// traceToolTitle builds a human-readable tool title. MCP tools are prefixed
// with their server name (matching the UI's "mcp · github-mcp calls tool …"
// rendering); other backends show the bare tool name.
func traceToolTitle(ev acpTraceEvent) string {
	if ev.BackendType == "mcp" && ev.BackendRef != "" {
		return ev.BackendRef + " · " + ev.Name
	}
	return ev.Name
}

// traceToolCallID derives a stable ACP toolCallId from the tool name + backend
// ref so toolCall and its matching toolResult collide onto the same update.
// ACP toolCallIds only need to be unique within a session; name-based ids
// could collide with concurrent same-tool calls, but agent-orca serializes
// tool calls within a run, so this is safe and gives clean pairing.
func traceToolCallID(name, backendRef string) string {
	id := "trace_" + strings.ReplaceAll(name, " ", "_")
	if backendRef != "" {
		id += "_" + strings.ReplaceAll(backendRef, " ", "_")
	}
	return id
}

// guardrailTitle renders a guardrail event title.
func (ev acpTraceEvent) guardrailTitle() string {
	switch ev.Action {
	case "blocked":
		return "guardrail blocked"
	case "redact":
		return "guardrail redaction"
	default:
		return "guardrail"
	}
}

// guardrailText renders the guardrail event body.
func (ev acpTraceEvent) guardrailText() string {
	if ev.Reason != "" {
		return ev.Reason
	}
	return ev.Action
}

// providerFallbackText renders a provider-fallback event body.
func (ev acpTraceEvent) providerFallbackText() string {
	var b strings.Builder
	b.WriteString("routing failed over from ")
	b.WriteString(ev.From)
	if ev.To != "" {
		b.WriteString(" to ")
		b.WriteString(ev.To)
	} else {
		b.WriteString(" (no fallback available)")
	}
	if ev.Reason != "" {
		fmt.Fprintf(&b, ": %s", ev.Reason)
	}
	return b.String()
}
