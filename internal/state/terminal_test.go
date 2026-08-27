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

package state

import "testing"

func TestIsTerminalTraceEventJSON(t *testing.T) {
	cases := []struct {
		name string
		ev   string
		want bool
	}{
		// Schema-aligned completion events. `fail` is terminal because it is an
		// explicit error path. `finalOutput` is terminal because it carries the
		// authoritative run output and signals the UI API to close the stream.
		// `done` is intentionally NOT terminal — it carries no output and the UI
		// relies on finalOutput (or the poll fallback) for completion.
		{"done with finish_reason", `{"type":"done","finish_reason":"stop"}`, false},
		{"done with output (explicit _done)", `{"type":"done","output":"x"}`, false},
		{"fail", `{"type":"fail","reason":"deadline"}`, true},
		{"finalOutput", `{"type":"finalOutput","output":"x"}`, true},
		// clarify pauses for human input and later resumes on the same key, so it
		// is NOT a hard close for TailTokens (the CRD WaitingForInput phase closes
		// the stream instead).
		{"clarify", `{"type":"clarify","question":"?"}`, false},
		// Non-terminal events must not close the stream.
		{"toolCall", `{"type":"toolCall","name":"x"}`, false},
		{"toolResult", `{"type":"toolResult","name":"x","result":""}`, false},
		{"token", `{"type":"token"}`, false},
		{"missing type", `{"content":"x"}`, false},
		// Malformed payloads must not be mistaken for terminal.
		{"empty", "", false},
		{"not json", "not json", false},
		{"type not string", `{"type":123}`, false},
		{"truncated", `{"type":"done"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTerminalTraceEventJSON(c.ev); got != c.want {
				t.Errorf("IsTerminalTraceEventJSON(%q) = %v, want %v", c.ev, got, c.want)
			}
		})
	}
}
