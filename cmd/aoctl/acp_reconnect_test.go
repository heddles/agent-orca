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
	"sync/atomic"
	"testing"
	"time"
)

// fastStreamRetries shrinks the reconnect policy for the duration of a test.
func fastStreamRetries(t *testing.T, maxFailures int) {
	t.Helper()
	base, maxDelay, maxF := streamRetryBaseDelay, streamRetryMaxDelay, maxStreamConnectFailures
	streamRetryBaseDelay, streamRetryMaxDelay, maxStreamConnectFailures = time.Millisecond, 5*time.Millisecond, maxFailures
	t.Cleanup(func() {
		streamRetryBaseDelay, streamRetryMaxDelay, maxStreamConnectFailures = base, maxDelay, maxF
	})
}

// writeSSEPart writes one message.part SSE frame.
func writeSSEPart(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: message.part\n"+
		`data: {"type":"message.part","part":{"content_type":"text/plain","content":%q}}`+"\n\n", text)
}

// gatewayTimeoutRunHandler reproduces the acp-senior-programmer-qmgmr incident:
// the first stream window drops mid-run (operator pod replaced), the reconnect
// is answered with a 504 by the ingress, and a later reconnect succeeds and
// replays the token stream. The run only completes after that last stream.
func gatewayTimeoutRunHandler(sseRequests *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			switch sseRequests.Add(1) {
			case 1:
				writeSSEPart(w, "hi")
			case 2:
				http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
			default:
				// Replay from the start plus the new tokens, then drop again
				// without a terminal event.
				writeSSEPart(w, "hi")
				writeSSEPart(w, " there")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if sseRequests.Load() < 3 {
			_, _ = fmt.Fprintf(w, `{"run_id":"run-1","status":%q}`, acpStatusInProgress)
			return
		}
		_, _ = fmt.Fprintf(w, `{"run_id":"run-1","status":%q,"output":[`+
			`{"role":"agent","parts":[{"content_type":"text/plain","content":"hi there"}]}]}`, acpStatusCompleted)
	}
}

