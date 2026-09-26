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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the bridge: it implements the ACP JSON-RPC method handlers over
// stdio on one side, and translates them into calls against agent-orca's ACP
// HTTP API on the other. Each Zed "External Agent" configured as
//
//	aoctl acp serve --agent <name>
//
// fronts exactly one agent-orca agent (ACP v1 is single-agent-per-server; the
// agent name is pinned at launch).
//
// Session continuity: agent-orca is stateless-HTTP; conversation state lives in
// its checkpoint store keyed off run_id/session_id. So each session/prompt
// starts a NEW run with the SAME session_id -- agent-orca chains runs across
// turns via LastRunRef<->PriorRunRef (the model-router reloads the prior
// checkpoint). The bridge pins Zed's sessionId to agent-orca's session_id.

// acpProtocolVersion is the ACP protocol MAJOR version this bridge supports,
// sent as a uint16 in initialize responses per the ACP spec
// (https://agentclientprotocol.com/protocol/v1/). Currently v1.
const acpProtocolVersion uint16 = 1

// acpBridgeVersion is the ACP protocol-bridge version reported in initialize
// responses. It is independent of the ACP protocol version (1) and tracks the
// aoctl bridge implementation.
const acpBridgeVersion = "0.1.0"

// acpSession is the live state for one Zed session.
type acpSession struct {
	SessionID string
	RunID     string
	Cancel    context.CancelFunc
}

// openAIStreamState accumulates translation state across the events of one
// streamed run: the assistant text streamed so far (so a premature stream
// close + polling fallback can emit only the remainder), the reasoning
// streamed so far (same dedup), and the tool-call deltas merged so far
// (keyed by OpenAI index, mirroring mergeToolCallDeltas in internal/router).
type openAIStreamState struct {
	streamed        strings.Builder // assistant text emitted as message chunks
	streamedThought strings.Builder // reasoning emitted as thought chunks
	toolCalls       map[int]openaiToolCall
}

// acpBridge owns method dispatch and per-session run state.
type acpBridge struct {
	srv       *acpStdioServer
	client    *Client
	agentName string
	manifest  ACPAgentManifest

	mu       sync.Mutex
	sessions map[string]*acpSession
	seq      atomic.Int64
}

func newACPBridge(srv *acpStdioServer, client *Client, agentName string) *acpBridge {
	return &acpBridge{
		srv:       srv,
		client:    client,
		agentName: agentName,
		sessions:  map[string]*acpSession{},
	}
}

// promptHandled is a sentinel result meaning "the handler already sent the ACP
// session/prompt response" (used because prompt streams then replies late).
type promptHandled struct{}

type requestHandler func(ctx context.Context, b *acpBridge, req jsonrpcRequest) (any, error)

// Dispatch routes one JSON-RPC request/notification. Serve() runs each request
// in its own goroutine, so a long-running session/prompt never blocks an
// incoming session/cancel.
func (b *acpBridge) Dispatch(ctx context.Context, req jsonrpcRequest) {
	isNotification := req.ID == nil //nolint:staticcheck

	// session/cancel can arrive as either a notification or a request.
	if req.Method == "session/cancel" {
		b.handleCancel(ctx, req.Params, isNotification, req.ID)
		return
	}

	handler, ok := requestHandlers[req.Method]
	if !ok {
		if !isNotification {
			b.srv.respondErr(req.ID, -32601, "method not found: "+req.Method)
		}
		return
	}
	result, err := handler(ctx, b, req)
	if isNotification {
		_ = result
		_ = err
		return
	}
	if err != nil {
		b.srv.respondErr(req.ID, -32000, err.Error())
		return
	}
	if _, skip := result.(promptHandled); skip {
		return
	}
	b.srv.respond(req.ID, result)
}

var requestHandlers = map[string]requestHandler{
	"initialize":     handleInitialize,
	"authenticate":   handleAuthenticate,
	"session/new":    handleSessionNew,
	"session/prompt": handleSessionPrompt,
	"session/resume": handleSessionResume,
	"session/list":   handleSessionList,
	"session/close":  handleSessionClose,
	"session/delete": handleSessionDelete,
}

// --- initialize ---

