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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

const replyDone = "done"

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

func TestInputPrefersExactMultiLineCopy(t *testing.T) {
	t.Setenv("AGENTORC_INPUT", "line one line two")
	t.Setenv("AGENTORC_INPUT_B64", base64.StdEncoding.EncodeToString([]byte("line one\nline two")))
	if got := Input(); got != "line one\nline two" {
		t.Errorf("Input() = %q", got)
	}
	t.Setenv("AGENTORC_INPUT_B64", "")
	if got := Input(); got != "line one line two" {
		t.Errorf("Input() without _B64 = %q", got)
	}
}

func conflict(w http.ResponseWriter) { http.Error(w, "request in progress", http.StatusConflict) }

// The router answers 409 while it finishes the previous turn; the SDK retries,
// sending the same request again.
func TestRunRetriesWhileTheRouterIsBusy(t *testing.T) {
	f := &fakeRouter{responses: []func(http.ResponseWriter){conflict, conflict, sse(textChunk(replyDone))}}
	srv := f.serve(t)
	a := New(WithBaseURL(srv.URL + "/v1"))
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != replyDone || len(f.requests) != 3 {
		t.Fatalf("output %q after %d requests, want done after 3", res.Output, len(f.requests))
	}
	for i, req := range f.requests {
		if m := messages(t, req); len(m) != 1 || m[0]["content"] != "go" {
			t.Errorf("request %d body = %v, want the same input every time", i, req)
		}
	}
}

func TestRunGivesUpOnAPermanent409(t *testing.T) {
	old := conflictRetries
	conflictRetries = 2
	t.Cleanup(func() { conflictRetries = old })
	f := &fakeRouter{responses: []func(http.ResponseWriter){conflict, conflict, conflict, sse(textChunk("never"))}}
	srv := f.serve(t)
	_, err := New(WithBaseURL(srv.URL+"/v1")).Run(context.Background(), "go")
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err = %v, want the router's 409", err)
	}
	if len(f.requests) != 3 {
		t.Errorf("%d requests, want the first plus 2 retries", len(f.requests))
	}
}

func TestRetryStopsWhenTheContextEnds(t *testing.T) {
	old := conflictRetryDelay
	conflictRetryDelay = time.Hour
	t.Cleanup(func() { conflictRetryDelay = old })
	f := &fakeRouter{responses: []func(http.ResponseWriter){conflict, sse(textChunk("never"))}}
	srv := f.serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := New(WithBaseURL(srv.URL+"/v1")).Run(ctx, "go"); err == nil {
		t.Fatal("Run returned no error although its context ended while waiting to retry")
	}
}

func TestRunRetriesWhileTheRouterIsUnreachable(t *testing.T) {
	old := connectRetryDelay
	connectRetryDelay = time.Millisecond
	t.Cleanup(func() { connectRetryDelay = old })
	f := &fakeRouter{responses: []func(http.ResponseWriter){sse(textChunk(replyDone))}}
	inner := f.serve(t)
	var drops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drops < 2 {
			drops++
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	res, err := New(WithBaseURL(srv.URL+"/v1")).Run(context.Background(), "go")
	if err != nil || res.Output != replyDone || drops != 2 {
		t.Fatalf("output %q, err %v after %d dropped connections, want done after 2", res.Output, err, drops)
	}
}

func TestRunProbesPastAHungRouterPath(t *testing.T) {
	oldDelay, oldProbe := connectRetryDelay, probeTimeout
	connectRetryDelay, probeTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { connectRetryDelay, probeTimeout = oldDelay, oldProbe })
	f := &fakeRouter{responses: []func(http.ResponseWriter){sse(textChunk(replyDone))}}
	inner := f.serve(t)
	var hung int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && hung < 2 {
			hung++
			<-r.Context().Done() // the supervisor holding a request it never answers
			return
		}
		if r.Method == http.MethodGet {
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	res, err := New(WithBaseURL(srv.URL+"/v1")).Run(context.Background(), "go")
	if err != nil || res.Output != replyDone || hung != 2 {
		t.Fatalf("output %q, err %v after %d hung probes, want done after 2", res.Output, err, hung)
	}
}
