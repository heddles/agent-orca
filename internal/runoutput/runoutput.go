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

// Package runoutput turns whatever an agent produced into the one thing a run
// reports: an answer, or the reason it has none.
//
// An agent hands its result back in different shapes depending on how it was
// started: an HTTP response (JSON envelope, streamed chat completion, plain
// text, an error status), or text on stdout (plain, or the streamed completion
// it printed). Every run goes through Interpret, so a person never receives a
// provider's raw frames, and a run with no answer fails instead of succeeding
// with garbage. Clip is the single size limit for a stored answer.
package runoutput

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxBytes caps one stored run output. It is a sanity limit, not a budget: the
// state store keeps values zstd-compressed up to 1 MiB.
const MaxBytes = 1 << 20

// truncationMarker tells a consumer that reads only the CRD that the output was
// clipped, instead of silently handing it a prefix.
const truncationMarker = "\n\n[output truncated]"

// Result is what a run reports. Exactly one of Text and Failure is set, except
// that an agent may legitimately answer with empty text (Failure is then "").
type Result struct {
	Text string
	// Failure is why the output is not an answer; non-empty fails the run.
	Failure string
}

// Interpret is the single place an agent's output becomes a run result.
// status is the HTTP status of the agent's response, or 0 for stdout.
func Interpret(status int, body []byte) Result {
	text := strings.TrimSpace(string(body))
	if status != 0 && (status < 200 || status > 299) {
		reason := fmt.Sprintf("agent returned HTTP %d", status)
		if msg := errorPayloadMessage(text); msg != "" {
			return Result{Failure: reason + ": " + msg}
		}
		if text != "" {
			return Result{Failure: reason + ": " + truncateReason(text)}
		}
		return Result{Failure: reason}
	}

	if out, ok := envelopeOutput(body); ok {
		return Result{Text: out}
	}
	if isStream(body) {
		if out, ok := streamText(body); ok {
			return Result{Text: out}
		}
		for line := range strings.SplitSeq(text, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if msg := errorPayloadMessage(line); msg != "" {
				return Result{Failure: "agent call failed: " + msg}
			}
		}
		// Chunks that carry no text (empty deltas, reasoning only, tool calls)
		// are not an answer, and their frames must never reach a person.
		return Result{Failure: "agent call failed: the model returned no answer"}
	}
	if out, ok := completionText(body); ok {
		return Result{Text: out}
	}
	return Result{Text: string(body)}
}

// Stdout interprets text an agent printed to its standard output.
func Stdout(out string) Result { return Interpret(0, []byte(out)) }

// LooksLikeStream reports whether text is a model provider's streaming
// response (server-sent "data: {...}" frames) rather than an answer. It is a
// last-resort guard for code that posts a stored output to a person.
func LooksLikeStream(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "data: {") || strings.HasPrefix(t, "data:{")
}

// Clip cuts out to MaxBytes (marker included) on a rune boundary, so a
// multi-byte character is never split.
func Clip(out string) string { return ClipTo(out, MaxBytes) }

// ClipTo is Clip with a different limit.
func ClipTo(out string, limit int) string {
	if len(out) <= limit {
		return out
	}
	cut := limit - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + truncationMarker
}

func isStream(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:"))
}

// envelopeOutput unwraps {"output": "..."}.
func envelopeOutput(body []byte) (string, bool) {
	var e struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(body, &e) == nil && e.Output != "" {
		return e.Output, true
	}
	return "", false
}

type chunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// streamText concatenates the choice-0 delta.content of every chat completion
// chunk. Reasoning deltas are skipped: they are the model's thinking, not its
// answer. ok is false when no chunk carried text.
func streamText(body []byte) (string, bool) {
	var sb strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		data, found := strings.CutPrefix(sc.Text(), "data:")
		if !found {
			continue
		}
		data = strings.TrimSpace(data)
		// [DONE] is not a stop signal: some proxies emit a premature one after a
		// reasoning block and then stream the real content.
		if data == "[DONE]" {
			continue
		}
		var c chunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			if ch.Index == 0 {
				sb.WriteString(ch.Delta.Content)
			}
		}
	}
	return sb.String(), sb.Len() > 0
}

// completionText returns the text of a non-streamed chat completion.
func completionText(body []byte) (string, bool) {
	var c struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &c) != nil || len(c.Choices) == 0 || c.Choices[0].Message.Content == "" {
		return "", false
	}
	return c.Choices[0].Message.Content, true
}

func truncateReason(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

// errorPayloadMessage extracts the message from {"error":"..."} or
// {"error":{"message":"..."}}; "" when s is not such a payload.
func errorPayloadMessage(s string) string {
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var p struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(s), &p) != nil || len(p.Error) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(p.Error, &str) == nil {
		return str
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(p.Error, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return string(p.Error)
}
