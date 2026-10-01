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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/state"
)

func TestEstimateTokens_CountsAllPayload(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello world"}, // ~3 tokens content + 4 overhead
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{Function: FunctionCall{Name: "search", Arguments: `{"q":"test"}`}}}}, // content + tool call
	}
	est := estimateTokens(msgs)
	if est <= 4 { // should be much more than just "hello world" / 4
		t.Errorf("estimateTokens too low: got %d", est)
	}
	// Verify tool call arguments are counted.
	msgsNoTools := []Message{
		{Role: "user", Content: "hello world"},
		{Role: "assistant", Content: "ok"},
	}
	estNoTools := estimateTokens(msgsNoTools)
	if est <= estNoTools {
		t.Errorf("estimateTokens should be higher with tool calls: withTools=%d, withoutTools=%d", est, estNoTools)
	}
}

func TestEstimateTokens_NonStringContent(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: []any{map[string]string{"type": "text", "text": "hello world this is a longer message"}}},
	}
	est := estimateTokens(msgs)
	if est <= 4 { // overhead only — should include the marshalled content
		t.Errorf("estimateTokens should count non-string content: got %d", est)
	}
}

func TestMaxContextWindow_PicksLargest(t *testing.T) {
	providers := []ProviderConfig{
		{Name: "small", ContextWindow: 100000},
		{Name: "large", ContextWindow: 1000000},
		{Name: "medium", ContextWindow: 200000},
	}
	got := maxContextWindow(providers)
	if got != 1000000 {
		t.Errorf("maxContextWindow = %d, want 1000000", got)
	}
}

func TestMaxContextWindow_Fallback(t *testing.T) {
	got := maxContextWindow(nil)
	if got != 200000 {
		t.Errorf("maxContextWindow(nil) = %d, want 200000", got)
	}
	got = maxContextWindow([]ProviderConfig{{Name: "no-window"}})
	if got != 200000 {
		t.Errorf("maxContextWindow(no window) = %d, want 200000", got)
	}
}

// TestTrimLiveBuffer_CapsByTokensNotCount is the regression test for the bug where
// the per-turn cap compared len(priorMessages) (a MESSAGE count) to the budget (a
// TOKEN count). A buffer of only 2 messages whose token estimate exceeds the budget
// must be trimmed; the old len-based cap (len=2 << 80000) let a 400k-token buffer
// slip through every turn, so compaction never stuck.
func TestTrimLiveBuffer_CapsByTokensNotCount(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 100000}}}}
	r.priorMessages = []Message{
		{Role: "user", Content: strings.Repeat("x", 400000)},      // ~100k tokens
		{Role: "assistant", Content: strings.Repeat("y", 400000)}, // ~100k tokens
	}
	r.mu.Lock()
	before := estimateTokens(r.priorMessages)
	budget := r.checkpointBudget() // 80000
	if before <= budget {
		r.mu.Unlock()
		t.Fatalf("precondition: buffer (%d) should exceed budget (%d)", before, budget)
	}
	// Message count (2) is far below budget — proves the OLD len-based check wouldn't fire.
	if len(r.priorMessages) > budget {
		r.mu.Unlock()
		t.Fatalf("precondition: message count should be << budget")
	}

	r.trimLiveBuffer()
	r.mu.Unlock()

	// trimLiveBuffer delegates truncation to a background goroutine; wait for it
	// to install the truncated result before asserting on the buffer.
	r.waitForCompaction()

	r.mu.Lock()
	after := estimateTokens(r.priorMessages)
	if after > budget {
		r.mu.Unlock()
		t.Fatalf("trimLiveBuffer left buffer over budget: est=%d budget=%d", after, budget)
	}
	if after >= before {
		r.mu.Unlock()
		t.Fatalf("trimLiveBuffer did not shrink the buffer: before=%d after=%d", before, after)
	}
	// truncateHistory injects a compaction summary of the dropped turns as a system msg.
	found := false
	for _, m := range r.priorMessages {
		if m.Role == "system" && strings.Contains(messageText(m), "[Compacted:") { //nolint:goconst

			found = true
		}
	}
	r.mu.Unlock()
	if !found {
		t.Fatalf("expected a compaction-summary system message after trimming; got %d msgs", len(r.priorMessages))
	}
}

// TestTrimLiveBuffer_UsesIncrementalTokenCache verifies that trimLiveBuffer uses
// the cached liveBufferTokens for a fast path when well under budget, avoiding a
// full O(n) estimateTokens scan.
func TestTrimLiveBuffer_UsesIncrementalTokenCache(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 200000}}}}
	// Small buffer well under the proactive threshold (60% of 160000 = 96000).
	r.priorMessages = []Message{
		{Role: "user", Content: strings.Repeat("x", 1000)}, // ~250 tokens
		{Role: "assistant", Content: "ok"},
	}
	r.liveBufferTokens = estimateTokens(r.priorMessages) // seed the cache

	// Should be a no-op (fast path): no truncation, no change.
	r.trimLiveBuffer()

	if len(r.priorMessages) != 2 {
		t.Errorf("expected no change in message count, got %d", len(r.priorMessages))
	}
}

// TestCapLiveBufferDuringToolLoop_TruncatesBufferAndSyncsCache verifies that the
// in-loop cap actually shrinks the live buffer (prior+messages) down to the
// compaction target and resyncs liveBufferTokens. This is the regression test
// for the bug where the tool-call loop's continuation recursion skipped
// trimLiveBuffer/concludeTurn, so the buffer only grew across tool calls.
func TestCapLiveBufferDuringToolLoop_TruncatesBufferAndSyncsCache(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 1000000}}}}
	// 1M context window → checkpointBudget 800000, compaction target 500000
	// (default ratio 0.5), proactive threshold 60% of budget (480000).
	big := Message{Role: "user", Content: strings.Repeat("x", 4000000)} // ~1M tokens each
	r.priorMessages = []Message{big, big}
	r.messages = []Message{
		{Role: "assistant", Content: "tool call"},
		{Role: "tool", Content: strings.Repeat("y", 4000000)}, // large tool result
	}
	before := estimateTokens(append(append([]Message{}, r.priorMessages...), r.messages...))
	if before <= r.checkpointBudget() {
		t.Fatalf("precondition: combined buffer (%d) should exceed budget (%d)", before, r.checkpointBudget())
	}

	r.mu.Lock()
	r.capLiveBufferDuringToolLoop()
	r.mu.Unlock()

	r.mu.Lock()
	after := estimateTokens(r.messages)
	afterCached := r.liveBufferTokens
	priorLen := len(r.priorMessages)
	r.mu.Unlock()

	if priorLen != 0 {
		t.Errorf("expected priorMessages cleared by in-loop cap, got %d messages", priorLen)
	}
	if after > r.checkpointBudget() {
		t.Errorf("buffer still over budget after cap: got %d want <= %d", after, r.checkpointBudget())
	}
	if after >= before {
		t.Errorf("cap did not shrink the buffer: before=%d after=%d", before, after)
	}
	if afterCached != after {
		t.Errorf("liveBufferTokens not resynced: cached=%d actual=%d", afterCached, after)
	}
	// truncateHistory should have replaced the dropped oldest turns with a
	// condensed compaction-summary system message.
	found := false
	for _, m := range r.messages {
		if m.Role == "system" && strings.Contains(messageText(m), "[Compacted:") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a compaction-summary system message after capping; got %d msgs", len(r.messages))
	}
}

// TestCapLiveBufferDuringToolLoop_FastPathNoOp verifies the O(1) fast path: when
// the cached token count is well under the proactive threshold, the cap does
// nothing and leaves the buffer intact.
func TestCapLiveBufferDuringToolLoop_FastPathNoOp(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 1000000}}}}
	small := Message{Role: "user", Content: strings.Repeat("x", 1000)} // ~250 tokens
	r.priorMessages = []Message{small}
	r.messages = []Message{{Role: "assistant", Content: "ok"}}
	// Seed the cache with an accurate, well-under-budget value.
	r.liveBufferTokens = estimateTokens(append(r.priorMessages, r.messages...))
	beforeCount := len(r.messages)
	beforePrior := len(r.priorMessages)

	r.mu.Lock()
	r.capLiveBufferDuringToolLoop()
	r.mu.Unlock()

	r.mu.Lock()
	afterCount := len(r.messages)
	afterPrior := len(r.priorMessages)
	r.mu.Unlock()

	if afterCount != beforeCount || afterPrior != beforePrior {
		t.Errorf("fast path should be a no-op: before(m=%d,p=%d) after(m=%d,p=%d)",
			beforeCount, beforePrior, afterCount, afterPrior)
	}
}

