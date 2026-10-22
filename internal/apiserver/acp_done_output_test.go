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
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// completedOutput returns the concatenated part content of the run.completed
// frame in an SSE body.
func completedOutput(t *testing.T, body string) string {
	t.Helper()
	for _, f := range sseFrames(t, body) {
		if f[0] != "run.completed" {
			continue
		}
		var ev struct {
			Run struct {
				Output []struct {
					Parts []struct {
						Content string `json:"content"`
					} `json:"parts"`
				} `json:"output"`
			} `json:"run"`
		}
		if err := json.Unmarshal([]byte(f[1]), &ev); err != nil {
			t.Fatalf("decoding run.completed: %v", err)
		}
		var sb strings.Builder
		for _, m := range ev.Run.Output {
			for _, p := range m.Parts {
				sb.WriteString(p.Content)
			}
		}
		return sb.String()
	}
	t.Fatalf("no run.completed frame in:\n%s", body)
	return ""
}

// Regression for #74: an agent that ends with _done(output=...) streams no
// content tokens, so run.completed used to carry an empty output.
func TestStreamACPRun_DoneOutputReachesRunCompleted(t *testing.T) {
	rec := traceStreamRun(t, []string{
		tracePrefix + `{"type":"toolCall","name":"read"}`,
		tracePrefix + `{"type":"done","output":"the full final answer"}`,
	})
	if got := completedOutput(t, rec.Body.String()); got != "the full final answer" {
		t.Fatalf("run.completed output = %q, want the _done output", got)
	}
}

func TestStreamACPRun_FinalOutputEventReachesRunCompleted(t *testing.T) {
	rec := traceStreamRun(t, []string{tracePrefix + `{"type":"finalOutput","output":"final"}`})
	if got := completedOutput(t, rec.Body.String()); got != "final" {
		t.Fatalf("run.completed output = %q, want %q", got, "final")
	}
}

// A plain text turn's done event repeats the streamed text; it must not be
// appended a second time.
func TestStreamACPRun_DoneRepeatingStreamedTextIsNotDuplicated(t *testing.T) {
	rec := traceStreamRun(t, []string{
		"Hello ", "world",
		tracePrefix + `{"type":"done","output":"Hello world"}`,
	})
	if got := completedOutput(t, rec.Body.String()); got != "Hello world" {
		t.Fatalf("run.completed output = %q, want %q", got, "Hello world")
	}
}

func TestStreamACPRun_DonePartiallyStreamedAddsOnlyRemainder(t *testing.T) {
	rec := traceStreamRun(t, []string{
		"Hello ",
		tracePrefix + `{"type":"done","output":"Hello world"}`,
	})
	if got := completedOutput(t, rec.Body.String()); got != "Hello world" {
		t.Fatalf("run.completed output = %q, want %q", got, "Hello world")
	}
}

func TestStreamACPRun_FailEventAddsNoOutput(t *testing.T) {
	rec := traceStreamRun(t, []string{tracePrefix + `{"type":"fail","reason":"boom"}`})
	if got := completedOutput(t, rec.Body.String()); got != "" {
		t.Fatalf("run.completed output = %q, want empty for fail", got)
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

func TestTerminalTraceOutput(t *testing.T) {
	cases := map[string]string{
		`{"type":"done","output":"x"}`:        "x",
		`{"type":"finalOutput","output":"y"}`: "y",
		`{"type":"fail","output":"z"}`:        "",
		`{"type":"toolCall","output":"z"}`:    "",
		`not json`:                            "",
	}
	for in, want := range cases {
		if got := terminalTraceOutput(in); got != want {
			t.Errorf("terminalTraceOutput(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunOutput(t *testing.T) {
	if got := truncateRunOutput("short"); got != "short" {
		t.Fatalf("short output changed: %q", got)
	}
	exact := strings.Repeat("a", maxRunOutputBytes)
	if got := truncateRunOutput(exact); got != exact {
		t.Fatal("output at exactly the cap must not be truncated")
	}

	long := strings.Repeat("a", maxRunOutputBytes+500)
	got := truncateRunOutput(long)
	if len(got) > maxRunOutputBytes {
		t.Fatalf("len = %d, want <= %d", len(got), maxRunOutputBytes)
	}
	if !strings.HasSuffix(got, runOutputTruncationMarker) {
		t.Fatalf("truncated output must say it was clipped, got tail %q", got[len(got)-30:])
	}

	// A multi-byte rune straddling the cut must not be split.
	multi := strings.Repeat("é", maxRunOutputBytes) // 2 bytes per rune
	got = truncateRunOutput(multi)
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a multi-byte rune")
	}
	if len(got) > maxRunOutputBytes {
		t.Fatalf("len = %d, want <= %d", len(got), maxRunOutputBytes)
	}
}
