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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Shared test fixtures: the bearer token used across the ACP tests and a
// normalized agent-orca host-root endpoint URL.
const (
	testToken         = "tok"
	testAgentOrcaHost = "http://agent-orca.local"
)

// --- JSON-RPC stdio transport tests ---

// unmarshalResult re-marshals a jsonrpcMessage.Result (which is `any`) back to
// JSON bytes so it can be decoded into a concrete struct.
func unmarshalResult(t *testing.T, resp jsonrpcMessage, v any) {
	t.Helper()
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal result into %T: %v (raw: %s)", v, err, b)
	}
}

// TestACPStdioServer_Responds verifies that the stdio transport encodes a
// response correctly on stdout and leaves stderr clean.
func TestACPStdioServer_Responds(t *testing.T) {
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"ping"}` + "\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	srv := &acpStdioServer{stdin: stdin, stdout: &stdout, stderr: &stderr}
	done := make(chan struct{})
	srv.handler = &echoHandler{srv: srv, done: done}

	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not complete in time")
	}

	line := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("expected JSON on stdout, got %q", line)
	}
	var msg jsonrpcMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if msg.JSONRPC != "2.0" {
		t.Fatalf("expected jsonrpc 2.0, got %q", msg.JSONRPC)
	}
	idStr := string(*msg.ID)
	if idStr != "42" {
		t.Fatalf("expected echoed id 42, got %q", idStr)
	}
	if stderr.String() != "" {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

// echoHandler is a minimal acpRequestHandler for transport-level tests.
type echoHandler struct {
	srv  *acpStdioServer
	done chan struct{}
}

func (h *echoHandler) Dispatch(_ context.Context, req jsonrpcRequest) {
	defer close(h.done)
	h.srv.respond(req.ID, map[string]any{"echo": req.Method})
}

// --- Bridge handler tests ---

// TestACPBridge_Initialize verifies that the initialize handler fetches the
// agent manifest and returns the correct protocol version and agent info.
func TestACPBridge_Initialize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/test-agent" || r.Method != http.MethodGet { //nolint:goconst
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"name":"test-agent","description":"A test agent","input_content_types":["text/plain"],"output_content_types":["text/plain"],"clarify_available":true}`) //nolint:lll
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	server := &acpStdioServer{stdout: &stdout, stderr: &stderr}
	client := newClient(srv.URL, srv.URL, testToken, defaultTimeout, true)
	bridge := newACPBridge(server, client, "test-agent")

	id := json.RawMessage(`1`)
	bridge.Dispatch(context.Background(), jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":1,"clientInfo":{"name":"zed"}}`),
	})

	var resp jsonrpcMessage
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("expected jsonrpc 2.0, got %q", resp.JSONRPC)
	}
	var initResp acpInitializeResponse
	unmarshalResult(t, resp, &initResp)
	if initResp.ProtocolVersion != acpProtocolVersion {
		t.Fatalf("expected protocol version %d, got %d", acpProtocolVersion, initResp.ProtocolVersion)
	}
	if initResp.AgentInfo.Name != "agent-orca: test-agent" {
		t.Fatalf("expected agent name 'agent-orca: test-agent', got %q", initResp.AgentInfo.Name)
	}
	if initResp.AgentInfo.Version != acpBridgeVersion {
		t.Fatalf("expected version %q, got %q", acpBridgeVersion, initResp.AgentInfo.Version)
	}
}

