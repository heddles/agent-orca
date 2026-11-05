package openshell

import (
	"os"
	"strings"
	"testing"
)

func TestParseClaudeStream_RealRun(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	events, consumed := ParseClaudeStream(string(raw), 0)
	if consumed != 6 {
		t.Fatalf("consumed %d lines, want 6", consumed)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := "init tool_use rate_limit tool_result message result"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds = %q, want %q", got, want)
	}
	use, res, last := events[1], events[3], events[5]
	if use.Tool != "Bash" || !strings.Contains(use.Input, `"command":"echo hello && ls /sandbox"`) {
		t.Errorf("tool_use = %+v", use)
	}
	if res.ToolUseID != use.ToolUseID || res.Text != "hello" || res.IsError {
		t.Errorf("tool_result = %+v, want it to answer %s", res, use.ToolUseID)
	}
	if rl := events[2]; rl.Status != "allowed" || rl.Utilization["five_hour"] != 0.11 || rl.Utilization["seven_day"] != 0.39 {
		t.Errorf("rate_limit = %+v", rl)
	}
	if last.Turns != 2 || last.Usage == nil || last.Usage.OutputTokens != 94 || last.Usage.CacheReadTokens != 29844 || last.Model != "claude-sonnet-5-5" {
		t.Errorf("result = %+v", last)
	}
}

func TestParseClaudeStream_Resumes(t *testing.T) {
	raw, _ := os.ReadFile("testdata/claude-stream.jsonl")
	lines := strings.SplitAfter(string(raw), "\n")
	first := strings.Join(lines[:3], "")
	ev1, n1 := ParseClaudeStream(first, 0)
	ev2, n2 := ParseClaudeStream(string(raw), n1)
	if n1 != 3 || n2 != 6 || len(ev1)+len(ev2) != 6 {
		t.Fatalf("n1=%d n2=%d events=%d+%d", n1, n2, len(ev1), len(ev2))
	}
}

func TestParseClaudeStream_PartialLastLine(t *testing.T) {
	raw, _ := os.ReadFile("testdata/claude-stream.jsonl")
	lines := strings.SplitAfter(string(raw), "\n")
	partial := lines[0] + lines[1][:40] // second line cut mid-write
	_, n := ParseClaudeStream(partial, 0)
	if n != 1 {
		t.Fatalf("consumed %d, want 1: the half-written line must wait", n)
	}
}

func TestParseClaudeStream_IgnoresNoise(t *testing.T) {
	events, n := ParseClaudeStream("warning: something\n{\"type\":\"result\",\"num_turns\":1}\n", 0)
	if n != 2 || len(events) != 1 || events[0].Kind != HarnessResult {
		t.Fatalf("n=%d events=%+v", n, events)
	}
}

func TestParseClaudeStream_BlockResult(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"boom"}]}]}}` + "\n"
	events, _ := ParseClaudeStream(line, 0)
	if len(events) != 1 || events[0].Text != "boom" || !events[0].IsError {
		t.Fatalf("events = %+v", events)
	}
}