// handleInitialize advertises this bridge as a single ACP agent backed by an
// agent-orca agent. authMethods is empty because the bearer token is resolved
// up-front by aoctl (see runACPServe), so no in-protocol login is needed.
func handleInitialize(ctx context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p acpInitializeParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	// Eagerly fetch the agent manifest so run creation is informed by the real
	// agent, and so we can validate the agent name up front.
	man, err := b.client.GetAgentManifest(ctx, b.agentName)
	if err != nil {
		return nil, fmt.Errorf("fetching agent manifest for %q via ACP API (%s): %w — "+
			"ensure --acp-endpoint points to the agent-orca ACP API host root", b.agentName, b.client.ACP, err)
	}
	b.manifest = man

	version := p.ProtocolVersion
	if version == 0 || version != acpProtocolVersion {
		// Client omitted protocolVersion (or sent an unparseable value), or
		// requested a version we don't support: negotiate down to our latest.
		version = acpProtocolVersion
	}
	return acpInitializeResponse{
		ProtocolVersion: version,
		AgentCapabilities: acpAgentCaps{
			LoadSession:        false,
			PromptCapabilities: acpPromptCaps{Image: false, Audio: false, EmbeddedContext: false},
			MCPCapabilities:    acpMCPCaps{HTTP: false, SSE: false},
			// session/new, session/prompt, session/cancel and session/update are
			// baseline capabilities (all agents MUST support them), so they are
			// NOT advertised here. Only optional capabilities that this bridge
			// actually implements are listed: list, close, delete, resume.
			SessionCapabilities: map[string]any{
				"list":   map[string]any{},
				"close":  map[string]any{},
				"delete": map[string]any{},
				"resume": map[string]any{},
			},
			Auth: map[string]any{},
		},
		AgentInfo: acpImplementation{
			Name:    "agent-orca: " + b.agentName,
			Version: acpBridgeVersion,
		},
		AuthMethods: []any{}, // pre-authenticated via aoctl login
	}, nil
}

// handleAuthenticate: auth is handled out-of-band by aoctl login, so this is a
// no-op acknowledgement. Zed won't call it because AuthMethods is empty.
func handleAuthenticate(_ context.Context, _ *acpBridge, _ jsonrpcRequest) (any, error) {
	return map[string]any{"authenticated": false}, nil
}

// --- session/new ---

func handleSessionNew(_ context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p acpSessionNewParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	sessionID := p.SessionID
	if sessionID == "" {
		sessionID = b.nextSessionID()
	}
	b.mu.Lock()
	b.sessions[sessionID] = &acpSession{SessionID: sessionID}
	b.mu.Unlock()
	return acpSessionNewResponse{SessionID: sessionID}, nil
}

// --- session/prompt (the core run+stream flow) ---

func handleSessionPrompt(ctx context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p acpPromptParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		b.srv.respondErr(req.ID, -32600, "invalid params: "+err.Error())
		return promptHandled{}, nil
	}
	sessionID := p.SessionID
	if sessionID == "" {
		sessionID = b.nextSessionID()
	}

	// Translate ACP content blocks -> agent-orca input, reusing the same
	// session_id so runs chain across turns.
	//
	// If a prior turn left a run in the "awaiting" state (e.g. the user
	// declined the elicitation form and is now answering in a new prompt),
	// resume that run with the user's input instead of starting fresh.
	var runID string
	if existingID, ok := b.tryResumeAwaiting(ctx, sessionID, p.Prompt); ok {
		runID = existingID
	} else {
		run, err := b.client.CreateAgentRun(ctx, b.agentName, ACPRunRequest{
			Input:     toAgentOrcaInput(p.Prompt),
			SessionID: sessionID,
		})
		if err != nil {
			b.srv.respondErr(req.ID, -32001, "create run: "+err.Error())
			return promptHandled{}, nil
		}
		runID = run.RunID
	}

	// Register a cancellable context so session/cancel can abort a live turn.
	// Derive from the dispatch context (not Background) so that a shutdown
	// cancels in-flight runs as well.
	runCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.sessions[sessionID] = &acpSession{SessionID: sessionID, RunID: runID, Cancel: cancel}
	b.mu.Unlock()
	defer func() {
		cancel() // release the context on completion (or cancellation)
		b.mu.Lock()
		if cur, ok := b.sessions[sessionID]; ok && cur.RunID == runID {
			cur.Cancel = nil
		}
		b.mu.Unlock()
	}()

	stopReason := b.awaitCompletion(runCtx, runID, sessionID)
	b.srv.respond(req.ID, acpPromptResponse{StopReason: stopReason})
	return promptHandled{}, nil
}

