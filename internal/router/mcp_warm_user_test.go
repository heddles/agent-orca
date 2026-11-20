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

	"github.com/heddles/agent-orca/internal/redact"
)

func warmUserRouter(t *testing.T, env *userMCPEnv, store *testStore) *Router {
	t.Helper()
	r := env.router(t, store)
	r.cfg.WarmMode = true
	r.cfg.RunName = ""
	r.cfg.ToolDefinitions = []ToolDefinition{{Name: "github", BackendType: backendTypeMCP, BackendRef: "github"}}
	return r
}

func hasTool(r *Router, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.cfg.ToolDefinitions {
		if d.Name == name {
			return true
		}
	}
	return false
}

func TestWarmUserMCP_InitDefersAndClaimDiscoversWithTheClaimedRun(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(redact.Handler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := newUserMCPEnv(t)
	store := newTestStore(t)
	r := warmUserRouter(t, env, store)
	r.InitMCPServers()
	t.Cleanup(func() { r.mcpClient.Close() })

	env.mu.Lock()
	if len(env.opCalls) != 0 || len(env.auths) != 0 {
		t.Errorf("warm init made requests: operator %v mcp %v", env.opCalls, env.auths)
	}
	env.mu.Unlock()
	if strings.Contains(logs.String(), "retry") || strings.Contains(logs.String(), "failed") {
		t.Errorf("warm init logged noise: %s", logs.String())
	}
	if hasTool(r, "whoami") {
		t.Fatal("tools before any claim")
	}

	r.ClaimRun(WarmRunInput{RunName: "run-7"})
	r.DiscoverUserMCP(context.Background())
	if !hasTool(r, "whoami") || !hasTool(r, "echo") {
		t.Fatalf("tools after claim: %+v (logs %s)", r.cfg.ToolDefinitions, logs.String())
	}
	env.mu.Lock()
	if len(env.opCalls) != 1 || env.opCalls[0] != "/agentrun/team-a/run-7/mcp-token|Bearer "+podSAToken {
		t.Errorf("operator calls = %v", env.opCalls)
	}
	env.mu.Unlock()

	// A later claim keeps the discovery: no new connection or token request.
	r.ClaimRun(WarmRunInput{RunName: "run-8"})
	r.DiscoverUserMCP(context.Background())
	env.mu.Lock()
	if len(env.opCalls) != 1 {
		t.Errorf("second claim re-discovered: %v", env.opCalls)
	}
	env.mu.Unlock()
	n := 0
	for _, d := range r.cfg.ToolDefinitions {
		if d.Name == "whoami" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("whoami defined %d times", n)
	}
	if strings.Contains(logs.String(), "SENTINEL") {
		t.Errorf("logs leak the token: %s", logs.String())
	}
	for _, ev := range store.traceEvents {
		if strings.Contains(ev, "SENTINEL") {
			t.Errorf("trace leaks: %s", ev)
		}
	}
}

func TestWarmUserMCP_FailureAtClaimDoesNotBreakAndNextClaimRetries(t *testing.T) {
	env := newUserMCPEnv(t)
	env.opStatus, env.opError = http.StatusConflict, "needs_reauth"
	r := warmUserRouter(t, env, newTestStore(t))
	r.InitMCPServers()
	t.Cleanup(func() { r.mcpClient.Close() })

	r.ClaimRun(WarmRunInput{RunName: "run-1"})
	r.DiscoverUserMCP(context.Background())
	if hasTool(r, "whoami") {
		t.Fatal("tools despite a refusal")
	}
	r.mu.Lock()
	if r.cfg.RunName != "run-1" {
		t.Errorf("claim lost: %q", r.cfg.RunName)
	}
	r.mu.Unlock()

	env.mu.Lock()
	env.opStatus = http.StatusOK
	env.mu.Unlock()
	r.ClaimRun(WarmRunInput{RunName: "run-2"})
	r.DiscoverUserMCP(context.Background())
	if !hasTool(r, "whoami") {
		t.Fatal("next claim did not retry")
	}
	env.mu.Lock()
	last := env.opCalls[len(env.opCalls)-1]
	env.mu.Unlock()
	if !strings.HasPrefix(last, "/agentrun/team-a/run-2/") {
		t.Errorf("retry used %q", last)
	}
}

func TestNonWarmUserMCP_InitStillDiscoversAndClaimHookIsANoop(t *testing.T) {
	env := newUserMCPEnv(t)
	r := env.router(t, newTestStore(t))
	r.InitMCPServers()
	t.Cleanup(func() { r.mcpClient.Close() })
	if !hasTool(r, "whoami") {
		t.Fatal("non-warm init lost the tools")
	}
	r.DiscoverUserMCP(context.Background())
	env.mu.Lock()
	if len(env.opCalls) != 1 {
		t.Errorf("operator calls = %v", env.opCalls)
	}
	env.mu.Unlock()
}
