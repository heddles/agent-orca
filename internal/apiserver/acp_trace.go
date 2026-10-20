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
	"fmt"
	"net/http"
	"strings"

	"github.com/heddles/agent-orca/internal/state"
)

// Trace events (structured activity the model-router interleaves into the
// run's token stream — tool calls, results, guardrails, provider fallbacks)
// are forwarded to ACP stream clients as `event: trace` SSE frames carrying
// the raw event JSON. The bridge (`aoctl acp serve`) translates them into ACP
// session/update notifications so editors can render live agent activity
// mid-turn instead of a spinner. The `\x00` prefix is an internal marker used
// by TailTokens/the UI SSE handler to distinguish trace events from token
// deltas on the same stream; it is stripped here because the SSE `event:`
// field already discriminates the frame type.
const acpTracePrefix = "\x00"

// stripTraceMarker removes the internal \x00 trace-event marker, if present.
func stripTraceMarker(token string) string {
	return strings.TrimPrefix(token, acpTracePrefix)
}

// isTraceToken reports whether a token from the run stream is a trace event
// rather than a plain token delta.
func isTraceToken(token string) bool {
	return strings.HasPrefix(token, acpTracePrefix)
}

// isTerminalTracePayload reports whether a trace-event JSON payload ends the
// run stream. The rule is the state store's terminal classification
// (fail / finalOutput / done — the model-router emits `done` on the genuine
// terminal text turn and from the _done built-in tool). `clarify` is
// deliberately not terminal — a clarified run pauses and later resumes on the
// same stream key, matching state.IsTerminalTraceEventJSON.
func isTerminalTracePayload(evJSON string) bool {
	return state.IsTerminalTraceEventJSON(evJSON)
}

// writeTraceEvent emits one `event: trace` SSE frame carrying the raw trace
// event JSON as the data payload (single line — json.Marshal escapes
// newlines). The bridge's SSE parser feeds `data` straight into its trace
// decoder, so no envelope wrapper here.
func writeTraceEvent(w http.ResponseWriter, flusher http.Flusher, evJSON string) {
	_, _ = fmt.Fprintf(w, "event: trace\ndata: %s\n\n", evJSON)
	flusher.Flush()
}
