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
// responses and logged at serve startup (so a Zed session's log panel shows
// which bridge build is actually running — stale subprocesses are a real
// failure mode). It is independent of the ACP protocol version (1).
// 0.2.0: fs/* relay (read/write/list) + write-approval diagnosability.
// 0.3.0: trace-event relay — model-router trace events (tool calls, results,
// guardrails, provider fallbacks, MCP discovery) stream to the editor as
// session updates during the turn instead of the client showing only a
// spinner until the final response arrives.
const acpBridgeVersion = "0.3.0"

// acpSession is the live state for one Zed session.
type acpSession struct {
	SessionID string
	RunID     string
	Cancel    context.CancelFunc
	// Cwd is the workspace directory the editor opened the session in
	// (session/new "cwd"); the primary root for the fs/* relay.
	Cwd string
	// WritesAllowed is set once the user approves a workspace write with
	// --fs-remember-approval active: subsequent writes in this session skip
	// the per-write elicitation.
	WritesAllowed bool

	// traceToolSeq numbers trace-relayed tool calls so every call gets a
	// unique ACP toolCallId even when the same tool runs twice in a session
	// (same-tool calls with a deterministic id collapse into one editor
	// entry). pendingTraceTool maps toolKey → the id awaiting its result.
	traceToolSeq     int
	pendingTraceTool map[string]string
}

// openAIStreamState accumulates translation state across the events of one
// streamed run: the assistant text streamed so far (so a premature stream
// close + polling fallback can emit only the remainder), the reasoning
// streamed so far (same dedup), and the tool-call deltas merged so far
// (keyed by OpenAI index, mirroring mergeToolCallDeltas in internal/router).
// traceSeen dedups trace events across reconnects: TailTokens replays all
// prior Redis entries on each new stream attempt, so without it every
// window re-emits the tool calls/results already shown.
type openAIStreamState struct {
	streamed        strings.Builder // assistant text emitted as message chunks
	streamedThought strings.Builder // reasoning emitted as thought chunks
	toolCalls       map[int]openaiToolCall
	traceSeen       map[string]bool // raw trace-event JSON → already emitted
	// Replay swallowing: every stream attempt replays the run's token stream
	// from the beginning. Before each attempt the bridge snapshots how much
	// text has already been emitted; the first N bytes of the replay are
	// known-duplicates and are swallowed instead of re-emitted (which would
	// duplicate the agent's entire output in the editor on every reconnect).
	replayTokenSkip   int
	replayThoughtSkip int
}

// acpBridge owns method dispatch and per-session run state.
type acpBridge struct {
	srv       *acpStdioServer
	client    *Client
	agentName string
	manifest  ACPAgentManifest
	// fsOpts carries the workstation-side fs policy knobs (--fs-roots,
	// --fs-require-approval).
	fsOpts fsBridgeOptions

	mu       sync.Mutex
	sessions map[string]*acpSession
	seq      atomic.Int64
}

func newACPBridge(srv *acpStdioServer, client *Client, agentName string) *acpBridge { //nolint:unparam
	return newACPBridgeWithOptions(srv, client, agentName, fsBridgeOptions{})
}

