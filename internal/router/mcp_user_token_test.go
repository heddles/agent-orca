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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/mcp"
	"github.com/heddles/agent-orca/internal/redact"
)

const (
	userTokenValue = "gho_USER_ACCESS_TOKEN_SENTINEL_abcdef0123456789"
	podSAToken     = "pod-sa-token-value"
)

// userMCPEnv is an operator API and an MCP server in one test: the operator hands
// out userTokenValue, and the MCP server requires it as the bearer, echoes it in a
// tool result when asked, and records every request's Authorization header.
type userMCPEnv struct {
	operator *httptest.Server
	mcp      *httptest.Server
	mu       sync.Mutex
	auths    []string
	opCalls  []string
	opBodies []string
	opStatus int
	opError  string
	expires  time.Time
}

func newUserMCPEnv(t *testing.T) *userMCPEnv {
	t.Helper()
	e := &userMCPEnv{expires: time.Now().Add(time.Hour), opStatus: http.StatusOK}
	e.operator = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.opCalls = append(e.opCalls, r.URL.Path+"|"+r.Header.Get("Authorization"))
		e.opBodies = append(e.opBodies, string(body))
		status, code := e.opStatus, e.opError
		exp := e.expires
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/clarify") { // the run pausing for a connection
			w.WriteHeader(http.StatusOK)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": "the connection needs to be renewed by its owner"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": userTokenValue, "expiresAt": exp.Format(time.RFC3339), "identity": "alice (invoker)"})
	}))
	e.mcp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.auths = append(e.auths, r.Header.Get("Authorization"))
		e.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+userTokenValue {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int64
			Method string
			Params struct {
				Name string
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{})
		case "tools/list":
			reply(map[string]any{"tools": []map[string]any{
				{"name": "whoami", "description": "d", "inputSchema": map[string]any{"type": "object"}},
				{"name": "echo", "description": "d", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			text := "hello from the server"
			if req.Params.Name == "echo" { // a server that reflects its credential
				text = "debug: Authorization was Bearer " + userTokenValue
			}
			reply(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(e.operator.Close)
	t.Cleanup(e.mcp.Close)
	t.Cleanup(func() { redact.Forget(userTokenValue) })
	return e
}

func (e *userMCPEnv) router(t *testing.T, store *testStore) *Router {
	t.Helper()
	satoken := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(satoken, []byte(podSAToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		RunName: "run-1", RunNamespace: "team-a", DeploymentName: "dep",
		OperatorAPIURL: e.operator.URL, SATokenFile: satoken,
		Providers: []ProviderConfig{{Name: "openai", LiteLLMModel: "openai/gpt-4o"}}, Strategy: strategyRuleBased,
		MCPServers:      []MCPServerConfig{{Name: "github", Transport: "http", URL: e.mcp.URL, UserConnection: &UserConnectionConfig{Server: "github"}}},
		ToolDefinitions: nil,
	}
	r, err := New(cfg, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUserMCPToken_IsInTheRequestHeaderAndNowhereElse(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(redact.Handler(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := newUserMCPEnv(t)
	store := newTestStore(t)
	r := env.router(t, store)
	r.InitMCPServers()
	t.Cleanup(func() { r.mcpClient.Close() })

	// The token is on every request the MCP server saw: initialize, initialized, tools/list.
	if len(r.mcpClient.Tools()) != 2 {
		t.Fatalf("tools = %+v (connect failed: logs %s)", r.mcpClient.Tools(), logs.String())
	}
	var td ToolDefinition
	for _, d := range r.cfg.ToolDefinitions {
		if d.Name == "whoami" {
			td = d
		}
	}
	if td.Name == "" {
		t.Fatalf("whoami not merged: %+v", r.cfg.ToolDefinitions)
	}
	res, _ := r.callMCPTool(context.Background(), td, "{}")
	if res != "hello from the server" {
		t.Errorf("result = %q", res)
	}
	env.mu.Lock()
	for i, a := range env.auths {
		if a != "Bearer "+userTokenValue {
			t.Errorf("MCP request %d carried Authorization %q", i, a)
		}
	}
	if len(env.auths) < 4 {
		t.Errorf("only %d requests seen", len(env.auths))
	}
	// One operator call serves them all (the token is cached until it nears expiry), it
	// authenticates with the pod's SA token and names only the server.
	if len(env.opCalls) != 1 || env.opCalls[0] != "/agentrun/team-a/run-1/mcp-token|Bearer "+podSAToken || env.opBodies[0] != `{"server":"github"}` {
		t.Errorf("operator calls = %v bodies = %v", env.opCalls, env.opBodies)
	}
	env.mu.Unlock()

	// A server that reflects its credential: the result the model sees is masked.
	var echo ToolDefinition
	for _, d := range r.cfg.ToolDefinitions {
		if d.Name == "echo" {
			echo = d
		}
	}
	res, _ = r.callMCPTool(context.Background(), echo, "{}")
	if strings.Contains(res, "SENTINEL") || !strings.Contains(res, redact.Mask) {
		t.Errorf("tool result leaks or is not masked: %q", res)
	}

	// Trace events and streamed tokens: a credential in an event is masked at the single
	// writers, so nothing stored or streamed carries it.
	r.emitTraceEventBoth(`{"type":"toolResult","result":"Bearer ` + userTokenValue + `"}`)
	sub, unsub := r.tokens.Subscribe()
	defer unsub()
	r.tokens.Send("token " + userTokenValue)
	for _, ev := range store.traceEvents {
		if strings.Contains(ev, "SENTINEL") {
			t.Errorf("stored trace event leaks: %s", ev)
		}
	}
	for len(sub) > 0 {
		if m := <-sub; strings.Contains(m, "SENTINEL") {
			t.Errorf("streamed event leaks: %q", m)
		}
	}

	// Logs: even a log line that formats the token is masked, and nothing logged above carries it.
	slog.Info("debug dump", "authorization", "Bearer "+userTokenValue)
	if strings.Contains(logs.String(), "SENTINEL") {
		t.Errorf("logs leak the token: %s", logs.String())
	}
	if !strings.Contains(logs.String(), redact.Mask) {
		t.Errorf("the log line was not masked: %s", logs.String())
	}
}

func TestUserMCPToken_ExpiryRefetchesAndRefusalsAreRelayableWithoutSecrets(t *testing.T) {
	env := newUserMCPEnv(t)
	env.expires = time.Now().Add(10 * time.Second) // inside the skew: every request refetches
	r := env.router(t, newTestStore(t))
	tok, err := r.MCPToken(context.Background(), "github")
	if err != nil || tok.Value != userTokenValue {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	for _, c := range []struct {
		status int
		code   string
	}{
		{http.StatusConflict, "needs_reauth"}, {http.StatusConflict, "connection_required"}, {http.StatusBadGateway, "connection_unavailable"},
	} {
		env.mu.Lock()
		env.opStatus, env.opError = c.status, c.code
		env.mu.Unlock()
		_, err := r.MCPToken(context.Background(), "github")
		var ce *mcp.ConnectionError
		if !asConnectionError(err, &ce) || ce.Code != c.code || strings.Contains(ce.Message, "SENTINEL") {
			t.Errorf("%s: err = %v", c.code, err)
		}
	}
	// With the operator unreachable the router says so without a stack of URLs or tokens.
	env.operator.Close()
	_, err = r.MCPToken(context.Background(), "github")
	var ce *mcp.ConnectionError
	if !asConnectionError(err, &ce) || ce.Code != "connection_unavailable" || strings.Contains(ce.Message, env.operator.URL) {
		t.Errorf("unreachable operator = %v", err)
	}
}

func asConnectionError(err error, target **mcp.ConnectionError) bool {
	ce, ok := err.(*mcp.ConnectionError)
	if ok {
		*target = ce
	}
	return ok
}

func TestUserMCPToken_NoConnectionMeansNoToolsAndNoTokenInTheLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(redact.Handler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })
	env := newUserMCPEnv(t)
	env.opStatus, env.opError = http.StatusConflict, "connection_required"
	r := env.router(t, newTestStore(t))
	// connect retries a few times with a delay; one attempt is enough to see the outcome.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := mcp.New(ctx, []mcp.ServerConfig{{Name: "github", Transport: mcp.TransportHTTP, URL: env.mcp.URL, User: &mcp.UserConnection{Server: "github", Tokens: r}}})
	if len(c.Tools()) != 0 {
		t.Errorf("tools without a connection: %+v", c.Tools())
	}
	env.mu.Lock()
	for _, a := range env.auths {
		if a != "" {
			t.Errorf("a request reached the MCP server with Authorization %q although no token was issued", a)
		}
	}
	env.mu.Unlock()
	if strings.Contains(logs.String(), "SENTINEL") {
		t.Errorf("logs: %s", logs.String())
	}
}
