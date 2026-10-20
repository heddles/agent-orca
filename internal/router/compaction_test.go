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
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testRoleAssistant = "assistant"

// sequenceProvider serves canned SSE bodies in order and records every request
// it receives.
type sequenceProvider struct {
	mu     sync.Mutex
	bodies [][]byte
	reqs   []ChatCompletionRequest
}

func newSequenceProvider(t *testing.T, bodies ...[]byte) (*httptest.Server, *sequenceProvider) {
	t.Helper()
	p := &sequenceProvider{bodies: bodies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req ChatCompletionRequest
		_ = json.Unmarshal(raw, &req)
		p.mu.Lock()
		idx := len(p.reqs)
		p.reqs = append(p.reqs, req)
		p.mu.Unlock()
		if idx >= len(p.bodies) {
			http.Error(w, "no more canned responses", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(p.bodies[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

func (p *sequenceProvider) requests() []ChatCompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ChatCompletionRequest(nil), p.reqs...)
}

// userTurn is a top-level request carrying only the new user message, the way
// the reference agent (pkg/python/openai-reference) calls the router.
func userTurn(t *testing.T, text string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": text}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

func storedConversation(r *Router) []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(append([]Message(nil), r.priorMessages...), r.messages...)
}

func roles(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role[:1])
	}
	return b.String()
}

func countContent(msgs []Message, text string) int {
	n := 0
	for _, m := range msgs {
		if messageText(m) == text {
			n++
		}
	}
	return n
}

func truncationEvents(store *testStore) int {
	n := 0
	for _, ev := range store.traceEvents {
		if strings.Contains(ev, `"type":"truncation"`) {
			n++
		}
	}
	return n
}

// Regression for #47: the sent request (stored history + injected system
// prompt + new input) was stored as the turn, so the history doubled on every
// turn: 3, 8, 18, 38 messages after four one-line turns.
func TestHistoryIsNotDuplicatedAcrossTurns(t *testing.T) {
	const turns = 4
	bodies := make([][]byte, turns)
	for i := range bodies {
		bodies[i] = textTurnSSE("stop")
	}
	srv, provider := newSequenceProvider(t, bodies...)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), newTestStore(t))
	r.cfg.SystemPrompt = "You are a careful engineer."

	for i := 1; i <= turns; i++ {
		consumeClientBody(t, r, userTurn(t, fmt.Sprintf("question %d", i)))
	}

	stored := storedConversation(r)
	if got, want := roles(stored), "uauauaua"; got != want {
		t.Fatalf("stored conversation roles = %q, want %q (one user + one assistant message per turn, no injected context)", got, want)
	}
	reqs := provider.requests()
	last := reqs[len(reqs)-1].Messages
	if got, want := roles(last), "suauauau"; got != want {
		t.Fatalf("last request roles = %q, want %q", got, want)
	}
	if n := countContent(last, r.cfg.SystemPrompt); n != 1 {
		t.Fatalf("system prompt sent %d times, want 1", n)
	}
	for i := 1; i <= turns; i++ {
		if n := countContent(last, fmt.Sprintf("question %d", i)); n != 1 {
			t.Fatalf("question %d sent %d times in the last request, want 1", i, n)
		}
	}
}

// A tool-call continuation is rebuilt from the stored conversation: it must
// carry the history and the turn exactly once, and the turn is stored once.
func TestToolLoopContinuationCarriesHistoryOnce(t *testing.T) {
	srv, provider := newSequenceProvider(t, textTurnSSE("stop"), toolCallTurnSSE(), textTurnSSE("stop"))
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), newTestStore(t))
	r.cfg.SystemPrompt = "You are a careful engineer."

	consumeClientBody(t, r, userTurn(t, "question 1"))
	consumeClientBody(t, r, userTurn(t, "question 2"))

	reqs := provider.requests()
	if len(reqs) != 3 {
		t.Fatalf("provider received %d requests, want 3", len(reqs))
	}
	cont := reqs[2].Messages
	if got, want := roles(cont), "suauat"; got != want {
		t.Fatalf("continuation roles = %q, want %q", got, want)
	}
	for _, text := range []string{r.cfg.SystemPrompt, "question 1", "question 2"} {
		if n := countContent(cont, text); n != 1 {
			t.Fatalf("%q sent %d times in the continuation, want 1", text, n)
		}
	}
	if got, want := roles(storedConversation(r)), "uauata"; got != want {
		t.Fatalf("stored conversation roles = %q, want %q", got, want)
	}
}