// TestACPBridge_SessionNew verifies that session/new generates a session ID and
// returns it in the expected JSON-RPC shape.
func TestACPBridge_SessionNew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"name":"test-agent","description":"test","input_content_types":["text/plain"],"output_content_types":["text/plain"],"clarify_available":true}`) //nolint:lll
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	server := &acpStdioServer{stdout: &stdout, stderr: &stderr}
	client := newClient(srv.URL, srv.URL, testToken, defaultTimeout, true)
	bridge := newACPBridge(server, client, "test-agent")
	_ = bridge // bridge not needed for session/new which doesn't call the server

	id := json.RawMessage(`1`)
	bridge.Dispatch(context.Background(), jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "session/new",
		Params:  json.RawMessage(`{"sessionId":"my-sess"}`),
	})

	var resp jsonrpcMessage
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var newResp acpSessionNewResponse
	unmarshalResult(t, resp, &newResp)
	if newResp.SessionID != "my-sess" {
		t.Fatalf("expected session id 'my-sess', got %q", newResp.SessionID)
	}
}

// TestACPBridge_SessionPrompt_SSE verifies the SSE streaming path: the run
// transitions to "in-progress", the bridge opens an SSE stream, and token
// chunks + terminal events are translated into ACP notifications.
func TestACPBridge_SessionPrompt_SSE(t *testing.T) {
	var pollCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agents/test-agent" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"name":"test-agent","description":"test","clarify_available":true}`)
		case r.URL.Path == "/agents/test-agent/run" && r.Method == http.MethodPost: //nolint:goconst
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"agent_name":"test-agent","run_id":"run-1","status":"created","created_at":"2024-01-01T00:00:00Z"}`) //nolint:lll
		case r.URL.Path == "/runs/run-1" && r.Method == http.MethodGet:
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "event-stream") {
				// SSE stream: one token chunk then run.completed.
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				_, _ = fmt.Fprint(w, "event: message.part\ndata: {\"type\":\"message.part\",\"part\":{\"content_type\":\"text/plain\",\"content\":\"chunk-1\"}}\n\n") //nolint:lll
				if flusher != nil {
					flusher.Flush()
				}
				_, _ = fmt.Fprint(w, "event: run.completed\ndata: {\"type\":\"run.completed\",\"run\":{\"run_id\":\"run-1\",\"status\":\"completed\",\"agent_name\":\"test-agent\",\"created_at\":\"2024-01-01T00:00:00Z\"}}\n\n") //nolint:lll
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			// JSON poll — alternate between created and in-progress.
			_ = atomic.AddInt32(&pollCount, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"run_id":"run-1","status":"completed","agent_name":"test-agent","created_at":"2024-01-01T00:00:00Z"}`) //nolint:lll
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	server := &acpStdioServer{stdout: &stdout, stderr: &stderr}
	// Use a longer timeout so the SSE stream doesn't get killed.
	client := newClient(srv.URL, srv.URL, testToken, 10*time.Second, true)
	bridge := newACPBridge(server, client, "test-agent")
	server.handler = bridge

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	id := json.RawMessage(`1`)
	bridge.Dispatch(ctx, jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "session/prompt",
		Params:  json.RawMessage(`{"sessionId":"sess-2","prompt":[{"type":"text","text":"hi"}]}`),
	})

	out := stdout.String()
	// Should have received a "plan" notification (run created, status pending).
	if !strings.Contains(out, `"sessionUpdate":"plan"`) {
		t.Fatalf("expected plan notification, stdout:\n%s", out)
	}
	// Should have received the SSE token chunk as an agent_message_chunk.
	if !strings.Contains(out, "chunk-1") {
		t.Fatalf("expected 'chunk-1' in SSE stream output, stdout:\n%s", out)
	}
	// Should have received the prompt response with stopReason end_turn.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var resp jsonrpcMessage
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &resp); err != nil {
		t.Fatalf("unmarshal last response: %v\nstdout:\n%s", err, out)
	}
	var promptResp acpPromptResponse
	unmarshalResult(t, resp, &promptResp)
	if promptResp.StopReason != acpStopReasonEndTurn {
		t.Fatalf("expected stopReason 'end_turn', got %q", promptResp.StopReason)
	}
}

// openAIChunkSSE builds a single OpenAI chat.completion.chunk SSE frame.
// Exactly one of content/reasoning should be non-empty.
func openAIChunkSSE(t *testing.T, content, reasoning string) string { //nolint:unparam
	t.Helper()
	delta := map[string]string{}
	if content != "" {
		delta["content"] = content
	}
	if reasoning != "" {
		delta["reasoning"] = reasoning
	}
	chunk := map[string]any{
		"id":     "c",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": delta,
		}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshalling openai chunk: %v", err)
	}
	return "data: " + string(b) + "\n\n"
}

const openAIDoneSSE = "data: [DONE]\n\n"

