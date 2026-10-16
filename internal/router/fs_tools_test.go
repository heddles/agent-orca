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

package router

import (
	"context"
	"encoding/json"
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

// fsSATokenFile writes a fake SA token file for relay authentication.
func fsSATokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return p
}

// fakeOperatorAPI emulates the operator internal-API endpoints the fs relay
// uses: POST /agentrun/{ns}/{run}/fs (enqueue) and GET /agentrun/{ns}/{run}/fs/{id}
// (take, ?wait= ignored). It records enqueued methods for assertions.
type fakeOperatorAPI struct {
	srv     *httptest.Server
	mu      sync.Mutex
	methods []string
	params  []json.RawMessage
	resp    string // response body returned for every take ("" = always 204)
}

func newFakeOperatorAPI(t *testing.T, takeResp string) *fakeOperatorAPI {
	t.Helper()
	f := &fakeOperatorAPI{resp: takeResp}
	mux := http.NewServeMux()
	mux.HandleFunc("/agentrun/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/agentrun/"), "/")
		if len(parts) == 3 && parts[2] == "fs" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(body, &req)
			f.mu.Lock()
			f.methods = append(f.methods, req.Method)
			f.params = append(f.params, req.Params)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "req-1", "method": req.Method, "params": req.Params})
			return
		}
		if len(parts) == 4 && parts[2] == "fs" && r.Method == http.MethodGet {
			if f.resp == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.resp))
			return
		}
		http.NotFound(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOperatorAPI) methodsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.methods...)
}

// newFSTestRouter builds a minimal Router with the given filesystem config.
func newFSTestRouter(t *testing.T, fs *FilesystemConfig, operatorURL, tokenFile string) *Router {
	t.Helper()
	cfg := &Config{
		RunName:              "run-1",
		RunNamespace:         "ns-1",
		CheckpointEvery:      0,
		ContextWindowReserve: 0.2,
		LLMRequestTimeout:    time.Hour,
		OperatorAPIURL:       operatorURL,
		SATokenFile:          tokenFile,
		Filesystem:           fs,
		Providers: []ProviderConfig{{
			Name:          "test",
			LiteLLMModel:  "openai/gpt-4o",
			APIKeyFile:    tokenFile,
			ContextWindow: 100000,
			Weight:        1,
		}},
		Safeguards: RouterSafeguards{ToolExecutionTimeoutSec: 60},
	}
	r, err := New(cfg, newTestStore(t), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.cancelFunc() })
	return r
}

func TestFSExecuteReadRelaySuccess(t *testing.T) {
	api := newFakeOperatorAPI(t, `{"id":"req-1","result":{"content":"hello workspace"}}`)
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly}, api.srv.URL, fsSATokenFile(t))

	out := r.executeFSRead(context.Background(), `{"path":"src/main.go","line":10,"limit":5}`)
	if out == "" || strings.Contains(out, "error") {
		t.Fatalf("expected file content result, got %s", out)
	}
	var res struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Content != "hello workspace" {
		t.Fatalf("bad result %s (%v)", out, err)
	}
	// The enqueue must have carried the normalized ACP params.
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.methods) != 1 || api.methods[0] != "fs/read_text_file" {
		t.Fatalf("expected one fs/read_text_file enqueue, got %v", api.methods)
	}
	var sent map[string]any
	_ = json.Unmarshal(api.params[0], &sent)
	if sent["path"] != "src/main.go" {
		t.Fatalf("params lost path: %s", api.params[0])
	}
}