// bigMessages returns n alternating user/assistant messages of ~tokensEach
// tokens (over maxExactTokenLen, so estimated at 4 chars per token).
func bigMessages(n, tokensEach int) []Message {
	msgs := make([]Message, n)
	for i := range msgs {
		role := testRoleUser
		if i%2 == 1 {
			role = testRoleAssistant
		}
		msgs[i] = Message{Role: role, Content: fmt.Sprintf("m%03d ", i) + strings.Repeat("x", tokensEach*4)}
	}
	return msgs
}

// Regression for #47: the pre-send step cut the outgoing request but never the
// stored conversation, so every turn re-truncated the same full history. The
// stored conversation is now compacted once and later turns build on it.
func TestStoredConversationCompactedOnceAcrossTurns(t *testing.T) {
	srv, provider := newSequenceProvider(t, textTurnSSE("stop"), textTurnSSE("stop"), textTurnSSE("stop"))
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), store)
	// Window 100k: trigger 80k (80%), target 50k (ratio 0.5). 20 × 5k = 100k.
	r.mu.Lock()
	r.priorMessages = bigMessages(20, 5000)
	r.mu.Unlock()

	consumeClientBody(t, r, userTurn(t, "next"))
	if n := truncationEvents(store); n != 1 {
		t.Fatalf("turn 1: %d truncation events, want 1", n)
	}
	if got := estimateTokens(storedConversation(r)); got > 50_000 {
		t.Fatalf("stored conversation is %d tokens after compaction, want <= the 50k target", got)
	}
	if got := estimateTokens(provider.requests()[0].Messages); got > 50_000 {
		t.Fatalf("first request carried %d tokens, want <= the 50k target", got)
	}

	consumeClientBody(t, r, userTurn(t, "again"))
	consumeClientBody(t, r, userTurn(t, "and again"))
	if n := truncationEvents(store); n != 1 {
		t.Fatalf("%d truncation events after three turns, want 1: later turns must reuse the compacted history", n)
	}
	reqs := provider.requests()
	if n := countContent(reqs[2].Messages, "next"); n != 1 {
		t.Fatalf("turn 1 input sent %d times on turn 3, want 1", n)
	}
}

// The stored conversation is compacted against the selected provider's
// budget, which leaves room for the tool definitions.
func TestContextBudgetFor(t *testing.T) {
	r := &Router{cfg: &Config{ContextWindowReserve: 0.2, ContextCompactionRatio: 0.3}}
	cases := []struct {
		name            string
		window, maxOut  int
		trigger, target int
	}{
		// 80% of the window, then the ratio.
		{"1M window", 1_000_000, 32_768, 800_000, 300_000},
		// The completion reserve binds: 128k − 32k < 80% of 128k.
		{"128k window", 128_000, 32_768, 95_232, 38_400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := r.contextBudgetFor(&ProviderConfig{ContextWindow: c.window, MaxOutputTokens: c.maxOut}, nil)
			if b.trigger != c.trigger || b.target != c.target {
				t.Fatalf("trigger/target = %d/%d, want %d/%d", b.trigger, b.target, c.trigger, c.target)
			}
		})
	}
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "t", "description": strings.Repeat("d", 4000)}}}
	b := r.contextBudgetFor(&ProviderConfig{ContextWindow: 1_000_000, MaxOutputTokens: 32_768}, tools)
	if toolTokens := estimateToolTokens(tools); b.trigger != 800_000-toolTokens || b.target != 300_000-toolTokens {
		t.Fatalf("trigger/target = %d/%d, want both reduced by the %d tool tokens", b.trigger, b.target, toolTokens)
	}
}

