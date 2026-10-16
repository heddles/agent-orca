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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fs confinement policy ---------------------------------------------------

func TestFSConfine(t *testing.T) {
	root := "/home/user/project"
	roots := []string{root}
	cases := []struct {
		name   string
		roots  []string
		path   string
		want   string
		wantOK bool
	}{
		{"relative inside", roots, "src/main.go", "/home/user/project/src/main.go", true},
		{"dot-relative inside", roots, "./cmd/x.go", "/home/user/project/cmd/x.go", true},
		{"absolute inside", roots, "/home/user/project/README.md", "/home/user/project/README.md", true},
		{"traversal escape", roots, "../../etc/passwd", "", false},
		{"dotdot only", roots, "..", "", false},
		{"empty path", roots, "", "", false},
		{"dot is the workspace root", roots, ".", root, true},
		{"dot with extra roots", []string{root, "/opt/work"}, ".", root, true}, // first root wins
		{"absolute sibling escape", roots, "/home/user/other/x.go", "", false},
		{"extra root allowed", []string{root, "/opt/work"}, "/opt/work/a.txt", "/opt/work/a.txt", true},
		{"outside extra root", []string{root, "/opt/work"}, "/opt/secrets", "", false},
		{"first root wins", []string{root, "/opt/work"}, "src/main.go", "/home/user/project/src/main.go", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := fsConfine(c.roots, c.path)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("fsConfine(%v, %q) = (%q, %v), want (%q, %v)", c.roots, c.path, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// --- initialize: fs capability advertisement ----------------------------------

func TestACPBridgeInitializeAdvertisesFSCaps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		policy    *ACPFSPolicy
		wantRead  bool
		wantWrite bool
	}{
		{name: "no filesystem policy", policy: nil, wantRead: false, wantWrite: false},
		{name: "readOnly", policy: &ACPFSPolicy{Mode: "readOnly"}, wantRead: true, wantWrite: false},
		{name: "readWrite", policy: &ACPFSPolicy{Mode: "readWrite"}, wantRead: true, wantWrite: true},
		{name: "empty mode treated as readOnly", policy: &ACPFSPolicy{}, wantRead: true, wantWrite: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(ACPAgentManifest{Name: "test-agent", Filesystem: tc.policy})
			}))
			defer srv.Close()

			c := newClient(srv.URL, srv.URL, testToken, time.Second, false)
			bridge := newACPBridge(nil, c, "test-agent")
			resp, err := handleInitialize(context.Background(), bridge, jsonrpcRequest{})
			if err != nil {
				t.Fatal("initialize:", err)
			}
			var init acpInitializeResponse
			unmarshalResult(t, newJSONRPCResult(resp), &init)
			if init.AgentCapabilities.FS.ReadTextFile != tc.wantRead ||
				init.AgentCapabilities.FS.WriteTextFile != tc.wantWrite {
				t.Fatalf("fs caps = %+v, want read=%v write=%v",
					init.AgentCapabilities.FS, tc.wantRead, tc.wantWrite)
			}
		})
	}
}

// newJSONRPCResult wraps a handler result the same way Dispatch would.
func newJSONRPCResult(result any) jsonrpcMessage {
	return jsonrpcMessage{JSONRPC: "2.0", Result: result}
}

// --- relay round trips --------------------------------------------------------

// fsFakeACPAPI emulates the ACP API fs endpoints for bridge tests: pending
// lists and response posts.
type fsFakeACPAPI struct {
	mu       sync.Mutex
	pending  []acpfsRelayRequest
	response *acpfsRelayResponse
	acpAPI   *httptest.Server
}

func newFSFakeACPAPI(t *testing.T, pending []acpfsRelayRequest) *fsFakeACPAPI {
	t.Helper()
	f := &fsFakeACPAPI{pending: pending}
	mux := http.NewServeMux()
	mux.HandleFunc("/agents/test-agent", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ACPAgentManifest{
			Name:       "test-agent",
			Filesystem: &ACPFSPolicy{Mode: "readWrite", WriteApproval: false},
		})
	})
	mux.HandleFunc("/runs/run-1/fs/pending", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			reqs := f.pending
			f.pending = nil
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": reqs})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/runs/run-1/fs/pending/req-1/response", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var resp acpfsRelayResponse
		if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.response = &resp
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.acpAPI = httptest.NewServer(mux)
	t.Cleanup(f.acpAPI.Close)
	return f
}