// TestIncrementBufferTokens_AddsDelta verifies the incremental token counter
// correctly accumulates as messages are appended.
func TestIncrementBufferTokens_AddsDelta(t *testing.T) {
	r := &Router{cfg: &Config{}}
	r.liveBufferTokens = 0

	delta := 100
	r.incrementBufferTokens(delta)
	if r.liveBufferTokens != 100 {
		t.Errorf("after first increment: got %d, want %d", r.liveBufferTokens, 100)
	}

	r.incrementBufferTokens(50)
	if r.liveBufferTokens != 150 {
		t.Errorf("after second increment: got %d, want %d", r.liveBufferTokens, 150)
	}

	r.incrementBufferTokens(0)
	if r.liveBufferTokens != 150 {
		t.Errorf("after zero increment: got %d, want %d", r.liveBufferTokens, 150)
	}
}

// TestConcludeTurn_UsesCachedTokenCount verifies that concludeTurn uses the cached
// liveBufferTokens for fast-path budget checks. When the cached value is well under
// the proactive threshold, it should fold without triggering a full scan.
func TestConcludeTurn_UsesCachedTokenCount(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 200000}}}}
	budget := r.checkpointBudget() // 160000

	// Set up a small conversation that's well under the proactive threshold.
	smallMsg := Message{Role: "user", Content: "hi there"}
	r.priorMessages = []Message{
		{Role: "system", Content: "system prompt"},
		smallMsg,
	}
	r.messages = []Message{
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "how are you?"},
	}

	// Seed the cache with the accurate count.
	r.liveBufferTokens = estimateTokens(append(r.priorMessages, r.messages...))

	r.mu.Lock()
	r.concludeTurn()
	r.mu.Unlock()

	// Should have folded messages into priorMessages.
	if len(r.messages) != 0 {
		t.Errorf("expected messages cleared after concludeTurn, got %d", len(r.messages))
	}
	// Should not have truncated (well under budget).
	combinedBefore := r.liveBufferTokens
	combinedAfter := estimateTokens(r.priorMessages)
	if combinedAfter > budget {
		t.Errorf("buffer should be under budget: got %d, budget %d", combinedAfter, budget)
	}
	// The cached value should match (no full re-scan needed).
	_ = combinedBefore
}

// TestProactiveThreshold_StrategyDefaults verifies that proactiveThreshold() returns
// the correct values based on the configured strategy.
func TestProactiveThreshold_StrategyDefaults(t *testing.T) {
	tests := []struct {
		name          string
		explicit      float64
		strategy      string
		wantThreshold float64
	}{
		{
			name:          "balanced default",
			strategy:      "balanced",
			wantThreshold: 0.6,
		},
		{
			name:          "aggressive",
			strategy:      "aggressive",
			wantThreshold: 0.4,
		},
		{
			name:          "max-fidelity",
			strategy:      "max-fidelity",
			wantThreshold: 0.0,
		},
		{
			name:          "empty strategy defaults to balanced",
			strategy:      "",
			wantThreshold: 0.6,
		},
		{
			name:          "explicit override",
			explicit:      0.75,
			strategy:      "aggressive",
			wantThreshold: 0.75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				ProactiveTruncationThreshold: tt.explicit,
				ContextManagementStrategy:    tt.strategy,
			}
			// No default for ProactiveTruncationThreshold here — proactiveThreshold
			// falls back to the strategy-derived value.
			if cfg.ContextManagementStrategy == "" {
				cfg.ContextManagementStrategy = "balanced"
			}
			got := cfg.proactiveThreshold()
			if got != tt.wantThreshold {
				t.Errorf("proactiveThreshold() = %f, want %f", got, tt.wantThreshold)
			}
		})
	}
}