// awaitCompletion drives one run to a terminal state. It first tries
// streaming (via streamRunEvents), which will fall back to polling if streaming
// is unavailable. If streaming reaches the "awaiting" state, it handles the
// elicitation flow and loops. Otherwise, it returns the terminal stop reason.
func (b *acpBridge) awaitCompletion(ctx context.Context, runID, sessionID string) string {
	for {
		// Fresh translation state per run (continuation runs after an await
		// resume carry their own output).
		st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}}
		// Path 1: Try streaming first
		term := b.streamRunEvents(ctx, runID, sessionID, st)
		if term != acpPendingAwaiting {
			return term
		}
		// Path 2: Stream surfaced awaiting — handle elicitation and loop
		run, err := b.client.GetACPRun(ctx, runID)
		if err != nil {
			return acpStopReasonUserCancel
		}
		contID, resumed := b.handleAwaitingRun(ctx, run, sessionID, runID, st)
		if resumed {
			runID = contID
			continue
		}
		return acpStopReasonEndTurn
	}
}

// maxStreamRetries bounds how many times streamRunEvents will re-attempt the
// SSE stream after it ends prematurely. The ACP API bounds each streaming
// request to 30s (acpAPIRequestTimeout), which can cut a long response into
// 30s windows; re-streaming lets the bridge recover tokens across windows
// (the server replays the token history on each new connection). After the
// budget is exhausted it falls back to polling, which is bounded by runCtx.
const maxStreamRetries = 60

// streamRunEvents consumes the SSE stream from GET /runs/{id} (Accept:
// text/event-stream) and translates it into session/update notifications.
// If streaming is unavailable, it falls back to polling via pollToCompletion.
//
// The ACP API bounds streaming requests to 30s, so a long response is delivered
// in windows: each window ends either with a genuine terminal event or with
// the connection dropping. A terminal event is verified against the run's real
// status (confirmTerminal) before ending the turn, so a premature run.completed
// caused by the 30s timeout does not truncate the response — instead the
// stream is re-attempted and the suffix-aware polling fallback fills in any
// gap.
func (b *acpBridge) streamRunEvents(ctx context.Context, runID, sessionID string, st *openAIStreamState) string {
	for attempt := 0; ; attempt++ {
		// Each stream attempt gets its own cancellable context derived from the
		// run context. When the attempt ends, the attempt context is cancelled
		// so the producer goroutine inside StreamRunEvents unblocks from its
		// channel send, closes the HTTP response body, and exits. Without this
		// teardown, every early-ended 30s window would leak a goroutine and an
		// open connection until the whole prompt turn finished.
		streamCtx, streamCancel := context.WithCancel(ctx)
		ch, err := b.client.StreamRunEvents(streamCtx, runID)
		if err != nil {
			// streamCancel()
			// SSE unavailable (run not yet in-progress, no state store, etc.).
			// Fall back to polling.
			b.srv.logf("acp: streaming of run (%v) failed with error: %s\n", err, runID)
			streamCancel()
			return ""
			// return b.pollToCompletion(ctx, runID, sessionID, st)
		}

		term := b.consumeAndStreamHTTP(streamCtx, ch, sessionID, "msg_"+runID, st)
		// Tear down this attempt before deciding what to do next: cancelling
		// streamCtx unblocks the producer (its send selects on streamCtx.Done)
		// and closes the response body via the request context.
		streamCancel()

		if term == acpStopReasonUserCancel {
			return term
		}
		if term != "" {
			// A terminal event (run.completed / [DONE] / finish_reason). Verify it
			// is genuine — the 30s request timeout can surface a premature
			// run.completed while the run is still in progress. Without this
			// check the turn ends at the first 30s window and the rest of the
			// response is lost ("only pieces of the streamed output").
			if b.confirmTerminal(ctx, runID) {
				return term
			}
			// Premature terminal event: fall through to re-attempt streaming.
		}
		// if attempt >= maxStreamRetries {
		// b.srv.logf("acp: stream for %s ended early repeatedly, falling back to polling\n", runID)
		// return b.pollToCompletion(ctx, runID, sessionID, st)
		// }
		b.srv.logf("acp: stream for %s ended early (attempt %d), re-attempting\n", runID, attempt+1)
		// Brief pause before reconnecting so a server that closes connections
		// immediately is not hammered in a tight reconnect loop.
		if !b.retryWait(ctx, 250*time.Millisecond) {
			return acpStopReasonUserCancel
		}
	}
}

