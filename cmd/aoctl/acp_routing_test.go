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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// routingAPI is an ACP API with the agents deploys (may write files and run
// commands) and billing (may do neither).
type routingAPI struct {
	*httptest.Server
	mu      sync.Mutex
	started []string // the agent each run request named
	routing string   // the answer to GET /routing; "" answers 404
	refuse  bool     // answer an auto run with agent_undecided
}

const routingAnswer = `{"auto":true,"agents":[` +
	`{"name":"billing","description":"Handles   invoices"},{"name":"deploys"}]}`

func newRoutingAPI(t *testing.T) *routingAPI {
	t.Helper()
	api := &routingAPI{routing: routingAnswer}
	mux := http.NewServeMux()
	mux.HandleFunc("/routing", func(w http.ResponseWriter, _ *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.routing == "" {
			http.NotFound(w, nil)
			return
		}
		_, _ = io.WriteString(w, api.routing)
	})
	mux.HandleFunc("GET /agents/deploys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"deploys","filesystem":{"mode":"readWrite"},"terminal":{"maxTimeoutSeconds":60}}`)
	})
	mux.HandleFunc("GET /agents/billing", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"billing"}`)
	})
	mux.HandleFunc("POST /agents/{name}/run", func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		name := r.PathValue("name")
		api.started = append(api.started, name)
		routing := ""
		if name == autoAgent {
			if api.refuse {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"code":"agent_undecided","message":"no","data":{"agents":["billing","deploys"]}}`)
				return
			}
			name, routing = "deploys", `,"routing":{"method":"decided","confidence":0.91}`
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"agent_name":"`+name+`","run_id":"run-1"`+routing+`}`)
	})
	mux.HandleFunc("/runs/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/fs/") {
			_, _ = io.WriteString(w, `{"requests":[]}`)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			_, _ = io.WriteString(w, `{"run_id":"run-1","status":"completed"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: run.completed\n"+
			"data: {\"type\":\"run.completed\",\"run\":{\"run_id\":\"run-1\",\"status\":\"completed\"}}\n\n")
	})
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	return api
}

func (api *routingAPI) runs() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.started)
}

// routedBridge is an initialized `--agent auto` bridge; out collects what it
// sends the editor.
func routedBridge(t *testing.T, api *routingAPI) (*acpBridge, *syncText) {
	t.Helper()
	out := &syncText{}
	srv := newACPStdioServer(nil, strings.NewReader(""), out, io.Discard)
	b := newACPBridge(srv, newClient(api.URL, api.URL, testToken, 5*time.Second, false), autoAgent)
	srv.handler = b
	return b, out
}

// call dispatches one request and returns the lines it made the bridge send.
func call(t *testing.T, b *acpBridge, out *syncText, method, params string) string {
	t.Helper()
	before := len(out.String())
	id := json.RawMessage(`1`)
	b.Dispatch(context.Background(), jsonrpcRequest{ID: &id, Method: method, Params: json.RawMessage(params)})
	return out.String()[before:]
}

func prompt(t *testing.T, b *acpBridge, out *syncText) string {
	t.Helper()
	return call(t, b, out, "session/prompt", `{"sessionId":"s1","prompt":[{"type":"text","text":"roll it back"}]}`)
}

