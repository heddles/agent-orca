package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// terminalAPI fakes the ACP API for local_exec: it lists the pending requests
// (every time, until each is answered, as the relay does), records answers, and
// runs one streamed turn that ends when finish is closed.
type terminalAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	pending  []acpfsRelayRequest
	answers  map[string][]acpfsRelayResponse
	sticky   bool // keep listing answered requests, like a stale listing
	creates  []string
	canceled bool
	finish   chan struct{}
}

func newTerminalAPI(t *testing.T) *terminalAPI {
	t.Helper()
	a := &terminalAPI{answers: map[string][]acpfsRelayResponse{}, finish: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/agents/test-agent", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ACPAgentManifest{Name: "test-agent",
			Terminal: &ACPTerminalPolicy{MaxTimeoutSeconds: 600}})
	})
	mux.HandleFunc("/agents/test-agent/run", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.creates = append(a.creates, string(body))
		a.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run_id":"run-1","status":"created"}`))
	})
	mux.HandleFunc("/runs/run-1/fs/pending", func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var out []acpfsRelayRequest
		for _, p := range a.pending {
			if a.sticky || len(a.answers[p.ID]) == 0 {
				out = append(out, p)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": out})
	})
	mux.HandleFunc("/runs/run-1/fs/pending/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/runs/run-1/fs/pending/"), "/")[0]
		var resp acpfsRelayResponse
		_ = json.NewDecoder(r.Body).Decode(&resp)
		a.mu.Lock()
		a.answers[id] = append(a.answers[id], resp)
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/runs/run-1/cancel", func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		a.canceled = true
		a.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		done := false
		select {
		case <-a.finish:
			done = true
		default:
		}
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			select {
			case <-a.finish:
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			case <-r.Context().Done():
			}
			return
		}
		status := "in-progress"
		if done {
			status = "completed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": "run-1", "status": status})
	})
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json") // the stream sets its own
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *terminalAPI) request(id, command string, extra ...func(*terminalRunParams)) {
	p := terminalRunParams{Command: command, TimeoutSeconds: 60}
	for _, f := range extra {
		f(&p)
	}
	raw, _ := json.Marshal(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = append(a.pending, acpfsRelayRequest{ID: id, Method: acpMethodTerminalRun, Params: raw})
}

func (a *terminalAPI) answered(id string) []acpfsRelayResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]acpfsRelayResponse(nil), a.answers[id]...)
}

// termEditor plays Zed: it answers permission prompts as told and really runs
// terminals, the way Zed does it: /bin/sh -c with stdin closed.
type termEditor struct {
	mu          sync.Mutex
	in          io.Writer
	choose      func(command string) map[string]any // permission outcome; nil = never answer
	failAsk     bool
	prompts     []acpPermissionParams
	creates     []map[string]any
	calls       []string
	updates     []string
	procs       map[string]*editorProc
	next        int
	promptReply chan jsonrpcMessage
}

type editorProc struct {
	cmd  *exec.Cmd
	out  *syncText
	done chan struct{}
}

// syncText is a bytes.Buffer safe to read while it is being written.
type syncText struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncText) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncText) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func outcome(o map[string]any) func(string) map[string]any {
	return func(string) map[string]any { return o }
}

func choose(option string) func(string) map[string]any {
	return outcome(map[string]any{"outcome": "selected", "optionId": option})
}

func (e *termEditor) send(msg jsonrpcMessage) {
	b, _ := json.Marshal(msg)
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.in.Write(append(b, '\n'))
}

func (e *termEditor) reply(id *json.RawMessage, result any) {
	e.send(jsonrpcMessage{JSONRPC: "2.0", ID: id, Result: result})
}

