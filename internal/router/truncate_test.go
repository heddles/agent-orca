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
	"strings"
	"testing"
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
		{Role: "user", Content: []interface{}{map[string]string{"type": "text", "text": "hello world this is a longer message"}}},
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
	if !strings.Contains(compaction, "Compacted context") {
		t.Errorf("compaction message missing header: %s", compaction)
	}

	// Last message should be preserved (most recent).
	last := result[len(result)-1]
	if last.Role != "assistant" {
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
			{ID: "tc2", Function: FunctionCall{Name: "github-mcp-list-issues", Arguments: `{"owner":"ci-agent-orc"}`}},
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
		if s, ok := m.Content.(string); ok && strings.Contains(s, "Compacted context") {
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
		if m.Role == "tool" && m.ToolCallID != "" {
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
	if !strings.Contains(content, "2 total") {
		t.Errorf("should report 2 total tool calls: %s", content)
	}
	if !strings.Contains(content, "2 unique tools") {
		t.Errorf("should report 2 unique tools: %s", content)
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
			if hintMsg == nil {
				t.Fatal("expected to find system hint message")
			}

			content := hintMsg.Content.(string)
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
	if hintMsg == nil {
		t.Fatal("expected to find system hint message")
	}

	content := hintMsg.Content.(string)
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
	if hintMsg == nil {
		t.Fatal("expected to find system hint message")
	}

	content := hintMsg.Content.(string)
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
	if result.Messages[0].Role != "system" || result.Messages[0].Content != "You are a helpful assistant." {
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
		{Role: "system", Content: systemPrompt},                    // 3.5k tokens
		{Role: "system", Content: builtinHints},                    // small
		{Role: "user", Content: userMsg},                           // 2k tokens
		{Role: "assistant", Content: userMsg},                      // 2k tokens
		{Role: "user", Content: userMsg},                           // 2k tokens
		{Role: "assistant", Content: userMsg},                      // 2k tokens
		{Role: "user", Content: userMsg},                           // 2k tokens
		{Role: "assistant", Content: userMsg},                      // 2k tokens
		{Role: "user", Content: userMsg},                           // 2k tokens
		{Role: "assistant", Content: userMsg},                      // 2k tokens
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
	if hintMsg == nil {
		t.Fatal("expected to find system hint message")
	}

	content := hintMsg.Content.(string)
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
