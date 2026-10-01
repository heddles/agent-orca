/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except this file except in compliance with the License.
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
	"errors"
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

func TestParseSSE(t *testing.T) {
	raw := "event: status\ndata: {\"phase\":\"Running\"}\n\n" +
		"event: token\ndata: hello\n\n" +
		"event: complete\ndata: {\"id\":\"t1\"}\n\n"
	events := parseSSE(strings.NewReader(raw))
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Type != "status" || events[0].Data != `{"phase":"Running"}` {
		t.Fatalf("bad event 0: %+v", events[0])
	}
	if events[1].Data != "hello" { //nolint:goconst

		t.Fatalf("bad event 1: %+v", events[1])
	}
	if events[2].Data != `{"id":"t1"}` {
		t.Fatalf("bad event 2: %+v", events[2])
	}
}

// TestNormalizeEndpoint verifies that known API route suffixes are stripped
// from endpoint URLs to prevent double-path issues, while legitimate proxy
// prefixes and bare host URLs are preserved.
func TestNormalizeACPEndpoint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare host", "http://agent-orca.local", "http://agent-orca.local"},
		{"with /agents", "http://agent-orca.local/agents", "http://agent-orca.local"},
		{"with /runs", "http://agent-orca.local/runs", "http://agent-orca.local"},
		{"with /sessions", "http://agent-orca.local/sessions", "http://agent-orca.local"},
		{"with /session", "http://agent-orca.local/session", "http://agent-orca.local"},
		{"with trailing slash /agents/", "http://agent-orca.local/agents/", "http://agent-orca.local"},
		{"with proxy prefix /acp", "http://agent-orca.local/acp", "http://agent-orca.local/acp"},
		{"localhost:8000", "http://localhost:8000", "http://localhost:8000"},
		{"localhost:8000/agents", "http://localhost:8000/agents", "http://localhost:8000"},
		{"endpoint with /tasks", "http://host/tasks", "http://host"},
		{"endpoint with /v1/tasks", "http://host/v1/tasks", "http://host"},
		{"endpoint with /admin/tenants", "http://host/admin/tenants", "http://host"},
		{"endpoint with /admin", "http://host/admin", "http://host"},
		{"endpoint with /oauth/token", "http://host/oauth/token", "http://host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeEndpoint(strings.TrimRight(tc.in, "/"))
			if got != tc.want {
				t.Errorf("normalizeEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewClient_NormalizesEndpoints verifies that newClient strips known API
// route suffixes from both the External Task API and ACP endpoints.
func TestNewClient_NormalizesACPEndpoint(t *testing.T) {
	c := newClient("http://host/tasks", "http://host/agents", "tok", defaultTimeout, false)
	if c.Endpoint != "http://host" {
		t.Fatalf("expected Endpoint http://host, got %q", c.Endpoint)
	}
	if c.ACP != "http://host" {
		t.Fatalf("expected ACP endpoint http://host, got %q", c.ACP)
	}
}

func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("bad content-type %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"abc123","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "", defaultTimeout, true)
	tok, err := c.Login(context.Background(), "cid", "secret")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok != "abc123" {
		t.Fatalf("expected abc123, got %q", tok)
	}
}

func TestLogin_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "", defaultTimeout, true)
	if _, err := c.Login(context.Background(), "cid", "secret"); err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestSubmitTask_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("bad auth header %q", got)
		}
		var sub TaskSubmission
		if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if sub.Agent != "a" || sub.Input != "i" {
			t.Fatalf("bad body %+v", sub)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"t1","agent":"a","status":"Pending","links":{"self":"/v1/tasks/t1","stream":"/v1/tasks/t1/stream"}}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	tr, code, err := c.SubmitTask(context.Background(), TaskSubmission{Agent: "a", Input: "i"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if code != 201 {
		t.Fatalf("expected 201, got %d", code)
	}
	if tr.ID != "t1" || tr.Links.Stream != "/v1/tasks/t1/stream" {
		t.Fatalf("bad response: %+v", tr)
	}
}

func TestSubmitTask_ErrorOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Header().Set("Retry-After", "59")
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	_, code, err := c.SubmitTask(context.Background(), TaskSubmission{Agent: "a", Input: "i"})
	if err == nil {
		t.Fatal("expected error for 429")
	}
	if code != 429 {
		t.Fatalf("expected code 429, got %d", code)
	}
}

func TestGetTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/t1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"t1","agent":"a","status":"Succeeded","output":"hi","spendUSD":"0.01"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	tr, err := c.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tr.Status != "Succeeded" || tr.Output != "hi" {
		t.Fatalf("bad task: %+v", tr)
	}
}

func TestListTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") != "a" {
			t.Errorf("expected agent filter")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"t1","agent":"a","status":"Pending"}],"count":1}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	tasks, err := c.ListTasks(context.Background(), "a", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("bad list: %+v", tasks)
	}
}

func TestCancelTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/tasks/t1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	if err := c.CancelTask(context.Background(), "t1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestListAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"a","description":"d","input_content_types":["text/plain"],"output_content_types":["text/plain"]}]}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	agents, err := c.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "a" {
		t.Fatalf("bad agents: %+v", agents)
	}
}

func TestStreamTask(t *testing.T) {
	// Hijack the connection to serve an SSE stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/t1/stream" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: token\ndata: hello\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("event: complete\ndata: {\"id\":\"t1\"}\n\n"))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", 5*time.Second, true)
	ch, err := c.StreamTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var got []Event //nolint:prealloc

	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].Data != "hello" || got[1].Type != "complete" {
		t.Fatalf("bad events: %+v", got)
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AOCTL_CONFIG_DIR", dir)

	if err := saveConfig(&Config{Endpoint: defaultEndpoint, ACP: defaultACP, Token: "xyz"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Token != "xyz" || cfg.Endpoint != defaultEndpoint {
		t.Fatalf("bad config: %+v", cfg)
	}
	// File written with 0600 perms.
	info, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %v", info.Mode().Perm())
	}
}

// TestConfigLoadDefaultsWhenAbsent verifies that loadConfig returns an empty
// Config (not defaults) when no config file exists. Defaults are now applied
// centrally in PersistentPreRunE so that the ACP endpoint can be derived from
// the External Task API endpoint when only one is configured.
func TestConfigLoadDefaultsWhenAbsent(t *testing.T) {
	t.Setenv("AOCTL_CONFIG_DIR", t.TempDir())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "" || cfg.ACP != "" {
		t.Fatalf("expected empty config when absent, got: %+v", cfg)
	}
}

// runCLI instantiates the root command with output captured into buffers and
// the given args, returning (stdout, stderr, error). `env` is merged into the
// process environment (e.g. to set AOCTL_ENDPOINT / AOCTL_CONFIG_DIR).
func runCLI(t *testing.T, env map[string]string, args ...string) (string, string, error) { //nolint:unparam

	t.Helper()
	t.Setenv("AOCTL_CONFIG_DIR", t.TempDir())
	root, s := newRootCmd()
	var out, errb bytes.Buffer
	s.out = &out
	s.errw = &errb
	// Route the cobra command's own stdout/stderr (used by `validate *`, which
	// writes via cmd.OutOrStdout) into the captured buffers so runCLI can assert
	// on it. Other commands write directly to s.out/s.errw and are unaffected.
	root.SetOut(&out)
	for k, v := range env {
		t.Setenv(k, v)
	}
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errb.String(), err
}

func TestCmdLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok123","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "login", "--endpoint", srv.URL, "--acp-endpoint", srv.URL,
		"--client-id", "cid", "--client-secret", "s")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(stdout, "logged in") {
		t.Fatalf("expected login message, got %q", stdout)
	}
	// Token persisted to config.
	t.Setenv("AOCTL_CONFIG_DIR", t.TempDir())
}

func TestCmdLogin_MissingCredentials(t *testing.T) {
	_, _, err := runCLI(t, nil, "login", "--endpoint", "http://x")
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
}

func TestCmdTasksSubmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"t1","agent":"a","status":"Pending","links":{"self":"/v1/tasks/t1"}}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "tasks", "submit",
		"--endpoint", srv.URL, "--token", "tok", "--agent", "a", "--input", "hello")
	if err != nil {
		t.Fatalf("submit: %v (stdout=%q)", err, stdout)
	}
	if !strings.Contains(stdout, "t1") {
		t.Fatalf("expected task id in output, got %q", stdout)
	}
}