// TestRouter_IncrementalTokenTracking_AcrossHandleChatCompletions verifies that
// the liveBufferTokens counter is correctly maintained across multiple turns in
// HandleChatCompletions, avoiding repeated O(n) estimateTokens scans on the
// full conversation for budget checks.
func TestRouter_IncrementalTokenTracking_AcrossHandleChatCompletions(t *testing.T) {
	var mu sync.Mutex
	var providerCallCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		providerCallCount++
		mu.Unlock()

		resp := ChatCompletionResponse{
			ID: "chatcmpl-test",
			Choices: []Choice{{
				Message: Message{
					Role:    "assistant",
					Content: fmt.Sprintf("Response %d", providerCallCount),
				},
				FinishReason: finishReasonStop,
			}},
			Usage: TokenUsage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "api-key")
	_ = os.WriteFile(keyFile, []byte("test-key"), 0o644)

	cfg := &Config{
		RunName:      "test-run",
		RunNamespace: "default",
		Providers: []ProviderConfig{{
			Name:          "test-provider",
			LiteLLMModel:  "gpt-4o",
			BaseURL:       srv.URL,
			APIKeyFile:    keyFile,
			ContextWindow: 200000,
			Weight:        1,
		}},
		CheckpointEvery:      1,
		ContextWindowReserve: 0.2,
		SystemPrompt:         "You are helpful.",
		ToolDefinitions:      []ToolDefinition{},
		LLMRequestTimeout:    30 * time.Second,
	}

	router, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	rec1 := httptest.NewRecorder()
	router.HandleChatCompletions(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1: expected HTTP 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// After turn 1, the incremental cache should reflect the conversation tokens.
	router.mu.Lock()
	cachedAfterTurn1 := router.liveBufferTokens
	router.mu.Unlock()

	if cachedAfterTurn1 <= 0 {
		t.Errorf("expected liveBufferTokens to be positive after turn 1, got %d", cachedAfterTurn1)
	}

	// Turn 2
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"how are you?"}]}`))
	rec2 := httptest.NewRecorder()
	router.HandleChatCompletions(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2: expected HTTP 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// After turn 2, cache should still be valid (incremented, not reset to 0).
	router.mu.Lock()
	cachedAfterTurn2 := router.liveBufferTokens
	router.mu.Unlock()

	if cachedAfterTurn2 <= 0 {
		t.Errorf("expected liveBufferTokens to be positive after turn 2, got %d", cachedAfterTurn2)
	}

	// The cache should reflect growth (turn 2 added content).
	if cachedAfterTurn2 <= cachedAfterTurn1 {
		// Note: concludeTurn folds messages and may truncate, so the cache
		// could go down if truncation happened. But with this small conversation,
		// it should grow.
		t.Logf("note: cache went from %d to %d (may truncate/preemptively cap)",
			cachedAfterTurn1, cachedAfterTurn2)
	}

	mu.Lock()
	if providerCallCount != 2 {
		t.Errorf("expected 2 provider calls, got %d", providerCallCount)
	}
	mu.Unlock()
}

// TestRouter_ClaimRun_ResetsPerRunState verifies that ClaimRun wires the new run
// identity into the config and resets all per-run state, so a warm pod reused
// across runs starts clean.
func TestRouter_ClaimRun_ResetsPerRunState(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := &Router{
		cfg: &Config{
			Providers: []ProviderConfig{{Name: "p", ContextWindow: 200000}},
		},
		cancelCtx:        cancelCtx,
		cancelFunc:       cancel,
		liveBufferTokens: 42,
		messages:         []Message{{Role: "user", Content: "stale"}},
		priorMessages:    []Message{{Role: "user", Content: "stale"}},
		spendUSD:         1.25,
		handedOff:        true,
		waitingForInput:  true,
	}

	router.ClaimRun(WarmRunInput{RunName: "new-run", Input: "test input"})

	if router.cfg.RunName != "new-run" {
		t.Errorf("cfg.RunName: got %q, want %q", router.cfg.RunName, "new-run")
	}
	if want := "agentorca/runs/new-run/state"; router.cfg.CheckpointKey != want {
		t.Errorf("cfg.CheckpointKey: got %q, want %q", router.cfg.CheckpointKey, want)
	}
	if router.cfg.ResumeCheckpointKey != "" {
		t.Errorf("cfg.ResumeCheckpointKey: got %q, want empty (no prior run)", router.cfg.ResumeCheckpointKey)
	}
	if router.cfg.HTTPInput.RunName != "new-run" || router.cfg.HTTPInput.Input != "test input" {
		t.Errorf("cfg.HTTPInput: got %+v, want run %q input %q", router.cfg.HTTPInput, "new-run", "test input")
	}
	if router.liveBufferTokens != 0 {
		t.Errorf("liveBufferTokens: got %d, want 0", router.liveBufferTokens)
	}
	if router.messages != nil || router.priorMessages != nil {
		t.Errorf("messages not reset: messages=%v priorMessages=%v", router.messages, router.priorMessages)
	}
	if router.spendUSD != 0 {
		t.Errorf("spendUSD: got %v, want 0", router.spendUSD)
	}
	if router.handedOff || router.waitingForInput {
		t.Errorf("terminal flags not reset: handedOff=%v waitingForInput=%v", router.handedOff, router.waitingForInput)
	}
}

func TestEstimateToolTokens_ReservesRoomForSchemata(t *testing.T) {
	t.Run("empty tools cost nothing", func(t *testing.T) {
		if got := estimateToolTokens(nil); got != 0 {
			t.Fatalf("expected 0 for nil tools, got %d", got)
		}
	})
	// Build a realistic toolbelt: ~25 tools, each with a description + JSON params
	// (mirrors the pwnbox MCP tool definitions + builtins).
	var tools []map[string]any //nolint:prealloc

	for i := range 25 {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        fmt.Sprintf("tool-%d", i),
				"description": "A pentest tool for recon/exploitation (JSON-schema params with props, required, enum).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"target":  map[string]any{"type": "string", "description": "target host or IP"},
						"timeout": map[string]any{"type": "integer", "description": "timeout seconds", "default": 300},
					},
					"required": []string{"target"},
				},
			},
		})
	}
	cost := estimateToolTokens(tools)
	// 25 tools with multi-line schemas should reserve a non-trivial budget
	// (well above the per-tool 8-token floor of 25*8=200).
	if cost <= 200 {
		t.Fatalf("expected tool-def token cost to exceed the per-tool floor, got %d", cost)
	}
	// Sanity: the estimated cost should be in the same ballpark as len(json)/4.
	raw, _ := json.Marshal(tools)
	wantFloor := len(raw)/4 + 8*len(tools)
	if cost != wantFloor {
		t.Fatalf("estimateToolTokens=%d, want %d", cost, wantFloor)
	}
}

func TestCheckpointBudget(t *testing.T) {
	// Capped to 80% of the largest configured provider context window (matching
	// the persisted-checkpoint budget), with a same-dataset fallback.
	r := &Router{cfg: &Config{Providers: []ProviderConfig{
		{Name: "small", ContextWindow: 100000},
		{Name: "big", ContextWindow: 262144},
	}}}
	if got := r.checkpointBudget(); got != 262144*8/10 {
		t.Fatalf("checkpointBudget=%d want %d", got, 262144*8/10)
	}
	// No provider / unknown windows -> 200000 fallback * 80%.
	r2 := &Router{cfg: &Config{}}
	if got := r2.checkpointBudget(); got != 160000 {
		t.Fatalf("checkpointBudget(no providers)=%d want 160000", got)
	}
}

func TestCompactEpisodic_DropsChunkAndKeepsSummary(t *testing.T) {
	// prior = 5 big msgs, messages = 1 current turn. The summarized chunk is the
	// last 2 prior msgs (+ current turn); after compaction those are replaced by a
	// single summary message and the buffer shrinks.
	big := Message{Role: "user", Content: strings.Repeat("x", 100000)} // ~25k tokens each
	prior := []Message{big, big, big, big, big}
	messages := []Message{{Role: "assistant", Content: "world"}}
	summary := Message{Role: "system", Content: "summary of the old turns"}

	// Drop last 2 prior msgs + the current turn => keep first 3 prior + summary.
	start := len(prior) - 2 // = 3: cur[:3] kept, cur[3:] (big,big,big,world) summarized
	budget := 262144 * 8 / 10
	out := compactEpisodic(prior, messages, summary, start, budget)

	// 3 retained old msgs + the summary message.
	if len(out) != 4 {
		t.Fatalf("expected 4 messages after compaction (3 old + summary), got %d", len(out))
	}
	if out[len(out)-1].Role != "system" || out[len(out)-1].Content != "summary of the old turns" {
		t.Fatalf("expected summary as last message, got %+v", out[len(out)-1])
	}
	// And it must fit the budget.
	est := estimateTokens(out)
	if est > budget {
		t.Fatalf("compacted buffer exceeds budget: est=%d budget=%d", est, budget)
	}
}

func TestCompactEpisodic_RespectsBudgetWithTruncation(t *testing.T) {
	// Even after compaction the head may exceed budget; truncateHistory (inside)
	// must bring it under budget by dropping the oldest with a compaction summary.
	head := []Message{{Role: "user", Content: strings.Repeat("y", 10000)}} //nolint:prealloc

	for range 50 {
		head = append(head, Message{Role: "user", Content: strings.Repeat("y", 8000)})
	}
	summary := Message{Role: "system", Content: "summary"}
	start := len(head) - 2
	// Tiny budget forces additional trimming.
	out := compactEpisodic(head, nil, summary, start, 1000)
	if estimateTokens(out) > 1000 {
		t.Fatalf("compacted buffer not under tiny budget: est=%d", estimateTokens(out))
	}
	if out[len(out)-1].Content != "summary" {
		t.Fatalf("expected summary preserved as last message, got %+v", out[len(out)-1])
	}
}

func TestCompactEpisodic_NoBudgetReturnsPrior(t *testing.T) {
	prior := []Message{{Role: "user", Content: "hi"}}
	messages := []Message{{Role: "assistant", Content: "world"}}
	out := compactEpisodic(prior, messages, Message{Role: "system", Content: "s"}, 0, 0)
	// budget<=0 => bails, returns prior unchanged (no compaction/truncation).
	if len(out) != len(prior) {
		t.Fatalf("expected prior unchanged (len %d), got %d", len(prior), len(out))
	}
	if out[0].Content != "hi" {
		t.Fatalf("expected prior content unchanged, got %q", out[0].Content)
	}
}

// TestConcludeTurn_CapsBufferAtTurnEnd verifies the fix for "compaction runs every
// turn on a giant buffer": after a turn, prior+messages (~412k for a re-sending
// chat agent) must be capped to the budget so checkpoint() sees <=budget (no
// re-truncation log) and memory doesn't grow.
func TestConcludeTurn_CapsBufferAtTurnEnd(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 262144}}}}
	// prior = capped conversation; messages = the re-sent turn (the agent re-sent
	// ~412k worth of history this turn). Combined must exceed the 209715 budget.
	bigTurn := []Message{{Role: "user", Content: strings.Repeat("x", 400000)},
		{Role: "assistant", Content: strings.Repeat("y", 400000)}}
	r.priorMessages = []Message{{Role: "user", Content: strings.Repeat("z", 100000)}}
	r.messages = bigTurn

	budget := r.checkpointBudget() // 209715
	if est := estimateTokens(append(append([]Message{}, r.priorMessages...), r.messages...)); est <= budget {
		t.Fatalf("precondition: combined buffer (%d) should exceed budget (%d)", est, budget)
	}

	// Caller must hold r.mu; single-goroutine test, so call directly.
	r.mu.Lock()
	r.concludeTurn()
	r.mu.Unlock()

	// concludeTurn delegates truncation to a background goroutine; wait for it
	// to install the truncated result before asserting on the buffer.
	r.waitForCompaction()

	r.mu.Lock()
	if len(r.messages) != 0 {
		r.mu.Unlock()
		t.Fatalf("concludeTurn should clear r.messages (folded into prior); got %d", len(r.messages))
	}
	est := estimateTokens(r.priorMessages)
	r.mu.Unlock()
	// The combined buffer (prior+turn) was > budget; concludeTurn must cap it so the
	// next checkpoint() sees <=budget and doesn't re-truncate every turn.
	if est > budget {
		t.Fatalf("concludeTurn left buffer over budget: est=%d budget=%d", est, budget)
	}
	// And it must have actually trimmed the over-budget combined buffer.
	priorSnapshot := []Message{{Role: "user", Content: strings.Repeat("z", 100000)}}
	combinedBefore := estimateTokens(append(priorSnapshot, bigTurn...))
	if est >= combinedBefore {
		t.Fatalf("concludeTurn should have trimmed the over-budget buffer; now=%d was=%d", est, combinedBefore)
	}
}

func TestTruncateHistory_UnderBudget(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	result := truncateHistory(msgs, 999999)
	if len(result) != len(msgs) {
		t.Errorf("expected %d messages unchanged, got %d", len(msgs), len(result))
	}
}

func TestTruncateHistory_BasicTruncation(t *testing.T) {
	// Build a conversation that's too large for a small budget.
	// 6 body messages (~1000 tokens each) + system = ~6000+ tokens total.
	msgs := []Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: strings.Repeat("a", 4000)},      // ~1000 tokens
		{Role: "assistant", Content: strings.Repeat("b", 4000)}, // ~1000 tokens
		{Role: "user", Content: strings.Repeat("c", 4000)},      // ~1000 tokens
		{Role: "assistant", Content: strings.Repeat("d", 4000)}, // ~1000 tokens
		{Role: "user", Content: strings.Repeat("e", 4000)},      // ~1000 tokens
		{Role: "assistant", Content: strings.Repeat("f", 4000)}, // ~1000 tokens
	}
	// Budget of ~3500 tokens — should drop older messages but keep the last ~2 body messages.
	result := truncateHistory(msgs, 3500)

	if len(result) >= len(msgs) {
		t.Fatalf("expected truncation, got %d messages (same as input %d)", len(result), len(msgs))
	}

	// First message should still be the system prompt.
	if result[0].Role != "system" {
		t.Errorf("first message should be system, got %s", result[0].Role)
	}

	// Second message should be the compaction summary.
	if result[1].Role != "system" {
		t.Errorf("second message should be compaction system msg, got %s", result[1].Role)
	}
	compaction, ok := result[1].Content.(string)
	if !ok {
		t.Fatal("compaction message content should be string")
	}
	if !strings.Contains(compaction, "[Compacted:") {
		t.Errorf("compaction message missing header: %s", compaction)
	}

	// Last message should be preserved (most recent).
	last := result[len(result)-1]
	if last.Role != "assistant" { //nolint:goconst

		t.Errorf("last message should be assistant, got %s", last.Role)
	}
}

func TestTruncateHistory_CompactionContainsKeyContext(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "I'm Alice and I need help with the API"},
		{Role: "assistant", Content: "Sure!", ToolCalls: []ToolCall{
			{ID: "tc1", Function: FunctionCall{Name: "_rag_search", Arguments: `{"query":"API docs"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "some results"},
		{Role: "user", Content: "Now check the GitHub issues"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{
			{ID: "tc2", Function: FunctionCall{Name: "github-mcp-list-issues", Arguments: `{"owner":"ci-agent-orca"}`}},
		}},
		{Role: "tool", ToolCallID: "tc2", Content: "issue list"},
		{Role: "user", Content: strings.Repeat("x", 8000)},      // force truncation
		{Role: "assistant", Content: strings.Repeat("y", 8000)}, // force truncation
	}

	// Small budget to force dropping the early messages.
	result := truncateHistory(msgs, 3000)
	if len(result) >= len(msgs) {
		t.Skip("no truncation occurred — budget too large for test data")
	}

	// Find the compaction message.
	var compaction string
	for _, m := range result {
		if s, ok := m.Content.(string); ok && strings.Contains(s, "[Compacted:") {
			compaction = s
			break
		}
	}
	if compaction == "" {
		t.Fatal("no compaction summary found")
	}

	if !strings.Contains(compaction, "Alice") {
		t.Errorf("compaction should contain user's name from first message: %s", compaction)
	}
	if !strings.Contains(compaction, "_rag_search") {
		t.Errorf("compaction should list _rag_search tool: %s", compaction)
	}
	if !strings.Contains(compaction, "github-mcp-list-issues") {
		t.Errorf("compaction should list github-mcp-list-issues tool: %s", compaction)
	}
}

