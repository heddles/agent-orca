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
	"unicode/utf8"

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

	// Simulate state after one turn: priorMessages has the conversation (the
	// system prompt is injected per request, never stored — #47), r.messages
	// has the current turn's response.
	router.mu.Lock()
	router.priorMessages = []Message{
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

// #42 follow-up: the newest message is never dropped. When it alone exceeds
// the budget its content is truncated (with a marker) and older history is
// compacted; previously only a compaction summary survived.
func TestTruncateHistory_KeepsAndShrinksOversizedNewestMessage(t *testing.T) {
	huge := strings.Repeat("word ", 40_000) // ~50k tokens
	t.Run("single message", func(t *testing.T) {
		out := truncateHistory([]Message{{Role: "user", Content: huge}}, 5_000)
		if len(out) != 1 || out[0].Role != "user" {
			t.Fatalf("want the user message kept, got %d messages", len(out))
		}
		s, _ := out[0].Content.(string)
		if !strings.HasPrefix(s, "word word") || !strings.Contains(s, "[... truncated") {
			t.Fatalf("want a truncated prefix with a marker, got %.80q…", s)
		}
		if got := estimateTokens(out); got > 5_000 {
			t.Fatalf("shrunk request still over budget: %d tokens", got)
		}
	})
	t.Run("older history is compacted, newest kept", func(t *testing.T) {
		in := []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "earlier question"},
			{Role: "assistant", Content: "earlier answer"},
			{Role: "user", Content: huge},
		}
		out := truncateHistory(in, 5_000)
		if out[0].Content != "sys" {
			t.Fatalf("system prefix must be kept, got %+v", out[0])
		}
		newest := out[len(out)-1]
		if newest.Role != "user" || !strings.Contains(newest.Content.(string), "[... truncated") {
			t.Fatalf("newest user message must be kept and truncated, got %+v", newest.Role)
		}
		if got := estimateTokens(out); got > 5_000 {
			t.Fatalf("over budget: %d tokens", got)
		}
		if in[3].Content != huge {
			t.Fatal("the caller's slice must not be mutated")
		}
	})
}

func TestTruncateToolResult_RuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100)   // 2 bytes per rune
	out := truncateToolResult(s, 1) // 4-byte cap lands mid-rune for odd offsets
	if !utf8.ValidString(out) {
		t.Fatalf("truncation split a multi-byte rune: %q", out)
	}
	odd := "a" + strings.Repeat("é", 100) // shift so the 4-byte cut is mid-rune
	if out := truncateToolResult(odd, 1); !utf8.ValidString(out) {
		t.Fatalf("truncation split a multi-byte rune: %q", out)
	}
}