func TestCmdTasksSubmit_MissingArgs(t *testing.T) {
	_, _, err := runCLI(t, nil, "tasks", "submit", "--endpoint", "http://x", "--token", "t", "--agent", "a")
	if err == nil {
		t.Fatal("expected error for missing --input")
	}
}

func TestCmdTasksList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"t1","agent":"a","status":"Pending"}],"count":1}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "tasks", "ls", "--endpoint", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(stdout, "t1") {
		t.Fatalf("expected task in output, got %q", stdout)
	}
}

func TestCmdAgentsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"my-agent","description":"d"}]}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "agents", "list", "--acp-endpoint", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	if !strings.Contains(stdout, "my-agent") {
		t.Fatalf("expected agent in output, got %q", stdout)
	}
}

// TestCmdACPEndpointDerivedFromEndpoint verifies that when AOCTL_ACP_ENDPOINT is
// not set, the ACP API endpoint is derived from AOCTL_ENDPOINT by stripping the
// path component (only scheme://host[:port] is kept). This mirrors real
// deployments where both APIs live behind the same ingress host.
func TestCmdACPEndpointDerivedFromEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s (expected /agents)", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"derived-agent","description":"d"}]}`))
	}))
	defer srv.Close()

	// Only set AOCTL_ENDPOINT (with a path prefix, as it would be behind an
	// ingress). The ACP endpoint should be derived as scheme://host only.
	stdout, _, err := runCLI(t, map[string]string{
		"AOCTL_ENDPOINT": "http://" + srv.Listener.Addr().String() + "/tasks/v1",
	}, "agents", "list", "--token", "tok")
	if err != nil {
		t.Fatalf("agents list with derived ACP endpoint: %v", err)
	}
	if !strings.Contains(stdout, "derived-agent") {
		t.Fatalf("expected agent from derived ACP endpoint, got %q", stdout)
	}
}