// newACPBridgeWithOptions builds the bridge with fs relay policy knobs.
func newACPBridgeWithOptions(srv *acpStdioServer, client *Client, agentName string, fsOpts fsBridgeOptions) *acpBridge { //nolint:lll
	return &acpBridge{
		srv:       srv,
		client:    client,
		agentName: agentName,
		fsOpts:    fsOpts,
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
	b.mu.Lock()
	b.manifest = man
	b.mu.Unlock()

	version := p.ProtocolVersion
	if version == 0 || version != acpProtocolVersion {
		// Client omitted protocolVersion (or sent an unparseable value), or
		// requested a version we don't support: negotiate down to our latest.
		version = acpProtocolVersion
	}
	// Filesystem capability (issue #34): advertise exactly what the agent's
	// policy grants, so a readOnly agent never leads the editor to believe it
	// can write. Zed answers the fs/* calls; the bridge relays pod requests to
	// it and enforces workspace confinement on every call.
	fsCaps := acpFSCaps{}
	if man.Filesystem != nil {
		fsCaps.ReadTextFile = true
		fsCaps.WriteTextFile = man.Filesystem.Mode == "readWrite"
	}
	return acpInitializeResponse{
		ProtocolVersion: version,
		AgentCapabilities: acpAgentCaps{
			LoadSession:        false,
			PromptCapabilities: acpPromptCaps{Image: false, Audio: false, EmbeddedContext: false},
			MCPCapabilities:    acpMCPCaps{HTTP: false, SSE: false},
			FS:                 fsCaps,
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
		if err := json.Unmarshal(req.Params, &p); err != nil {
			b.srv.logf("acp: session/new params undecodable (%v): %.200s\n", err, string(req.Params))
		}
	}
	if p.Cwd == "" {
		// ACP v1 requires cwd on session/new; a client that omits it leaves the
		// fs/* relay without a workspace root. Surface it — the relay falls back
		// to the bridge process's working directory (see workspaceRoots).
		b.srv.logf("acp: session/new arrived without cwd (params: %.200s) — fs relay will "+
			"confine to the bridge process working directory\n", string(req.Params))
	}
	sessionID := p.SessionID
	if sessionID == "" {
		sessionID = b.nextSessionID()
		b.srv.logf("acp: session/new without sessionId — generated %q\n", sessionID)
	}
	// Update the session record in place: Zed may re-send session/new for a
	// session whose prompt turn is still in flight, and replacing the record
	// would drop its RunID — orphaning the fs/* relay watcher mid-turn (fs
	// calls then pend until the router's fs timeout).
	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		sess = &acpSession{SessionID: sessionID}
		b.sessions[sessionID] = sess
	}
	sess.Cwd = p.Cwd
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
	// cancels in-flight runs as well. The session record may already exist
	// (session/new stored the workspace cwd): update it in place — replacing
	// it would drop the cwd and break the fs/* relay's workspace confinement.
	runCtx, cancel := context.WithCancel(ctx)
	b.registerRun(sessionID, runID, cancel)
	defer func() {
		cancel() // release the context on completion (or cancellation)
		b.mu.Lock()
		if cur, ok := b.sessions[sessionID]; ok && cur.RunID == runID {
			cur.Cancel = nil
		}
		b.mu.Unlock()
	}()

	// Start the fs/* relay watcher (issue #34) before the run starts: fs tool
	// calls fire mid-turn. It follows continuation runs via the session record
	// (awaiting→resume chains a new run under the same session) and dies with
	// the run context via the defer above.
	go b.watchFSSession(runCtx, sessionID)

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
		st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}, traceSeen: map[string]bool{}}
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
			// Keep the session record (and the fs/* relay watcher) pointed at
			// the continuation run.
			b.setSessionRun(sessionID, contID)
			continue
		}
		return acpStopReasonEndTurn
	}
}

