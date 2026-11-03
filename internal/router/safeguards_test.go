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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func toolCall(name, args string) ToolCall {
	return ToolCall{ID: "c", Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
}

func guardRouter(s RouterSafeguards) *Router {
	return &Router{cfg: &Config{Safeguards: s}}
}

func TestRecordToolCall_RepeatedIdenticalCalls(t *testing.T) {
	r := guardRouter(RouterSafeguards{MaxRepeatedToolCalls: 3})
	for i := range 2 {
		if trip := r.recordToolCallLocked(toolCall("vault_fortune", `{"topic":"sandboxes"}`)); trip != nil {
			t.Fatalf("call %d tripped early: %+v", i+1, trip)
		}
	}
	// Key order and whitespace do not make a call distinct.
	trip := r.recordToolCallLocked(toolCall("vault_fortune", `{ "topic" : "sandboxes" }`))
	if trip == nil || trip.ToolName != "vault_fortune" || trip.TripCount != 3 {
		t.Fatalf("third identical call = %+v, want a trip", trip)
	}
}

func TestRecordToolCall_DistinctArgumentsDoNotTrip(t *testing.T) {
	r := guardRouter(RouterSafeguards{MaxRepeatedToolCalls: 3})
	for i := range 100 {
		args, _ := json.Marshal(map[string]string{"path": "file" + strings.Repeat("x", i)})
		if trip := r.recordToolCallLocked(toolCall("fs_read_text_file", string(args))); trip != nil {
			t.Fatalf("reading %d distinct files tripped: %+v", i+1, trip)
		}
	}
}

func TestRecordToolCall_FrequencyCap(t *testing.T) {
	r := guardRouter(RouterSafeguards{ToolFrequencyCap: 2})
	for i := range 2 {
		if trip := r.recordToolCallLocked(toolCall("search", `{"q":"`+string(rune('a'+i))+`"}`)); trip != nil {
			t.Fatalf("call %d tripped at the cap: %+v", i+1, trip)
		}
	}
	if trip := r.recordToolCallLocked(toolCall("search", `{"q":"z"}`)); trip == nil {
		t.Fatal("want a trip once the cap is exceeded")
	}
}

func TestRecordToolCall_DisabledChecks(t *testing.T) {
	r := guardRouter(RouterSafeguards{})
	for range 500 {
		if trip := r.recordToolCallLocked(toolCall("t", `{}`)); trip != nil {
			t.Fatalf("disabled safeguards tripped: %+v", trip)
		}
	}
}

func TestRecordTextTurn(t *testing.T) {
	s := RouterSafeguards{MaxConsecutiveNoopTurns: 3, MinSubstantiveTokens: 20}

	r := guardRouter(s)
	shortTurns := func(n int) *safeguardTrip {
		var trip *safeguardTrip
		for range n {
			if trip = r.recordTextTurnLocked(5); trip != nil {
				return trip
			}
		}
		return nil
	}
	if shortTurns(2) != nil {
		t.Fatal("tripped before the streak limit")
	}
	if r.recordTextTurnLocked(50) != nil {
		t.Fatal("a substantive turn must not trip")
	}
	// The substantive turn reset the streak.
	if shortTurns(2) != nil {
		t.Fatal("streak was not reset by the substantive turn")
	}
	_ = r.recordToolCallLocked(toolCall("t", `{}`)) // a tool call resets it too
	if shortTurns(2) != nil {
		t.Fatal("streak was not reset by the tool call")
	}
	if trip := r.recordTextTurnLocked(5); trip == nil || trip.TripCount != 3 {
		t.Fatalf("third consecutive empty turn = %+v, want a trip", trip)
	}

	chat := guardRouter(s)
	chat.cfg.ChatMode = true
	for range 10 {
		if trip := chat.recordTextTurnLocked(1); trip != nil {
			t.Fatalf("chat replies are answered by a person and must not trip: %+v", trip)
		}
	}
}

// A model that keeps calling the same tool is stopped inside the router's
// own tool loop: the agent gets a halt message, the operator is told, and
// later requests are refused.
func TestStreamToolLoopHaltedBySafeguard(t *testing.T) {
	turns := make([][]byte, 10)
	for i := range turns {
		turns[i] = toolCallTurnSSE()
	}
	srv, calls := fakeProvider(t, turns...)
	keyFile := writeKeyFile(t)
	store := newTestStore(t)

	reported := make(chan safeguardTrip, 1)
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/agentrun/default/test-run/loop-detected") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var trip safeguardTrip
		body, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(body, &trip)
		reported <- trip
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(operator.Close)

	r := newStreamingTestRouter(t, srv.URL, keyFile, store)
	r.cfg.OperatorAPIURL = operator.URL
	r.cfg.Safeguards.MaxRepeatedToolCalls = 3

	body := []byte(`{"model":"default","stream":true,"messages":[{"role":"user","content":"store this"}]}`)
	out, _, _, _ := consumeClientBody(t, r, body)

	if n := calls.Load(); n != 3 {
		t.Fatalf("provider called %d times, want 3: the third identical call trips", n)
	}
	if !strings.Contains(out, "Run halted by safeguard") || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("agent response must explain the halt and end the stream, got:\n%s", out)
	}
	select {
	case trip := <-reported:
		if trip.ToolName != "_write_state" || trip.TripCount != 3 || trip.Reason == "" {
			t.Fatalf("reported trip = %+v", trip)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operator was not notified of the trip")
	}

	rec := httptest.NewRecorder()
	r.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusGone {
		t.Fatalf("request after the trip = %d, want 410", rec.Code)
	}
}