// openAIRunOutputJSON builds a completed-run JSON payload whose single output
// part is the given raw OpenAI SSE stream (the shape http/env-mode
// openai-reference runs leave in run.Status.Output).
func openAIRunOutputJSON(t *testing.T, runID, stream string) string {
	t.Helper()
	run := acpRun{
		RunID:     runID,
		Status:    acpStatusCompleted,
		AgentName: "test-agent",
		CreatedAt: "2024-01-01T00:00:00Z",
		Output: []ACPMessage{{
			Role: "assistant",
			Parts: []ACPMessagePart{{
				ContentType: "text/plain",
				Content:     stream,
			}},
		}},
	}
	b, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshalling run output: %v", err)
	}
	return string(b)
}

// acpUpdateTexts returns the concatenated text of all session/update
// notifications whose update.sessionUpdate equals wantUpdate, in arrival order.
func acpUpdateTexts(out, wantUpdate string) []string {
	var texts []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Method != acpMethodSessionUpdate || msg.Params == nil {
			continue
		}
		var p acpUpdateParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			continue
		}
		raw, _ := json.Marshal(p.Update)
		var u struct {
			SessionUpdate string           `json:"sessionUpdate"`
			Content       *acpContentBlock `json:"content"`
		}
		if json.Unmarshal(raw, &u) != nil || u.SessionUpdate != wantUpdate {
			continue
		}
		if u.Content != nil {
			texts = append(texts, u.Content.Text)
		}
	}
	return texts
}

// --- Command-level tests (via runCLI) ---

// TestACPUnit_NoToken verifies that `serve` errors when no token is cached.
func TestACPUnit_NoToken(t *testing.T) {
	_, _, err := runCLI(t, nil, "acp", "serve", "--agent", "test-agent")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "no bearer token") {
		t.Fatalf("expected 'no bearer token' error, got: %v", err)
	}
}

// TestACPUnit_ServeMissingAgent verifies that `serve` requires --agent.
func TestACPUnit_ServeMissingAgent(t *testing.T) {
	_, _, err := runCLI(t, nil, "acp", "serve", "--token", testToken)
	if err == nil {
		t.Fatal("expected error for missing --agent")
	}
}

// TestACPUnit_SetupUnsupportedEditor verifies a clear error for unknown editors.
func TestACPUnit_SetupUnsupportedEditor(t *testing.T) {
	_, _, err := runCLI(t, nil, "acp", "setup", "--editor", "vscode")
	if err == nil {
		t.Fatal("expected error for unsupported editor")
	}
	if !strings.Contains(err.Error(), "unsupported editor") {
		t.Fatalf("expected 'unsupported editor' error, got: %v", err)
	}
}

// TestACPSetup_Zed writes a fresh Zed settings.json and verifies the agent_servers
// entry matches what Zed expects for an ACP External Agent.
func TestACPSetup_Zed(t *testing.T) {
	zedDir := t.TempDir()
	stdout, _, err := runCLI(t, map[string]string{
		"ZED_CONFIG_DIR": zedDir,
	}, "acp", "setup", "--editor", "zed", "--agent", "support-bot",
		"--token", testToken, "--acp-endpoint", "http://localhost:8000")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(stdout, "Configured Zed") {
		t.Fatalf("expected success message, got: %q", stdout)
	}

	// Read back and verify the JSON structure.
	data, werr := os.ReadFile(filepath.Join(zedDir, "settings.json"))
	if werr != nil {
		t.Fatalf("reading settings.json: %v", werr)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing settings.json: %v\n%s", err, data)
	}

	agentServers, ok := settings["agent_servers"].(map[string]any)
	if !ok {
		t.Fatalf("agent_servers not found in %s", data)
	}
	entry, ok := agentServers["support-bot"].(map[string]any)
	if !ok {
		t.Fatalf("support-bot not found in agent_servers: %s", data)
	}
	if entry["type"] != "custom" {
		t.Errorf("expected type 'custom', got %v", entry["type"])
	}
	if entry["command"] != "aoctl" {
		t.Errorf("expected command 'aoctl', got %v", entry["command"])
	}
	args, ok := entry["args"].([]any)
	if !ok {
		t.Fatalf("expected args array, got %T", entry["args"])
	}
	expectedArgs := []string{"acp", "serve", "--agent", "support-bot"}
	if len(args) != len(expectedArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(expectedArgs), len(args), args)
	}
	for i, want := range expectedArgs {
		if args[i] != want {
			t.Errorf("arg %d: expected %q, got %v", i, want, args[i])
		}
	}

	// Verify the env map propagates endpoints + token so the Zed-launched
	// `aoctl acp serve` subprocess reaches the same agent-orca instance.
	env, ok := entry["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map in agent entry, got %T", entry["env"])
	}
	if env["AOCTL_ENDPOINT"] != "http://localhost:8084" {
		t.Errorf("expected AOCTL_ENDPOINT http://localhost:8084, got %v", env["AOCTL_ENDPOINT"])
	}
	if env["AOCTL_ACP_ENDPOINT"] != "http://localhost:8000" {
		t.Errorf("expected AOCTL_ACP_ENDPOINT http://localhost:8000, got %v", env["AOCTL_ACP_ENDPOINT"])
	}
	if env["AOCTL_TOKEN"] != testToken {
		t.Errorf("expected AOCTL_TOKEN 'tok', got %v", env["AOCTL_TOKEN"])
	}
}

