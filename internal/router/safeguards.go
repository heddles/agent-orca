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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// loopGuard holds the per-run counters behind cfg.Safeguards. It is guarded by
// Router.mu and reset whenever the router starts a new run.
type loopGuard struct {
	// repeated counts calls per tool name + normalized arguments.
	repeated map[string]int
	// perTool counts calls per tool name.
	perTool map[string]int
	// noopStreak counts consecutive text-only turns below MinSubstantiveTokens.
	noopStreak int
}

// safeguardTrip describes why a run was halted. Its JSON matches the
// operator's LoopDetectedInfo.
type safeguardTrip struct {
	Reason    string `json:"reason"`
	TripCount int    `json:"tripCount"`
	ToolName  string `json:"toolName,omitempty"`
}

// toolCallKey identifies a tool call by name and arguments. Arguments are
// re-encoded so key order and whitespace do not make identical calls distinct.
func toolCallKey(tc ToolCall) string {
	args := tc.Function.Arguments
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			args = string(b)
		}
	}
	return tc.Function.Name + "\x00" + args
}

// recordToolCallLocked counts a tool call the LLM requested and returns the
// trip it causes, if any. Any tool call also ends a no-op streak. The caller
// holds r.mu.
func (r *Router) recordToolCallLocked(tc ToolCall) *safeguardTrip {
	g := &r.guard
	if g.repeated == nil {
		g.repeated = map[string]int{}
		g.perTool = map[string]int{}
	}
	g.noopStreak = 0
	s := r.cfg.Safeguards
	name := tc.Function.Name

	key := toolCallKey(tc)
	g.repeated[key]++
	if n := g.repeated[key]; s.MaxRepeatedToolCalls > 0 && n >= s.MaxRepeatedToolCalls {
		return &safeguardTrip{
			Reason:    fmt.Sprintf("tool %q was called %d times with identical arguments", name, n),
			TripCount: n,
			ToolName:  name,
		}
	}
	g.perTool[name]++
	if n := g.perTool[name]; s.ToolFrequencyCap > 0 && n > s.ToolFrequencyCap {
		return &safeguardTrip{
			Reason:    fmt.Sprintf("tool %q was called %d times, over the cap of %d", name, n, s.ToolFrequencyCap),
			TripCount: n,
			ToolName:  name,
		}
	}
	return nil
}

// recordTextTurnLocked counts a turn that ended without tool calls and returns
// the trip it causes, if any. Chat-mode runs are exempt: there a short reply is
// answered by a person, not fed back to the model, so it cannot loop. The
// caller holds r.mu.
func (r *Router) recordTextTurnLocked(completionTokens int) *safeguardTrip {
	s := r.cfg.Safeguards
	if s.MaxConsecutiveNoopTurns <= 0 || r.cfg.ChatMode {
		return nil
	}
	if completionTokens >= s.MinSubstantiveTokens {
		r.guard.noopStreak = 0
		return nil
	}
	r.guard.noopStreak++
	if n := r.guard.noopStreak; n >= s.MaxConsecutiveNoopTurns {
		return &safeguardTrip{
			Reason:    fmt.Sprintf("%d consecutive turns produced no tool calls and fewer than %d tokens", n, s.MinSubstantiveTokens),
			TripCount: n,
		}
	}
	return nil
}

// tripSafeguard halts the run: later LLM requests get 410 Gone, the trace
// stream ends with a fail event, and the operator is told so it fails the run
// instead of retrying it.
func (r *Router) tripSafeguard(trip safeguardTrip) {
	r.mu.Lock()
	already := r.loopDetected
	r.loopDetected = true
	r.mu.Unlock()
	if already {
		return
	}
	slog.Warn("safeguard tripped; halting run", "run", r.cfg.RunName, "reason", trip.Reason)

	failEv, _ := json.Marshal(map[string]string{"type": "fail", "reason": "safeguard: " + trip.Reason})
	r.emitTraceEventBoth(string(failEv))
	go r.checkpoint(context.Background())
	go func() {
		if err := r.notifyOperatorLoopDetected(trip); err != nil {
			slog.Error("notifying operator of safeguard trip", "run", r.cfg.RunName, "err", err)
		}
	}()
}

// notifyOperatorLoopDetected records the trip on the AgentRun via the
// operator's internal API.
func (r *Router) notifyOperatorLoopDetected(trip safeguardTrip) error {
	if r.cfg.OperatorAPIURL == "" {
		return nil
	}
	tokenBytes, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return fmt.Errorf("reading SA token: %w", err)
	}
	url := fmt.Sprintf("%s/agentrun/%s/%s/loop-detected", r.cfg.OperatorAPIURL, r.cfg.RunNamespace, r.cfg.RunName)
	body, _ := json.Marshal(trip)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("operator returned %d", resp.StatusCode)
	}
	return nil
}

// writeSafeguardHalt ends the agent's SSE response with an explanation instead
// of dispatching the offending tool call.
func writeSafeguardHalt(w http.ResponseWriter, flusher http.Flusher, trip safeguardTrip) {
	stop := finishReasonStop
	chunk := streamingChatCompletionChunk{
		ID:     "safeguard",
		Object: "chat.completion.chunk",
		Choices: []streamChoice{{
			Index:        0,
			Delta:        streamDelta{Content: "Run halted by safeguard: " + trip.Reason},
			FinishReason: &stop,
		}},
	}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}