// fsEditorClient simulates the ACP client (Zed) on the other side of the
// stdio channel: answers every fs/read_text_file and fs/write_text_file it
// sees, recording what it received.
type fsEditorClient struct {
	mu                sync.Mutex
	reads             []acpFSReadParams
	writes            []acpFSWriteParams
	elicit            []acpElicitParams
	readOut           string // content to return for reads
	writeOK           bool   // whether writes "succeed"
	elicitDecline     bool   // respond to the approval form with action "decline"
	elicitAcceptEmpty bool   // respond accept with no form content (shape-tolerant clients)
	elicitFail        bool   // fail the elicitation request (no editor support)
}

// run drives the fake editor loop until ctx is done.
func (e *fsEditorClient) run(ctx context.Context, out io.Reader, in io.Writer) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if !scanner.Scan() {
			return
		}
		line := scanner.Text()
		if line == "" {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			fmt.Fprintf(os.Stderr, "fsEditor: undecodable line %q: %v\n", line, err)
			continue
		}
		if msg.ID == nil || msg.Method == "" {
			continue
		}
		raw, _ := json.Marshal(msg.Params)
		switch msg.Method {
		case acpMethodFSRead:
			var p acpFSReadParams
			_ = json.Unmarshal(raw, &p)
			e.mu.Lock()
			e.reads = append(e.reads, p)
			e.mu.Unlock()
			content := e.readOut
			e.reply(in, string(*msg.ID), map[string]any{"content": content})
		case acpMethodFSWrite:
			var p acpFSWriteParams
			_ = json.Unmarshal(raw, &p)
			e.mu.Lock()
			e.writes = append(e.writes, p)
			e.mu.Unlock()
			if e.writeOK {
				e.reply(in, string(*msg.ID), nil)
			} else {
				e.replyErr(in, string(*msg.ID), -32000, "refused")
			}
		case acpMethodElicitationCreate:
			var p acpElicitParams
			_ = json.Unmarshal(raw, &p)
			e.mu.Lock()
			e.elicit = append(e.elicit, p)
			e.mu.Unlock()
			switch {
			case e.elicitFail:
				// Editor without elicitation/create support.
				e.replyErr(in, string(*msg.ID), -32601, "method not found: elicitation/create")
			case e.elicitDecline:
				e.reply(in, string(*msg.ID), map[string]any{"action": "decline"})
			case e.elicitAcceptEmpty:
				e.reply(in, string(*msg.ID), map[string]any{"action": "accept"})
			default:
				e.reply(in, string(*msg.ID), map[string]any{
					"action": "accept", "content": map[string]any{"answer": "allow"},
				})
			}
		default:
			e.replyErr(in, string(*msg.ID), -32601, "method not found: "+msg.Method)
		}
	}
}

func (e *fsEditorClient) reply(w io.Writer, id string, result any) {
	b, _ := json.Marshal(jsonrpcMessage{JSONRPC: "2.0", ID: rawID(id), Result: result})
	_, _ = fmt.Fprintln(w, string(b))
}

func (e *fsEditorClient) replyErr(w io.Writer, id string, code int, msg string) {
	b, _ := json.Marshal(jsonrpcMessage{JSONRPC: "2.0", ID: rawID(id),
		Error: &jsonrpcError{Code: code, Message: msg}})
	_, _ = fmt.Fprintln(w, string(b))
}

func rawID(s string) *json.RawMessage {
	id := json.RawMessage(s) // s already carries its JSON quotes
	return &id
}

// fsTestBridge builds a bridge with a session pinned to a temp workspace and
// a live stdio editor loop, returning the pieces tests need. Callers cancel
// the returned context to stop the editor loop.
type fsTestBridge struct {
	bridge    *acpBridge
	srv       *acpStdioServer
	editor    *fsEditorClient
	api       *fsFakeACPAPI
	cancel    context.CancelFunc
	workspace string
}

