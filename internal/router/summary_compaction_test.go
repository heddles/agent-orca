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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// validSummary has every required section.
func validSummary(note string) string {
	var b strings.Builder
	for _, s := range summarySections {
		fmt.Fprintf(&b, "%s\n%s\n\n", s, note)
	}
	return b.String()
}

// summaryProvider answers streaming requests with a text turn and
// non-streaming requests (compaction summaries) with the next queued summary
// text, recording every request.
type summaryProvider struct {
	mu        sync.Mutex
	streams   [][]byte // streamed bodies to serve first; then text turns
	summaries []string
	streamed  []ChatCompletionRequest
	summary   []ChatCompletionRequest
}

func newSummaryProvider(t *testing.T, summaries ...string) (*httptest.Server, *summaryProvider) {
	t.Helper()
	p := &summaryProvider{summaries: summaries}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req ChatCompletionRequest
		_ = json.Unmarshal(raw, &req)
		p.mu.Lock()
		defer p.mu.Unlock()
		if req.Stream {
			p.streamed = append(p.streamed, req)
			w.Header().Set("Content-Type", "text/event-stream")
			body := textTurnSSE("stop")
			if len(p.streams) > 0 {
				body, p.streams = p.streams[0], p.streams[1:]
			}
			_, _ = w.Write(body)
			return
		}
		p.summary = append(p.summary, req)
		text := "no summary queued"
		if len(p.summaries) > 0 {
			text, p.summaries = p.summaries[0], p.summaries[1:]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatCompletionResponse{
			Choices: []Choice{{Message: Message{Role: "assistant", Content: text}, FinishReason: finishReasonStop}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

func (p *summaryProvider) counts() (streamed, summary int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.streamed), len(p.summary)
}

func (p *summaryProvider) lastStreamed() ChatCompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamed[len(p.streamed)-1]
}

func (p *summaryProvider) summaryRequest(i int) ChatCompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.summary[i]
}

func eventsOfType(store *testStore, typ string) []map[string]any {
	var out []map[string]any
	for _, raw := range store.traceEvents {
		var ev map[string]any
		if json.Unmarshal([]byte(raw), &ev) == nil && ev["type"] == typ {
			out = append(out, ev)
		}
	}
	return out
}

const originalTask = "Original task: build the widget with a blue frame."

// summaryTestRouter returns a router (100k window: trigger 80k) whose stored
// conversation is the original task plus 100k tokens of history.
func summaryTestRouter(t *testing.T, summaries ...string) (*Router, *summaryProvider, *testStore) {
	t.Helper()
	srv, provider := newSummaryProvider(t, summaries...)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), store)
	r.mu.Lock()
	r.priorMessages = append([]Message{
		{Role: testRoleUser, Content: originalTask},
		{Role: testRoleAssistant, Content: "Understood."},
	}, bigMessages(20, 5000)...)
	r.mu.Unlock()
	return r, provider, store
}