// TestCmdACPEndpointExplicitStaysIntact verifies that when AOCTL_ACP_ENDPOINT is
// explicitly set, it is used as-is and NOT overwritten by the derivation logic.
func TestCmdACPEndpointExplicitStaysIntact(t *testing.T) {
	acpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"explicit-agent","description":"d"}]}`))
	}))
	defer acpSrv.Close()

	// Set a bogus task endpoint but the correct ACP endpoint explicitly.
	stdout, _, err := runCLI(t, map[string]string{
		"AOCTL_ENDPOINT":     "http://127.0.0.1:1/tasks/v1",
		"AOCTL_ACP_ENDPOINT": acpSrv.URL,
	}, "agents", "list", "--token", "tok")
	if err != nil {
		t.Fatalf("agents list with explicit ACP endpoint: %v", err)
	}
	if !strings.Contains(stdout, "explicit-agent") {
		t.Fatalf("expected agent from explicit ACP endpoint, got %q", stdout)
	}
}

func TestCmdTasksCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, nil, "tasks", "cancel", "t1", "--endpoint", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(stdout, "cancelled") {
		t.Fatalf("expected cancelled, got %q", stdout)
	}
}

func TestGetTask_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	if _, err := c.GetTask(context.Background(), "missing"); err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestListTasks_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	if _, err := c.ListTasks(context.Background(), "", ""); err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestCancelTask_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	if err := c.CancelTask(context.Background(), "t1"); err == nil {
		t.Fatal("expected error for 409")
	}
}

func TestListAgents_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	if _, err := c.ListAgents(context.Background()); err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestConfigDir_UsesHomeWhenDirUnset(t *testing.T) {
	t.Setenv("AOCTL_CONFIG_DIR", "")
	d, err := configDir()
	if err != nil {
		t.Fatalf("configDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "."+appName)
	if d != want {
		t.Fatalf("expected %q, got %q", want, d)
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AOCTL_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for malformed config JSON")
	}
}

func TestSaveConfig_MkdirAllFails(t *testing.T) {
	// Point config dir at an existing file path so MkdirAll fails.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AOCTL_CONFIG_DIR", blocker)
	if err := saveConfig(&Config{Endpoint: defaultEndpoint, ACP: defaultACP, Token: "t"}); err == nil {
		t.Fatal("expected MkdirAll error when config dir is a file")
	}
}

// TestStreamAndPrint covers the SSE rendering path (token/complete/status/error).
func TestStreamAndPrint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: status\ndata: {\"phase\":\"Running\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("event: token\ndata: hello-world\n\n"))
		_, _ = w.Write([]byte("event: complete\ndata: {\"id\":\"t1\",\"agent\":\"a\",\"status\":\"Succeeded\"}\n\n"))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", 5*time.Second, true)
	var out, errb bytes.Buffer
	if err := streamAndPrint(context.Background(), c, "t1", &out, &errb); err != nil {
		t.Fatalf("streamAndPrint: %v", err)
	}
	if !strings.Contains(out.String(), "hello-world") {
		t.Fatalf("expected token text in output, got %q", out.String())
	}
	if !strings.Contains(out.String(), "t1") {
		t.Fatalf("expected complete event id in output, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "phase") {
		t.Fatalf("expected phase status on stderr, got %q", errb.String())
	}
}

// TestStreamAndPrint_ErrorEvent verifies an "error" event surfaces as an error.
func TestStreamAndPrint_ErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: something went wrong\n\n"))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", 5*time.Second, true)
	err := streamAndPrint(context.Background(), c, "t1", &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "something went wrong") {
		t.Fatalf("expected stream error, got %v", err)
	}
}

// TestCmdEndpointFromEnv verifies AOCTL_ENDPOINT is honored when --endpoint is
// not passed (covers the env-resolution branch in PersistentPreRunE).
func TestCmdEndpointFromEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"from-env","agent":"a","status":"Pending"}],"count":1}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL},
		"tasks", "ls")
	if err != nil {
		t.Fatalf("ls with env endpoint: %v", err)
	}
	if !strings.Contains(stdout, "from-env") {
		t.Fatalf("expected task from env-endpoint server, got %q", stdout)
	}
}

// TestSubmitTask_TransportError covers the network-error branch of SubmitTask.
func TestSubmitTask_TransportError(t *testing.T) {
	c := newClient("http://127.0.0.1:1", "", "tok", 200*time.Millisecond, true)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := c.SubmitTask(ctx, TaskSubmission{Agent: "a", Input: "i"}); err == nil {
		t.Fatal("expected transport error for unreachable endpoint")
	}
}

// --- Admin API client tests ---

func TestCreateTenant_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants" || r.Method != http.MethodPost { //nolint:goconst

			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sa-token" {
			t.Errorf("bad auth header %q", got)
		}
		var req AdminTenantCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if req.Name != "acme" || len(req.AllowedNamespaces) != 1 || //nolint:goconst
			req.AllowedNamespaces[0] != "tenant-acme" ||
			req.ClientID != "acme-client" {

			t.Fatalf("bad request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orca-system","clientID":"acme-client","clientSecret":"gen-secret","allowedNamespaces":["tenant-acme"]}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "sa-token", defaultTimeout, true)
	resp, err := c.CreateTenant(context.Background(), AdminTenantCreateRequest{
		Name:              "acme",
		AllowedNamespaces: []string{"tenant-acme"},
		ClientID:          "acme-client",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Name != "acme" || resp.ClientSecret != "gen-secret" {
		t.Fatalf("bad response: %+v", resp)
	}
}

func TestCreateTenant_ErrorOnNon201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	_, err := c.CreateTenant(context.Background(), AdminTenantCreateRequest{Name: "acme"})
	if err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestListTenants_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenants":[{"name":"acme","clientID":"c1"},{"name":"beta","clientID":"c2"}],"count":2}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "sa-token", defaultTimeout, true)
	tenants, err := c.ListTenants(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tenants) != 2 || tenants[0].Name != "acme" || tenants[1].Name != "beta" {
		t.Fatalf("bad list: %+v", tenants)
	}
}

func TestListTenants_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	_, err := c.ListTenants(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestGetTenant_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants/acme" || r.Method != http.MethodGet { //nolint:goconst

			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orca-system","clientID":"c1","allowedNamespaces":["tenant-acme"]}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "sa-token", defaultTimeout, true)
	tenant, err := c.GetTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tenant.Name != "acme" || tenant.ClientID != "c1" {
		t.Fatalf("bad response: %+v", tenant)
	}
}

func TestGetTenant_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	_, err := c.GetTenant(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestRotateTenantSecret_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants/acme/rotate-secret" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clientSecret":"new-secret-123"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "sa-token", defaultTimeout, true)
	secret, err := c.RotateTenantSecret(context.Background(), "acme")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if secret != "new-secret-123" {
		t.Fatalf("expected new-secret-123, got %q", secret)
	}
}

func TestRotateTenantSecret_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	_, err := c.RotateTenantSecret(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// --- auto-refresh transport tests ---
//
// These cover the change that lets a long-lived `aoctl acp serve` process
// survive an id_token expiry (mid-session 401) without the user re-logging in
// and re-creating the Zed agent.

// withRefreshTransport wires a refresh callback + the refreshable transport onto
// a test client, mirroring what runACPServe does for an OIDC session.
func withRefreshTransport(t *testing.T, c *Client, refresh func(context.Context) (string, error)) {
	t.Helper()
	c.refresh = refresh
	base := c.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.HTTP.Transport = &refreshableTransport{base: base, owner: c}
}

// testRefreshedToken is the token the refresh callbacks in these tests swap in.
const testRefreshedToken = "refreshed-token"

// TestRefreshableTransport_401RefreshesAndRetries verifies that a 401 triggers
// exactly one refresh and one retry, with the retried request carrying the
// refreshed bearer token. Uses GetACPRun-equivalent GET (ListAgents) — the
// common case where a mid-session poll is rejected after expiry.
func TestRefreshableTransport_401RefreshesAndRetries(t *testing.T) {
	var (
		mu       sync.Mutex
		n        int
		lastAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		lastAuth = r.Header.Get("Authorization")
		attempt := n
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized) // stale token
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[]}`))
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.URL, "stale-token", defaultTimeout, true)
	refreshed := false
	withRefreshTransport(t, c, func(ctx context.Context) (string, error) {
		mu.Lock()
		refreshed = true
		mu.Unlock()
		return testRefreshedToken, nil
	})

	agents, err := c.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected empty agent list, got %+v", agents)
	}
	mu.Lock()
	defer mu.Unlock()
	if !refreshed {
		t.Fatal("expected the refresh callback to fire on 401")
	}
	if n != 2 {
		t.Fatalf("expected 2 requests (401 then retry), got %d", n)
	}
	if lastAuth != "Bearer refreshed-token" {
		t.Fatalf("expected retried request to use refreshed token, got %q", lastAuth)
	}
	if c.getToken() != testRefreshedToken {
		t.Fatalf("expected client token refreshed in memory, got %q", c.getToken())
	}
}