func newFSTestBridge(t *testing.T, requireApproval bool, pending []acpfsRelayRequest) *fsTestBridge {
	t.Helper()
	api := newFSFakeACPAPI(t, pending)
	editor := &fsEditorClient{readOut: "file body", writeOK: true}
	serverURL := api.acpAPI.URL
	c := newClient(serverURL, serverURL, testToken, time.Second, false)

	stdinR, stdinW := io.Pipe()   // editor→bridge
	stdoutR, stdoutW := io.Pipe() // bridge→editor
	srv := newACPStdioServer(nil, stdinR, stdoutW, os.Stderr)

	ws := t.TempDir()
	bridge := newACPBridgeWithOptions(srv, c, "test-agent", fsBridgeOptions{requireApproval: requireApproval})
	bridge.mu.Lock()
	bridge.manifest = ACPAgentManifest{
		Name:       "test-agent",
		Filesystem: &ACPFSPolicy{Mode: "readWrite", WriteApproval: false},
	}
	bridge.sessions[fsTestSessionID] = &acpSession{SessionID: fsTestSessionID, RunID: "run-1", Cwd: ws}
	bridge.mu.Unlock()
	srv.handler = bridge

	ctx, cancel := context.WithCancel(context.Background())
	// Drive the transport loop so agent→client request replies are matched.
	go func() { _ = srv.Serve(ctx) }()
	go editor.run(ctx, stdoutR, stdinW)
	t.Cleanup(cancel)
	return &fsTestBridge{bridge: bridge, srv: srv, editor: editor, api: api, cancel: cancel, workspace: ws}
}

func (f *fsFakeACPAPI) responseRecorded(t *testing.T) *acpfsRelayResponse {
	t.Helper()
	fsWaitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.response != nil
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.response == nil {
		t.Fatal("no fs response posted back to the ACP API within 2s")
	}
	return f.response
}

// fsTestSessionID is the ACP session id used across the fs bridge tests.
const fsTestSessionID = "sess-1"

// fsWaitFor polls a condition until true or a 2s deadline.
func fsWaitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !cond() {
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFSBridgeRelayRead(t *testing.T) {
	tb := newFSTestBridge(t, false, []acpfsRelayRequest{{
		ID:     "req-1",
		Method: acpMethodFSRead,
		Params: json.RawMessage(`{"path":"src/main.go","line":1,"limit":10}`),
	}})

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID: "req-1", Method: acpMethodFSRead,
		Params: json.RawMessage(`{"path":"src/main.go","line":1,"limit":10}`),
	}, tb.bridge.fsPolicy())

	// The editor must have received an absolute, confined path and the
	// sessionId it needs in the ACP v1 fs request.
	fsWaitFor(t, func() bool {
		tb.editor.mu.Lock()
		defer tb.editor.mu.Unlock()
		return len(tb.editor.reads) > 0
	})
	if len(tb.editor.reads) == 0 {
		t.Fatal("editor never received fs/read_text_file")
	}
	tb.editor.mu.Lock()
	gotRead := tb.editor.reads[0]
	tb.editor.mu.Unlock()
	if gotRead.SessionID != fsTestSessionID || gotRead.Path != filepath.Join(tb.workspace, "src/main.go") ||
		gotRead.Line == nil || *gotRead.Line != 1 || gotRead.Limit == nil || *gotRead.Limit != 10 {
		t.Fatalf("editor got %+v", gotRead)
	}

	resp := tb.api.responseRecorded(t)
	if resp.Error != nil || string(resp.Result) != `{"content":"file body"}` {
		t.Fatalf("relayed response wrong: %+v", resp)
	}
}

