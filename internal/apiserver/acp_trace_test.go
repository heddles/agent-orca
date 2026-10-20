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

package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
)

// traceTailStore yields a fixed sequence of token-stream entries from
// TailTokens, then closes. Only TailTokens is reachable from streamACPRun's
// streaming path; the embedded nil interface panics on any other use, which
// keeps accidental extra dependencies loud in tests.
type traceTailStore struct {
	state.Store
	entries []string
}

func (s *traceTailStore) TailTokens(context.Context, string) (<-chan string, error) {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for _, e := range s.entries {
			ch <- e
		}
	}()
	return ch, nil
}

// tracePrefix mirrors the internal \x00 marker state.Store saves before every
// trace event.
const tracePrefix = "\x00"

// traceStreamRun builds an ACPServer with the trace store and a running run,
// streams it, and returns the recorded SSE response.
func traceStreamRun(t *testing.T, entries []string) *httptest.ResponseRecorder {
	t.Helper()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "acp-run-1",
			Namespace: "tenant-acme",
			Labels:    map[string]string{"agentorca.io/session-id": "zed-1"},
		},
	}
	s := newACPServer(t, run)
	s.store = &traceTailStore{entries: entries}

	req := httptest.NewRequest(http.MethodGet, "/runs/acp-run-1", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	err := s.streamACPRun(rec, req, run)
	if err != nil {
		t.Fatalf("streamACPRun: %v", err)
	}
	return rec
}

// sseFrames splits an SSE body into (event, data) frames.
func sseFrames(t *testing.T, body string) [][2]string {
	t.Helper()
	var frames [][2]string
	var ev, data string
	flush := func() {
		if ev != "" || data != "" {
			frames = append(frames, [2]string{ev, data})
		}
		ev, data = "", ""
	}
	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			flush()
		}
	}
	flush()
	return frames
}

// TestStreamACPRun_ForwardsTraceEvents verifies the SSE handler forwards
// model-router trace events as `event: trace` frames (raw event JSON, no
// envelope), interleaves them with token frames, and keeps trace content out
// of the run output.
func TestStreamACPRun_ForwardsTraceEvents(t *testing.T) {
	toolCall := `{"type":"toolCall","name":"issue_read","backendType":"mcp","backendRef":"github-mcp"}`
	toolResult := `{"type":"toolResult","name":"issue_read","result":"{\"number\":34}","backendType":"mcp","backendRef":"github-mcp","durationMs":667}`
	entries := []string{
		"Hello ",
		tracePrefix + toolCall,
		" world",
		tracePrefix + toolResult,
	}
	rec := traceStreamRun(t, entries)

	frames := sseFrames(t, rec.Body.String())
	types := make([]string, 0, len(frames))
	for _, f := range frames {
		types = append(types, f[0])
	}
	want := []string{"run.created", "run.in-progress", "message.created", "message.part", "trace", "message.part", "trace", "run.completed"}
	if len(types) != len(want) {
		t.Fatalf("frame types = %v, want %v (body: %s)", types, want, rec.Body.String())
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("frame types = %v, want %v", types, want)
		}
	}

	// The trace frames carry the raw event JSON (no {"event": …} envelope).
	for i, f := range frames {
		if types[i] != "trace" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(f[1]), &ev); err != nil {
			t.Fatalf("trace frame %d is not raw JSON: %v (data %q)", i, err, f[1])
		}
		if ev["event"] != nil {
			t.Fatalf("trace frame %d is envelope-wrapped: %s", i, f[1])
		}
	}

	// Trace content must not leak into the run output.
	var completedEvt struct {
		Run struct {
			Output []struct {
				Parts []struct {
					Content string `json:"content"`
				} `json:"parts"`
			} `json:"output"`
		} `json:"run"`
	}
	last := frames[len(frames)-1]
	if last[0] != "run.completed" {
		t.Fatalf("expected run.completed last, got %s", last[0])
	}
	if err := json.Unmarshal([]byte(last[1]), &completedEvt); err != nil {
		t.Fatalf("decoding run.completed: %v", err)
	}
	var out string
	for _, m := range completedEvt.Run.Output {
		for _, p := range m.Parts {
			out += p.Content
		}
	}
	if out != "Hello  world" {
		t.Fatalf("run output contains non-token content: %q", out)
	}
}

// TestStreamACPRun_TerminalTraceEndsStream verifies the stream concludes right
// after a terminal trace event (fail / finalOutput) and still emits
// run.completed — matching the model-router's true end-of-turn marker.
func TestStreamACPRun_TerminalTraceEndsStream(t *testing.T) {
	finalOutput := `{"type":"finalOutput","output":"all done"}`
	lateToken := "should-not-appear"
	rec := traceStreamRun(t, []string{
		"partial ",
		tracePrefix + finalOutput,
		lateToken,
	})

	frames := sseFrames(t, rec.Body.String())
	types := make([]string, 0, len(frames))
	for _, f := range frames {
		types = append(types, f[0])
	}
	want := []string{"run.created", "run.in-progress", "message.created", "message.part", "trace", "run.completed"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("frame types = %v, want %v (body: %s)", types, want, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), lateToken) {
		t.Fatal("token after the terminal trace event must not be delivered")
	}
}

// TestIsTerminalTracePayload guards the terminal classification.
func TestIsTerminalTracePayload(t *testing.T) {
	terminal := []string{
		`{"type":"fail","reason":"boom"}`,
		`{"type":"finalOutput","output":"done"}`,
		// done: the model-router emits it on the genuine terminal text turn and
		// from the _done built-in tool — terminal per state.IsTerminalTraceEventJSON.
		`{"type":"done","output":"all done"}`,
	}
	notTerminal := []string{
		`{"type":"clarify","question":"..."}`,
		`{"type":"toolCall","name":"x"}`,
		`not-json`,
		`{"type":""}`,
	}
	for _, p := range terminal {
		if !isTerminalTracePayload(p) {
			t.Fatalf("%q should be terminal", p)
		}
	}
	for _, p := range notTerminal {
		if isTerminalTracePayload(p) {
			t.Fatalf("%q should not be terminal", p)
		}
	}
}