// TestRefreshableTransport_NoRefreshWithoutCallback verifies that a client
// without a refresh callback surfaces a 401 unchanged (existing behaviour for
// one-shot commands): a single request, then an error.
func TestRefreshableTransport_NoRefreshWithoutCallback(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	// No refresh callback wired: behaves like the old client.
	_, err := c.ListAgents(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 with no refresh callback")
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 request (no retry), got %d", n)
	}
}

// TestRefreshableTransport_Post401RetriesWithBody verifies that a 401 on a
// POST with a body retries with the body intact (not a re-read of an already
// drained stream) and the refreshed token. runCreateAgentRun-style calls must
// survive a mid-session expiry too.
func TestRefreshableTransport_Post401RetriesWithBody(t *testing.T) {
	var (
		mu       sync.Mutex
		n        int
		bodies   []string
		lastAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n++
		bodies = append(bodies, string(b))
		lastAuth = r.Header.Get("Authorization")
		attempt := n
		mu.Unlock()
		_ = r.Body.Close()
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"test-agent","run_id":"run-1","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.URL, "stale-token", defaultTimeout, true)
	withRefreshTransport(t, c, func(ctx context.Context) (string, error) {
		return testRefreshedToken, nil
	})

	_, err := c.CreateAgentRun(context.Background(), "test-agent", ACPRunRequest{
		Input: []ACPMessage{{Role: "user", Parts: []ACPMessagePart{
			{ContentType: "text/plain", Content: "hello"},
		}}},
	})
	if err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 requests, got %d", n)
	}
	if lastAuth != "Bearer refreshed-token" {
		t.Fatalf("expected retried request to use refreshed token, got %q", lastAuth)
	}
	for i, b := range bodies {
		if !strings.Contains(b, "hello") {
			t.Fatalf("request %d body missing payload: %q", i, b)
		}
	}
}