// messageChunkText concatenates every agent_message_chunk text in the buffer.
func messageChunkText(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	var sb strings.Builder
	for _, n := range acpSessionUpdates(t, buf) {
		u, ok := n["update"].(map[string]any)
		if !ok || u["sessionUpdate"] != acpUpdateMessageChunk {
			continue
		}
		if c, ok := u["content"].(map[string]any); ok {
			text, _ := c["text"].(string)
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// TestStreamRunEventsRetriesAfterGatewayTimeout is the regression for the
// mid-turn "unknown variant" parse error: a failed stream re-open (HTTP
// 504) used to return an empty stopReason and end the turn while the run was
// still going. It must back off, reconnect, and finish the turn normally
// without duplicating replayed output.
func TestStreamRunEventsRetriesAfterGatewayTimeout(t *testing.T) {
	fastStreamRetries(t, 5)
	var sseRequests atomic.Int32
	api := httptest.NewServer(gatewayTimeoutRunHandler(&sseRequests))
	t.Cleanup(api.Close)

	c := newClient(api.URL, api.URL, testToken, time.Second, false)
	stdout := &bytes.Buffer{}
	srv := newACPStdioServer(nil, strings.NewReader(""), stdout, discardWriter{})
	bridge := newACPBridge(srv, c, "test-agent")
	st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}, traceSeen: map[string]bool{}}

	stop := bridge.streamRunEvents(context.Background(), "run-1", "zed-1", st)
	if stop != acpStopReasonEndTurn {
		t.Fatalf("streamRunEvents stopReason = %q, want end_turn after reconnecting", stop)
	}
	if got := sseRequests.Load(); got < 3 {
		t.Fatalf("stream opened %d times, want >= 3 (initial, 504, successful reconnect)", got)
	}
	if got := messageChunkText(t, stdout); got != "hi there" {
		t.Fatalf("streamed text = %q, want %q (replay must not duplicate)", got, "hi there")
	}
}

// TestStreamRunEventsGivesUpAfterSustainedOutage verifies the retry bound: when
// every re-open fails the turn ends with a valid stopReason and a visible
// notice instead of hanging the editor forever.
func TestStreamRunEventsGivesUpAfterSustainedOutage(t *testing.T) {
	fastStreamRetries(t, 3)
	var sseRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			sseRequests.Add(1)
			http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"run_id":"run-1","status":%q}`, acpStatusInProgress)
	}))
	t.Cleanup(api.Close)

	c := newClient(api.URL, api.URL, testToken, time.Second, false)
	stdout := &bytes.Buffer{}
	srv := newACPStdioServer(nil, strings.NewReader(""), stdout, discardWriter{})
	bridge := newACPBridge(srv, c, "test-agent")
	st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}, traceSeen: map[string]bool{}}

	stop := bridge.streamRunEvents(context.Background(), "run-1", "zed-1", st)
	if stop != acpStopReasonEndTurn {
		t.Fatalf("streamRunEvents stopReason = %q, want end_turn after giving up", stop)
	}
	if got := sseRequests.Load(); got != 3 {
		t.Fatalf("stream opened %d times, want exactly maxStreamConnectFailures (3)", got)
	}
	if got := messageChunkText(t, stdout); !strings.Contains(got, "Lost the connection to run run-1") {
		t.Fatalf("expected a lost-connection notice, got %q", got)
	}
}

// TestStreamRunEventsCancelDuringBackoff verifies session/cancel (context
// cancellation) is honoured while waiting to reconnect.
func TestStreamRunEventsCancelDuringBackoff(t *testing.T) {
	fastStreamRetries(t, 100)
	streamRetryBaseDelay, streamRetryMaxDelay = time.Hour, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the user cancels while the stream is failing
		http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
	}))
	t.Cleanup(api.Close)

	c := newClient(api.URL, api.URL, testToken, time.Second, false)
	srv := newACPStdioServer(nil, strings.NewReader(""), &bytes.Buffer{}, discardWriter{})
	bridge := newACPBridge(srv, c, "test-agent")
	st := &openAIStreamState{toolCalls: map[int]openaiToolCall{}, traceSeen: map[string]bool{}}

	done := make(chan string, 1)
	go func() { done <- bridge.streamRunEvents(ctx, "run-1", "zed-1", st) }()
	select {
	case stop := <-done:
		if stop != acpStopReasonUserCancel {
			t.Fatalf("streamRunEvents stopReason = %q, want cancelled", stop)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streamRunEvents did not return after cancellation during backoff")
	}
}

// TestSessionPrompt_StreamGatewayTimeoutMidTurn drives the full session/prompt
// JSON-RPC flow through the incident sequence and checks the wire-level
// response carries a spec-valid stopReason (Zed rejected `"stopReason": ""`).
func TestSessionPrompt_StreamGatewayTimeoutMidTurn(t *testing.T) {
	fastStreamRetries(t, 5)
	var sseRequests atomic.Int32
	runHandler := gatewayTimeoutRunHandler(&sseRequests)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agents/test-agent/run" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"agent_name":"test-agent","run_id":"run-1","status":"created"}`)
		case r.URL.Path == "/runs/run-1" && r.Method == http.MethodGet:
			runHandler(w, r)
		default:
			// fs relay polls and other side requests are irrelevant here.
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	var stdout, stderr bytes.Buffer
	server := &acpStdioServer{stdout: &stdout, stderr: &stderr}
	client := newClient(api.URL, api.URL, testToken, 10*time.Second, true)
	bridge := newACPBridge(server, client, "test-agent")
	server.handler = bridge

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := json.RawMessage(`1`)
	bridge.Dispatch(ctx, jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "session/prompt",
		Params:  json.RawMessage(`{"sessionId":"sess-504","prompt":[{"type":"text","text":"hi"}]}`),
	})

	var result map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.ID == nil || msg.Method != "" {
			continue
		}
		unmarshalResult(t, msg, &result)
	}
	if result == nil {
		t.Fatalf("no session/prompt response; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if got := result["stopReason"]; got != acpStopReasonEndTurn {
		t.Fatalf("stopReason = %#v, want %q; stderr:\n%s", got, acpStopReasonEndTurn, stderr.String())
	}
	if !strings.Contains(stderr.String(), "HTTP 504") {
		t.Fatalf("expected the 504 to be logged, stderr:\n%s", stderr.String())
	}
}

func TestNormalizeStopReason(t *testing.T) {
	cases := map[string]string{
		"":                           acpStopReasonEndTurn,
		acpPendingAwaiting:           acpStopReasonEndTurn,
		"bogus":                      acpStopReasonEndTurn,
		acpStopReasonEndTurn:         acpStopReasonEndTurn,
		acpStopReasonUserCancel:      acpStopReasonUserCancel,
		acpStopReasonMaxTokens:       acpStopReasonMaxTokens,
		acpStopReasonMaxTurnRequests: acpStopReasonMaxTurnRequests,
		acpStopReasonRefusal:         acpStopReasonRefusal,
	}
	for in, want := range cases {
		if got := normalizeStopReason(in); got != want {
			t.Errorf("normalizeStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamRetryBackoff(t *testing.T) {
	want := []time.Duration{
		250 * time.Millisecond, 500 * time.Millisecond, time.Second,
		2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second,
	}
	for i, w := range want {
		if got := streamRetryBackoff(i + 1); got != w {
			t.Errorf("streamRetryBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}