// Summary mode (#47): at the threshold the model replaces the stored history
// with a structured summary; the original task stays verbatim; the cut is
// made once and the next turn builds on it.
func TestSummaryCompactionReplacesHistoryOnce(t *testing.T) {
	r, provider, store := summaryTestRouter(t, validSummary("widget progress"))

	consumeClientBody(t, r, userTurn(t, "next"))

	if _, n := provider.counts(); n != 1 {
		t.Fatalf("summary requests = %d, want 1", n)
	}
	sreq := provider.summaryRequest(0)
	if sreq.MaxTokens != summaryMaxTokens {
		t.Fatalf("summary max_tokens = %d, want %d", sreq.MaxTokens, summaryMaxTokens)
	}
	instr := messageText(sreq.Messages[len(sreq.Messages)-1])
	if !strings.Contains(instr, "COMPACTION REQUEST") || !strings.Contains(instr, summarySections[0]) {
		t.Fatalf("last summary-request message is not the instruction: %.200q", instr)
	}

	sent := provider.lastStreamed().Messages
	if got, want := roles(sent), "ssu"; got != want {
		t.Fatalf("request after compaction roles = %q, want %q (pinned task, summary, new input)", got, want)
	}
	if got := messageText(sent[0]); got != pinnedTaskOpen+originalTask {
		t.Fatalf("pinned task = %q", got)
	}
	if got := messageText(sent[1]); !strings.HasPrefix(got, summaryOpenPrefix+"1 — replaces 22 earlier messages]") ||
		!strings.Contains(got, "widget progress") {
		t.Fatalf("summary message = %.200q", got)
	}

	if n := len(eventsOfType(store, "compaction.started")); n != 1 {
		t.Fatalf("compaction.started events = %d, want 1", n)
	}
	done := eventsOfType(store, "truncation")
	if len(done) != 1 || done[0]["mode"] != compactionModeSummary {
		t.Fatalf("truncation events = %v, want one with mode summary", done)
	}
	if len(store.savedKeys) == 0 || !strings.HasPrefix(store.savedKeys[0], "agentorca/runs/test-run/archive/") {
		t.Fatalf("pre-compaction archive not saved first; saved keys = %v", store.savedKeys)
	}

	consumeClientBody(t, r, userTurn(t, "again"))
	if _, n := provider.counts(); n != 1 {
		t.Fatalf("summary requests after a second turn = %d, want still 1", n)
	}
	if got, want := roles(storedConversation(r)), "ssuaua"; got != want {
		t.Fatalf("stored conversation roles = %q, want %q", got, want)
	}
}

// A second compaction folds the first summary into the new one (it is part of
// what gets summarized) and keeps a single pinned task.
func TestSummaryCompactionIsRolling(t *testing.T) {
	r, provider, _ := summaryTestRouter(t, validSummary("first"), validSummary("second"))
	consumeClientBody(t, r, userTurn(t, "next"))

	r.mu.Lock()
	r.priorMessages = append(r.priorMessages, bigMessages(20, 5000)...)
	r.mu.Unlock()
	consumeClientBody(t, r, userTurn(t, "more"))

	if _, n := provider.counts(); n != 2 {
		t.Fatalf("summary requests = %d, want 2", n)
	}
	second := provider.summaryRequest(1).Messages
	if countPrefix(second, summaryOpenPrefix+"1 ") != 1 {
		t.Fatalf("second summary request does not carry summary #1")
	}
	if countPrefix(second, pinnedTaskOpen) != 0 {
		t.Fatalf("the pinned task is kept aside, not summarized")
	}
	stored := storedConversation(r)
	if countPrefix(stored, pinnedTaskOpen) != 1 || countPrefix(stored, summaryOpenPrefix+"2 ") != 1 ||
		countPrefix(stored, summaryOpenPrefix+"1 ") != 0 {
		t.Fatalf("stored conversation after the second compaction: roles %q", roles(stored))
	}
}

func countPrefix(msgs []Message, prefix string) int {
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(messageText(m), prefix) {
			n++
		}
	}
	return n
}

// An invalid summary is retried once; a valid retry is used.
func TestSummaryCompactionRetriesOnce(t *testing.T) {
	r, provider, store := summaryTestRouter(t, "just some prose", validSummary("retry"))
	consumeClientBody(t, r, userTurn(t, "next"))
	if _, n := provider.counts(); n != 2 {
		t.Fatalf("summary requests = %d, want 2 (one retry)", n)
	}
	if done := eventsOfType(store, "truncation"); len(done) != 1 || done[0]["mode"] != compactionModeSummary {
		t.Fatalf("truncation events = %v, want one with mode summary", done)
	}
}

// When both attempts fail, the oldest messages are dropped instead and the
// event says so; the turn still goes through.
func TestSummaryCompactionFallsBackToTruncation(t *testing.T) {
	r, provider, store := summaryTestRouter(t, "bad", "still bad")
	consumeClientBody(t, r, userTurn(t, "next"))

	if _, n := provider.counts(); n != 2 {
		t.Fatalf("summary requests = %d, want 2", n)
	}
	done := eventsOfType(store, "truncation")
	if len(done) != 1 || done[0]["mode"] != compactionDegraded || !strings.Contains(fmt.Sprint(done[0]["reason"]), "missing sections") {
		t.Fatalf("truncation events = %v, want one degraded with the reason", done)
	}
	if streamed, _ := provider.counts(); streamed != 1 {
		t.Fatalf("the turn must still be answered; streamed requests = %d", streamed)
	}
	if got := estimateTokens(storedConversation(r)); got > 50_000 {
		t.Fatalf("stored conversation is %d tokens after the fallback, want <= the 50k target", got)
	}
}