// TestRefreshableTransport_RefreshErrorPropagates verifies that when the refresh
// callback itself fails, the original 401 is surfaced as an error (no silent
// retry loop, no stale-token success).
func TestRefreshableTransport_RefreshErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClient(srv.URL, srv.URL, "bad-token", defaultTimeout, true)
	withRefreshTransport(t, c, func(ctx context.Context) (string, error) {
		return "", errors.New("idp unreachable")
	})
	_, err := c.ListAgents(context.Background())
	if err == nil {
		t.Fatal("expected error when refresh fails")
	}
	if !strings.Contains(err.Error(), "idp unreachable") {
		t.Fatalf("expected refresh error to surface, got: %v", err)
	}
}

func TestDeleteTenant_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants/acme" || r.Method != http.MethodDelete {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "sa-token", defaultTimeout, true)
	if err := c.DeleteTenant(context.Background(), "acme"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestDeleteTenant_ErrorOnNon204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "tok", defaultTimeout, true)
	if err := c.DeleteTenant(context.Background(), "missing"); err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestCreateTenant_TransportError(t *testing.T) {
	c := newClient("http://127.0.0.1:1", "", "tok", 200*time.Millisecond, true)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := c.CreateTenant(ctx, AdminTenantCreateRequest{Name: "acme"}); err == nil {
		t.Fatal("expected transport error for unreachable endpoint")
	}
}

// TestCmdAdminTenantsList verifies the `aoctl admin tenants list` CLI command.
func TestCmdAdminTenantsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenants":[{"name":"acme","clientID":"c1","allowedNamespaces":["tenant-acme"]}],"count":1}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL},
		"admin", "tenants", "list", "--token", "sa-token")
	if err != nil {
		t.Fatalf("admin tenants list: %v", err)
	}
	if !strings.Contains(stdout, "acme") || !strings.Contains(stdout, "c1") {
		t.Fatalf("expected tenant in output, got %q", stdout)
	}
}

// TestCmdAdminTenantsGet verifies the `aoctl admin tenants get` CLI command.
func TestCmdAdminTenantsGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orca-system","clientID":"c1","allowedNamespaces":["tenant-acme"]}`)) //nolint:lll

	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL},
		"admin", "tenants", "get", "acme", "--token", "sa-token")
	if err != nil {
		t.Fatalf("admin tenants get: %v", err)
	}
	if !strings.Contains(stdout, "acme") || !strings.Contains(stdout, "c1") {
		t.Fatalf("expected tenant details in output, got %q", stdout)
	}
}

// TestCmdAdminTenantsCreate verifies the `aoctl admin tenants create` CLI command.
func TestCmdAdminTenantsCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orca-system","clientID":"c1","clientSecret":"gen-secret","allowedNamespaces":["tenant-acme"]}`)) //nolint:lll

	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL},
		"admin", "tenants", "create", "acme",
		"--namespace", "tenant-acme", "--client-id", "c1", "--token", "sa-token")
	if err != nil {
		t.Fatalf("admin tenants create: %v", err)
	}
	if !strings.Contains(stdout, "acme") || !strings.Contains(stdout, "gen-secret") {
		t.Fatalf("expected tenant + secret in output, got %q", stdout)
	}
}

// TestCmdAdminTenantsCreate_MissingArgs verifies error when required flags are missing.
func TestCmdAdminTenantsCreate_MissingArgs(t *testing.T) {
	_, _, err := runCLI(t, nil, "admin", "tenants", "create", "acme", "--token", "t")
	if err == nil {
		t.Fatal("expected error for missing --namespace and --client-id")
	}
}