func (e *termEditor) serve(t *testing.T, out io.Reader) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<22)
	for scanner.Scan() {
		var msg jsonrpcMessage
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method == "" { // the reply to one of our requests
			e.promptReply <- msg
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(msg.Params, &p)
		e.mu.Lock()
		if msg.Method == acpMethodSessionUpdate {
			e.updates = append(e.updates, string(msg.Params))
			e.mu.Unlock()
			continue
		}
		e.calls = append(e.calls, msg.Method)
		e.mu.Unlock()
		switch msg.Method {
		case acpMethodRequestPermission:
			var ask acpPermissionParams
			_ = json.Unmarshal(msg.Params, &ask)
			e.mu.Lock()
			e.prompts = append(e.prompts, ask)
			pick, fail := e.choose, e.failAsk
			e.mu.Unlock()
			switch {
			case fail:
				e.send(jsonrpcMessage{JSONRPC: "2.0", ID: msg.ID, Error: &jsonrpcError{Code: -32601, Message: "nope"}})
			case pick != nil:
				e.reply(msg.ID, map[string]any{"outcome": pick(ask.ToolCall.Title)})
			}
		case acpMethodTerminalCreate:
			command, _ := p["command"].(string)
			cwd, _ := p["cwd"].(string)
			cmd := exec.Command("/bin/sh", "-c", "exec </dev/null\n"+command)
			cmd.Dir = cwd
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			proc := &editorProc{cmd: cmd, out: &syncText{}, done: make(chan struct{})}
			cmd.Stdout, cmd.Stderr = proc.out, proc.out
			if err := cmd.Start(); err != nil {
				t.Errorf("start: %v", err)
				continue
			}
			go func() { _ = cmd.Wait(); close(proc.done) }()
			e.mu.Lock()
			e.next++
			id := "term-" + strconv.Itoa(e.next)
			e.procs[id] = proc
			e.creates = append(e.creates, p)
			e.mu.Unlock()
			e.reply(msg.ID, map[string]any{"terminalId": id})
		case acpMethodTerminalWait, acpMethodTerminalOutput, acpMethodTerminalKill, acpMethodTerminalRelease:
			id, _ := p["terminalId"].(string)
			e.mu.Lock()
			proc := e.procs[id]
			e.mu.Unlock()
			if proc == nil {
				t.Errorf("%s of unknown terminal %q", msg.Method, id)
				continue
			}
			switch msg.Method {
			case acpMethodTerminalWait:
				go func(rid *json.RawMessage) {
					<-proc.done
					e.reply(rid, map[string]any{"exitCode": proc.cmd.ProcessState.ExitCode(), "signal": nil})
				}(msg.ID)
			case acpMethodTerminalOutput:
				e.reply(msg.ID, map[string]any{"output": proc.out.String(), "truncated": false})
			default:
				_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
				<-proc.done
				e.reply(msg.ID, map[string]any{})
			}
		default:
			t.Errorf("unexpected editor request %s", msg.Method)
		}
	}
}

func (e *termEditor) count(method string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if c == method {
			n++
		}
	}
	return n
}

type termBridge struct {
	api    *terminalAPI
	editor *termEditor
	bridge *acpBridge
	ws     string
}