// Checkpoints saved before #47 hold a copy of the injected context for every
// turn; loading one must drop those copies and keep the conversation.
func TestStripInjectedContext(t *testing.T) {
	cfg := &Config{
		SystemPrompt:    "You are a careful engineer.",
		ToolDefinitions: []ToolDefinition{{Name: "_clarify", BackendType: backendTypeBuiltin}},
	}
	hint := builtinHintContent(cfg)
	if hint == "" {
		t.Fatal("expected a built-in hint for an agent with a built-in tool")
	}
	summary := Message{Role: "system", Content: "[Compacted: 3msg/~1k] Intent: x"}
	msgs := []Message{
		{Role: "system", Content: hindsightRecallOpen + "recalled\n</hindsight-recall>"},
		{Role: "system", Content: ltmContextOpen + "\nmemory\n</rag-context>"},
		{Role: "system", Content: episodicMemoryOpen + "\nChunk 1: s\n"},
		{Role: "system", Content: cfg.SystemPrompt},
		{Role: "system", Content: hint},
		summary,
		{Role: "user", Content: "question 1"},
		{Role: "assistant", Content: "answer 1"},
		{Role: "system", Content: cfg.SystemPrompt},
		{Role: "user", Content: "question 2"},
	}
	got := stripInjectedContext(msgs, cfg)
	if r := roles(got); r != "suau" {
		t.Fatalf("roles after strip = %q, want %q", r, "suau")
	}
	if messageText(got[0]) != messageText(summary) {
		t.Fatalf("compaction summary must be kept, got %q", messageText(got[0]))
	}
}

func TestTokenBreakdown(t *testing.T) {
	msgs := make([]Message, 0, 25)
	msgs = append(msgs, Message{Role: "user", Content: "hi"})
	for i := range 12 {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Function: FunctionCall{Name: fmt.Sprintf("tool_%02d", i)}}}},
			Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", (i+1)*100)},
		)
	}
	b := tokenBreakdown(msgs)
	byRole := b["byRole"].(map[string]int)
	if byRole["user"] == 0 || byRole["assistant"] == 0 || byRole["tool"] == 0 {
		t.Fatalf("byRole = %v, want user, assistant and tool totals", byRole)
	}
	top := b["topTools"].(map[string]int)
	if len(top) != breakdownTopTools {
		t.Fatalf("topTools has %d entries, want %d", len(top), breakdownTopTools)
	}
	if _, ok := top["tool_00"]; ok {
		t.Fatalf("smallest tool must not make the top %d: %v", breakdownTopTools, top)
	}
	if _, ok := top["tool_11"]; !ok {
		t.Fatalf("largest tool missing from topTools: %v", top)
	}
}

// The compaction header reported "~0k" for every compaction: droppedTokens
// was never computed.
func TestCompactDroppedMessagesReportsDroppedTokens(t *testing.T) {
	summary := messageText(compactDroppedMessages(bigMessages(2, 5000)))
	if !strings.HasPrefix(summary, "[Compacted: 2msg/~10k]") {
		t.Fatalf("summary header = %q, want it to report ~10k dropped tokens", summary)
	}
}

// BenchmarkTruncateHistory cuts a ~700K-token conversation of realistically
// sized messages (exact BPE counting) down to 300K. Re-estimating the kept
// tail for every candidate cut made this take ~10s (#47).
func BenchmarkTruncateHistory(b *testing.B) {
	msgs := benchConversation()
	for b.Loop() {
		truncateHistory(msgs, 300_000)
	}
}

// BenchmarkEstimateTokens counts the same ~700K-token conversation that every
// request of a long run re-counts (budget check, pre-send fit, checkpoint).
func BenchmarkEstimateTokens(b *testing.B) {
	msgs := benchConversation()
	for b.Loop() {
		estimateTokens(msgs)
	}
}

// benchConversation is ~700K tokens over 2,000 realistically sized messages.
func benchConversation() []Message {
	rng := rand.New(rand.NewPCG(1, 2))
	msgs := make([]Message, 2000)
	for i := range msgs {
		role := testRoleUser
		if i%2 == 1 {
			role = testRoleAssistant
		}
		// Varied words: highly repetitive input is BPE's slow path, not the
		// shape of real conversation text.
		var text strings.Builder
		for range 90 {
			fmt.Fprintf(&text, "w%x ", rng.IntN(1<<20))
		}
		msgs[i] = Message{Role: role, Content: text.String()}
	}
	return msgs
}