// TestCmdAdminTenantsRotateSecret verifies the `aoctl admin tenants rotate-secret` CLI command.
func TestCmdAdminTenantsRotateSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants/acme/rotate-secret" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clientSecret":"new-secret-456"}`))
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL},
		"admin", "tenants", "rotate-secret", "acme", "--token", "sa-token")
	if err != nil {
		t.Fatalf("admin tenants rotate-secret: %v", err)
	}
	if !strings.Contains(stdout, "new-secret-456") {
		t.Fatalf("expected new secret in output, got %q", stdout)
	}
}

// TestCmdAdminTenantsDelete verifies the `aoctl admin tenants delete` CLI command.
func TestCmdAdminTenantsDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/tenants/acme" || r.Method != http.MethodDelete {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL},
		"admin", "tenants", "delete", "acme", "--token", "sa-token")
	if err != nil {
		t.Fatalf("admin tenants delete: %v", err)
	}
	if !strings.Contains(stdout, "deleted") {
		t.Fatalf("expected 'deleted' in output, got %q", stdout)
	}
}

// TestCmdAgentsDescribe verifies the `aoctl agents describe <name>` CLI command.
func TestCmdAgentsDescribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/support-bot" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"support-bot","description":"Support bot","input_schema":{"type":"object"},"allowed_tools":[{"name":"_clarify"},{"name":"web-search"}],"knowledge_bases":["docs"],"guardrail_policy":"phi-redact","clarify_available":true}`)) //nolint:lll

	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL, "AOCTL_ACP_ENDPOINT": srv.URL},
		"agents", "describe", "support-bot", "--token", "tok")
	if err != nil {
		t.Fatalf("agents describe: %v", err)
	}
	if !strings.Contains(stdout, "support-bot") {
		t.Fatalf("expected 'support-bot' in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "web-search") {
		t.Fatalf("expected 'web-search' in output, got %q", stdout)
	}
}

// TestCmdAgentsRun verifies the `aoctl agents run <name> --input <s>` CLI command.
func TestCmdAgentsRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/support-bot/run" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req ACPRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		// Verify the ACP-spec format: input[0] should be a message with role "user"
		// and one part with content_type "text/plain" and the expected content.
		if len(req.Input) != 1 {
			t.Fatalf("expected 1 message, got %d", len(req.Input))
		}
		msg := req.Input[0]
		if msg.Role != "user" {
			t.Errorf("expected role 'user', got %q", msg.Role)
		}
		if len(msg.Parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(msg.Parts))
		}
		if msg.Parts[0].ContentType != "text/plain" {
			t.Errorf("expected content_type 'text/plain', got %q", msg.Parts[0].ContentType)
		}
		if msg.Parts[0].Content != "hello" {
			t.Errorf("expected content 'hello', got %q", msg.Parts[0].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"support-bot","run_id":"run-123","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll

	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL, "AOCTL_ACP_ENDPOINT": srv.URL},
		"agents", "run", "support-bot", "--input", "hello", "--token", "tok")
	if err != nil {
		t.Fatalf("agents run: %v", err)
	}
	if !strings.Contains(stdout, "run-123") {
		t.Fatalf("expected run ID in output, got %q", stdout)
	}
}

// TestCmdAgentsRunStdin verifies that input can be piped via stdin.
func TestCmdAgentsRunStdin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ACPRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		if len(req.Input) != 1 || len(req.Input[0].Parts) != 1 ||
			req.Input[0].Parts[0].Content != "piped input" {
			t.Errorf("bad input: %+v", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"bot","run_id":"run-stdin","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll

	}))
	defer srv.Close()

	root, s := newRootCmd()
	var out, errb bytes.Buffer
	s.out = &out
	s.errw = &errb
	s.endpoint = srv.URL
	s.acp = srv.URL
	s.token = "tok"
	s.timeout = defaultTimeout
	s.isTerminal = func() bool { return false }                 // stdin is NOT a terminal
	s.stdin = bufio.NewReader(strings.NewReader("piped input")) // simulate piped input
	root.SetArgs([]string{"agents", "run", "bot"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("agents run (stdin): %v", err)
	}
	if !strings.Contains(out.String(), "run-stdin") {
		t.Fatalf("expected run ID in output, got %q", out.String())
	}
}