// TestACPSetup_Zed_MergesExisting verifies that setup merges into an existing
// settings.json without clobbering other agent_servers entries or top-level keys.
func TestACPSetup_Zed_MergesExisting(t *testing.T) {
	zedDir := t.TempDir()

	// Pre-write a settings.json with an existing agent and a theme.
	existing := map[string]any{
		"agent_servers": map[string]any{
			"other-agent": map[string]any{
				"type":    "custom",
				"command": "node",
				"args":    []string{"index.js", "--acp"},
			},
		},
		"theme": "one-dark-pro",
	}
	existingData, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(zedDir, "settings.json"), existingData, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := runCLI(t, map[string]string{
		"ZED_CONFIG_DIR": zedDir,
	}, "acp", "setup", "--editor", "zed", "--agent", "support-bot",
		"--token", testToken, "--acp-endpoint", "http://localhost:8000")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(zedDir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing settings.json: %v", err)
	}

	agentServers := settings["agent_servers"].(map[string]any)
	if _, ok := agentServers["other-agent"]; !ok {
		t.Error("existing 'other-agent' entry was clobbered")
	}
	if _, ok := agentServers["support-bot"]; !ok {
		t.Error("new 'support-bot' entry was not added")
	}
	if settings["theme"] != "one-dark-pro" {
		t.Errorf("existing 'theme' key was clobbered: %v", settings["theme"])
	}

	// Verify the new entry has the propagated env vars.
	newEntry, ok := agentServers["support-bot"].(map[string]any)
	if !ok {
		t.Fatal("support-bot entry not found or not a map")
	}
	if env, ok := newEntry["env"].(map[string]any); !ok {
		t.Fatalf("expected env map in support-bot entry, got %T", newEntry["env"])
	} else if env["AOCTL_TOKEN"] != testToken {
		t.Errorf("expected AOCTL_TOKEN 'tok' in merged entry, got %v", env["AOCTL_TOKEN"])
	}
}

// TestZedSettingsPath verifies the platform-specific path resolution.
func TestZedSettingsPath(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		overrideDir := t.TempDir()
		t.Setenv("ZED_CONFIG_DIR", overrideDir)
		t.Setenv("XDG_CONFIG_HOME", "")
		path, err := zedSettingsPath()
		if err != nil {
			t.Fatalf("zedSettingsPath: %v", err)
		}
		expected := filepath.Join(overrideDir, "settings.json")
		if path != expected {
			t.Fatalf("expected %q, got %q", expected, path)
		}
	})

	t.Run("xdg", func(t *testing.T) {
		if os.Getenv("ZED_CONFIG_DIR") != "" {
			t.Skip("ZED_CONFIG_DIR set")
		}
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		path, err := zedSettingsPath()
		if err != nil {
			t.Fatalf("zedSettingsPath: %v", err)
		}
		expected := filepath.Join(xdg, "zed", "settings.json")
		if path != expected {
			t.Fatalf("expected %q, got %q", expected, path)
		}
	})
}