func TestFSExecuteWritePolicyGating(t *testing.T) {
	api := newFakeOperatorAPI(t, `{"id":"req-1","result":null}`)

	// readWrite: the write flows through the relay and a null ACP result is
	// surfaced as a positive confirmation rather than a bare "null".
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadWrite}, api.srv.URL, fsSATokenFile(t))
	out := r.executeFSWrite(context.Background(), `{"path":"src/new.go","content":"package main"}`)
	if strings.Contains(out, "error") {
		t.Fatalf("expected write to succeed, got %s", out)
	}
	var res struct {
		Written bool `json:"written"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Written {
		t.Fatalf("write should confirm as {\"written\":true}, got %s", out)
	}

	// readOnly: refused inside the pod — nothing left the cluster.
	r = newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly}, api.srv.URL, fsSATokenFile(t))
	out = r.executeFSWrite(context.Background(), `{"path":"src/new.go","content":"package main"}`)
	if !strings.Contains(out, "error") {
		t.Fatalf("readOnly write should fail, got %s", out)
	}
	if n := len(api.methodsSeen()); n != 1 {
		t.Fatalf("readOnly write must not reach the operator, saw %d enqueues", n)
	}
}

func TestFSExecuteDisabled(t *testing.T) {
	api := newFakeOperatorAPI(t, "")
	r := newFSTestRouter(t, nil, api.srv.URL, fsSATokenFile(t))
	if out := r.executeFSRead(context.Background(), `{"path":"a.go"}`); !strings.Contains(out, "error") {
		t.Fatalf("fs read should fail when disabled, got %s", out)
	}
	if out := r.executeFSWrite(context.Background(), `{"path":"a.go","content":"x"}`); !strings.Contains(out, "error") {
		t.Fatalf("fs write should fail when disabled, got %s", out)
	}
	if n := len(api.methodsSeen()); n != 0 {
		t.Fatalf("disabled fs must not enqueue, saw %d", n)
	}
}

func TestFSExecuteInputValidation(t *testing.T) {
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadWrite}, "http://unreachable.invalid", fsSATokenFile(t))
	cases := []struct{ name, args, expect string }{
		{"missing path read", `{}`, "path is required"},
		{"missing path write", `{"content":"x"}`, "path is required"},
		{"bad read json", `not json`, "invalid arguments"},
		{"bad line", `{"path":"a.go","line":0}`, "line must be >= 1"},
		{"bad limit", `{"path":"a.go","limit":0}`, "limit must be >= 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := r.executeFSRead(context.Background(), c.args)
			if !strings.Contains(out, c.expect) {
				// write-only validations go through executeFSWrite
				out = r.executeFSWrite(context.Background(), c.args)
				if !strings.Contains(out, c.expect) {
					t.Fatalf("expected %q in error result, got %s", c.expect, out)
				}
			}
		})
	}
}

func TestFSRelayTimeoutSurfacesStructuredError(t *testing.T) {
	api := newFakeOperatorAPI(t, "") // takes always 204 → never answered
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly, ToolTimeoutSeconds: 1},
		api.srv.URL, fsSATokenFile(t))
	out := r.executeFSRead(context.Background(), `{"path":"a.go"}`)
	if !strings.Contains(out, "timed out") || !strings.Contains(out, "editor bridge") {
		t.Fatalf("expected timeout error, got %s", out)
	}
}

func TestFSRelayRequestExpiredError(t *testing.T) {
	// The take endpoint returns 410 once the request expired unserved.
	mux := http.NewServeMux()
	mux.HandleFunc("/agentrun/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/agentrun/"), "/")
		if len(parts) == 3 && parts[2] == "fs" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "req-1"})
			return
		}
		if len(parts) == 4 && parts[2] == "fs" {
			http.Error(w, "fs request not found (expired)", http.StatusGone)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly}, srv.URL, fsSATokenFile(t))
	out := r.executeFSRead(context.Background(), `{"path":"a.go"}`)
	if !strings.Contains(out, "expired") || !strings.Contains(out, "aoctl acp serve") {
		t.Fatalf("expected actionable expired error, got %s", out)
	}
}

func TestFSRelayEditorErrorSurfaces(t *testing.T) {
	api := newFakeOperatorAPI(t, `{"id":"req-1","error":{"code":-32002,"message":"path escapes the workspace"}}`)
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly}, api.srv.URL, fsSATokenFile(t))
	out := r.executeFSRead(context.Background(), `{"path":"../../etc/passwd"}`)
	if !strings.Contains(out, "path escapes the workspace") {
		t.Fatalf("expected editor error to surface, got %s", out)
	}
}

func TestFSRelayedToolSkipsGenericToolTimeout(t *testing.T) {
	// The generic per-tool timeout is 60s; an fs tool approved manually can take
	// minutes. Verify dispatchToolCall does not override an fs result with the
	// generic timeout error even when toolCtx would have fired.
	api := newFakeOperatorAPI(t, `{"id":"req-1","result":{"content":"slow but fine"}}`)
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly, ToolTimeoutSeconds: 30},
		api.srv.URL, fsSATokenFile(t))
	out := r.dispatchToolCall(context.Background(), ToolCall{
		Function: FunctionCall{Name: "fs_read_text_file", Arguments: `{"path":"a.go"}`},
	})
	var res struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Content != "slow but fine" {
		t.Fatalf("dispatched fs result wrong: %s", out)
	}
}

func TestFSExecuteListRelay(t *testing.T) {
	api := newFakeOperatorAPI(t, `{"id":"req-1","result":{"path":"/ws","entries":[{"name":"main.go","type":"file","size":12}]}}`)
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly}, api.srv.URL, fsSATokenFile(t))

	// Empty path defaults to the workspace root.
	out := r.executeFSList(context.Background(), `{}`)
	if !strings.Contains(out, "entries") {
		t.Fatalf("expected listing result, got %s", out)
	}
	api.mu.Lock()
	method, params := api.methods[0], api.params[0]
	api.mu.Unlock()
	if method != "fs/list_dir" {
		t.Fatalf("expected fs/list_dir enqueue, got %s", method)
	}
	var sent map[string]any
	_ = json.Unmarshal(params, &sent)
	if sent["path"] != "." {
		t.Fatalf("empty path should default to workspace root, got %s", params)
	}

	// Disabled config refuses to enqueue.
	r = newFSTestRouter(t, nil, api.srv.URL, fsSATokenFile(t))
	if out := r.executeFSList(context.Background(), `{"path":"."}`); !strings.Contains(out, "error") {
		t.Fatalf("list should fail when disabled, got %s", out)
	}
}

func TestFSVersionSkewGuardSurfacesClearError(t *testing.T) {
	// Regression: an operator newer than the model-router image injects a
	// builtin the running router doesn't implement. Use a name that is NOT in
	// this build's dispatch switch (the skew scenario by definition) — the
	// dispatch must return a clear, actionable error instead of falling
	// through to the executor (which used to fail with the cryptic
	// "unknown backend type: builtin").
	api := newFakeOperatorAPI(t, "")
	r := newFSTestRouter(t, &FilesystemConfig{Mode: fsModeReadOnly}, api.srv.URL, fsSATokenFile(t))
	r.cfg.ToolDefinitions = append(r.cfg.ToolDefinitions, ToolDefinition{
		Name: "fs_chmod", BackendType: backendTypeBuiltin,
	})

	out := r.dispatchToolCall(context.Background(), ToolCall{
		Function: FunctionCall{Name: "fs_chmod", Arguments: `{}`},
	})
	if !strings.Contains(out, "not implemented by this model-router build") ||
		!strings.Contains(out, "rebuild and redeploy") {
		t.Fatalf("expected actionable version-skew error, got %s", out)
	}
	if strings.Contains(out, `"+`) {
		t.Fatalf("error message contains raw string-concat junk: %s", out)
	}
	// The executor must never be reached for builtin fall-throughs.
	if n := len(api.methodsSeen()); n != 0 {
		t.Fatalf("skew guard must not enqueue anything, saw %d", n)
	}
}

