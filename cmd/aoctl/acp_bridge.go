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

// acpBridgeVersion is the ACP protocol-bridge version reported in initialize
// responses. It is independent of the ACP spec version (0.2.0) and tracks the
// aoctl bridge implementation.
const acpBridgeVersion = "0.1.0"

// acpSession is the live state for one Zed session.
type acpSession struct {
	SessionID string
	RunID     string
	Cancel    context.CancelFunc
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
		return nil, err
	}
	b.manifest = man

	version := p.ProtocolVersion
	if version == "" {
		version = "0.2.0"
	}
	return acpInitializeResponse{
		ProtocolVersion: version,
		AgentCapabilities: acpAgentCaps{
			LoadSession:        false,
			PromptCapabilities: acpPromptCaps{Image: false, Audio: false, EmbeddedContext: false},
			MCPCapabilities:    acpMCPCaps{HTTP: false, SSE: false},
			SessionCapabilities: map[string]any{
				"prompt": true,
				"cancel": true,
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
	run, err := b.client.CreateAgentRun(ctx, b.agentName, ACPRunRequest{
		Input:     toAgentOrcaInput(p.Prompt),
		SessionID: sessionID,
	})
	if err != nil {
		b.srv.respondErr(req.ID, -32001, "create run: "+err.Error())
		return promptHandled{}, nil
	}
	runID := run.RunID

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

// awaitCompletion drives one run to a terminal state. It polls the run status
// and, once the run reaches "in-progress", hands off to the live SSE token
// stream; if SSE is unavailable (e.g. no state store in local dev) it falls
// back to polling.
func (b *acpBridge) awaitCompletion(ctx context.Context, runID, sessionID string) string {
	for {
		if err := ctx.Err(); err != nil {
			return acpStopReasonUserCancel
		}
		run, err := b.client.GetACPRun(ctx, runID)
		if err != nil {
			b.srv.logf("acp: poll run error: %v\n", err)
			if !b.retryWait(ctx, 200*time.Millisecond) {
				return acpStopReasonUserCancel
			}
			continue
		}
		switch run.Status {
		case acpStatusInProgress:
			return b.streamRunEvents(ctx, runID, sessionID)
		case acpStatusCompleted, acpStatusFailed, acpStatusCancelled:
			b.emitTerminalOutput(run, sessionID)
			return runStatusToStopReason(run.Status)
		case acpStatusAwaiting:
			b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
				SessionID: sessionID,
				Update:    acpUpdate{SessionUpdate: acpStatusAwaiting},
			})
			return acpStatusAwaiting
		default: // created / pending
			b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
				SessionID: sessionID,
				Update: acpUpdate{
					SessionUpdate: "plan",
					PlanEntries: []acpPlanEntry{{
						Content:  "Starting agent-orca run " + runID,
						Priority: "low",
						Status:   "in-progress",
					}},
				},
			})
			if !b.retryWait(ctx, 200*time.Millisecond) {
				return acpStopReasonUserCancel
			}
		}
	}
}

// streamRunEvents consumes the SSE stream from GET /runs/{id} (Accept:
// text/event-stream) and translates it into session/update notifications.
func (b *acpBridge) streamRunEvents(ctx context.Context, runID, sessionID string) string {
	ch, err := b.client.StreamRunEvents(ctx, runID)
	if err != nil {
		// Race: run left in-progress before SSE opened, or no state store.
		b.srv.logf("acp: sse unavailable (%v), polling run %s\n", err, runID)
		return b.pollToCompletion(ctx, runID, sessionID)
	}

	msgID := "msg_" + runID
	emitted := false
	for {
		select {
		case <-ctx.Done():
			return acpStopReasonUserCancel
		case ev, ok := <-ch:
			if !ok {
				// Channel closed; final-poll to determine stop reason.
				return b.pollToCompletion(ctx, runID, sessionID)
			}
			if term := b.handleEvent(ev, sessionID, msgID, &emitted); term != "" {
				return term
			}
		}
	}
}

// handleEvent translates one SSE event into ACP notifications. Returns a
// non-empty stopReason when the event is terminal.
func (b *acpBridge) handleEvent(ev Event, sessionID, msgID string, emitted *bool) string {
	switch ev.Type {
	case "message.part":
		var payload struct {
			Part struct {
				ContentType string `json:"content_type"`
				Content     string `json:"content"`
			} `json:"part"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil && payload.Part.Content != "" {
			b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
				SessionID: sessionID,
				Update: acpUpdate{
					SessionUpdate: "agent_message_chunk",
					MessageID:     msgID,
					Content:       &acpContentBlock{Type: "text", Text: payload.Part.Content},
				},
			})
			*emitted = true
		}
	case "run.completed":
		var payload struct {
			Run acpRun `json:"run"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
			b.emitTerminalOutput(payload.Run, sessionID)
		}
		return acpStopReasonEndTurn
	case "run.failed":
		var payload struct {
			Run acpRun `json:"run"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
			b.emitTerminalOutput(payload.Run, sessionID)
		}
		return acpStopReasonError
	case "run.cancelled":
		return acpStopReasonUserCancel
	case "run.awaiting":
		return acpStatusAwaiting
		// run.created / run.in-progress / message.created / message.completed are
		// either already announced or folded into the chunk stream; ignore them so
		// we don't emit spurious empty notifications.
	}
	return ""
}

// emitTerminalOutput surfaces any final run output not captured by token chunks
// (e.g. on the polling path) as assistant text chunks.
func (b *acpBridge) emitTerminalOutput(run acpRun, sessionID string) {
	for _, m := range run.Output {
		for _, part := range m.Parts {
			if part.Content == "" {
				continue
			}
			b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
				SessionID: sessionID,
				Update: acpUpdate{
					SessionUpdate: "agent_message_chunk",
					MessageID:     "msg_" + run.RunID,
					Content:       &acpContentBlock{Type: "text", Text: part.Content},
				},
			})
		}
	}
}

// pollToCompletion polls GET /runs/{id} until terminal. Used as the SSE
// fallback when the state store is unavailable or the run never reaches
// in-progress in time.
func (b *acpBridge) pollToCompletion(ctx context.Context, runID, sessionID string) string {
	for {
		if err := ctx.Err(); err != nil {
			return acpStopReasonUserCancel
		}
		run, err := b.client.GetACPRun(ctx, runID)
		if err == nil {
			switch run.Status {
			case acpStatusCompleted:
				b.emitTerminalOutput(run, sessionID)
				return acpStopReasonEndTurn
			case acpStatusFailed, acpStatusCancelled:
				b.emitTerminalOutput(run, sessionID)
				return runStatusToStopReason(run.Status)
			case acpStatusAwaiting:
				return acpStatusAwaiting
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

// --- session lifecycle: resume/list/close/delete (stubs for v1) ---

func handleSessionResume(_ context.Context, _ *acpBridge, _ jsonrpcRequest) (any, error) {
	// ACP v1 doesn't require loadSession; acknowledge.
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