// streamRunEvents consumes the SSE stream from GET /runs/{id} (Accept:
// text/event-stream) and translates it into ACP session/update notifications.
//
// The ACP API bounds streaming requests (non-SSE requests to 30s; SSE to the
// 10-minute stream window), so a long response is delivered
// in windows: each window ends either with a genuine terminal event or with
// the connection dropping. A terminal event is verified against the run's real
// status (confirmTerminal) before ending the turn, so a premature run.completed
// caused by a window boundary does not truncate the response — instead the
// stream is re-attempted. Before each re-attempt the run's CRD state is polled
// directly: if the run is already terminal (or awaiting input) the turn
// finalizes immediately even when the stream never delivered a terminal event
// (e.g. the terminal trace event landed in the gap between windows).
func (b *acpBridge) streamRunEvents(ctx context.Context, runID, sessionID string, st *openAIStreamState) string {
	for attempt := 0; ; attempt++ {
		// Gap-filling CRD poll: when the previous stream window ended without a
		// terminal event (the terminal trace marker landed in the gap between
		// windows, or the stream dropped for any other reason), consult the
		// run's authoritative CRD state before re-streaming — a terminal (or
		// awaiting) phase ends the turn immediately.
		if attempt > 0 {
			if run, err := b.client.GetACPRun(ctx, runID); err == nil {
				if stop := b.terminalStopForRun(run, sessionID, st); stop != "" {
					return stop
				}
			}
		}
		// Every attempt replays the run's token stream from the start — arm the
		// swallow counters so already-emitted text is not re-emitted (which
		// would duplicate the agent's entire output in the editor).
		st.replayTokenSkip = st.streamed.Len()
		st.replayThoughtSkip = st.streamedThought.Len()
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
			// The stream signalled a terminal event but the CRD hasn't flipped yet
			// (the runner is still persisting output / the operator is patching
			// status): an imminent event. Poll briefly instead of re-streaming —
			// re-streaming replays the entire token stream, duplicating every
			// already-rendered token in the editor. If the CRD still isn't
			// terminal after the bounded wait, fall back to the re-attempt loop
			// (replay-safe via the swallow counters).
			pollDeadline := time.Now().Add(6 * time.Second)
			for time.Now().Before(pollDeadline) {
				select {
				case <-ctx.Done():
					return acpStopReasonUserCancel
				case <-time.After(300 * time.Millisecond):
				}
				if run, err := b.client.GetACPRun(ctx, runID); err == nil {
					if stop := b.terminalStopForRun(run, sessionID, st); stop != "" {
						return stop
					}
				}
			}
			// Bounded poll exhausted: fall through to re-attempting the stream.
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
func (b *acpBridge) consumeAndStreamHTTP(
	ctx context.Context, ch <-chan Event, sessionID, msgID string, st *openAIStreamState,
) string {
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

			// Model-router trace events (tool calls/results, guardrails,
			// provider fallbacks, MCP discovery, routing, hindsight) ride the
			// run stream as `event: trace` SSE frames. Translate them into
			// session/update notifications so editors render live agent
			// activity mid-turn instead of a bare spinner. st.traceSeen dedups
			// across reconnects: TailTokens replays prior Redis entries on
			// every new stream attempt.
			if ev.Type == acpEventTypeTrace {
				if tev, ok := parseACPTraceEvent(ev.Data); ok {
					if tev.Type == "thought" {
						// Reasoning deltas bypass traceSeen — identical consecutive
						// deltas produce identical JSON, so exact-JSON dedup would
						// drop real thinking text. Replay dedup uses the same
						// byte-swallow counter as the OpenAI-chunk reasoning path.
						b.emitTraceThoughtDelta(sessionID, tev.Content, st)
					} else if !st.traceSeen[ev.Data] {
						st.traceSeen[ev.Data] = true
						b.handleTraceEvent(tev, sessionID)
					}
				}
				continue
			}

			term := b.handleEvent(ev, sessionID, msgID, st)
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

// terminalStopForRun maps a fetched run's status to a stop reason when the run
// has reached a terminal (or input-awaiting) phase, emitting any output that
// was never streamed (suffix-aware, so already-delivered text is not
// duplicated). Returned reasons:
//
//	completed/failed → end_turn, cancelled → user-cancelled,
//	awaiting → the internal pending-awaiting sentinel (the caller's loop runs
//	the elicitation flow). Empty string = still running.
func (b *acpBridge) terminalStopForRun(run acpRun, sessionID string, st *openAIStreamState) string {
	switch run.Status {
	case acpStatusCompleted, acpStatusFailed:
		b.emitTerminalOutput(run, sessionID, st)
		return acpStopReasonEndTurn
	case acpStatusCancelled:
		return acpStopReasonUserCancel
	case acpStatusAwaiting:
		return acpPendingAwaiting
	}
	return ""
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
		reasoning := chunk.Reasoning
		if st != nil && st.replayThoughtSkip > 0 {
			// Reconnect replay: swallow the already-streamed prefix (see
			// emitMessageChunk for the rationale).
			if len(reasoning) <= st.replayThoughtSkip {
				st.replayThoughtSkip -= len(reasoning)
				reasoning = ""
			} else {
				reasoning = reasoning[st.replayThoughtSkip:]
				st.replayThoughtSkip = 0
			}
		}
		if reasoning != "" {
			b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
				SessionID: sessionID,
				Update: acpUpdate{
					SessionUpdate: acpUpdateThoughtChunk,
					Content:       &acpContentBlock{Type: "text", Text: reasoning},
				},
			})
		}
		if st != nil {
			// Track the EMITTED suffix (not the raw chunk) so the skip counters
			// and emitThoughtRemainder's suffix check stay aligned.
			st.streamedThought.WriteString(reasoning)
		}
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
	if st != nil && st.replayTokenSkip > 0 {
		// Reconnect replay: this and usually several following chunks repeat
		// text already streamed in a previous window. Swallow the known-
		// duplicate prefix, emit the remainder.
		if len(text) <= st.replayTokenSkip {
			st.replayTokenSkip -= len(text)
			return
		}
		text = text[st.replayTokenSkip:]
		st.replayTokenSkip = 0
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
func (b *acpBridge) emitToolCallUpdate(
	sessionID string, tc openaiToolCall, update, status string, content []acpContentBlock,
) {
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("tool_%d", tc.Index)
	}
	title := tc.Name
	if title == "" {
		title = "tool call"
	}
	// ACP v1 ToolCallContent entries are discriminated wrappers: a bare
	// ContentBlock ("type":"text") is an unknown variant for strict clients.
	wrapped := make([]acpToolCallContent, 0, len(content))
	for i := range content {
		wrapped = append(wrapped, acpToolCallContent{Type: "content", Content: &content[i]})
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpToolCallUpdate{
			SessionUpdate: update,
			ToolCallID:    id,
			Title:         title,
			Kind:          "other",
			Status:        status,
			Content:       wrapped,
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
		text = strings.TrimPrefix(text, st.streamed.String())
	}
	b.emitMessageChunk(sessionID, msgID, text, st)
}

// handleAwaitingRun processes a run in the "awaiting" state. It emits the
// clarification question as agent_message_chunk notifications, then sends an
// elicitation/create request to the client asking for the user's input. If the
// user accepts, it resumes the run (creating a continuation run via the ACP
// server) and returns the continuation run ID. Returns ("", false) if the user
// declined, the elicitation failed, or an error occurred.
func (b *acpBridge) handleAwaitingRun(
	ctx context.Context, run acpRun, sessionID, runID string, st *openAIStreamState,
) (string, bool) {
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
		SessionID: sessionID,
		Message:   question,
		Mode:      "form",
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