// TestCmdAgentsRunFile verifies that input can be read from a file.
func TestCmdAgentsRunFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ACPRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		if len(req.Input) != 1 || len(req.Input[0].Parts) != 1 ||
			req.Input[0].Parts[0].Content != "file contents" {
			t.Errorf("bad input: %+v", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"bot","run_id":"run-file","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll

	}))
	defer srv.Close()

	tmp := t.TempDir() + "/input.txt"
	if err := os.WriteFile(tmp, []byte("file contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL, "AOCTL_ACP_ENDPOINT": srv.URL},
		"agents", "run", "bot", "--file", tmp, "--token", "tok")
	if err != nil {
		t.Fatalf("agents run (file): %v", err)
	}
	if !strings.Contains(stdout, "run-file") {
		t.Fatalf("expected run ID in output, got %q", stdout)
	}
}

// TestCmdAgentsRunContentType verifies the --content-type flag is sent correctly.
func TestCmdAgentsRunContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ACPRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		if len(req.Input) != 1 || len(req.Input[0].Parts) != 1 ||
			req.Input[0].Parts[0].ContentType != "application/json" {
			t.Errorf("expected content_type 'application/json', got %+v", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"bot","run_id":"run-ct","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll

	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": srv.URL, "AOCTL_ACP_ENDPOINT": srv.URL},
		"agents", "run", "bot", "--input", "hello", "--content-type", "application/json", "--token", "tok")
	if err != nil {
		t.Fatalf("agents run (content-type): %v", err)
	}
	if !strings.Contains(stdout, "run-ct") {
		t.Fatalf("expected run ID in output, got %q", stdout)
	}
}

// TestCmdAgentsRunMissingInput verifies that input is required.
func TestCmdAgentsRunMissingInput(t *testing.T) {
	_, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": "http://localhost:8084"},
		"agents", "run", "support-bot", "--token", "tok")
	if err == nil {
		t.Fatal("expected error for missing --input")
	}
	if !strings.Contains(err.Error(), "is required") {
		t.Fatalf("expected 'required' error, got: %v", err)
	}
}

// TestCmdAgentsRunInputAndFileMutuallyExclusive verifies error when both --input and --file given.
func TestCmdAgentsRunInputAndFileMutuallyExclusive(t *testing.T) {
	tmp := t.TempDir() + "/input.txt"
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": "http://localhost:8084"},
		"agents", "run", "bot", "--input", "hello", "--file", tmp, "--token", "tok")
	if err == nil {
		t.Fatal("expected error for --input + --file")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected 'mutually exclusive' error, got: %v", err)
	}
}

// TestGetAgentManifest verifies the Client.GetAgentManifest method.
func TestGetAgentManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/bot" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"bot","description":"A bot","clarify_available":true}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	manifest, err := c.GetAgentManifest(context.Background(), "bot")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	if manifest.Name != "bot" {
		t.Fatalf("expected name 'bot', got %q", manifest.Name)
	}
	if !manifest.ClarifyAvailable {
		t.Fatal("expected clarify_available to be true")
	}
}

// TestCreateAgentRun verifies the Client.CreateAgentRun method.
func TestCreateAgentRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/bot/run" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req ACPRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		if len(req.Input) != 1 || req.Input[0].Role != "user" ||
			len(req.Input[0].Parts) != 1 || req.Input[0].Parts[0].Content != "hello" {
			t.Errorf("bad input: %+v", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"bot","run_id":"run-xyz","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	resp, err := c.CreateAgentRun(context.Background(), "bot", ACPRunRequest{
		Input: []ACPMessage{{
			Role: "user",
			Parts: []ACPMessagePart{{
				ContentType: "text/plain",
				Content:     "hello",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	if resp.RunID != "run-xyz" {
		t.Fatalf("expected run ID 'run-xyz', got %q", resp.RunID)
	}
}

// TestCreateAgentRunError verifies error handling for CreateAgentRun, including
// parsing of ACP-structured error responses.
func TestCreateAgentRunError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_input","message":"input content is required"}`)) //nolint:lll
	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	_, err := c.CreateAgentRun(context.Background(), "nonexistent", ACPRunRequest{
		Input: []ACPMessage{{
			Role:  "user",
			Parts: []ACPMessagePart{{ContentType: "text/plain", Content: "hello"}},
		}},
	})
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("expected ACP error code in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "input content is required") {
		t.Fatalf("expected ACP error message in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Hint:") {
		t.Fatalf("expected remediation hint in error message, got: %v", err)
	}
}

// Ensure the buffer import is used.
var _ = bytes.Buffer{}