func TestRoutedBridge(t *testing.T) {
	api := newRoutingAPI(t)
	b, out := routedBridge(t, api)

	got := call(t, b, out, "initialize", `{"protocolVersion":1,"clientCapabilities":{"terminal":true}}`)
	if !strings.Contains(got, `"name":"Agent Orca"`) || !strings.Contains(got, `"writeTextFile":true`) {
		t.Fatalf("initialize: %s", got)
	}

	got = call(t, b, out, "session/new", `{"sessionId":"s1","cwd":"`+t.TempDir()+`"}`)
	var created struct {
		Result acpSessionNewResponse `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &created); err != nil {
		t.Fatalf("session/new: %v: %s", err, got)
	}
	option, modes := created.Result.ConfigOptions[0], created.Result.Modes
	if option.Category != "mode" || option.CurrentValue != autoAgent || modes.CurrentModeID != autoAgent {
		t.Fatalf("choices: %+v %+v", option, modes)
	}
	values := make([]string, 0, len(option.Options))
	for _, c := range option.Options {
		values = append(values, c.Value)
	}
	if !slices.Equal(values, []string{autoAgent, "billing", "deploys"}) || len(modes.AvailableModes) != 3 ||
		option.Options[1].Description != "Handles invoices" {
		t.Fatalf("options: %+v", option.Options)
	}
	// Before an agent has the thread, nothing may touch this computer.
	if b.fsPolicy("s1") != nil || b.terminalPolicy("s1") != nil {
		t.Fatal("a thread with no agent has access")
	}

	got = prompt(t, b, out)
	if !strings.Contains(got, "Routed to **deploys** (91% sure).") {
		t.Fatalf("no routing note: %s", got)
	}
	if b.fsPolicy("s1") == nil || b.terminalPolicy("s1") == nil {
		t.Fatal("the picked agent's policy was not loaded")
	}
	b.markSessionWritesAllowed("s1")
	b.alwaysAllowCommand("s1", "dir\x00make")

	// Auto again: the server keeps the thread, and says nothing new.
	if got = prompt(t, b, out); strings.Contains(got, "Routed to") {
		t.Fatalf("repeated note: %s", got)
	}
	if !b.sessionWritesAllowed("s1") || !b.commandAlwaysAllowed("s1", "dir\x00make") {
		t.Fatal("approvals were lost while the agent stayed")
	}

	got = call(t, b, out, "session/set_config_option", `{"sessionId":"s1","configId":"agent","value":"billing"}`)
	if !strings.Contains(got, `"currentValue":"billing"`) {
		t.Fatalf("set_config_option: %s", got)
	}
	got = prompt(t, b, out)
	if !strings.Contains(got, "Now with **billing**. It does not see what other agents said in this thread.") {
		t.Fatalf("no hand-over note: %s", got)
	}
	if b.fsPolicy("s1") != nil || b.terminalPolicy("s1") != nil {
		t.Fatal("billing has no access of its own, but kept deploys' access")
	}
	if b.sessionWritesAllowed("s1") || b.commandAlwaysAllowed("s1", "dir\x00make") {
		t.Fatal("what the user allowed deploys carried to billing")
	}
	if want := []string{autoAgent, autoAgent, "billing"}; !slices.Equal(api.runs(), want) {
		t.Fatalf("runs named %v, want %v", api.runs(), want)
	}

	// The older mode form chooses the same way.
	call(t, b, out, "session/set_mode", `{"sessionId":"s1","modeId":"deploys"}`)
	if b.chosenAgent("s1") != "deploys" {
		t.Fatal("set_mode did not choose")
	}
	for _, bad := range []string{
		`{"sessionId":"s1","modeId":"payroll"}`, `{"sessionId":"nope","modeId":"billing"}`,
	} {
		if got = call(t, b, out, "session/set_mode", bad); !strings.Contains(got, `"error"`) {
			t.Errorf("set_mode %s was accepted: %s", bad, got)
		}
	}
}

func TestRoutedBridgeAsksWhenNoAgentCanBePicked(t *testing.T) {
	api := newRoutingAPI(t)
	api.refuse = true
	b, out := routedBridge(t, api)
	call(t, b, out, "initialize", `{"protocolVersion":1}`)
	call(t, b, out, "session/new", `{"sessionId":"s1","cwd":"/w"}`)
	got := prompt(t, b, out)
	if !strings.Contains(got, "Choose one in the agent menu and send it again: billing, deploys.") ||
		!strings.Contains(got, `"stopReason":"end_turn"`) || strings.Contains(got, `"error"`) {
		t.Fatalf("got %s", got)
	}
}

func TestAutoBridgeStartup(t *testing.T) {
	t.Run("routing is off", func(t *testing.T) {
		api := newRoutingAPI(t)
		api.routing = `{"auto":false,"reason":"it is turned off for this tenant","agents":[]}`
		b, out := routedBridge(t, api)
		got := call(t, b, out, "initialize", `{"protocolVersion":1}`)
		if !strings.Contains(got, "turned off for this tenant") || !strings.Contains(got, "--agent \\u003cname\\u003e") {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("an agent really called auto is served as itself", func(t *testing.T) {
		for _, routing := range []string{`{"auto":true,"agents":[{"name":"auto"}]}`, ""} {
			api := newRoutingAPI(t)
			api.routing = routing
			api.Config.Handler = withAgentCalledAuto(api.Config.Handler)
			b, out := routedBridge(t, api)
			got := call(t, b, out, "initialize", `{"protocolVersion":1}`)
			if !strings.Contains(got, `"name":"agent-orca: auto"`) || b.isRouted() {
				t.Fatalf("routing %q: %s", routing, got)
			}
		}
	})
}

func withAgentCalledAuto(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents/auto" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"auto"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestACPSetupDefaultsToAutomaticRouting(t *testing.T) {
	api := newRoutingAPI(t)
	dir := t.TempDir()
	t.Setenv("ZED_CONFIG_DIR", dir)
	var out bytes.Buffer
	s := &settings{endpoint: api.URL, acp: api.URL, token: testToken, out: &out, errw: io.Discard}
	if err := s.setupZed(""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Servers map[string]struct {
			Args []string `json:"args"`
		} `json:"agent_servers"`
	}
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	if want := []string{"acp", "serve", "--agent", autoAgent}; !slices.Equal(written.Servers["Agent Orca"].Args, want) {
		t.Fatalf("settings: %s", data)
	}

	// Where routing is off, setup says why and asks for an agent as before.
	api.mu.Lock()
	api.routing = `{"auto":false,"reason":"it is turned off for this tenant","agents":[]}`
	api.mu.Unlock()
	out.Reset()
	err = s.setupZed("")
	if err == nil || !strings.Contains(err.Error(), "--agent is required") ||
		!strings.Contains(out.String(), "turned off for this tenant") {
		t.Fatalf("err %v, output %q", err, out.String())
	}
}
