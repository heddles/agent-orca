package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/heddles/agent-orca/internal/acpfs"
)

// terminalOperator fakes the relay endpoints: it records what was enqueued and
// answers the enqueue with enqueueStatus and the take with answer.
type terminalOperator struct {
	mu            sync.Mutex
	sent          []acpfs.TerminalParams
	methods       []string
	enqueueStatus int
	enqueueBody   string
	takeStatus    int
	answer        string
}

func (o *terminalOperator) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			var in acpfs.EnqueueRequest
			var params acpfs.TerminalParams
			_ = json.Unmarshal(raw, &in)
			_ = json.Unmarshal(in.Params, &params)
			o.methods, o.sent = append(o.methods, in.Method), append(o.sent, params)
			w.WriteHeader(o.enqueueStatus)
			_, _ = w.Write([]byte(o.enqueueBody))
			return
		}
		w.WriteHeader(o.takeStatus)
		_, _ = w.Write([]byte(o.answer))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func answeringOperator(answer string) *terminalOperator {
	return &terminalOperator{enqueueStatus: http.StatusCreated, enqueueBody: `{"id":"req-1"}`, takeStatus: http.StatusOK, answer: answer}
}

func terminalRouter(t *testing.T, cfg *TerminalConfig, url string) *Router {
	t.Helper()
	r := newFSTestRouter(t, nil, url, fsSATokenFile(t))
	r.cfg.Terminal = cfg
	return r
}

func toolResult(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("result is not JSON: %s", out)
	}
	return m
}

func TestLocalExecRelaysTheCommandAndResult(t *testing.T) {
	op := answeringOperator(`{"id":"req-1","result":{"exitCode":0,"output":"ok\n","workDir":"/Users/me/p","os":"darwin"}}`)
	r := terminalRouter(t, &TerminalConfig{MaxTimeoutSeconds: 300}, op.serve(t))
	command := `cd sub && echo "a b" | grep a`
	raw, _ := json.Marshal(map[string]any{"command": command, "work_dir": "x"})

	res := toolResult(t, r.executeLocalExec(context.Background(), string(raw)))
	if res["exit_code"] != float64(0) || res["output"] != "ok\n" || res["os"] != "darwin" || res["work_dir"] != "/Users/me/p" || res["error"] != nil {
		t.Fatalf("result = %v", res)
	}
	if len(op.sent) != 1 || op.methods[0] != acpfs.MethodRunTerminal {
		t.Fatalf("enqueued = %v", op.methods)
	}
	if got := op.sent[0]; got.Command != command || got.WorkDir != "x" || got.TimeoutSeconds != 120 {
		t.Fatalf("the command must arrive unchanged with the default timeout, got %+v", got)
	}
}

func TestLocalExecCapsTheTimeout(t *testing.T) {
	for _, tc := range []struct{ cap, asked, want int }{
		{300, 9999, 300},
		{300, 45, 45},
		{60, 0, 60},          // the default is capped too
		{99999, 99999, 1200}, // never past what the relay can hold
		{0, 9999, 600},
	} {
		op := answeringOperator(`{"result":{"exitCode":0,"output":""}}`)
		r := terminalRouter(t, &TerminalConfig{MaxTimeoutSeconds: tc.cap}, op.serve(t))
		raw, _ := json.Marshal(map[string]any{"command": "ls", "timeout_seconds": tc.asked})
		r.executeLocalExec(context.Background(), string(raw))
		if got := op.sent[0].TimeoutSeconds; got != tc.want {
			t.Errorf("cap %d asked %d: timeout %d, want %d", tc.cap, tc.asked, got, tc.want)
		}
	}
}

func TestLocalExecFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		op   *terminalOperator
		want string
	}{
		"no editor": {&terminalOperator{enqueueStatus: http.StatusConflict, enqueueBody: "no editor terminal is connected to this run\n"},
			"no editor terminal is connected"},
		"declined":  {answeringOperator(`{"error":{"code":-32003,"message":"the user declined to run this command"}}`), "declined"},
		"expired":   {&terminalOperator{enqueueStatus: http.StatusCreated, enqueueBody: `{"id":"r"}`, takeStatus: http.StatusGone}, "did not answer"},
		"timed out": {answeringOperator(`{"result":{"output":"partial","timedOut":true}}`), "did not finish within its timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			r := terminalRouter(t, &TerminalConfig{}, tc.op.serve(t))
			res := toolResult(t, r.executeLocalExec(context.Background(), `{"command":"ls"}`))
			if msg, _ := res["error"].(string); !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", msg, tc.want)
			}
			if _, ok := res["exit_code"]; ok {
				t.Fatalf("a command that did not finish has no exit code: %v", res)
			}
		})
	}
}

func TestLocalExecRefusedWithoutPolicyOrCommand(t *testing.T) {
	op := answeringOperator(`{}`)
	url := op.serve(t)
	for _, out := range []string{
		terminalRouter(t, nil, url).executeLocalExec(context.Background(), `{"command":"ls"}`),
		terminalRouter(t, &TerminalConfig{}, url).executeLocalExec(context.Background(), `{"command":"  "}`),
	} {
		if toolResult(t, out)["error"] == nil {
			t.Fatalf("expected an error, got %s", out)
		}
	}
	if len(op.sent) != 0 {
		t.Fatalf("nothing should have been sent, got %v", op.sent)
	}
}

// The description is how the agent learns when and how to use the tool.
func TestLocalExecDefinition(t *testing.T) {
	if defs := (*TerminalConfig)(nil).ToolDefinitions(); defs != nil {
		t.Fatalf("no policy, no tool: %v", defs)
	}
	defs := (&TerminalConfig{MaxTimeoutSeconds: 60}).ToolDefinitions()
	if len(defs) != 1 || defs[0].Name != "local_exec" || !json.Valid(defs[0].Parameters) {
		t.Fatalf("definitions = %+v", defs)
	}
	for _, want := range []string{"user's own computer", "sandbox_exec", "approve", "do not retry", "no input", "POSIX"} {
		if !strings.Contains(strings.ToLower(defs[0].Description), strings.ToLower(want)) {
			t.Errorf("description does not mention %q", want)
		}
	}
	if !strings.Contains(string(defs[0].Parameters), "default 60, at most 60") {
		t.Errorf("parameters do not state the limits: %s", defs[0].Parameters)
	}
}