func TestMissingSummarySections(t *testing.T) {
	if m := missingSummarySections(validSummary("x")); len(m) != 0 {
		t.Fatalf("valid summary reported missing %v", m)
	}
	// Small wording drift in the parenthesised part is accepted.
	drift := strings.ReplaceAll(validSummary("x"), "(don't retry)", "(do not retry)")
	if m := missingSummarySections(drift); len(m) != 0 {
		t.Fatalf("drifted summary reported missing %v", m)
	}
	if m := missingSummarySections("## Goal & original constraints\nx"); len(m) != len(summarySections)-1 {
		t.Fatalf("missing = %v", m)
	}
}

// Mid tool loop, the latest user message and the last tailToolGroups tool-call
// groups stay verbatim; older groups of the turn are summarized.
func TestSplitTurnTail(t *testing.T) {
	group := func(i, tokens int) []Message {
		id := fmt.Sprintf("call_%d", i)
		return []Message{
			{Role: testRoleAssistant, ToolCalls: []ToolCall{{ID: id, Function: FunctionCall{Name: "read"}}}},
			{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", tokens*4)},
		}
	}
	msgs := []Message{{Role: testRoleUser, Content: "old question"}, {Role: testRoleAssistant, Content: "old answer"},
		{Role: testRoleUser, Content: "current question"}}
	for i := 1; i <= 4; i++ {
		msgs = append(msgs, group(i, 5000)...) // ~5k tokens per group
	}

	rest, tail := splitTurnTail(msgs, 20_000)
	if got, want := roles(rest), "uaat"; got != want {
		t.Fatalf("rest roles = %q, want %q (old turn + oldest group)", got, want)
	}
	if got, want := roles(tail), "uatatat"; got != want || messageText(tail[0]) != "current question" {
		t.Fatalf("tail roles = %q, want %q starting with the current question", got, want)
	}

	// Groups over the budget are dropped oldest-first.
	_, tail = splitTurnTail(msgs, 11_000)
	if got, want := roles(tail), "uatat"; got != want {
		t.Fatalf("tail roles under an 11k-token budget = %q, want %q", got, want)
	}
}

func TestPinnedTask(t *testing.T) {
	stored := []Message{{Role: testRoleUser, Content: "do X"}, {Role: testRoleAssistant, Content: "ok"}}
	pinned, rest := pinnedTask(stored)
	if pinned == nil || messageText(*pinned) != pinnedTaskOpen+"do X" || len(rest) != 2 {
		t.Fatalf("pinned = %v, rest = %d", pinned, len(rest))
	}
	// An existing pinned message is reused and taken out of the rest.
	existing := append([]Message{*pinned}, Message{Role: testRoleUser, Content: "later"})
	pinned2, rest2 := pinnedTask(existing)
	if messageText(*pinned2) != messageText(*pinned) || roles(rest2) != "u" {
		t.Fatalf("existing pinned not reused: %v / %q", pinned2, roles(rest2))
	}
	// A long task keeps its start and end.
	long := "START " + strings.Repeat("word ", 20_000) + " END"
	got := elideMiddle(long, 1000)
	if !strings.HasPrefix(got, "START") || !strings.HasSuffix(got, "END") || !strings.Contains(got, "elided by compaction") ||
		countTokens(got) > 1100 {
		t.Fatalf("elideMiddle kept %d tokens: %.80q…", countTokens(got), got)
	}
}

// An oversized checkpoint is no longer cut when it is loaded; its first
// request summarizes it instead (#47).
func TestNewKeepsOversizedCheckpoint(t *testing.T) {
	raw := make([]json.RawMessage, 0, 40)
	for _, m := range bigMessages(40, 5000) { // 200k tokens in a 100k window
		b, _ := json.Marshal(m)
		raw = append(raw, b)
	}
	cfg := &Config{
		RunName:   "test-run",
		Providers: []ProviderConfig{{Name: "p", ContextWindow: 100_000}},
	}
	r, err := New(cfg, newTestStore(t), raw, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.cancelFunc() })
	if n := len(storedConversation(r)); n != 40 {
		t.Fatalf("loaded %d of 40 checkpoint messages", n)
	}
}