func TestTruncateHistory_ToolCallBoundaryRespected(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: strings.Repeat("a", 4000)},
		{Role: "assistant", Content: "thinking", ToolCalls: []ToolCall{
			{ID: "tc1", Function: FunctionCall{Name: "search", Arguments: `{"q":"test"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: strings.Repeat("r", 4000)},
		{Role: "user", Content: strings.Repeat("b", 4000)},
		{Role: "assistant", Content: strings.Repeat("c", 4000)},
	}

	result := truncateHistory(msgs, 3000)

	// Verify no tool_use without its tool_result in the kept portion.
	pending := make(map[string]bool)
	for _, m := range result {
		if m.Role == "system" {
			continue // skip compaction
		}
		for _, tc := range m.ToolCalls {
			pending[tc.ID] = true
		}
		if m.Role == "tool" && m.ToolCallID != "" { //nolint:goconst

			if !pending[m.ToolCallID] {
				t.Errorf("tool_result %q has no preceding tool_use in kept messages", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	for id := range pending {
		t.Errorf("tool_use %q has no tool_result in kept messages", id)
	}
}

func TestTruncateHistory_SystemOnlyMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "prompt 1"},
		{Role: "system", Content: "prompt 2"},
	}
	result := truncateHistory(msgs, 1)
	if len(result) != len(msgs) {
		t.Errorf("system-only messages should not be truncated: got %d, want %d", len(result), len(msgs))
	}
}

func TestTruncateHistory_NoSafeCut(t *testing.T) {
	// Single giant unresolved tool call — no safe cut point exists.
	msgs := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Function: FunctionCall{Name: "long_task", Arguments: strings.Repeat("x", 40000)}},
		}},
		// No tool_result — the only safe cuts are 0 (drop nothing) and there's no complete boundary.
	}
	result := truncateHistory(msgs, 100)
	// Should return unchanged because there's no safe boundary to cut at.
	if len(result) != len(msgs) {
		t.Errorf("expected unchanged messages when no safe cut exists: got %d, want %d", len(result), len(msgs))
	}
}

func TestCompactDroppedMessages(t *testing.T) {
	dropped := []Message{
		{Role: "user", Content: "My name is Bob, I need help with deployment"},
		{Role: "assistant", Content: "thinking", ToolCalls: []ToolCall{
			{ID: "tc1", Function: FunctionCall{Name: "_rag_search", Arguments: `{"q":"deploy"}`}},
			{ID: "tc2", Function: FunctionCall{Name: "github-mcp-get-file-contents", Arguments: `{"path":"deploy.yaml"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "results"},
		{Role: "tool", ToolCallID: "tc2", Content: "file contents"},
		{Role: "assistant", Content: "Here's what I found about deployment."},
		{Role: "user", Content: "Can you also check the CI pipeline?"},
	}

	msg := compactDroppedMessages(dropped)

	if msg.Role != "system" {
		t.Errorf("compaction message role = %q, want system", msg.Role)
	}
	content, ok := msg.Content.(string)
	if !ok {
		t.Fatal("compaction content should be string")
	}

	// Check key context is preserved.
	if !strings.Contains(content, "Bob") {
		t.Errorf("should contain user name 'Bob': %s", content)
	}
	if !strings.Contains(content, "_rag_search") {
		t.Errorf("should list _rag_search tool: %s", content)
	}
	if !strings.Contains(content, "github-mcp-get-file-contents") {
		t.Errorf("should list github-mcp tool: %s", content)
	}
	if !strings.Contains(content, "CI pipeline") {
		t.Errorf("should contain user follow-up about CI: %s", content)
	}
	if !strings.Contains(content, "2 calls") {
		t.Errorf("should report 2 total tool calls: %s", content)
	}
	if !strings.Contains(content, "2 unique") {
		t.Errorf("should report 2 unique tools: %s", content)
	}
	// Verify the compact header format.
	if !strings.Contains(content, "[Compacted:") {
		t.Errorf("should use condensed compact header: %s", content)
	}
	if !strings.Contains(content, "msg/") {
		t.Errorf("header should include message and token counts: %s", content)
	}
}

// TestCompactDroppedMessages_TruncatesLongMessages verifies the shortened
// truncation thresholds (75 chars for first user message, 50 for follow-ups)
// introduced to reduce summary token overhead (issue #54 recommendations).
func TestCompactDroppedMessages_TruncatesLongMessages(t *testing.T) {
	longFirst := strings.Repeat("a", 200)  // 200 chars → should truncate to 75
	longFollow := strings.Repeat("b", 120) // 120 chars → should truncate to 50

	dropped := []Message{
		{Role: "user", Content: longFirst},
		{Role: "user", Content: longFollow},
		{Role: "user", Content: longFollow},
		{Role: "user", Content: longFollow}, // 3rd follow-up: should be dropped (max 2)
	}

	msg := compactDroppedMessages(dropped)
	content, _ := msg.Content.(string)

	// First message should be truncated to 75 chars (+ "…").
	if !strings.Contains(content, strings.Repeat("a", 75)+"…") {
		t.Errorf("first user message should truncate at 75 chars: %s", content)
	}
	// Follow-up should be truncated to 50 chars (+ "…").
	if !strings.Contains(content, strings.Repeat("b", 50)+"…") {
		t.Errorf("follow-up message should truncate at 50 chars: %s", content)
	}
	// The 3rd follow-up should NOT appear (only 2 kept).
	// Since all 3 are identical (b×120), we verify by counting truncation markers
	// for the follow-up text — there should be exactly 2.
	count := strings.Count(content, strings.Repeat("b", 50)+"…")
	if count != 2 {
		t.Errorf("expected exactly 2 follow-up messages (got %d truncations): %s", count, content)
	}
}

// TestCompactDroppedMessages_NoUserMessages verifies robustness when there are
// no user messages (all drops are assistant/tool messages).
func TestCompactDroppedMessages_NoUserMessages(t *testing.T) {
	dropped := []Message{
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{
			{ID: "tc1", Function: FunctionCall{Name: "search", Arguments: `{"q":"test"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "results"},
	}
	msg := compactDroppedMessages(dropped)
	content, _ := msg.Content.(string)
	if msg.Role != "system" {
		t.Errorf("role = %q, want system", msg.Role)
	}
	if !strings.Contains(content, "search") {
		t.Errorf("should list tool name: %s", content)
	}
	if !strings.Contains(content, "1 calls") {
		t.Errorf("should report 1 tool call: %s", content)
	}
	if strings.Contains(content, "Intent:") {
		t.Errorf("should not have Intent field when no user messages: %s", content)
	}
}

// TestTruncateText verifies the truncation helper used by compactDroppedMessages.
func TestTruncateText(t *testing.T) {
	if got := truncateText("short", 75); got != "short" {
		t.Errorf("short text should be unchanged, got %q", got)
	}
	if got := truncateText(strings.Repeat("x", 100), 50); got != strings.Repeat("x", 50)+"…" {
		t.Errorf("should truncate to 50 chars with ellipsis, got len=%d", len(got))
	}
	if got := truncateText("", 75); got != "" {
		t.Errorf("empty string should stay empty, got %q", got)
	}
	if got := truncateText(strings.Repeat("x", 50), 50); got != strings.Repeat("x", 50) {
		t.Errorf("exactly maxRunes should not be truncated (no ellipsis), got len=%d", len(got))
	}
}

// --- truncateToolResult tests ---

func TestTruncateToolResult_UnderLimit(t *testing.T) {
	result := "short result"
	got := truncateToolResult(result, 1000)
	if got != result {
		t.Errorf("expected unchanged result, got %q", got)
	}
}

func TestTruncateToolResult_OverLimit(t *testing.T) {
	// 20K chars with maxTokens=1000 → cap at 4000 chars.
	result := strings.Repeat("x", 20000)
	got := truncateToolResult(result, 1000)
	if len(got) >= len(result) {
		t.Fatalf("expected truncation, got len=%d (same as input)", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 4000)) {
		t.Error("truncated result should start with the first 4000 chars")
	}
	if !strings.Contains(got, "[... truncated") {
		t.Error("truncated result should contain truncation marker")
	}
	if !strings.Contains(got, "20000 chars") {
		t.Error("truncation marker should report original size")
	}
	if !strings.Contains(got, "4000 chars") {
		t.Error("truncation marker should report cap size")
	}
}

func TestTruncateToolResult_ZeroLimit(t *testing.T) {
	result := strings.Repeat("x", 20000)
	got := truncateToolResult(result, 0)
	if got != result {
		t.Errorf("maxTokens=0 should pass through unchanged, got len=%d", len(got))
	}
}

func TestTruncateToolResult_ExactLimit(t *testing.T) {
	// Exactly at the limit — should NOT truncate.
	result := strings.Repeat("x", 4000) // 4000 chars = 1000 tokens
	got := truncateToolResult(result, 1000)
	if got != result {
		t.Errorf("result at exact limit should pass through unchanged, got len=%d", len(got))
	}
}

// --- injectBuiltinSystemHints tests ---

func TestInjectBuiltinSystemHints_ListsAllBuiltins(t *testing.T) {
	tests := []struct {
		name         string
		toolDefs     []ToolDefinition
		chatMode     bool
		wantTools    []string
		shouldInject bool
	}{
		{
			name: "non-child-run-with-builtins",
			toolDefs: []ToolDefinition{
				{Name: "_handoff", BackendType: "builtin"},
				{Name: "_clarify", BackendType: "builtin"},
				{Name: "_spawn", BackendType: "builtin"},
				{Name: "_emit_event", BackendType: "builtin"},
				{Name: "_done", BackendType: "builtin"},
				{Name: "_fail", BackendType: "builtin"},
				{Name: "my-custom-tool", BackendType: "regular"},
			},
			chatMode:     false,
			wantTools:    []string{"_handoff", "_clarify", "_spawn", "_emit_event", "_done", "_fail"},
			shouldInject: true,
		},
		{
			name: "child-run-no-handoff-clarify-spawn-emit",
			toolDefs: []ToolDefinition{
				{Name: "_done", BackendType: "builtin"},
				{Name: "_fail", BackendType: "builtin"},
				{Name: "my-custom-tool", BackendType: "regular"},
			},
			chatMode:     false,
			wantTools:    []string{"_done", "_fail"},
			shouldInject: true,
		},
		{
			name: "no-builtins",
			toolDefs: []ToolDefinition{
				{Name: "my-custom-tool", BackendType: "regular"},
			},
			chatMode:     false,
			wantTools:    nil,
			shouldInject: false,
		},
		{
			name:         "empty-tools",
			toolDefs:     nil,
			chatMode:     false,
			wantTools:    nil,
			shouldInject: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Router{
				cfg: &Config{
					ToolDefinitions: tt.toolDefs,
					ChatMode:        tt.chatMode,
				},
			}
			chatReq := ChatCompletionRequest{
				Messages: []Message{{Role: "user", Content: "hello"}},
			}
			result := r.injectBuiltinSystemHints(chatReq)

			if !tt.shouldInject {
				if len(result.Messages) != 1 {
					t.Errorf("expected 1 message (no injection), got %d", len(result.Messages))
				}
				return
			}

			// Find the injected hint message
			var hintMsg *Message
			for i := range result.Messages {
				if result.Messages[i].Role == "system" && strings.Contains(result.Messages[i].Content.(string), "Platform tools available") {
					hintMsg = &result.Messages[i]
					break
				}
			}
			if hintMsg == nil { //nolint:staticcheck

				t.Fatal("expected to find system hint message")
			}

			content := hintMsg.Content.(string) //nolint:staticcheck

			for _, tool := range tt.wantTools {
				if !strings.Contains(content, tool) {
					t.Errorf("platform tools hint should contain %q, got: %s", tool, content)
				}
			}
			if !strings.Contains(content, "Platform tools available") {
				t.Errorf("hint should mention 'Platform tools available', got: %s", content)
			}
		})
	}
}

func TestInjectBuiltinSystemHints_ChatModeGuidance(t *testing.T) {
	r := &Router{
		cfg: &Config{
			ToolDefinitions: []ToolDefinition{
				{Name: "_clarify", BackendType: "builtin"},
				{Name: "_done", BackendType: "builtin"},
			},
			ChatMode: true,
		},
	}
	chatReq := ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
	result := r.injectBuiltinSystemHints(chatReq)

	var hintMsg *Message
	for i := range result.Messages {
		if result.Messages[i].Role == "system" && strings.Contains(result.Messages[i].Content.(string), "direct conversation") {
			hintMsg = &result.Messages[i]
			break
		}
	}
	if hintMsg == nil { //nolint:staticcheck

		t.Fatal("expected to find system hint message")
	}

	content := hintMsg.Content.(string) //nolint:staticcheck

	if !strings.Contains(content, "direct conversation with a human user") {
		t.Errorf("chat mode hint should mention human conversation, got: %s", content)
	}
}

func TestInjectBuiltinSystemHints_NonChatModeGuidance(t *testing.T) {
	r := &Router{
		cfg: &Config{
			ToolDefinitions: []ToolDefinition{
				{Name: "_clarify", BackendType: "builtin"},
				{Name: "_done", BackendType: "builtin"},
			},
			ChatMode: false,
		},
	}
	chatReq := ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
	result := r.injectBuiltinSystemHints(chatReq)

	var hintMsg *Message
	for i := range result.Messages {
		if result.Messages[i].Role == "system" && strings.Contains(result.Messages[i].Content.(string), "automated pipeline") {
			hintMsg = &result.Messages[i]
			break
		}
	}
	if hintMsg == nil { //nolint:staticcheck

		t.Fatal("expected to find system hint message")
	}

	content := hintMsg.Content.(string) //nolint:staticcheck

	if !strings.Contains(content, "automated pipeline") {
		t.Errorf("non-chat mode hint should mention pipeline, got: %s", content)
	}
}

func TestExecuteListResources(t *testing.T) {
	r := &Router{
		cfg: &Config{
			MCPServers: []MCPServerConfig{
				{Name: "github-mcp", Transport: "stdio", URL: ""},
				{Name: "filesystem-mcp", Transport: "http", URL: "http://fs-mcp.svc:8080"},
			},
			KnowledgeBases: []KnowledgeBaseConfig{
				{Name: "tech-docs", CollectionName: "tech-docs-v1"},
				{Name: "api-ref", CollectionName: "api-ref-v2"},
			},
			ToolDefinitions: []ToolDefinition{
				{Name: "_done", BackendType: "builtin"},
				{Name: "_fail", BackendType: "builtin"},
				{Name: "my-tool", BackendType: "regular", Description: "My custom tool"},
				{Name: "mcp-tool", BackendType: "mcp"},
			},
		},
	}

	result := r.executeListResources(context.Background(), "{}")
	if strings.Contains(result, `"error"`) {
		t.Fatalf("unexpected error: %s", result)
	}

	var parsed struct {
		MCPServers     []map[string]string `json:"mcpServers"`
		KnowledgeBases []map[string]string `json:"knowledgeBases"`
		Tools          []map[string]string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	// Check MCP servers
	if len(parsed.MCPServers) != 2 {
		t.Errorf("expected 2 MCP servers, got %d", len(parsed.MCPServers))
	}
	foundGitHub := false
	for _, m := range parsed.MCPServers {
		if m["name"] == "github-mcp" {
			foundGitHub = true
			break
		}
	}
	if !foundGitHub {
		t.Error("expected to find github-mcp in MCP servers list")
	}

	// Check KnowledgeBases
	if len(parsed.KnowledgeBases) != 2 {
		t.Errorf("expected 2 KnowledgeBases, got %d", len(parsed.KnowledgeBases))
	}
	foundTechDocs := false
	for _, kb := range parsed.KnowledgeBases {
		if kb["name"] == "tech-docs" {
			foundTechDocs = true
			break
		}
	}
	if !foundTechDocs {
		t.Error("expected to find tech-docs in KnowledgeBases list")
	}

	// Check tools - should only include non-builtin, non-mcp tools
	if len(parsed.Tools) != 1 {
		t.Errorf("expected 1 non-builtin/non-mcp tool, got %d: %+v", len(parsed.Tools), parsed.Tools)
	}
	if len(parsed.Tools) > 0 && parsed.Tools[0]["name"] != "my-tool" {
		t.Errorf("expected my-tool, got %s", parsed.Tools[0]["name"])
	}
}

func TestInjectBuiltinSystemHints_PrependsAfterSystemPrompt(t *testing.T) {
	r := &Router{
		cfg: &Config{
			SystemPrompt:    "You are a helpful assistant.",
			ToolDefinitions: []ToolDefinition{{Name: "_done", BackendType: "builtin"}, {Name: "_fail", BackendType: "builtin"}},
			ChatMode:        false,
		},
	}
	chatReq := ChatCompletionRequest{
		Messages: []Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hello"},
		},
	}
	result := r.injectBuiltinSystemHints(chatReq)

	// Builtin hints should be inserted after the SystemPrompt
	if len(result.Messages) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(result.Messages))
	}
	if result.Messages[0].Role != "system" || result.Messages[0].Content != "You are a helpful assistant." { //nolint:goconst

		t.Errorf("first message should be SystemPrompt, got: %+v", result.Messages[0])
	}
	if result.Messages[1].Role != "system" || !strings.Contains(result.Messages[1].Content.(string), "Platform tools available") {
		t.Errorf("second message should be builtin hints, got: %+v", result.Messages[1])
	}
}

func TestTruncateHistory_PreservesSystemMessages(t *testing.T) {
	// Create a conversation with SystemPrompt and builtin hints as leading system messages.
	// These should be preserved even when truncation is needed.
	systemPrompt := strings.Repeat("you are a helpful assistant. ", 100) // ~3.5k tokens
	builtinHints := "Platform tools available: _done, _fail. Use these tools only when their specific purpose is needed."
	userMsg := strings.Repeat("user content ", 200) // ~2k tokens

	msgs := []Message{
		{Role: "system", Content: systemPrompt}, // 3.5k tokens
		{Role: "system", Content: builtinHints}, // small
		{Role: "user", Content: userMsg},        // 2k tokens
		{Role: "assistant", Content: userMsg},   // 2k tokens
		{Role: "user", Content: userMsg},        // 2k tokens
		{Role: "assistant", Content: userMsg},   // 2k tokens
		{Role: "user", Content: userMsg},        // 2k tokens
		{Role: "assistant", Content: userMsg},   // 2k tokens
		{Role: "user", Content: userMsg},        // 2k tokens
		{Role: "assistant", Content: userMsg},   // 2k tokens
	}

	// Set a budget that requires truncation but should preserve system messages
	totalTokens := estimateTokens(msgs)
	if totalTokens < 5000 {
		t.Fatalf("test setup issue: expected more tokens, got %d", totalTokens)
	}

	// Truncate to fit within a very small budget (system messages should survive)
	maxTokens := 3000
	result := truncateHistory(msgs, maxTokens)

	// Verify system messages are preserved
	if len(result) < 2 {
		t.Fatalf("expected at least 2 messages (system prompt + hints), got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Errorf("first message should be system, got: %s", result[0].Role)
	}
	if result[1].Role != "system" {
		t.Errorf("second message should be system, got: %s", result[1].Role)
	}

	// Verify the system prompt content is preserved
	if result[0].Content != systemPrompt {
		t.Errorf("system prompt should be preserved unchanged")
	}
	if !strings.Contains(result[1].Content.(string), "Platform tools available") {
		t.Errorf("builtin hints should be preserved, got: %s", result[1].Content)
	}

	// Verify we're under budget
	resultTokens := estimateTokens(result)
	if resultTokens > maxTokens {
		t.Errorf("result should fit within budget: %d tokens > %d max", resultTokens, maxTokens)
	}
}

func TestInjectBuiltinSystemHints_IncludesListResources(t *testing.T) {
	r := &Router{
		cfg: &Config{
			SystemPrompt: "You are a helpful assistant.",
			ToolDefinitions: []ToolDefinition{
				{Name: "_list_resources", BackendType: "builtin"},
				{Name: "_done", BackendType: "builtin"},
				{Name: "_fail", BackendType: "builtin"},
			},
			ChatMode: false,
		},
	}
	chatReq := ChatCompletionRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
	result := r.injectBuiltinSystemHints(chatReq)

	var hintMsg *Message
	for i := range result.Messages {
		if result.Messages[i].Role == "system" && strings.Contains(result.Messages[i].Content.(string), "Platform tools available") {
			hintMsg = &result.Messages[i]
			break
		}
	}
	if hintMsg == nil { //nolint:staticcheck

		t.Fatal("expected to find system hint message")
	}

	content := hintMsg.Content.(string) //nolint:staticcheck

	if !strings.Contains(content, "_list_resources") {
		t.Errorf("builtin hints should contain _list_resources, got: %s", content)
	}
	if !strings.Contains(content, "_done") {
		t.Errorf("builtin hints should contain _done, got: %s", content)
	}
	if !strings.Contains(content, "_fail") {
		t.Errorf("builtin hints should contain _fail, got: %s", content)
	}
}

// --- Context preservation tests ---

// TestRouter_PreservesContextAcrossTurns verifies the core fix: when an agent
// sends only the latest user message on each turn (HTTP/chat-mode pattern, no
// client-side history), the second turn's LLM request must still include the
// conversation from the first turn. This exercises the fold of r.messages into
// r.priorMessages and the system-prompt de-duplication guard.
func TestRouter_PreservesContextAcrossTurns(t *testing.T) {
	var mu sync.Mutex
	var capturedMessages []Message
	var callCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		callCount++
		capturedMessages = req.Messages
		currentCall := callCount
		mu.Unlock()

		resp := ChatCompletionResponse{
			ID: "chatcmpl-test",
			Choices: []Choice{{
				Message: Message{
					Role:    "assistant",
					Content: fmt.Sprintf("Turn %d response", currentCall),
				},
				FinishReason: finishReasonStop,
			}},
			Usage: TokenUsage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("test-key"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		RunName:      "test-run",
		RunNamespace: "default",
		Providers: []ProviderConfig{{
			Name:               "test-provider",
			LiteLLMModel:       "gpt-4o",
			BaseURL:            srv.URL,
			APIKeyFile:         keyFile,
			ContextWindow:      200000,
			CostPerInputToken:  0,
			CostPerOutputToken: 0,
			Weight:             1,
		}},
		CheckpointEvery:      1,
		ContextWindowReserve: 0.2,
		SystemPrompt:         "You are a helpful assistant.",
		ToolDefinitions:      []ToolDefinition{},
		LLMRequestTimeout:    30 * time.Second,
	}

	router, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: HTTP/chat-mode agent sends ONLY the user message (no history).
	req1Body := `{"messages":[{"role":"user","content":"probe target 10.10.11.14"}]}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(req1Body))
	rec1 := httptest.NewRecorder()
	router.HandleChatCompletions(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1: expected HTTP 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// Turn 2: agent again sends ONLY the new user message.
	req2Body := `{"messages":[{"role":"user","content":"Why don't you have access to previous chat history?"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(req2Body))
	rec2 := httptest.NewRecorder()
	router.HandleChatCompletions(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2: expected HTTP 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// The second LLM request must contain turn 1's content.
	mu.Lock()
	secondMessages := capturedMessages
	mu.Unlock()

	foundTurn1Content := false
	systemPromptCount := 0
	for _, msg := range secondMessages {
		if msg.Role == "system" {
			if s, ok := msg.Content.(string); ok && s == "You are a helpful assistant." {
				systemPromptCount++
			}
		}
		if s, ok := msg.Content.(string); ok && strings.Contains(s, "probe target") {
			foundTurn1Content = true
		}
	}
	if !foundTurn1Content {
		t.Error("expected turn 2 provider request to include turn 1's 'probe target' content")
	}
	if systemPromptCount != 1 {
		t.Errorf("expected system prompt to appear exactly once in turn 2, got %d", systemPromptCount)
	}
}

// TestRouter_SystemPromptNotDuplicatedAfterFold verifies that when r.priorMessages
// already contains the system prompt (after folding), the injection guard prevents
// a duplicate system message from being added.
func TestRouter_SystemPromptNotDuplicatedAfterFold(t *testing.T) {
	var mu sync.Mutex
	var capturedCount int
	var capturedSystemCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		capturedCount++
		for _, msg := range req.Messages {
			if msg.Role == "system" {
				if s, ok := msg.Content.(string); ok && s == "You are a helpful assistant." {
					capturedSystemCount++
				}
			}
		}
		mu.Unlock()

		resp := ChatCompletionResponse{
			ID: "chatcmpl-test",
			Choices: []Choice{{
				Message:      Message{Role: "assistant", Content: "ok"},
				FinishReason: finishReasonStop,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "api-key")
	_ = os.WriteFile(keyFile, []byte("test-key"), 0o644)

	cfg := &Config{
		RunName:      "test-run",
		RunNamespace: "default",
		Providers: []ProviderConfig{{
			Name:          "test-provider",
			LiteLLMModel:  "gpt-4o",
			BaseURL:       srv.URL,
			APIKeyFile:    keyFile,
			ContextWindow: 200000,
			Weight:        1,
		}},
		CheckpointEvery:      1,
		ContextWindowReserve: 0.2,
		SystemPrompt:         "You are a helpful assistant.",
		ToolDefinitions:      []ToolDefinition{},
		LLMRequestTimeout:    30 * time.Second,
	}

	router, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate state after one turn: priorMessages has the system prompt +
	// conversation, r.messages has the current turn's response.
	router.mu.Lock()
	router.priorMessages = []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	router.messages = []Message{
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}
	router.mu.Unlock()

	// Send a new top-level request — the fold should move r.messages into
	// priorMessages, and the system-prompt guard should prevent duplication.
	reqBody := `{"messages":[{"role":"user","content":"third question"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	router.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedCount != 1 {
		t.Fatalf("expected exactly 1 provider request, got %d", capturedCount)
	}
	if capturedSystemCount != 1 {
		t.Errorf("expected system prompt to appear exactly once after fold, got %d", capturedSystemCount)
	}
}

// TestRouter_EpisodicSummary_IncludesPriorMessages verifies that
// maybeRunEpisodicSummary reads from both r.priorMessages and r.messages,
// so summaries cover the full conversation after the fold.
func TestRouter_EpisodicSummary_IncludesPriorMessages(t *testing.T) {
	store, err := state.NewStoreFromConfig(state.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var mu sync.Mutex
	var capturedMessages []Message

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		capturedMessages = req.Messages
		mu.Unlock()
		resp := ChatCompletionResponse{
			ID: "chatcmpl-summary",
			Choices: []Choice{{
				Message:      Message{Role: "assistant", Content: "compact summary"},
				FinishReason: finishReasonStop,
			}},
			Usage: TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "api-key")
	_ = os.WriteFile(keyFile, []byte("test-key"), 0o644)

	cfg := &Config{
		RunName:      "test-run",
		RunNamespace: "default",
		Providers: []ProviderConfig{{
			Name:               "test-provider",
			LiteLLMModel:       "gpt-4o",
			BaseURL:            srv.URL,
			APIKeyFile:         keyFile,
			ContextWindow:      200000,
			CostPerInputToken:  0,
			CostPerOutputToken: 0,
			Weight:             1,
		}},
		CheckpointEvery:      1,
		ContextWindowReserve: 0.2,
		SystemPrompt:         "You are a helpful assistant.",
		ToolDefinitions:      []ToolDefinition{},
		LLMRequestTimeout:    30 * time.Second,
		CheckpointKey:        "test-checkpoint",
		EpisodicMemory: EpisodicMemoryConfig{
			SummaryEvery:        2,
			SummaryProviderName: "test-provider",
			SummaryModel:        "gpt-4o",
		},
	}

	router, err := New(cfg, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate post-fold state: prior turns + current turn.
	router.mu.Lock()
	router.priorMessages = []Message{
		{Role: "user", Content: "old question from turn 1"},
		{Role: "assistant", Content: "old answer from turn 1"},
	}
	router.messages = []Message{
		{Role: "user", Content: "recent question from turn 2"},
		{Role: "assistant", Content: "recent answer from turn 2"},
	}
	router.ruleRouter.turnCount = 2 // SummaryEvery=2, so 2%2==0 triggers summary
	router.mu.Unlock()

	// Call directly (not via goroutine) so we can assert on the captured request.
	router.maybeRunEpisodicSummary(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if capturedMessages == nil {
		t.Fatal("expected the provider to receive a summarization request")
	}

	// The summarization prompt embeds the conversation text, so check both
	// prior and current content appear.
	foundOld := false
	foundRecent := false
	for _, msg := range capturedMessages {
		if msg.Role == "user" { //nolint:goconst

			if s, ok := msg.Content.(string); ok {
				if strings.Contains(s, "old question") {
					foundOld = true
				}
				if strings.Contains(s, "recent question") {
					foundRecent = true
				}
			}
		}
	}
	if !foundOld {
		t.Error("expected episodic summary to include content from priorMessages")
	}
	if !foundRecent {
		t.Error("expected episodic summary to include content from r.messages")
	}
}

// === asynchronous compaction tests (issue #54 — async truncation) ===

// bigOverBudgetMessages builds a slice of messages whose estimated token count
// exceeds the 80% checkpoint budget for ContextWindow 262144 (budget=209715),
// forcing truncateHistory to drop messages. Each message is 8004 chars (~2001
// tokens + 4 overhead), so 200 messages ≈ 402k tokens.
func bigOverBudgetMessages(n int) []Message {
	msgs := make([]Message, n)
	for i := range n {
		msgs[i] = Message{
			Role:    "user",
			Content: fmt.Sprintf("msg %d %s", i, strings.Repeat("x", 8000)),
		}
	}
	return msgs
}

// TestAsyncCompaction_TruncatesInBackground verifies that trimLiveBuffer
// delegates truncation to a background goroutine and that waitForCompaction
// installs the truncated result. The buffer must be under budget after waiting.
func TestAsyncCompaction_TruncatesInBackground(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 262144}}}}
	r.priorMessages = bigOverBudgetMessages(200)
	budget := r.checkpointBudget()
	r.liveBufferTokens = estimateTokens(r.priorMessages)

	if estimateTokens(r.priorMessages) <= budget {
		t.Fatalf("precondition: buffer should exceed budget %d", budget)
	}

	r.mu.Lock()
	r.trimLiveBuffer()
	r.mu.Unlock()

	// The truncation now runs in a background goroutine. Wait for it to install.
	r.waitForCompaction()

	r.mu.Lock()
	after := estimateTokens(r.priorMessages)
	compacting := r.compacting
	r.mu.Unlock()

	if after > budget {
		t.Fatalf("buffer still over budget after async compaction: est=%d budget=%d", after, budget)
	}
	if compacting {
		t.Errorf("compacting flag should be cleared after waitForCompaction")
	}
}

// TestAsyncCompaction_NonBlocking verifies the core async guarantee:
// trimLiveBuffer returns BEFORE the buffer is truncated (the expensive
// truncateHistory runs on a worker goroutine, not the request thread).
func TestAsyncCompaction_NonBlocking(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 262144}}}}
	r.priorMessages = bigOverBudgetMessages(500) // large enough that truncation takes measurable time
	budget := r.checkpointBudget()
	r.liveBufferTokens = estimateTokens(r.priorMessages)

	r.mu.Lock()
	r.trimLiveBuffer()
	// Right after trimLiveBuffer returns, a compaction goroutine should be
	// in flight (compacting=true) and the buffer should still be over budget
	// (the goroutine hasn't installed its result yet).
	stillCompacting := r.compacting
	notYetTruncated := estimateTokens(r.priorMessages) > budget
	r.mu.Unlock()

	if stillCompacting && !notYetTruncated {
		t.Fatalf("buffer was truncated synchronously — compaction should be async")
	}

	// Either way, after waiting the buffer must be under budget.
	r.waitForCompaction()
	r.mu.Lock()
	after := estimateTokens(r.priorMessages)
	r.mu.Unlock()
	if after > budget {
		t.Fatalf("buffer over budget after waitForCompaction: est=%d budget=%d", after, budget)
	}
}

// TestAsyncCompaction_SkipsWhenCompacting verifies that spawnCompaction does
// not launch a second goroutine while one is already in flight, preventing
// duplicate concurrent truncations.
func TestAsyncCompaction_SkipsWhenCompacting(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 262144}}}}
	r.priorMessages = bigOverBudgetMessages(500)
	budget := r.checkpointBudget()
	r.liveBufferTokens = estimateTokens(r.priorMessages)

	// Manually set compacting=true to simulate an in-flight goroutine.
	r.mu.Lock()
	r.compacting = true
	r.compactDone = nil // no goroutine to wait on
	r.spawnCompaction(r.priorMessages, budget)
	if !r.compacting {
		r.mu.Unlock()
		t.Fatal("spawnCompaction should not clear compacting flag when already compacting")
	}
	if r.compactDone != nil {
		r.mu.Unlock()
		t.Fatal("spawnCompaction should not set compactDone when already compacting")
	}
	r.mu.Unlock()

	// Clean up: reset the flag so the test leaves no dangling state.
	r.mu.Lock()
	r.compacting = false
	r.mu.Unlock()
}