func TestFSBridgeRelayWriteWithElicitApproval(t *testing.T) {
	pending := []acpfsRelayRequest{{
		ID:     "req-1",
		Method: acpMethodFSWrite,
		Params: json.RawMessage(`{"path":"new.go","content":"package main"}`),
	}}
	tb := newFSTestBridge(t, true, pending) // force approval even though policy says none

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", pending[0], tb.bridge.fsPolicy())

	fsWaitFor(t, func() bool {
		tb.editor.mu.Lock()
		defer tb.editor.mu.Unlock()
		return len(tb.editor.writes) > 0
	})
	if len(tb.editor.writes) != 1 {
		t.Fatal("editor never received fs/write_text_file")
	}
	if w := tb.editor.writes[0]; w.SessionID != fsTestSessionID ||
		w.Path != filepath.Join(tb.workspace, "new.go") || w.Content != "package main" {
		t.Fatalf("editor write wrong: %+v", w)
	}
	if len(tb.editor.elicit) != 1 {
		t.Fatal("expected one approval elicitation")
	}
	// ACP v1 form-mode elicitations require the session scope: without
	// sessionId strict clients (Zed) reject the request with -32602 Invalid
	// params and no prompt ever appears.
	if tb.editor.elicit[0].SessionID != fsTestSessionID {
		t.Fatalf("elicitation missing sessionId: %+v", tb.editor.elicit[0])
	}
	// The requestedSchema must be a proper ElicitationSchema envelope
	// (type/properties/required) — a bare properties map is rejected by the
	// client with Invalid params (-32602).
	schema := tb.editor.elicit[0].RequestedSchema
	if schema["type"] != "object" {
		t.Fatalf("requestedSchema missing type=object: %v", schema)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || props["answer"] == nil {
		t.Fatalf("requestedSchema missing properties.answer: %v", schema)
	}
	resp := tb.api.responseRecorded(t)
	if resp.Error != nil {
		t.Fatalf("approved write should succeed, got error %+v", resp.Error)
	}
}

func TestFSBridgeWriteDeniedByUser(t *testing.T) {
	pending := []acpfsRelayRequest{{
		ID:     "req-1",
		Method: acpMethodFSWrite,
		Params: json.RawMessage(`{"path":"evil.go","content":"hmm"}`),
	}}
	tb := newFSTestBridge(t, true, pending)
	tb.editor.elicitDecline = true

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", pending[0], tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error == nil || resp.Error.Message != "write declined by the user in the editor" {
		t.Fatalf("expected user-decline error, got %+v", resp)
	}
	if len(tb.editor.writes) != 0 {
		t.Fatal("declined write must never reach the editor")
	}
}

func TestFSBridgeWriteApprovedWithEmptyFormContent(t *testing.T) {
	// Regression: some clients submit the approval form without the enum
	// field. The accept action is the approval signal — an accepted form must
	// not be misread as a decline (which used to surface as the misleading
	// "write declined by the user in the editor").
	pending := []acpfsRelayRequest{{
		ID:     "req-1",
		Method: acpMethodFSWrite,
		Params: json.RawMessage(`{"path":"ok.go","content":"package main"}`),
	}}
	tb := newFSTestBridge(t, true, pending)
	tb.editor.elicitAcceptEmpty = true

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", pending[0], tb.bridge.fsPolicy())

	fsWaitFor(t, func() bool {
		tb.editor.mu.Lock()
		defer tb.editor.mu.Unlock()
		return len(tb.editor.writes) > 0
	})
	if len(tb.editor.writes) != 1 {
		t.Fatal("accepted approval must forward the write to the editor")
	}
	resp := tb.api.responseRecorded(t)
	if resp.Error != nil {
		t.Fatalf("approved write should succeed, got %+v", resp.Error)
	}
}

func TestFSBridgeWriteApprovalPromptFailed(t *testing.T) {
	// Regression: a failed approval round-trip (editor without elicitation
	// support) must be reported as "prompt failed", not "declined by the
	// user" — the two need different remedies.
	pending := []acpfsRelayRequest{{
		ID:     "req-1",
		Method: acpMethodFSWrite,
		Params: json.RawMessage(`{"path":"x.go","content":"y"}`),
	}}
	tb := newFSTestBridge(t, true, pending)
	tb.editor.elicitFail = true

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", pending[0], tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "write approval prompt failed") {
		t.Fatalf("expected prompt-failed error, got %+v", resp)
	}
	if len(tb.editor.writes) != 0 {
		t.Fatal("write must never proceed when the approval prompt fails")
	}
}

func TestFSBridgeWriteBlockedByReadOnlyPolicy(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	tb.bridge.mu.Lock()
	tb.bridge.manifest.Filesystem = &ACPFSPolicy{Mode: "readOnly"}
	tb.bridge.mu.Unlock()

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID: "req-1", Method: acpMethodFSWrite, Params: json.RawMessage(`{"path":"x.go","content":"y"}`),
	}, tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "writes are not enabled") {
		t.Fatalf("expected readOnly enforcement, got %+v", resp)
	}
	if len(tb.editor.writes) != 0 {
		t.Fatal("readOnly write must never reach the editor")
	}
}