// consumeStream drains one SSE stream, translating events into ACP notifications
// via handleEvent. It returns a non-empty stopReason when a terminal event is
// observed, acpStopReasonUserCancel when the stream context is cancelled (user
// cancel or shutdown), or "" when the channel closes without a terminal event
// (premature close, e.g. the ACP API's 30s streaming window expiring).
//
// ctx is the per-attempt stream context owned by streamRunEvents: it is
// cancelled right after consumeStream returns so the producer goroutine can
// unblock from its channel send and release the HTTP response body.
func (b *acpBridge) consumeAndStreamHTTP(ctx context.Context, ch <-chan Event, sessionID, msgID string, st *openAIStreamState) string {
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				// Channel closed — the stream ended. If the stream context was
				// cancelled (user cancel / shutdown) report that; otherwise it
				// was a premature close and the caller should re-attempt.
				if ctx.Err() != nil {
					return acpStopReasonUserCancel
				}
				return ""
			}

			term := b.handleEvent(ev, sessionID, msgID, st)

			if ev.Type != "thought" {
				b.srv.notify("session/update", ev)
			}

			if term != "" {
				return term
			}

		case <-ctx.Done():
			// Cancelled while events were still flowing — session/cancel or
			// shutdown. End the turn as user-cancelled.
			return acpStopReasonUserCancel
		}
	}
}

// confirmTerminal reports whether the run has reached a terminal phase
// (completed/failed/cancelled/awaiting). It is used to avoid ending the turn on a
// premature run.completed: the ACP API bounds streaming requests to 30s, which
// can surface a run.completed while the run is still in progress. GetACPRun
// returns a non-JSON (SSE) body for in-progress runs, so an error here means the
// run is still running.
func (b *acpBridge) confirmTerminal(ctx context.Context, runID string) bool {
	run, err := b.client.GetACPRun(ctx, runID)
	if err != nil {
		return false
	}
	switch run.Status {
	case acpStatusCompleted, acpStatusFailed, acpStatusCancelled, acpStatusAwaiting:
		return true
	}
	return false
}

