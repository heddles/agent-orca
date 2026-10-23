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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRouter answers each request with the next canned response and records
// the request bodies.
type fakeRouter struct {
	mu        sync.Mutex
	responses []func(w http.ResponseWriter)
	requests  []map[string]any
}

func (f *fakeRouter) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		idx := len(f.requests)
		f.requests = append(f.requests, body)
		f.mu.Unlock()
		if idx >= len(f.responses) {
			http.Error(w, "no more responses", http.StatusInternalServerError)
			return
		}
		f.responses[idx](w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeRouter) request(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func sse(chunks ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func gone(reason string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { http.Error(w, reason, http.StatusGone) }
}

func textChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, s)
}

func toolChunk(index int, id, name, args string) string {
	call := fmt.Sprintf(`{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`,
		index, id, name, args)
	return `{"choices":[{"index":0,"delta":{"tool_calls":[` + call + `]}}]}`
}

const finishToolCalls = `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

func messages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestNewDefaults(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	if a := New(); a.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", a.baseURL, defaultBaseURL)
	}

	t.Setenv("OPENAI_BASE_URL", "http://router:8080/v1/")
	t.Setenv("OPENAI_API_KEY", "tok")
	a := New()
	if a.baseURL != "http://router:8080/v1" || a.apiKey != "tok" {
		t.Errorf("env not applied: baseURL=%q apiKey=%q", a.baseURL, a.apiKey)
	}
	if a := New(WithBaseURL("http://x/v1"), WithAPIKey("k")); a.baseURL != "http://x/v1" || a.apiKey != "k" {
		t.Errorf("options do not override env: %q %q", a.baseURL, a.apiKey)
	}
}

func TestRunExecutesAgentToolCalls(t *testing.T) {
	f := &fakeRouter{responses: []func(http.ResponseWriter){
		sse(textChunk("Building."), toolChunk(0, "call_1", "shell", `{"command":"go build ./..."}`), finishToolCalls),
		sse(textChunk("Build passed.")),
	}}
	srv := f.serve(t)

	var gotArgs map[string]any
	a := New(WithBaseURL(srv.URL + "/v1"))
	a.Tool("shell", "Run a command", nil, func(args map[string]any) (string, error) {
		gotArgs = args
		return "exit code: 0", nil
	})
	res, err := a.Run(context.Background(), "build it")
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseSucceeded || res.Output != "Build passed." {
		t.Errorf("result = %+v", res)
	}
	if gotArgs["command"] != "go build ./..." {
		t.Errorf("handler args = %v", gotArgs)
	}

	first := f.request(0)
	if first["stream"] != true {
		t.Error("requests must stream: the router only answers SSE")
	}
	tools, _ := first["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools sent = %v, want only the agent's shell tool", first["tools"])
	}
	// The router keeps the conversation: the second request carries only the result.
	second := messages(t, f.request(1))
	if len(second) != 1 || second[0]["role"] != "tool" || second[0]["tool_call_id"] != "call_1" ||
		second[0]["content"] != "exit code: 0" {
		t.Errorf("second request messages = %v, want just the tool result", second)
	}
}

func TestRunAnswersEveryCall(t *testing.T) {
	// Unknown tools, bad arguments and handler errors are still answered: the
	// router rejects a request that leaves a call unanswered.
	f := &fakeRouter{responses: []func(http.ResponseWriter){
		sse(
			toolChunk(0, "a", "nope", `{}`),
			toolChunk(1, "b", "fails", `{}`),
			toolChunk(2, "c", "fails", `{not json`),
			finishToolCalls),
		sse(textChunk("ok")),
	}}
	srv := f.serve(t)
	a := New(WithBaseURL(srv.URL + "/v1"))
	a.Tool("fails", "", nil, func(map[string]any) (string, error) { return "", fmt.Errorf("boom") })
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	results := messages(t, f.request(1))
	if len(results) != 3 {
		t.Fatalf("results = %v, want one per call", results)
	}
	for i, want := range []string{"unknown tool", "boom", "invalid JSON"} {
		if c, _ := results[i]["content"].(string); !strings.Contains(c, want) {
			t.Errorf("result %d = %q, want it to mention %q", i, c, want)
		}
	}
}

func TestRunClosedByRouter(t *testing.T) {
	for reason, phase := range map[string]string{
		"run completed by agent":      PhaseSucceeded,
		"run failed by agent":         PhaseFailed,
		"run handed off":              PhaseHandedOff,
		"run waiting for human input": PhaseWaitingForInput,
		"run halted by safeguard":     PhaseFailed,
	} {
		t.Run(reason, func(t *testing.T) {
			f := &fakeRouter{responses: []func(http.ResponseWriter){gone(reason)}}
			srv := f.serve(t)
			res, err := New(WithBaseURL(srv.URL+"/v1")).Run(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			if res.Phase != phase {
				t.Errorf("phase = %s, want %s", res.Phase, phase)
			}
		})
	}
}

func TestRunContinuePrompt(t *testing.T) {
	f := &fakeRouter{responses: []func(http.ResponseWriter){
		sse(textChunk("I will now run the tests.")),
		gone("run completed by agent"),
	}}
	srv := f.serve(t)
	a := New(WithBaseURL(srv.URL + "/v1"))
	res, err := a.Run(context.Background(), "go", WithContinuePrompt("Continue. Call _done when finished."))
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseSucceeded {
		t.Errorf("phase = %s", res.Phase)
	}
	msgs := messages(t, f.request(1))
	if len(msgs) != 1 || msgs[0]["role"] != "user" || msgs[0]["content"] != "Continue. Call _done when finished." {
		t.Errorf("continue request = %v", msgs)
	}
}

func TestRunMaxTurns(t *testing.T) {
	loop := sse(toolChunk(0, "x", "noop", `{}`), finishToolCalls)
	f := &fakeRouter{responses: []func(http.ResponseWriter){loop, loop, loop}}
	srv := f.serve(t)
	a := New(WithBaseURL(srv.URL + "/v1"))
	a.Tool("noop", "", nil, func(map[string]any) (string, error) { return "ok", nil })
	res, err := a.Run(context.Background(), "go", WithMaxTurns(3))
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseFailed || res.FailureReason != ErrMaxTurns.Error() {
		t.Errorf("result = %+v", res)
	}
}

func TestRunRouterError(t *testing.T) {
	f := &fakeRouter{responses: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			http.Error(w, "tool \"done\" is provided by the model-router", http.StatusBadRequest)
		},
	}}
	srv := f.serve(t)
	_, err := New(WithBaseURL(srv.URL+"/v1")).Run(context.Background(), "go")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want the router's 400", err)
	}
}

func TestRunStream(t *testing.T) {
	f := &fakeRouter{responses: []func(http.ResponseWriter){sse(textChunk("Hel"), textChunk("lo"))}}
	srv := f.serve(t)
	content, errs := New(WithBaseURL(srv.URL+"/v1")).RunStream(context.Background(), "hi")
	var got strings.Builder
	for s := range content {
		got.WriteString(s)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if got.String() != "Hello" {
		t.Errorf("streamed %q", got.String())
	}
}