func TestFSBridgePathEscapeRejected(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID:     "req-1",
		Method: acpMethodFSRead,
		Params: json.RawMessage(`{"path":"../../etc/shadow"}`),
	}, tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "escapes the workspace roots") {
		t.Fatalf("expected confinement error, got %+v", resp)
	}
	if len(tb.editor.reads) != 0 {
		t.Fatal("escaping read must never reach the editor")
	}
}

// TestACPBridgeSessionNewPreservesInFlightRun: Zed may re-send session/new
// for a session while its prompt turn is still executing. The record must be
// updated in place — replacing it would drop the RunID and orphan the fs/*
// watcher (fs calls then pend until the router's fs timeout).
func TestACPBridgeSessionNewPreservesInFlightRun(t *testing.T) {
	c := newClient(testAgentOrcaHost, testAgentOrcaHost, testToken, time.Second, false)
	bridge := newACPBridgeWithOptions(nil, c, "test-agent", fsBridgeOptions{})
	ctx := context.Background()

	// Simulate an in-flight prompt: session registered with a run + cancel.
	_, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	bridge.registerRun(fsTestSessionID, "run-1", cancel1)

	// session/new arrives again for the same session with a (new) cwd.
	params := fmt.Sprintf(`{"sessionId":%q,"cwd":"%s"}`, fsTestSessionID, t.TempDir())
	_, err := handleSessionNew(ctx, bridge, jsonrpcRequest{Params: json.RawMessage(params)})
	if err != nil {
		t.Fatal("session/new:", err)
	}

	bridge.mu.Lock()
	sess := bridge.sessions[fsTestSessionID]
	var runID, cwd string
	var cancel context.CancelFunc
	if sess != nil {
		runID, cwd, cancel = sess.RunID, sess.Cwd, sess.Cancel
	}
	bridge.mu.Unlock()
	if runID != "run-1" {
		t.Fatalf("in-flight run lost by session/new: RunID=%q", runID)
	}
	if cancel == nil {
		t.Fatal("cancel func lost by session/new")
	}
	if cwd == "" {
		t.Fatal("cwd not stored by session/new")
	}
	// The fs watcher can still resolve the run it polls.
	if got := bridge.sessionRunID(fsTestSessionID); got != "run-1" {
		t.Fatalf("sessionRunID = %q, want run-1", got)
	}
}

// TestACPBridgePromptPreservesSessionCwd is the regression test for the
// workspace-root loss: handleSessionPrompt used to replace the session record
// created by session/new, dropping the cwd and breaking fs/* confinement
// ("no workspace root is known for this session").
func TestACPBridgePromptPreservesSessionCwd(t *testing.T) {
	ws := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/agents/test-agent/run", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"test-agent","run_id":"run-1","status":"created"}`))
	})
	// The run stream: one terminal SSE event so the prompt completes.
	mux.HandleFunc("/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: run.completed\n" +
			"data: {\"type\":\"run.completed\",\"run\":{\"run_id\":\"run-1\",\"status\":\"completed\"}}\n\n"))
	})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)

	c := newClient(api.URL, api.URL, testToken, time.Second, false)
	stdinR, stdinW := io.Pipe()
	var mu sync.Mutex
	var stdout bytes.Buffer
	srv := newACPStdioServer(nil, stdinR, &lockedBuffer{mu: &mu, buf: &stdout}, os.Stderr)
	bridge := newACPBridgeWithOptions(srv, c, "test-agent", fsBridgeOptions{})
	bridge.mu.Lock()
	bridge.manifest = ACPAgentManifest{
		Name:       "test-agent",
		Filesystem: &ACPFSPolicy{Mode: "readWrite"},
	}
	bridge.mu.Unlock()
	srv.handler = bridge

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(cancel)

	send := func(line string) {
		if _, err := stdinW.Write([]byte(line + "\n")); err != nil {
			t.Fatal("writing stdin:", err)
		}
	}
	waitResponse := func(id string) {
		t.Helper()
		fsWaitFor(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return strings.Contains(stdout.String(), `"id":`+id)
		})
	}

	send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/new",`+
		`"params":{"sessionId":%q,"cwd":%q}}`, fsTestSessionID, ws))
	waitResponse("1")
	send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":fsTestSessionID,` +
		`"prompt":[{"type":"text","text":"hi"}]}}`)
	waitResponse("2")

	bridge.mu.Lock()
	sess := bridge.sessions[fsTestSessionID]
	var cwd string
	if sess != nil {
		cwd = sess.Cwd
	}
	bridge.mu.Unlock()
	if cwd != ws {
		t.Fatalf("session cwd lost after session/prompt: got %q, want %q", cwd, ws)
	}
	roots := bridge.workspaceRoots(fsTestSessionID)
	if len(roots) == 0 || roots[0] != ws {
		t.Fatalf("workspace roots after prompt = %v, want [%s]", roots, ws)
	}
}