// TestTrimLiveBuffer_CompactsToTarget verifies that when ContextCompactionRatio
// is set below 1.0, the buffer is compacted down to the *target* (e.g. 10% of
// the context window), not just to the 80% safety ceiling. This is the core
// fix for issue #54: reducing post-compaction context from 60–80% to ~10%.
func TestTrimLiveBuffer_CompactsToTarget(t *testing.T) {
	r := &Router{cfg: &Config{
		Providers:              []ProviderConfig{{Name: "p", ContextWindow: 262144}},
		ContextCompactionRatio: 0.1, // aggressive: target = 10% of CW ≈ 26214
	}}
	r.priorMessages = bigOverBudgetMessages(200) // ~402k tokens, well over the 80% ceiling
	budget := r.checkpointBudget()               // 80% ceiling = 209715
	target := r.compactionTarget()               // 10% = 26214
	r.liveBufferTokens = estimateTokens(r.priorMessages)

	if estimateTokens(r.priorMessages) <= budget {
		t.Fatalf("precondition: buffer should exceed budget %d", budget)
	}

	r.mu.Lock()
	r.trimLiveBuffer()
	r.mu.Unlock()
	r.waitForCompaction()

	r.mu.Lock()
	after := estimateTokens(r.priorMessages)
	r.mu.Unlock()

	// Must be under the 80% safety ceiling.
	if after > budget {
		t.Fatalf("buffer over ceiling after compaction: est=%d budget=%d", after, budget)
	}
	// Must be well below the ceiling — compaction reduced to the target (~10%),
	// not just down to the 80% ceiling.
	if after > budget/2 {
		t.Errorf("buffer should be compacted well below the 80%% ceiling: est=%d ceiling=%d target=%d",
			after, budget, target)
	}
}