// newTermBridge starts a real bridge between a fake API and a fake editor,
// initialized as an editor with or without a terminal, with one session.
func newTermBridge(t *testing.T, editorHasTerminal, noTerminal bool) *termBridge {
	t.Helper()
	api := newTerminalAPI(t)
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	srv := newACPStdioServer(nil, stdinR, stdoutW, io.Discard)
	bridge := newACPBridge(srv, newClient(api.srv.URL, api.srv.URL, testToken, 5*time.Second, false), "test-agent")
	bridge.terminalDisabled = noTerminal
	srv.handler = bridge
	editor := &termEditor{in: stdinW, choose: choose(permissionAllowOnce), procs: map[string]*editorProc{},
		promptReply: make(chan jsonrpcMessage, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	go editor.serve(t, stdoutR)
	t.Cleanup(func() { cancel(); _ = stdinW.Close(); _ = stdoutW.Close() })

	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tb := &termBridge{api: api, editor: editor, bridge: bridge, ws: ws}
	for _, reply := range []jsonrpcMessage{
		tb.call(t, "1", "initialize", map[string]any{"protocolVersion": 1,
			"clientCapabilities": map[string]any{"terminal": editorHasTerminal}}),
		tb.call(t, "2", "session/new", map[string]any{"sessionId": "s1", "cwd": ws}),
	} {
		if reply.Error != nil {
			t.Fatalf("setting up the session: %+v", reply.Error)
		}
	}
	return tb
}

func (tb *termBridge) post(id, method string, params any) {
	raw, _ := json.Marshal(params)
	rid := json.RawMessage(id)
	tb.editor.send(jsonrpcMessage{JSONRPC: "2.0", ID: &rid, Method: method, Params: raw})
}

func (tb *termBridge) await(t *testing.T) jsonrpcMessage {
	t.Helper()
	select {
	case m := <-tb.editor.promptReply:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no reply from the bridge")
		return jsonrpcMessage{}
	}
}

func (tb *termBridge) call(t *testing.T, id, method string, params any) jsonrpcMessage {
	t.Helper()
	tb.post(id, method, params)
	return tb.await(t)
}

func (tb *termBridge) prompt() {
	tb.post("3", "session/prompt", map[string]any{"sessionId": "s1",
		"prompt": []map[string]any{{"type": "text", "text": "go"}}})
}

func (tb *termBridge) endTurn(t *testing.T) {
	t.Helper()
	close(tb.api.finish)
	tb.await(t)
}

func (tb *termBridge) answer(t *testing.T, id string) (terminalRunResult, *acpfsRelayError) {
	t.Helper()
	eventually(t, func() bool { return len(tb.api.answered(id)) > 0 }, "an answer for "+id)
	resp := tb.api.answered(id)[0]
	var res terminalRunResult
	if resp.Error == nil {
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("result %s: %v", resp.Result, err)
		}
	}
	return res, resp.Error
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminalRunsAnApprovedCommandAsShown(t *testing.T) {
	tb := newTermBridge(t, true, false)
	// Quoting, pipes, &&, a second line, stderr, no stdin and an exit code.
	command := "printf '%s\\n' \"a b\" 'c`d' | grep 'a b' && echo err >&2\nread x || echo no-input; exit 3"
	tb.api.request("r1", command)
	tb.prompt()
	res, rerr := tb.answer(t, "r1")
	if rerr != nil {
		t.Fatalf("error: %+v", rerr)
	}
	if res.ExitCode == nil || *res.ExitCode != 3 || res.Output != "a b\nerr\nno-input\n" {
		t.Fatalf("result = %+v", res)
	}
	if res.WorkDir != tb.ws || res.OS == "" {
		t.Fatalf("result does not say where it ran: %+v", res)
	}
	tb.endTurn(t)

	if !strings.Contains(tb.api.creates[0], `"client_capabilities":{"terminal":true}`) {
		t.Fatalf("the run was not marked: %s", tb.api.creates[0])
	}
	created := tb.editor.creates[0]
	if created["command"] != command || created["args"] != nil || created["env"] != nil || created["cwd"] != tb.ws {
		t.Fatalf("the editor must get exactly the approved string: %v", created)
	}
	ask := tb.editor.prompts[0]
	shown := ask.ToolCall.Content[0].Content.Text
	if !strings.Contains(shown, command) || !strings.Contains(shown, tb.ws) {
		t.Fatalf("prompt does not show the command and folder:\n%s", shown)
	}
	kinds := make([]string, 0, len(ask.Options))
	for _, o := range ask.Options {
		kinds = append(kinds, o.Kind)
	}
	if strings.Join(kinds, ",") != "allow_once,allow_always,reject_once" {
		t.Fatalf("options = %v", kinds)
	}
	if got := tb.editor.count(acpMethodTerminalRelease); got != 1 {
		t.Fatalf("released %d times", got)
	}
	all := strings.Join(tb.editor.updates, "\n")
	if !strings.Contains(all, `"type":"terminal","terminalId":"term-1"`) {
		t.Fatalf("the terminal was not shown in the tool call:\n%s", all)
	}
}

// Anything but an explicit allow refuses the command.
func TestTerminalRefusals(t *testing.T) {
	for name, set := range map[string]func(*termEditor){
		"rejected":   func(e *termEditor) { e.choose = choose(permissionReject) },
		"cancelled":  func(e *termEditor) { e.choose = outcome(map[string]any{"outcome": "cancelled"}) },
		"unknown id": func(e *termEditor) { e.choose = choose("allow") },
		"empty":      func(e *termEditor) { e.choose = outcome(map[string]any{}) },
		"no prompt":  func(e *termEditor) { e.failAsk = true },
	} {
		t.Run(name, func(t *testing.T) {
			tb := newTermBridge(t, true, false)
			set(tb.editor)
			tb.api.request("r1", "touch ran")
			tb.prompt()
			_, rerr := tb.answer(t, "r1")
			if rerr == nil || !strings.Contains(rerr.Message, "declined") && !strings.Contains(rerr.Message, "did not run") {
				t.Fatalf("error = %+v", rerr)
			}
			tb.endTurn(t)
			if _, err := os.Stat(filepath.Join(tb.ws, "ran")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused command ran")
			}
			if tb.editor.count(acpMethodTerminalCreate) != 0 {
				t.Fatal("a terminal was created")
			}
		})
	}
}

func TestTerminalApprovalTimesOut(t *testing.T) {
	old := terminalApprovalWindow
	terminalApprovalWindow = 200 * time.Millisecond
	t.Cleanup(func() { terminalApprovalWindow = old })
	tb := newTermBridge(t, true, false)
	tb.editor.choose = nil
	tb.api.request("r1", "touch ran")
	tb.prompt()
	if _, rerr := tb.answer(t, "r1"); rerr == nil || !strings.Contains(rerr.Message, "in time") {
		t.Fatalf("error = %+v", rerr)
	}
	tb.endTurn(t)
	if tb.editor.count(acpMethodTerminalCreate) != 0 {
		t.Fatal("an unanswered command ran")
	}
}

// "Always allow" covers that exact command in that folder, and nothing else.
func TestTerminalAlwaysAllowIsExact(t *testing.T) {
	tb := newTermBridge(t, true, false)
	if err := os.Mkdir(filepath.Join(tb.ws, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	tb.editor.choose = choose(permissionAllowAlways)
	tb.prompt()
	in := func(dir string) func(*terminalRunParams) { return func(p *terminalRunParams) { p.WorkDir = dir } }
	steps := []struct {
		command, dir string
		prompts      int
	}{
		{"echo hi", "", 1},
		{"echo hi", "", 1},
		{"echo hi ", "", 2},
		{"echo hi; true", "", 3},
		{"echo hi", "sub", 4},
	}
	for i, s := range steps {
		id := "r" + strconv.Itoa(i)
		tb.api.request(id, s.command, in(s.dir))
		if _, rerr := tb.answer(t, id); rerr != nil {
			t.Fatalf("step %d: %+v", i, rerr)
		}
		if got := tb.editor.count(acpMethodRequestPermission); got != s.prompts {
			t.Fatalf("step %d (%q in %q): %d prompts, want %d", i, s.command, s.dir, got, s.prompts)
		}
	}
	tb.endTurn(t)
}

// A request the relay keeps listing, or lists again after the answer, runs once.
func TestTerminalRequestRunsOnce(t *testing.T) {
	tb := newTermBridge(t, true, false)
	tb.api.sticky = true
	tb.editor.choose = choose(permissionAllowAlways)
	tb.api.request("r1", "echo x >> count; sleep 1")
	tb.prompt()
	tb.answer(t, "r1")
	time.Sleep(3 * fsPollInterval)
	tb.endTurn(t)
	got, err := os.ReadFile(filepath.Join(tb.ws, "count"))
	if err != nil || string(got) != "x\n" {
		t.Fatalf("the command ran %q times (%v)", got, err)
	}
	if n := len(tb.api.answered("r1")); n != 1 {
		t.Fatalf("answered %d times", n)
	}
}

// A long command must not hold up other requests.
func TestTerminalCommandsRunBesideEachOther(t *testing.T) {
	tb := newTermBridge(t, true, false)
	tb.api.request("slow", "while [ ! -f release ]; do sleep 0.05; done")
	tb.api.request("fast", "touch release")
	tb.prompt()
	tb.answer(t, "slow")
	tb.endTurn(t)
}

func TestTerminalTimeoutStopsTheCommand(t *testing.T) {
	tb := newTermBridge(t, true, false)
	tb.api.request("r1", "echo started; sleep 30; touch finished",
		func(p *terminalRunParams) { p.TimeoutSeconds = 1 })
	tb.prompt()
	res, rerr := tb.answer(t, "r1")
	if rerr != nil || !res.TimedOut || res.ExitCode != nil || res.Output != "started\n" {
		t.Fatalf("result = %+v, %+v", res, rerr)
	}
	tb.endTurn(t)
	if tb.editor.count(acpMethodTerminalKill) != 1 || tb.editor.count(acpMethodTerminalRelease) != 1 {
		t.Fatalf("calls = %v", tb.editor.calls)
	}
}

func TestTerminalCancelStopsTheCommand(t *testing.T) {
	tb := newTermBridge(t, true, false)
	tb.api.request("r1", "touch started; sleep 30; touch finished")
	tb.prompt()
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(tb.ws, "started")); return err == nil }, "the command")
	raw, _ := json.Marshal(map[string]any{"sessionId": "s1"})
	tb.editor.send(jsonrpcMessage{JSONRPC: "2.0", Method: "session/cancel", Params: raw})
	// The turn's reply comes only after the command was dealt with. The editor
	// also answers the abandoned wait, late; the bridge must not reply to that.
	if m := tb.await(t); string(*m.ID) != "3" {
		t.Fatalf("expected the turn's reply, got %s %+v", *m.ID, m.Error)
	}
	if tb.editor.count(acpMethodTerminalKill) != 1 || tb.editor.count(acpMethodTerminalRelease) != 1 {
		t.Fatalf("calls = %v", tb.editor.calls)
	}
	if _, rerr := tb.answer(t, "r1"); rerr == nil || !strings.Contains(rerr.Message, "cancelled") {
		t.Fatalf("error = %+v", rerr)
	}
}

func TestTerminalRejectsWhatCannotBeJudgedOrPlaced(t *testing.T) {
	outside := t.TempDir()
	tb := newTermBridge(t, true, false)
	if err := os.Symlink(outside, filepath.Join(tb.ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tb.ws, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tb.prompt()
	cases := map[string]struct{ command, dir string }{
		"bidi override":   {"echo safe \u202e hs.live", ""},
		"zero width":      {"rm\u200b -rf x", ""},
		"carriage return": {"rm -rf x\recho hello", ""},
		"escape":          {"echo \x1b[2Kls", ""},
		"line separator":  {"ls\u2028rm x", ""},
		"too long":        {strings.Repeat("a", terminalCommandLimit+1), ""},
		"blank":           {" \n ", ""},
		"parent folder":   {"ls", ".."},
		"absolute":        {"ls", outside},
		"symlink out":     {"ls", "link"},
		"not a folder":    {"ls", "file"},
		"missing":         {"ls", "nope"},
	}
	for name, c := range cases {
		tb.api.request(name, c.command, func(p *terminalRunParams) { p.WorkDir = c.dir })
	}
	for name := range cases {
		id := strings.ReplaceAll(name, " ", "%20")
		eventually(t, func() bool { return len(tb.api.answered(name))+len(tb.api.answered(id)) > 0 }, name)
		answers := append(tb.api.answered(name), tb.api.answered(id)...)
		if answers[0].Error == nil {
			t.Errorf("%s: accepted: %s", name, answers[0].Result)
		}
	}
	tb.endTurn(t)
	if tb.editor.count(acpMethodRequestPermission)+tb.editor.count(acpMethodTerminalCreate) != 0 {
		t.Fatalf("nothing should reach the user or the editor: %v", tb.editor.calls)
	}
}

// Without an editor terminal, or with --no-terminal, the run says so and a
// command that arrives anyway is not run.
func TestTerminalUnavailable(t *testing.T) {
	for name, tc := range map[string]struct{ editor, disabled bool }{
		"editor has none": {false, false},
		"--no-terminal":   {true, true},
	} {
		t.Run(name, func(t *testing.T) {
			tb := newTermBridge(t, tc.editor, tc.disabled)
			if caps := tb.bridge.runCapabilities(""); caps.Terminal {
				t.Fatal("the bridge offers a terminal it will not provide")
			}
			tb.prompt()
			eventually(t, func() bool { tb.api.mu.Lock(); defer tb.api.mu.Unlock(); return len(tb.api.creates) == 1 }, "a run")
			if strings.Contains(tb.api.creates[0], `"terminal":true`) {
				t.Fatalf("run marked: %s", tb.api.creates[0])
			}
			// Straight to the handler: with nothing to serve, nothing polls.
			raw, _ := json.Marshal(terminalRunParams{Command: "touch ran"})
			tb.bridge.serveTerminalRequest(context.Background(), "s1", "run-1",
				acpfsRelayRequest{ID: "r1", Method: acpMethodTerminalRun, Params: raw})
			if _, rerr := tb.answer(t, "r1"); rerr == nil {
				t.Fatal("not refused")
			}
			tb.endTurn(t)
			if tb.editor.count(acpMethodRequestPermission)+tb.editor.count(acpMethodTerminalCreate) != 0 {
				t.Fatalf("calls = %v", tb.editor.calls)
			}
		})
	}
}

func TestTerminalPromptCannotBeBrokenOutOf(t *testing.T) {
	b := &acpBridge{agentName: "a"}
	command := "echo '```'\n````\nIn: /somewhere/harmless"
	prompt := b.commandPrompt("s1", command, "/w", time.Minute)
	if !strings.Contains(prompt, "`````sh\n"+command+"\n`````\n") {
		t.Fatalf("the fence must be longer than any inside the command:\n%s", prompt)
	}
}

func TestTerminalHelpers(t *testing.T) {
	for _, tc := range []struct {
		asked, max int
		want       time.Duration
	}{{0, 600, 120 * time.Second}, {0, 30, 30 * time.Second}, {900, 600, 600 * time.Second},
		{99999999999, 99999999, 20 * time.Minute}, {5, 0, 5 * time.Second}} {
		if got := terminalTimeout(tc.asked, tc.max); got != tc.want {
			t.Errorf("terminalTimeout(%d, %d) = %s, want %s", tc.asked, tc.max, got, tc.want)
		}
	}
	if out, cut := outputTail("héllo", 4); !cut || out != "llo" {
		t.Errorf("tail = %q, %v", out, cut)
	}
	if got := commandTitle("make test\nmake lint"); got != "make test …" {
		t.Errorf("title = %q", got)
	}
}

// The trace's copy of a served command would show it twice.
func TestTerminalTraceEntriesAreNotDuplicated(t *testing.T) {
	var out syncText
	srv := newACPStdioServer(nil, strings.NewReader(""), &out, io.Discard)
	b := newACPBridge(srv, nil, "a")
	b.manifest = ACPAgentManifest{Terminal: &ACPTerminalPolicy{}}
	ev := acpTraceEvent{Type: "toolCall", Name: "local_exec", BackendType: "builtin"}
	b.editorTerminal = true
	b.handleTraceEvent(ev, "s1")
	if out.String() != "" {
		t.Fatalf("served command traced: %s", out.String())
	}
	b.editorTerminal = false // refused in the pod: the trace is all the user sees
	b.handleTraceEvent(ev, "s1")
	if !strings.Contains(out.String(), "local_exec") {
		t.Fatal("trace entry lost")
	}
}