// handleEvent translates one SSE event into ACP notifications. It handles both
// framings the run endpoint produces: typed ACP run/message events, and raw
// OpenAI-schema streaming chunks (bare "data:" lines with no event name,
// proxied from internal/router/stream.go or an OpenAI-schema agent such as
// pkg/go/openai-reference) via handleOpenAIChunk. Returns a non-empty
// stopReason when the event is terminal.
func (b *acpBridge) handleEvent(ev Event, sessionID, msgID string, st *openAIStreamState) string {
	switch ev.Type {
	case "message.part":
		var payload struct {
			Part struct {
				ContentType string `json:"content_type"`
				Content     string `json:"content"`
			} `json:"part"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil && payload.Part.Content != "" {
			b.emitMessageChunk(sessionID, msgID, payload.Part.Content, st)
		}
	case "run.created", "run.in-progress":
		// Surface the run lifecycle as an ACP plan so the client can show
		// progress while the agent works.
		var payload struct {
			Run acpRun `json:"run"`
		}
		_ = json.Unmarshal([]byte(ev.Data), &payload)
		status := acpPlanPending
		if ev.Type == "run.in-progress" {
			status = acpPlanInProgress
		}
		b.emitPlanUpdate(sessionID, payload.Run.RunID, status)
	case "run.completed":
		var payload struct {
			Run acpRun `json:"run"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
			b.emitPlanUpdate(sessionID, payload.Run.RunID, acpPlanCompleted)
			// Emit any output not already streamed as message.part chunks.
			// emitTerminalOutput is suffix-aware: it skips text already
			// delivered, so a partially-streamed run only emits the remainder.
			b.emitTerminalOutput(payload.Run, sessionID, st)
		}
		return acpStopReasonEndTurn
	case "run.failed":
		var payload struct {
			Run acpRun `json:"run"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
			// Emit any output not already streamed (suffix-aware, see above).
			b.emitTerminalOutput(payload.Run, sessionID, st)
		}
		return acpStopReasonEndTurn
	case "run.cancelled":
		return acpStopReasonUserCancel
	case "run.awaiting":
		// The run is waiting for human input. Return the pending-awaiting
		// sentinel so awaitCompletion can run the elicitation/create flow.
		// (Token chunks from the model's turn were already streamed as
		// message.part events; the clarification question itself lives in the
		// run's output, surfaced by handleAwaitingRun on re-poll.)
		return acpPendingAwaiting
	default:
		// Unknown or absent event type: try parsing the payload as an OpenAI
		// streaming chunk. OpenAI-schema streams carry bare "data:" lines with
		// no "event:" name, so they arrive here with an empty Type.
		// (message.created / message.completed also fall through here; they
		// don't parse as chunks, so they are ignored.)
		return b.handleOpenAIChunk(ev.Data, sessionID, msgID, st)
	}
	return ""
}

// handleOpenAIChunk translates one OpenAI streaming payload — a
// chat.completion.chunk or the terminal "[DONE]" sentinel — into ACP session
// updates. This is the openai -> acp translation for streams proxied raw from
// internal/router/stream.go or produced by an OpenAI-schema agent
// (pkg/go/openai-reference): content deltas become agent_message_chunk
// updates, reasoning (extended-thinking) deltas become agent_thought_chunk
// updates, tool-call deltas become tool_call updates, and the terminal
// sentinel / finish_reason ends the turn. Returns a non-empty stopReason when
// the event is terminal.
func (b *acpBridge) handleOpenAIChunk(data, sessionID, msgID string, st *openAIStreamState) string {
	chunk, ok := parseOpenAIChunk(data)
	if !ok {
		return ""
	}
	// if chunk.Done {
	// 	// The OpenAI schema's terminal sentinel. The model-router emits exactly
	// 	// one canonical [DONE] at the true end of a turn (withholding premature
	// 	// upstream ones), so this reliably ends the turn.
	// 	return acpStopReasonEndTurn
	// }
	if chunk.Reasoning != "" {
		b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
			SessionID: sessionID,
			Update: acpUpdate{
				SessionUpdate: acpUpdateThoughtChunk,
				Content:       &acpContentBlock{Type: "text", Text: chunk.Reasoning},
			},
		})
	}
	if chunk.Content != "" {
		b.emitMessageChunk(sessionID, msgID, chunk.Content, st)
	}
	b.mergeToolCallChunks(chunk, sessionID, st)
	switch chunk.FinishReason {
	case "":
		return ""
	case "tool_calls":
		// The model requested tool execution; the router dispatches the tools
		// and continues the turn with a new stream. Not terminal — keep
		// consuming, and if this stream ends first, streamRunEvents falls
		// back to polling for the run's true terminal state.
		return ""
	default:
		return acpStopReasonEndTurn
	}
}

// mergeToolCallChunks merges the tool-call deltas of one OpenAI chunk into the
// stream state (keyed by index) and emits ACP tool_call updates: an
// "in_progress" tool_call when a call is first seen, and a "completed"
// tool_call_update carrying the accumulated invocation when the turn's
// finish_reason is "tool_calls".
func (b *acpBridge) mergeToolCallChunks(chunk openaiChunk, sessionID string, st *openAIStreamState) {
	for _, tc := range chunk.ToolCalls {
		existing, seen := st.toolCalls[tc.Index]
		if !seen {
			st.toolCalls[tc.Index] = tc
			if tc.ID != "" || tc.Name != "" {
				b.emitToolCallUpdate(sessionID, tc, acpUpdateToolCall, acpToolStatusInProgress, nil)
			}
			continue
		}
		if tc.ID != "" {
			existing.ID = tc.ID
		}
		if tc.Name != "" {
			existing.Name = tc.Name
		}
		existing.Arguments += tc.Arguments
		st.toolCalls[tc.Index] = existing
	}
	if chunk.FinishReason != "tool_calls" {
		return
	}
	// The turn's tool calls are complete: emit their accumulated invocations
	// in index order, then reset so a subsequent turn starts fresh.
	indices := make([]int, 0, len(st.toolCalls))
	for idx := range st.toolCalls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		tc := st.toolCalls[idx]
		call := strings.TrimSpace(tc.Name + " " + tc.Arguments)
		b.emitToolCallUpdate(sessionID, tc, acpUpdateToolCallUpd, acpToolStatusCompleted,
			[]acpContentBlock{{Type: "text", Text: call}})
	}
	st.toolCalls = map[int]openaiToolCall{}
}

// emitMessageChunk sends one assistant text delta as an agent_message_chunk and
// records it in the stream state so the polling fallback can skip it.
func (b *acpBridge) emitMessageChunk(sessionID, msgID, text string, st *openAIStreamState) {
	if text == "" {
		return
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpUpdate{
			SessionUpdate: acpUpdateMessageChunk,
			MessageID:     msgID,
			Content:       &acpContentBlock{Type: "text", Text: text},
		},
	})
	if st != nil {
		st.streamed.WriteString(text)
	}
}

// emitPlanUpdate surfaces the run lifecycle as an ACP plan with a single
// entry, so ACP clients can render progress while the agent works.
func (b *acpBridge) emitPlanUpdate(sessionID, runID, status string) {
	content := "agent-orca run"
	if runID != "" {
		content = "agent-orca run " + runID
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpUpdate{
			SessionUpdate: acpUpdatePlan,
			PlanEntries: []acpPlanEntry{{
				Content:  content,
				Priority: acpPlanPriority,
				Status:   status,
			}},
		},
	})
}

// emitToolCallUpdate sends one ACP tool_call / tool_call_update notification.
func (b *acpBridge) emitToolCallUpdate(sessionID string, tc openaiToolCall, update, status string, content []acpContentBlock) {
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("tool_%d", tc.Index)
	}
	title := tc.Name
	if title == "" {
		title = "tool call"
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpToolCallUpdate{
			SessionUpdate: update,
			ToolCallID:    id,
			Title:         title,
			Kind:          "other",
			Status:        status,
			Content:       content,
		},
	})
}

// emitTerminalOutput surfaces any final run output not captured by token chunks
// (e.g. on the polling path) as assistant text chunks. Run output produced by
// OpenAI-schema agents (pkg/go/openai-reference) or the model-router proxy is a
// raw OpenAI SSE stream; this translates it so the user sees the assistant's
// text and reasoning rather than the raw chunk JSON. Only the suffix not
// already streamed is emitted, so a premature stream close followed by
// polling never drops or duplicates the response.
func (b *acpBridge) emitTerminalOutput(run acpRun, sessionID string, st *openAIStreamState) {
	var combined strings.Builder
	for _, m := range run.Output {
		for _, part := range m.Parts {
			if part.Content != "" {
				combined.WriteString(part.Content)
			}
		}
	}
	content := combined.String()
	if content == "" {
		return
	}
	// If the output is a raw OpenAI SSE stream, extract the assistant text and
	// reasoning; otherwise treat the raw text as the content.
	text, reasoning, isSSE := extractOpenAIStreamText(content)
	if !isSSE {
		text = content
	}
	b.emitThoughtRemainder(reasoning, sessionID, st)
	b.emitTextRemainder(text, "msg_"+run.RunID, sessionID, st)
}

// emitThoughtRemainder emits reasoning as an agent_thought_chunk, skipping any
// prefix already streamed as thought chunks.
func (b *acpBridge) emitThoughtRemainder(reasoning, sessionID string, st *openAIStreamState) {
	if reasoning == "" {
		return
	}
	rem := reasoning
	if st != nil {
		seen := st.streamedThought.String()
		if strings.HasPrefix(reasoning, seen) {
			rem = reasoning[len(seen):]
		}
	}
	if rem == "" {
		return
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpUpdate{
			SessionUpdate: acpUpdateThoughtChunk,
			Content:       &acpContentBlock{Type: "text", Text: rem},
		},
	})
	if st != nil {
		st.streamedThought.WriteString(rem)
	}
}

// emitTextRemainder emits text as an agent_message_chunk, skipping any prefix
// already streamed as message chunks (tracked in st).
func (b *acpBridge) emitTextRemainder(text, msgID, sessionID string, st *openAIStreamState) {
	if text == "" {
		return
	}
	if st != nil {
		streamed := st.streamed.String()
		if strings.HasPrefix(text, streamed) {
			text = text[len(streamed):]
		}
	}
	b.emitMessageChunk(sessionID, msgID, text, st)
}

// handleAwaitingRun processes a run in the "awaiting" state. It emits the
// clarification question as agent_message_chunk notifications, then sends an
// elicitation/create request to the client asking for the user's input. If the
// user accepts, it resumes the run (creating a continuation run via the ACP
// server) and returns the continuation run ID. Returns ("", false) if the user
// declined, the elicitation failed, or an error occurred.
func (b *acpBridge) handleAwaitingRun(ctx context.Context, run acpRun, sessionID, runID string, st *openAIStreamState) (string, bool) {
	// Emit the clarification question as agent output so the user can see what
	// was asked before the elicitation form appears. emitTerminalOutput is
	// suffix-aware: if the question was already streamed as message.part
	// chunks, only the remainder (often nothing) is re-emitted.
	b.emitTerminalOutput(run, sessionID, st)

	question := ""
	if run.AwaitRequest != nil && run.AwaitRequest.Question != "" {
		question = run.AwaitRequest.Question
	} else if len(run.Output) > 0 && len(run.Output[0].Parts) > 0 {
		question = run.Output[0].Parts[0].Content
	}
	if question == "" {
		question = "The agent is waiting for your input."
	}

	resp, err := b.srv.sendRequest(ctx, acpMethodElicitationCreate, acpElicitParams{
		Message: question,
		Mode:    "form",
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer": map[string]any{
					"type":        "string",
					"description": "Your response to the agent's question",
				},
			},
			"required": []string{"answer"},
		},
	})
	if err != nil {
		b.srv.logf("acp: elicitation/create failed: %v\n", err)
		return "", false
	}

	var elicit acpElicitResponse
	raw, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(raw, &elicit); err != nil {
		b.srv.logf("acp: unmarshal elicitation response: %v\n", err)
		return "", false
	}

	if elicit.Action != "accept" {
		b.srv.logf("acp: elicitation %s by user\n", elicit.Action)
		return "", false
	}

	answer, _ := elicit.Content["answer"].(string)
	continuationID, err := b.client.ResumeRun(ctx, runID, answer)
	if err != nil {
		b.srv.logf("acp: resume run %s: %v\n", runID, err)
		return "", false
	}
	if continuationID == "" {
		// No continuation run returned — the server may have resumed in place.
		// Fall back to polling the original run.
		return runID, true
	}
	return continuationID, true
}

// tryResumeAwaiting checks whether the session has a prior run still in the
// "awaiting" state (e.g. the user declined the elicitation form and is now
// answering in a new session/prompt). If so, it resumes that run with the user's
// input text as the answer. Returns the run ID and true on success.
func (b *acpBridge) tryResumeAwaiting(ctx context.Context, sessionID string, prompt []acpContentBlock) (string, bool) {
	b.mu.Lock()
	sess := b.sessions[sessionID]
	b.mu.Unlock()
	if sess == nil || sess.RunID == "" {
		return "", false
	}
	run, err := b.client.GetACPRun(ctx, sess.RunID)
	if err != nil || run.Status != acpStatusAwaiting {
		return "", false
	}

	// Extract text from the prompt blocks as the user's answer.
	var parts []string
	for _, block := range prompt {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	answer := strings.Join(parts, "\n")

	continuationID, err := b.client.ResumeRun(ctx, sess.RunID, answer)
	if err != nil {
		b.srv.logf("acp: resume awaiting run %s: %v\n", sess.RunID, err)
		return "", false
	}
	if continuationID != "" {
		return continuationID, true
	}
	return sess.RunID, true
}

// pollToCompletion polls GET /runs/{id} until terminal. Used as the SSE
// fallback when the state store is unavailable or the run never reaches
// in-progress in time.
func (b *acpBridge) pollToCompletion(ctx context.Context, runID, sessionID string, st *openAIStreamState) string {
	for {
		if err := ctx.Err(); err != nil {
			return acpStopReasonUserCancel
		}
		run, err := b.client.GetACPRun(ctx, runID)
		if err == nil {
			switch run.Status {
			case acpStatusCompleted:
				// Emit any output not already streamed (suffix-aware, see above).
				b.emitTerminalOutput(run, sessionID, st)
				return acpStopReasonEndTurn
			case acpStatusFailed, acpStatusCancelled:
				b.emitTerminalOutput(run, sessionID, st)
				return runStatusToStopReason(run.Status)
			case acpStatusAwaiting:
				return acpPendingAwaiting
			}
		}
		if !b.retryWait(ctx, 300*time.Millisecond) {
			return acpStopReasonUserCancel
		}
	}
}

func (b *acpBridge) retryWait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// --- session/cancel ---

// handleCancel aborts the in-flight run for a session. Accepts the sessionId
// either as JSON params ({sessionId:...}) or, for the notification form, from
// the same shape.
func (b *acpBridge) handleCancel( //nolint:gocyclo
	ctx context.Context,
	params json.RawMessage,
	isNotification bool,
	id *json.RawMessage,
) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	b.mu.Lock()
	sess, ok := b.sessions[p.SessionID]
	b.mu.Unlock()
	if !ok || sess.Cancel == nil {
		if !isNotification && id != nil {
			b.srv.respond(id, map[string]any{})
		}
		return
	}
	// Stopping the run server-side; the in-flight awaitCompletion/streamRun
	// goroutine observes the cancelled context and ends the turn.
	_ = b.client.CancelACPRun(ctx, sess.RunID)
	sess.Cancel()
	if !isNotification && id != nil {
		b.srv.respond(id, map[string]any{})
	}
}

// --- session lifecycle: resume/list/close/delete ---

func handleSessionResume(_ context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p struct {
		SessionID string `json:"sessionId,omitempty"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	// Ensure the session exists so subsequent session/prompt calls chain
	// runs correctly (agent-orca restores conversation context via session_id).
	if p.SessionID != "" {
		b.mu.Lock()
		if _, ok := b.sessions[p.SessionID]; !ok {
			b.sessions[p.SessionID] = &acpSession{}
		}
		b.mu.Unlock()
	}
	return map[string]any{}, nil
}

func handleSessionList(_ context.Context, _ *acpBridge, _ jsonrpcRequest) (any, error) {
	return map[string]any{"sessions": []acpSessionNewResponse{}}, nil
}

func handleSessionClose(_ context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	b.forgetSession(p.SessionID, "")
	return map[string]any{}, nil
}

func handleSessionDelete(_ context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	b.forgetSession(p.SessionID, "")
	return map[string]any{}, nil
}

// forgetSession cancels an in-flight run (if any) and drops the session entry.
func (b *acpBridge) forgetSession(sessionID, runID string) {
	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if ok && runID != "" && sess.RunID != runID {
		ok = false
	}
	if ok {
		delete(b.sessions, sessionID)
	}
	b.mu.Unlock()
	if ok && sess.Cancel != nil {
		sess.Cancel()
	}
}

func (b *acpBridge) nextSessionID() string {
	n := b.seq.Add(1)
	return "zed-" + sessionIDEscaped(n)
}

// sessionIDEscaped produces a short, URL-safe id without adding crypto deps.
func sessionIDEscaped(n int64) string {
	const hex = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 16)
	for n > 0 {
		b = append([]byte{hex[n&0xf]}, b...)
		n >>= 4
	}
	return string(b)
}