// TestACPSetup_Zed_PropagatesDerivedEndpoint verifies that when only --endpoint
// is provided (no --acp-endpoint), the ACP endpoint is auto-derived from the
// External Task API endpoint's host, and BOTH endpoints are normalized
// (known API route suffixes stripped) and written into the Zed agent_servers
// env map so the `aoctl acp serve` subprocess launched by Zed reaches the
// correct agent-orca instance.
func TestACPSetup_Zed_PropagatesDerivedEndpoint(t *testing.T) {
	zedDir := t.TempDir()
	stdout, _, err := runCLI(t, map[string]string{
		"ZED_CONFIG_DIR": zedDir,
		"AOCTL_ENDPOINT": "http://agent-orca.local/tasks",
	}, "acp", "setup", "--editor", "zed", "--agent", "senior-programmer",
		"--token", testToken)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(stdout, "Configured Zed") {
		t.Fatalf("expected success message, got: %q", stdout)
	}

	// Read back and verify env vars in the Zed config.
	data, err := os.ReadFile(filepath.Join(zedDir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing settings.json: %v\n%s", err, data)
	}
	agentServers, ok := settings["agent_servers"].(map[string]any)
	if !ok {
		t.Fatalf("agent_servers not found in %s", data)
	}
	entry, ok := agentServers["senior-programmer"].(map[string]any)
	if !ok {
		t.Fatalf("senior-programmer not found in agent_servers: %s", data)
	}
	env, ok := entry["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map, got %T", entry["env"])
	}
	// The /tasks path suffix should be stripped from the External Task API
	// endpoint (the CLI appends /v1/tasks itself).
	if env["AOCTL_ENDPOINT"] != testAgentOrcaHost {
		t.Errorf("expected normalized AOCTL_ENDPOINT http://agent-orca.local, got %v", env["AOCTL_ENDPOINT"])
	}
	// The ACP endpoint should be derived as host root (path stripped).
	if env["AOCTL_ACP_ENDPOINT"] != testAgentOrcaHost {
		t.Errorf("expected derived AOCTL_ACP_ENDPOINT http://agent-orca.local, got %v", env["AOCTL_ACP_ENDPOINT"])
	}
	if env["AOCTL_TOKEN"] != testToken {
		t.Errorf("expected AOCTL_TOKEN 'tok', got %v", env["AOCTL_TOKEN"])
	}
}

// TestACPSetup_Zed_NormalizesACPEndpoint verifies that when the user passes
// --acp-endpoint with a trailing ACP API route (e.g. http://host/agents), it
// is normalized to the host root before being written to the Zed config,
// preventing double-path URLs like http://host/agents/agents/{name}.
func TestACPSetup_Zed_NormalizesACPEndpoint(t *testing.T) {
	zedDir := t.TempDir()
	stdout, _, err := runCLI(t, map[string]string{
		"ZED_CONFIG_DIR": zedDir,
	}, "acp", "setup", "--editor", "zed", "--agent", "senior-programmer",
		"--token", testToken,
		"--endpoint", "http://agent-orca.local/tasks",
		"--acp-endpoint", "http://agent-orca.local/agents")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(stdout, "Configured Zed") {
		t.Fatalf("expected success message, got: %q", stdout)
	}

	data, err := os.ReadFile(filepath.Join(zedDir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing settings.json: %v\n%s", err, data)
	}
	agentServers, ok := settings["agent_servers"].(map[string]any)
	if !ok {
		t.Fatalf("agent_servers not found in %s", data)
	}
	entry, ok := agentServers["senior-programmer"].(map[string]any)
	if !ok {
		t.Fatalf("senior-programmer not found in agent_servers: %s", data)
	}
	env, ok := entry["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map, got %T", entry["env"])
	}
	// The ACP endpoint should be normalized to host root, not http://.../agents.
	if env["AOCTL_ACP_ENDPOINT"] != testAgentOrcaHost {
		t.Errorf("expected normalized AOCTL_ACP_ENDPOINT http://agent-orca.local, got %v", env["AOCTL_ACP_ENDPOINT"])
	}
}
