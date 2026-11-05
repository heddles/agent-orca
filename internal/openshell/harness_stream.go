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

package openshell

// The Claude Code CLI run with --output-format stream-json --verbose prints one
// JSON object per line: a system init, assistant messages (text and tool_use
// blocks), user messages (tool_result blocks), rate_limit_event lines and a
// final result. The sandbox supervisor sees only network connections, so this
// stream is where a harness run shows what the agent did: its tool calls, their
// results, its token usage and how much of the subscription it has used.

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// HarnessClaudeCode names the Claude Code CLI harness.
const HarnessClaudeCode = "claude-code"

// Harness event kinds.
const (
	HarnessInit      = "init"
	HarnessMessage   = "message"
	HarnessToolUse   = "tool_use"
	HarnessToolRes   = "tool_result"
	HarnessResult    = "result"
	HarnessRateLimit = "rate_limit"
)

// maxHarnessText bounds the text, tool input and tool result kept per event.
const maxHarnessText = 2000

// HarnessUsage is a run's token usage as the CLI reports it.
type HarnessUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
}

// HarnessEvent is one thing the harness did, read from its output stream.
type HarnessEvent struct {
	// Time is when the CLI reported the step. A step the CLI does not timestamp takes
	// its neighbour's time; Time is nil only when no step in the read has one.
	Time *time.Time `json:"time,omitempty"`
	Kind string     `json:"kind"`
	// Model is the model that answered (init, result).
	Model string `json:"model,omitempty"`
	// Tool and ToolUseID identify a tool call; a tool_result carries the ID of
	// the call it answers.
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"toolUseId,omitempty"`
	// Input is the tool call's input as compact JSON, truncated.
	Input string `json:"input,omitempty"`
	// Text is a message, a tool result or the final result, truncated.
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"isError,omitempty"`
	// Result events only.
	Turns      int           `json:"turns,omitempty"`
	DurationMS int64         `json:"durationMs,omitempty"`
	CostUSD    float64       `json:"costUsd,omitempty"` // list-price equivalent, not what a subscription bills
	Usage      *HarnessUsage `json:"usage,omitempty"`
	// RateLimit events only: the status of the limit that applies and the share
	// of each subscription window used (0 to 1), keyed by window, e.g. five_hour.
	Status      string             `json:"status,omitempty"`
	Utilization map[string]float64 `json:"utilization,omitempty"`
}

type streamLine struct {
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype"`
	Model     string    `json:"model"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// result
	IsError      bool                       `json:"is_error"`
	Result       string                     `json:"result"`
	NumTurns     int                        `json:"num_turns"`
	DurationMS   int64                      `json:"duration_ms"`
	TotalCostUSD float64                    `json:"total_cost_usd"`
	Usage        *streamUsage               `json:"usage"`
	ModelUsage   map[string]json.RawMessage `json:"modelUsage"`
	// rate_limit_event
	RateLimit *struct {
		Status  string `json:"status"`
		Type    string `json:"rateLimitType"`
		Windows map[string]struct {
			Utilization float64 `json:"utilization"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

type streamUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ParseClaudeStream reads the harness events in logs after the first skip
// lines. It returns the events and the number of lines consumed, which the
// caller passes as skip next time so each line is read once. Lines that are not
// JSON objects (stderr, warnings) are consumed and ignored. A last line
// without its newline may be half written, so it is left for the next read
// unless it already parses.
func ParseClaudeStream(logs string, skip int) (events []HarnessEvent, consumed int) {
	lines := strings.Split(logs, "\n")
	for i, raw := range lines {
		last := i == len(lines)-1
		raw = strings.TrimSpace(raw)
		if last && raw == "" {
			break // the logs ended with a newline
		}
		var l streamLine
		err := json.Unmarshal([]byte(raw), &l)
		if err != nil && last {
			break // possibly incomplete
		}
		consumed = i + 1
		if i < skip || err != nil {
			continue
		}
		events = append(events, eventsOf(&l)...)
	}
	if consumed < skip {
		consumed = skip
	}
	fillNeighbourTimes(events)
	return events, consumed
}

// fillNeighbourTimes gives the steps the CLI does not timestamp (init, rate_limit,
// result) the time of the step before them, or of the one after for those at the
// start. Left blank they would show when the operator read them, which can be ten
// seconds after the step and sorts them out of order against their neighbours.
func fillNeighbourTimes(events []HarnessEvent) {
	var prev *time.Time
	for i := range events {
		if events[i].Time != nil {
			prev = events[i].Time
		} else if prev != nil {
			t := *prev
			events[i].Time = &t
		}
	}
	var next *time.Time
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Time != nil {
			next = events[i].Time
		} else if next != nil {
			t := *next
			events[i].Time = &t
		}
	}
}

func eventsOf(l *streamLine) []HarnessEvent {
	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			return []HarnessEvent{{Kind: HarnessInit, Model: l.Model}}
		}
	case "assistant", "user":
		return blockEvents(l)
	case "result":
		ev := HarnessEvent{
			Kind: HarnessResult, Text: truncateText(l.Result), IsError: l.IsError,
			Turns: l.NumTurns, DurationMS: l.DurationMS, CostUSD: l.TotalCostUSD,
		}
		if u := l.Usage; u != nil {
			ev.Usage = &HarnessUsage{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens}
		}
		models := make([]string, 0, len(l.ModelUsage))
		for m := range l.ModelUsage {
			models = append(models, m)
		}
		sort.Strings(models)
		if len(models) > 0 {
			ev.Model = models[0]
		}
		return []HarnessEvent{ev}
	case "rate_limit_event":
		if rl := l.RateLimit; rl != nil {
			ev := HarnessEvent{Kind: HarnessRateLimit, Status: rl.Status, Utilization: map[string]float64{}}
			for w, v := range rl.Windows {
				ev.Utilization[w] = v.Utilization
			}
			return []HarnessEvent{ev}
		}
	}
	return nil
}

func blockEvents(l *streamLine) []HarnessEvent {
	var blocks []contentBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil // a plain-string user prompt
	}
	var out []HarnessEvent
	for _, b := range blocks {
		ev := HarnessEvent{}
		if !l.Timestamp.IsZero() {
			t := l.Timestamp
			ev.Time = &t
		}
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			ev.Kind, ev.Text = HarnessMessage, truncateText(b.Text)
		case "tool_use":
			ev.Kind, ev.Tool, ev.ToolUseID = HarnessToolUse, b.Name, b.ID
			ev.Input = truncateText(compactJSON(b.Input))
		case "tool_result":
			ev.Kind, ev.ToolUseID, ev.IsError = HarnessToolRes, b.ToolUseID, b.IsError
			ev.Text = truncateText(resultText(b.Content))
		default:
			continue
		}
		out = append(out, ev)
	}
	return out
}

// resultText flattens a tool result, which is a string or a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep && and < readable in a shell command
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

func truncateText(s string) string {
	if len(s) <= maxHarnessText {
		return s
	}
	cut := maxHarnessText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ClaudeResult returns what the CLI's final result line says: the answer, or
// the error when isError is set. ok is false while the stream has no result
// line, as when the CLI was killed.
func ClaudeResult(logs string) (text string, isError, ok bool) {
	events, _ := ParseClaudeStream(logs, 0)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == HarnessResult {
			return events[i].Text, events[i].IsError, true
		}
	}
	return "", false, false
}