func TestConfigFromEnv_CompactionDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	if err := os.WriteFile(path, []byte(`{"episodicMemory":{"summaryEvery":5}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTORC_ROUTER_CONFIG", path)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.ContextCompactionThreshold != 0.8 || cfg.ContextCompactionMode != compactionModeSummary {
		t.Fatalf("threshold/mode = %v/%q, want 0.8/summary", cfg.ContextCompactionThreshold, cfg.ContextCompactionMode)
	}
	if cfg.EpisodicMemory.SummaryEvery != 0 {
		t.Fatalf("deprecated summaryEvery must be ignored, got %d", cfg.EpisodicMemory.SummaryEvery)
	}
}

// Mid tool loop (a continuation), the summary keeps the latest user message and
// the turn's tool-call groups verbatim so the agent does not lose its place.
func TestSummaryCompactionMidToolLoopKeepsTurn(t *testing.T) {
	srv, provider := newSummaryProvider(t, validSummary("mid-loop"))
	provider.streams = [][]byte{toolCallTurnSSE()}
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), store)

	// Fill the history to just under the 80k trigger for the first request, so
	// the tool call and its result push the continuation over it.
	input := []Message{{Role: testRoleUser, Content: "store this"}}
	prior := append([]Message{{Role: testRoleUser, Content: originalTask}, {Role: testRoleAssistant, Content: "Understood."}},
		bigMessages(15, 5000)...)
	pad := 80_000 - estimateTokens(prior) - estimateTokens(input) - 8
	prior = append(prior, Message{Role: testRoleAssistant, Content: strings.Repeat("p", pad*4)})
	if got := estimateTokens(prior) + estimateTokens(input); got > 80_000 || got < 79_950 {
		t.Fatalf("setup: first request is %d tokens, want just under 80k", got)
	}
	r.mu.Lock()
	r.priorMessages = prior
	r.mu.Unlock()

	consumeClientBody(t, r, userTurn(t, "store this"))

	if _, n := provider.counts(); n != 1 {
		t.Fatalf("summary requests = %d, want 1 (in the continuation)", n)
	}
	for _, m := range provider.summaryRequest(0).Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 { //nolint:goconst
			t.Fatalf("the turn's tool-call group must stay verbatim, not be summarized")
		}
	}
	p := provider
	p.mu.Lock()
	cont := p.streamed[1].Messages
	p.mu.Unlock()
	if got, want := roles(cont), "ssuat"; got != want {
		t.Fatalf("continuation roles = %q, want %q (pinned, summary, latest user, tool call, tool result)", got, want)
	}
	if messageText(cont[2]) != "store this" {
		t.Fatalf("latest user message = %q", messageText(cont[2]))
	}
	if got, want := roles(storedConversation(r)), "ssuata"; got != want {
		t.Fatalf("stored conversation roles = %q, want %q", got, want)
	}
}

func TestSummaryProvider(t *testing.T) {
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "main"}, {Name: "cheap"}}}}
	main := &r.cfg.Providers[0]
	if got := r.summaryProvider(main); got != main {
		t.Fatalf("unset summary provider: got %q, want the request's", got.Name)
	}
	r.cfg.EpisodicMemory.SummaryProviderName = "cheap"
	if got := r.summaryProvider(main); got.Name != "cheap" {
		t.Fatalf("configured summary provider: got %q, want cheap", got.Name)
	}
	r.cfg.EpisodicMemory.SummaryProviderName = "missing"
	if got := r.summaryProvider(main); got != main {
		t.Fatalf("unknown summary provider: got %q, want the request's", got.Name)
	}
}

// Review fix 1 (#51): when the summary provider's window cannot hold a summary
// request at all, no summary call is made (two certain rejections before) and
// the oldest messages are dropped instead.
func TestSummaryCompactionNoRoomSkipsProviderCall(t *testing.T) {
	srv, provider := newSummaryProvider(t, validSummary("unused"))
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), store)
	// 12k window: a 12k summary leaves no room. Trigger 9.6k, fallback target 6k.
	r.cfg.Providers[0].ContextWindow = 12_000
	r.mu.Lock()
	r.priorMessages = bigMessages(4, 5000) // 20k tokens (over 16k chars each: estimated at 4 chars per token)
	r.mu.Unlock()

	consumeClientBody(t, r, userTurn(t, "next"))

	if _, n := provider.counts(); n != 0 {
		t.Fatalf("summary requests = %d, want 0 when no summary request can fit", n)
	}
	done := eventsOfType(store, "truncation")
	if len(done) != 1 || done[0]["mode"] != compactionDegraded || !strings.Contains(fmt.Sprint(done[0]["reason"]), "no room for a summary request") {
		t.Fatalf("truncation events = %v, want one degraded with the no-room reason", done)
	}
}

// Review fix 2 (#51): a huge latest user message mid tool loop keeps only its
// start and end in the tail and is summarized whole, so the compacted
// conversation is not held over the trigger by it.
func TestSplitTurnTailElidesOversizedUserMessage(t *testing.T) {
	huge := "BEGIN " + strings.Repeat("y", 160_000) + " END" // ~40k tokens
	msgs := []Message{
		{Role: testRoleUser, Content: "old question"}, {Role: testRoleAssistant, Content: "old answer"},
		{Role: testRoleUser, Content: huge},
		{Role: testRoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Function: FunctionCall{Name: "read"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "result"},
	}
	rest, tail := splitTurnTail(msgs, 10_000)
	if got, want := roles(rest), "uau"; got != want || messageText(rest[2]) != huge {
		t.Fatalf("rest roles = %q, want %q ending with the whole user message", got, want)
	}
	kept := messageText(tail[0])
	if !strings.HasPrefix(kept, "BEGIN") || !strings.HasSuffix(kept, "END") || countTokens(kept) > pinnedTaskMaxTokens+100 {
		t.Fatalf("tail keeps %d tokens of the user message, want its start and end within ~%d", countTokens(kept), pinnedTaskMaxTokens)
	}
	if got, want := roles(tail), "uat"; got != want {
		t.Fatalf("tail roles = %q, want %q", got, want)
	}
}

// Review fix 2 (#51): a conversation barely grown since the last compaction is
// not compacted again, so an irreducible conversation cannot trigger a
// summary call on every request.
func TestCompactionNotRepeatedWhenBarelyGrown(t *testing.T) {
	r, provider, store := summaryTestRouter(t, validSummary("unused"))
	stored := storedConversation(r)
	r.mu.Lock()
	r.lastCompactedTokens = estimateTokens(stored) - 1000 // grown by 1k; tail budget is 5k
	r.mu.Unlock()

	consumeClientBody(t, r, userTurn(t, "next"))

	if _, n := provider.counts(); n != 0 {
		t.Fatalf("summary requests = %d, want 0", n)
	}
	if n := len(eventsOfType(store, "compaction.started")); n != 0 {
		t.Fatalf("compaction.started events = %d, want 0", n)
	}
}

// Review nit 2 (#51): a mistyped mode falls back to summary (with a warning)
// rather than being silently treated as a valid choice.
func TestConfigFromEnv_UnknownCompactionMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-config.json")
	if err := os.WriteFile(path, []byte(`{"contextCompactionMode":"truncte"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTORC_ROUTER_CONFIG", path)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.ContextCompactionMode != compactionModeSummary {
		t.Fatalf("mode = %q, want summary", cfg.ContextCompactionMode)
	}
}
