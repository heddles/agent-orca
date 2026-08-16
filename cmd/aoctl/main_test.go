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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestConfigLoadDefaultsWhenAbsent(t *testing.T) {
	t.Setenv("AOCTL_CONFIG_DIR", t.TempDir())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != defaultEndpoint || cfg.ACP != defaultACP {
		t.Fatalf("bad defaults: %+v", cfg)
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
		if req.Name != "acme" || req.TargetNamespace != "tenant-acme" || req.ClientID != "acme-client" { //nolint:goconst

			t.Fatalf("bad request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orc-system","clientID":"acme-client","clientSecret":"gen-secret","targetNamespace":"tenant-acme"}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, "", "sa-token", defaultTimeout, true)
	resp, err := c.CreateTenant(context.Background(), AdminTenantCreateRequest{
		Name:            "acme",
		TargetNamespace: "tenant-acme",
		ClientID:        "acme-client",
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
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orc-system","clientID":"c1","targetNamespace":"tenant-acme"}`)) //nolint:lll

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
		_, _ = w.Write([]byte(`{"tenants":[{"name":"acme","clientID":"c1","targetNamespace":"tenant-acme"}],"count":1}`))
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
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orc-system","clientID":"c1","targetNamespace":"tenant-acme"}`)) //nolint:lll

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
		_, _ = w.Write([]byte(`{"name":"acme","namespace":"agent-orc-system","clientID":"c1","clientSecret":"gen-secret","targetNamespace":"tenant-acme"}`)) //nolint:lll

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
		if len(req.Input) != 1 || req.Input[0].Content != "hello" {
			t.Errorf("bad input: %+v", req.Input)
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

// TestCmdAgentsRunMissingInput verifies that --input is required.
func TestCmdAgentsRunMissingInput(t *testing.T) {
	_, _, err := runCLI(t, map[string]string{"AOCTL_ENDPOINT": "http://localhost:8084"},
		"agents", "run", "support-bot", "--token", "tok")
	if err == nil {
		t.Fatal("expected error for missing --input")
	}
	if !strings.Contains(err.Error(), "--input is required") {
		t.Fatalf("expected --input is required error, got: %v", err)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_name":"bot","run_id":"run-xyz","status":"created","created_at":"2024-01-01T00:00:00Z"}`)) //nolint:lll

	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	resp, err := c.CreateAgentRun(context.Background(), "bot", ACPRunRequest{
		Input: []ACPMessagePart{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	if resp.RunID != "run-xyz" {
		t.Fatalf("expected run ID 'run-xyz', got %q", resp.RunID)
	}
}

// TestCreateAgentRunError verifies error handling for CreateAgentRun.
func TestCreateAgentRunError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()
	c := newClient(srv.URL, srv.URL, "tok", defaultTimeout, true)
	_, err := c.CreateAgentRun(context.Background(), "nonexistent", ACPRunRequest{
		Input: []ACPMessagePart{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// Ensure the buffer import is used.
var _ = bytes.Buffer{}
