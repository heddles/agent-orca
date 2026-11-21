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
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/mcp"
	"github.com/heddles/agent-orca/internal/redact"
)

const parentRunName = "parent"

var githubTool = ToolDefinition{Name: "whoami", BackendType: backendTypeMCP, BackendRef: "github"}

// connectedRouter is a router whose github server connected with the user's token;
// then the user's connection goes away (the operator answers connection_required).
func connectedRouter(t *testing.T, env *userMCPEnv) *Router {
	t.Helper()
	env.expires = time.Now().Add(10 * time.Second) // inside the skew: every request refetches
	r := env.router(t, newTestStore(t))
	r.mcpClient = mcp.New(context.Background(), []mcp.ServerConfig{{Name: "github", Transport: mcp.TransportHTTP, URL: env.mcp.URL,
		User: &mcp.UserConnection{Server: "github", Tokens: r}}})
	if len(r.mcpClient.Tools()) == 0 {
		t.Fatal("setup: no tools discovered")
	}
	env.mu.Lock()
	env.opStatus, env.opError = http.StatusConflict, "connection_required"
	env.mu.Unlock()
	return r
}

func TestInvokerConnectionRequired_ToolErrorAndPauseForTheUser(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(redact.Handler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := newUserMCPEnv(t)
	r := connectedRouter(t, env)
	out, _ := r.callMCPTool(context.Background(), githubTool, `{}`)
	if !strings.Contains(out, "connection_required") || !strings.Contains(out, "github") || strings.Contains(out, "SENTINEL") || strings.Contains(out, "Bearer") {
		t.Errorf("tool result = %s", out)
	}
	if !r.waitingForInput {
		t.Error("the run did not move to WaitingForInput")
	}
	if !r.connectionPause {
		t.Error("the run is not marked to be checkpointed after the result")
	}
	env.mu.Lock()
	var clarify string
	for i, c := range env.opCalls {
		if strings.Contains(c, "/clarify|") {
			clarify = env.opBodies[i]
		}
	}
	env.mu.Unlock()
	if !strings.Contains(clarify, "This agent needs your github account") || strings.Contains(clarify, "SENTINEL") {
		t.Errorf("clarification = %q", clarify)
	}
	for _, ev := range r.store.(*testStore).traceEvents {
		if strings.Contains(ev, "SENTINEL") {
			t.Errorf("trace event leaks the token: %s", ev)
		}
	}
	if strings.Contains(logs.String(), "SENTINEL") {
		t.Errorf("logs leak the token: %s", logs.String())
	}
}

func TestInvokerConnectionRequired_AChildRunReturnsTheErrorWithoutAskingAHuman(t *testing.T) {
	env := newUserMCPEnv(t)
	r := connectedRouter(t, env)
	r.cfg.ParentRunName = parentRunName
	out, _ := r.callMCPTool(context.Background(), githubTool, `{}`)
	if !strings.Contains(out, "connection_required") || !strings.Contains(out, "cannot ask the user") {
		t.Errorf("tool result = %s", out)
	}
	if r.waitingForInput {
		t.Error("a child run must not wait for input")
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	for _, c := range env.opCalls {
		if strings.Contains(c, "/clarify|") {
			t.Errorf("a child run asked a human: %v", env.opCalls)
		}
	}
}

func TestInvokerToolsNeedApprovalUnlessThePolicySaysOtherwise(t *testing.T) {
	gate := func(mode string, cfg *ToolApprovalConfig, class string) (string, int) {
		svc := newFakeApprovalService(t, "rejected")
		r, _ := toolRouter(t, svc, cfg, class)
		r.cfg.MCPServers = []MCPServerConfig{{Name: "github", ConnectionMode: mode}}
		r.cfg.ToolDefinitions = []ToolDefinition{{Name: "create_issue", BackendType: backendTypeMCP, BackendRef: "github", SideEffect: class}}
		got := r.gateToolCall(context.Background(), ToolCall{ID: "c", Function: FunctionCall{Name: "create_issue", Arguments: `{}`}})
		return got, svc.postCount()
	}
	if got, posts := gate("invoker", nil, "write"); posts != 1 || got == "" {
		t.Errorf("invoker write: asked %d times, result %q; want an approval", posts, got)
	}
	if got, posts := gate("invoker", nil, ""); posts != 1 || got == "" {
		t.Errorf("invoker unclassified: asked %d times", posts)
	}
	if got, posts := gate("invoker", nil, "read"); posts != 0 || got != "" {
		t.Errorf("invoker read: asked %d times", posts)
	}
	// The policy's explicit word wins either way.
	if got, posts := gate("invoker", &ToolApprovalConfig{Write: "none"}, "write"); posts != 0 || got != "" {
		t.Errorf("explicit write: none: asked %d times", posts)
	}
	if got, posts := gate("invoker", &ToolApprovalConfig{Write: "deny"}, "write"); posts != 0 || got == "" {
		t.Errorf("explicit write: deny: asked %d times, result %q", posts, got)
	}
	// Other modes are unchanged.
	if got, posts := gate("maintainer", nil, "write"); posts != 0 || got != "" {
		t.Errorf("maintainer write: asked %d times", posts)
	}
}

func TestInvokerIdentityIsTracedOncePerServer(t *testing.T) {
	env := newUserMCPEnv(t)
	env.expires = time.Now().Add(10 * time.Second) // every request refetches
	store := newTestStore(t)
	r := env.router(t, store)
	for range 3 {
		if _, err := r.MCPToken(context.Background(), "github"); err != nil {
			t.Fatal(err)
		}
	}
	evs := stringEventsOfType(store, "mcpConnection")
	if len(evs) != 1 || evs[0]["server"] != "github" || evs[0]["identity"] != "alice (invoker)" {
		t.Errorf("events = %v", evs)
	}
}
