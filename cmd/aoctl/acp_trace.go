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
//	{"type":"thought","content":"..."}            (streamed as agent_thought_chunk deltas)
//	{"type":"fail","reason":"..."}                 (terminal)
//	{"type":"finalOutput","output":"..."}           (terminal)
//
// Infrastructure-originated events (the model-router/operator deciding things
// behind the agent's back) keep the human informed about silent background
// work — without them a run with no visible tool activity looks hung:
//
//	{"type":"routing","model":"z-ai/glm-5.3-flash","provider":"openrouter-free",
//	          "strategy":"rule-based","reason":"weighted random","confidence":0.5}
//	{"type":"hindsight","op":"bank|recall|retain","bankID":"...","failed":false,"reason":"..."}
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
	// thought (extended-thinking delta from the model-router).
	Content string `json:"content,omitempty"`
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
	// routing fields (mirrors the AgentRun status RoutingDecision). Confidence
	// is a fraction in [0,1] — rendered as a percentage.
	Model      string  `json:"model,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	Strategy   string  `json:"strategy,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// hindsight fields: op is one of bank / recall / retain; failed marks a
	// failed background operation (they explain perceived slowness).
	Op     string `json:"op,omitempty"`
	BankID string `json:"bankID,omitempty"`
	Failed bool   `json:"failed,omitempty"`
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
	case "routing":
		b.emitTraceNote(sessionID, "model routing", ev.routingText())
	case "hindsight":
		b.emitTraceNote(sessionID, "hindsight memory", ev.hindsightText())
	case "clarify":
		// The clarify question is surfaced via the elicitation/create flow
		// (handleAwaitingRun); emitting it as a note would duplicate it.
	// thought trace events are intercepted by the stream consumer before this
	// switch — they need the per-attempt replay-swallow state (openAIStreamState)
	// that handleTraceEvent doesn't carry.
	case "thought", "token":
		// `token` is not a trace-event type the router emits. `thought` deltas
		// are handled by emitTraceThoughtDelta in consumeAndStreamHTTP.
	}
}

// emitTraceToolCall emits a tool call as an ACP tool_call update in the
// in_progress state. The toolCallId is sequenced per session (unique per call
// — same-tool calls with a deterministic id collapse into one editor entry)
// and recorded as pending so the matching toolResult maps onto the same id.
func (b *acpBridge) emitTraceToolCall(sessionID string, ev acpTraceEvent) {
	b.emitToolCallUpdate(sessionID, openaiToolCall{
		Index:     0,
		ID:        b.nextTraceToolCallID(sessionID, ev),
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
		ID:        b.popTraceToolCallID(sessionID, ev),
		Name:      title,
		Arguments: "",
	}, acpUpdateToolCallUpd, acpToolStatusCompleted,
		[]acpContentBlock{{Type: "text", Text: detail}})
}

// nextTraceToolCallID assigns a fresh, session-unique toolCallId for a trace
// tool call and records it as pending for its result. Falls back to the
// deterministic name-based id when the session record is gone (id uniqueness
// then relies on the tool not repeating — acceptable for a degraded path).
func (b *acpBridge) nextTraceToolCallID(sessionID string, ev acpTraceEvent) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	if !ok {
		return traceToolCallID(ev.Name, ev.BackendRef)
	}
	s.traceToolSeq++
	id := fmt.Sprintf("%s_%d", traceToolCallID(ev.Name, ev.BackendRef), s.traceToolSeq)
	if s.pendingTraceTool == nil {
		s.pendingTraceTool = map[string]string{}
	}
	s.pendingTraceTool[traceToolKey(ev)] = id
	return id
}

// popTraceToolCallID returns the pending toolCallId for this tool (agent-orca
// serializes tool calls within a run, so the pending entry is the match), or
// the deterministic fallback when nothing is pending.
func (b *acpBridge) popTraceToolCallID(sessionID string, ev acpTraceEvent) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	if !ok {
		return traceToolCallID(ev.Name, ev.BackendRef)
	}
	key := traceToolKey(ev)
	if id, ok := s.pendingTraceTool[key]; ok {
		delete(s.pendingTraceTool, key)
		return id
	}
	return traceToolCallID(ev.Name, ev.BackendRef)
}

// traceToolKey pairs a toolResult with its toolCall by backend + tool name.
func traceToolKey(ev acpTraceEvent) string {
	return ev.BackendRef + ":" + ev.Name
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

// emitTraceThoughtDelta emits one `thought` trace-event delta — a slice of the
// model's extended-thinking output — as an agent_thought_chunk. Reasoning
// reaches ACP clients ONLY as thought trace events (the model-router emits it
// via emitTraceEventBoth per delta; it is never part of the proxied content),
// so without this the editor shows no thinking at all.
//
// Replay handling mirrors the OpenAI-chunk reasoning path: the per-attempt
// swallow counter (st.replayThoughtSkip, armed in streamRunEvents before each
// stream attempt) drops the prefix already emitted on a reconnect replay, and
// the emitted suffix accumulates in st.streamedThought. Deliberately NOT added
// to st.traceSeen: trace deltas repeat verbatim for identical consecutive
// deltas (e.g. whitespace), and exact-JSON dedup would silently drop real
// thinking text on the live path.
func (b *acpBridge) emitTraceThoughtDelta(sessionID, text string, st *openAIStreamState) {
	if text == "" {
		return
	}
	if st != nil {
		if st.replayThoughtSkip > 0 {
			if len(text) <= st.replayThoughtSkip {
				st.replayThoughtSkip -= len(text)
				return
			}
			text = text[st.replayThoughtSkip:]
			st.replayThoughtSkip = 0
		}
		st.streamedThought.WriteString(text)
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpUpdate{
			SessionUpdate: acpUpdateThoughtChunk,
			Content:       &acpContentBlock{Type: "text", Text: text},
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

// traceToolCallID is the deterministic base id for a trace tool call
// (name + backend). The full id carries a per-session sequence suffix so
// repeated calls to the same tool stay distinct in the editor.
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

// routingText renders a routing-decision event body, mirroring the web UI's
// modelSelected line: "routed <model> — [strategy] provider — reason (confidence: NN%)".
func (ev acpTraceEvent) routingText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "routed %s", ev.Model)
	if ev.Strategy != "" || ev.Provider != "" {
		fmt.Fprintf(&b, " — [%s] %s", ev.Strategy, ev.Provider)
	}
	if ev.Reason != "" {
		fmt.Fprintf(&b, " — %s", ev.Reason)
	}
	if ev.Confidence > 0 {
		fmt.Fprintf(&b, " (confidence: %.0f%%)", ev.Confidence*100)
	}
	return b.String()
}

// hindsightText renders a hindsight memory-system event body — these run
// silently before/after each LLM call and can add visible latency.
func (ev acpTraceEvent) hindsightText() string {
	var b strings.Builder
	switch ev.Op {
	case "recall":
		b.WriteString("recalled relevant memories")
	case "retain":
		b.WriteString("retained this turn to memory")
	case "bank":
		b.WriteString("memory bank ready")
	default:
		fmt.Fprintf(&b, "memory operation %s", ev.Op)
	}
	if ev.BankID != "" {
		fmt.Fprintf(&b, " (bank %q)", ev.BankID)
	}
	if ev.Failed {
		fmt.Fprintf(&b, " — FAILED: %s", ev.Reason)
	}
	return b.String()
}