// lockedBuffer is an io.Writer safe for the bridge's concurrent stdout writes.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestFSBridgeListDirAnsweredLocally(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	// Populate the workspace: one dir, one file.
	if err := os.Mkdir(filepath.Join(tb.workspace, "sub"), 0o755); err != nil {
		t.Fatal("mkdir:", err)
	}
	if err := os.WriteFile(filepath.Join(tb.workspace, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal("write:", err)
	}

	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID:     "req-1",
		Method: acpMethodFSList,
		Params: json.RawMessage(`{"path":"."}`),
	}, tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error != nil {
		t.Fatalf("listing should succeed, got %+v", resp.Error)
	}
	var out struct {
		Path    string `json:"path"`
		Entries []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("bad listing result %s: %v", resp.Result, err)
	}
	if out.Path != tb.workspace || len(out.Entries) != 2 {
		t.Fatalf("unexpected listing: %+v", out)
	}
	byName := map[string]string{}
	for _, e := range out.Entries {
		byName[e.Name] = e.Type
	}
	if byName["sub"] != "dir" || byName["main.go"] != "file" {
		t.Fatalf("entry types wrong: %+v", out.Entries)
	}
	// Listing is answered by the bridge itself: the editor sees nothing.
	if len(tb.editor.reads) != 0 || len(tb.editor.writes) != 0 {
		t.Fatal("fs_list_dir must not be forwarded to the editor")
	}
}

func TestFSBridgeListDirEscapeRejected(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID:     "req-1",
		Method: acpMethodFSList,
		Params: json.RawMessage(`{"path":"../.."}`),
	}, tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "escapes the workspace roots") {
		t.Fatalf("expected confinement error, got %+v", resp)
	}
}

func TestFSBridgeEditorErrorRelayed(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	tb.editor.writeOK = false
	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID:     "req-1",
		Method: acpMethodFSWrite,
		Params: json.RawMessage(`{"path":"x.go","content":"y"}`),
	}, tb.bridge.fsPolicy())

	resp := tb.api.responseRecorded(t)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "refused") {
		t.Fatalf("expected editor error relayed, got %+v", resp)
	}
}

func TestBridgeWorkspaceRootsFromSessionCwd(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	roots := tb.bridge.workspaceRoots(fsTestSessionID)
	if len(roots) != 1 || roots[0] != tb.workspace {
		t.Fatalf("workspaceRoots = %v, want [%s]", roots, tb.workspace)
	}

	// Extra --fs-roots come after the session cwd.
	tb.bridge.fsOpts.extraRoots = []string{"~/extra"}
	roots = tb.bridge.workspaceRoots(fsTestSessionID)
	if len(roots) != 2 || roots[0] != tb.workspace || !strings.HasSuffix(roots[1], "extra") {
		t.Fatalf("workspaceRoots with extras = %v", roots)
	}

	// Unknown sessions fall back to the bridge process working directory as
	// the primary root (plus the extra roots) — still confined, just to the
	// process cwd since no session/new delivered one.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal("getwd:", err)
	}
	roots = tb.bridge.workspaceRoots("unknown-session")
	if len(roots) != 2 || roots[0] != wd || !strings.HasSuffix(roots[1], "extra") {
		t.Fatalf("unknown-session roots = %v, want [%s …extra]", roots, wd)
	}
}