func TestFilesystemToolDefinitions(t *testing.T) {
	if defs := (*FilesystemConfig)(nil).ToolDefinitions(); defs != nil {
		t.Fatalf("nil config should inject nothing, got %+v", defs)
	}
	ro := &FilesystemConfig{Mode: fsModeReadOnly}
	roDefs := ro.ToolDefinitions()
	roNames := map[string]bool{}
	for _, d := range roDefs {
		roNames[d.Name] = true
	}
	if len(roDefs) != 2 || !roNames[fsToolNameRead] || !roNames[fsToolNameList] || roNames[fsToolNameWrite] {
		t.Fatalf("readOnly should expose list + read only, got %+v", roDefs)
	}
	rw := &FilesystemConfig{Mode: fsModeReadWrite}
	rwDefs := rw.ToolDefinitions()
	rwNames := map[string]bool{}
	for _, d := range rwDefs {
		rwNames[d.Name] = true
	}
	if len(rwDefs) != 3 || !rwNames[fsToolNameRead] || !rwNames[fsToolNameList] || !rwNames[fsToolNameWrite] {
		t.Fatalf("readWrite should expose list + read + write, got %+v", rwDefs)
	}
	for _, td := range rwDefs {
		var schema map[string]any
		if err := json.Unmarshal(td.Parameters, &schema); err != nil {
			t.Fatalf("tool %s has invalid JSON schema: %v", td.Name, err)
		}
	}
}
