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
	"strings"
	"testing"
)

// messageText concatenates the agent_message_chunk text written so far.
func messageText(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	var sb strings.Builder
	for _, n := range traceNotifications(t, buf) {
		update, _ := n["update"].(map[string]any)
		if update["sessionUpdate"] != acpUpdateMessageChunk {
			continue
		}
		content, _ := update["content"].(map[string]any)
		sb.WriteString(content["text"].(string))
	}
	return sb.String()
}

// finalAnswer is the _done output every test run ends with.
const finalAnswer = "the final answer"

func consumeEvents(t *testing.T, bridge *acpBridge, st *openAIStreamState, evs ...Event) {
	t.Helper()
	ch := make(chan Event, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	bridge.consumeAndStreamHTTP(context.Background(), ch, "zed-1", "msg_1", st)
}

func traceEv(data string) Event { return Event{Type: acpEventTypeTrace, Data: data} }

// Regression for #74: the bridge used to drop the done event's output, so an
// agent ending via _done(output=...) showed no final answer in the editor.
func TestACPDoneTraceEventEmitsOutput(t *testing.T) {
	for _, typ := range []string{"done", "finalOutput"} {
		t.Run(typ, func(t *testing.T) {
			bridge, st := traceTestBridge(t)
			buf := bridge.srv.stdout.(*bytes.Buffer)

			consumeEvents(t, bridge, st, traceEv(`{"type":"`+typ+`","output":"the final answer"}`))

			if got := messageText(t, buf); got != finalAnswer {
				t.Fatalf("message text = %q, want the done output", got)
			}
			if got := st.streamed.String(); got != finalAnswer {
				t.Fatalf("streamed = %q; the output must be recorded so run.completed does not repeat it", got)
			}
		})
	}
}

func TestACPDoneTraceEventDoesNotDuplicateStreamedText(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)
	bridge.emitMessageChunk("zed-1", "msg_1", "Hello world", st)

	consumeEvents(t, bridge, st, traceEv(`{"type":"done","output":"Hello world"}`))

	if got := messageText(t, buf); got != "Hello world" {
		t.Fatalf("message text = %q, want it streamed once", got)
	}
}

func TestACPDoneTraceEventEmitsOnlyUnstreamedRemainder(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)
	bridge.emitMessageChunk("zed-1", "msg_1", "Hello ", st)

	consumeEvents(t, bridge, st, traceEv(`{"type":"done","output":"Hello world"}`))

	if got := messageText(t, buf); got != "Hello world" {
		t.Fatalf("message text = %q, want %q", got, "Hello world")
	}
}

// A stream reconnect replays the run's token stream, including the done event.
// The final answer must not be emitted a second time.
func TestACPDoneTraceEventReplaySwallowed(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)
	done := traceEv(`{"type":"done","output":"the final answer"}`)

	consumeEvents(t, bridge, st, done)
	// Second attempt: arm the swallow counters exactly as streamRunEvents does.
	st.replayTokenSkip = st.streamed.Len()
	st.replayThoughtSkip = st.streamedThought.Len()
	consumeEvents(t, bridge, st, done)

	if got := messageText(t, buf); got != finalAnswer {
		t.Fatalf("message text after replay = %q, want a single copy", got)
	}
}

// The done event is followed by run.completed, whose output also carries the
// answer (the server now includes it). emitTerminalOutput must add nothing.
func TestACPRunCompletedAfterDoneDoesNotRepeatOutput(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	consumeEvents(t, bridge, st,
		traceEv(`{"type":"done","output":"the final answer"}`),
		Event{Type: "run.completed", Data: `{"type":"run.completed","run":{"run_id":"r1","status":"completed",` +
			`"output":[{"role":"agents","parts":[{"content_type":"text/plain","content":"the final answer"}]}]}}`},
	)

	if got := messageText(t, buf); got != finalAnswer {
		t.Fatalf("message text = %q, want a single copy", got)
	}
}

// An older apiserver omits the done output from run.completed; the trace event
// alone must still deliver it.
func TestACPDoneOutputWithoutRunCompletedOutput(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	consumeEvents(t, bridge, st,
		traceEv(`{"type":"done","output":"the final answer"}`),
		Event{Type: "run.completed", Data: `{"type":"run.completed","run":{"run_id":"r1","status":"completed"}}`},
	)

	if got := messageText(t, buf); got != finalAnswer {
		t.Fatalf("message text = %q, want the done output", got)
	}
}

func TestACPFailTraceEventStaysSilent(t *testing.T) {
	bridge, st := traceTestBridge(t)
	buf := bridge.srv.stdout.(*bytes.Buffer)

	consumeEvents(t, bridge, st, traceEv(`{"type":"fail","reason":"boom","output":"ignored"}`))

	if got := messageText(t, buf); got != "" {
		t.Fatalf("fail event emitted message text %q, want none", got)
	}
}

func TestFinalOutputSuffix(t *testing.T) {
	cases := []struct {
		name, streamed, output, want string
	}{
		{"empty output", "abc", "", ""},
		{"nothing streamed", "", "answer", "answer"},
		{"identical", "same", "same", ""},
		{"prefix streamed", "ab", "abcd", "cd"},
		{"already ends with output", "thinking... answer", "answer", ""},
		{"unrelated streamed text", "hello", "answer", "\n\nanswer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := finalOutputSuffix(tc.streamed, tc.output); got != tc.want {
				t.Fatalf("finalOutputSuffix(%q, %q) = %q, want %q", tc.streamed, tc.output, got, tc.want)
			}
		})
	}
}
