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
	"strings"
	"testing"
	"time"
)

// TestTerminalStopForRun verifies the CRD-state → stopReason mapping used by
// the pre-stream check in streamRunEvents.
func TestTerminalStopForRun(t *testing.T) {
	bridge, _ := traceTestBridge(t)

	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"in progress still runs", acpStatusInProgress, ""},
		{"created still runs", "created", ""},
		{"completed ends turn", acpStatusCompleted, acpStopReasonEndTurn},
		{"failed ends turn", acpStatusFailed, acpStopReasonEndTurn},
		{"cancelled is user cancel", acpStatusCancelled, acpStopReasonUserCancel},
		{"awaiting defers to elicitation", acpStatusAwaiting, acpPendingAwaiting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := acpRun{Status: tc.status, RunID: "run-1"}
			if got := bridge.terminalStopForRun(run, "zed-1", nil); got != tc.want {
				t.Fatalf("terminalStopForRun(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestStreamRunEventsFinalizesViaCRDCheck is the regression for editor runs
// staying "running" after the agent finished: the pre-stream CRD poll must end
// the turn as soon as the run is terminal even when the SSE stream never
// delivered a terminal event (the terminal marker only exists when the router
// emits a terminal trace event, and confirmTerminal is otherwise only
// consulted on terminal events).
func TestStreamRunEventsFinalizesViaCRDCheck(t *testing.T) {
	mux := http.NewServeMux()
	// SSE request: one token chunk, then the stream closes with NO terminal
	// event (the pre-fix failure mode).
	mux.HandleFunc("/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: message.part\n"+
				`data: {"type":"message.part","part":{"content_type":"text/plain","content":"hi"}}`+"\n\n")
			return
		}
		// CRD poll: the run is already completed, and its output matches what
		// was streamed (emitTerminalOutput must not duplicate it).
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"completed","output":[`+
			`{"role":"agent","parts":[{"content_type":"text/plain","content":"hi"}]}]}`)
	})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)

	c := newClient(api.URL, api.URL, testToken, time.Second, false)
	stdout := &bytes.Buffer{}
	srv := newACPStdioServer(nil, strings.NewReader(""), stdout, discardWriter{})
	bridge := newACPBridge(srv, c, "test-agent")
	st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}, traceSeen: map[string]bool{}}

	stop := bridge.streamRunEvents(context.Background(), "run-1", "zed-1", st)
	if stop != acpStopReasonEndTurn {
		t.Fatalf("streamRunEvents stopReason = %q, want end_turn via the CRD check", stop)
	}

	// Exactly one final message chunk must have been emitted: the streamed
	// token, with emitTerminalOutput suppressing the duplicate.
	notes := acpSessionUpdates(t, stdout)
	chunks := 0
	for _, n := range notes {
		if u, ok := n["update"].(map[string]any); ok && u["sessionUpdate"] == acpUpdateMessageChunk {
			chunks++
		}
	}
	if chunks != 1 {
		t.Fatalf("got %d message chunks, want exactly 1 (streamed token, no CRD duplicate): %v", chunks, notes)
	}
}

// acpSessionUpdates decodes every session/update notification from the buffer.
func acpSessionUpdates(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("notification line is not valid JSON: %q", line)
		}
		if msg.Method != acpMethodSessionUpdate {
			continue
		}
		raw, err := json.Marshal(msg.Params)
		if err != nil {
			t.Fatalf("re-marshal params: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decoding params: %v", err)
		}
		out = append(out, p)
	}
	return out
}
