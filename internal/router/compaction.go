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

// Conversation compaction (#47). When a request would carry more than the
// selected provider's threshold (80% of its window by default), the stored
// conversation is compacted once, before the request is sent, and the result
// is kept for the following turns. In "summary" mode the model replaces the
// history with a structured summary; "truncate" drops the oldest messages.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	compactionModeSummary  = "summary"
	compactionModeTruncate = "truncate"
	// Compaction results reported in the trace event besides the modes: a
	// summary that fell back to truncation, and no compaction at all.
	compactionDegraded = "degraded"
	compactionSkipped  = "skipped"

	// pinnedTaskOpen opens the system message that keeps the conversation's
	// original task verbatim across compactions.
	pinnedTaskOpen = "[Original task — kept verbatim by compaction]\n"
	// summaryOpenPrefix opens a compaction summary message; the full opening
	// line is summaryOpenFmt, numbered so each compaction counts up.
	summaryOpenPrefix = "[Conversation summary #"
	summaryOpenFmt    = summaryOpenPrefix + "%d — replaces %d earlier messages]\n"

	// pinnedTaskMaxTokens caps the verbatim original task; a longer one keeps
	// its start and end and the summary covers the middle. The latest user
	// message kept mid tool loop is capped the same way.
	pinnedTaskMaxTokens = 8000
	// summaryMaxTokens caps the summary (and the provider's MaxOutputTokens).
	summaryMaxTokens = 12000
	// Mid tool loop, the last tailToolGroups assistant tool-call groups stay
	// verbatim so the agent keeps its place, dropping the oldest of them while
	// they exceed tailMaxWindowFraction of the window.
	tailToolGroups        = 3
	tailMaxWindowFraction = 0.05
	// summaryAttempts is one try plus one retry before falling back to
	// drop-oldest truncation.
	summaryAttempts = 2
)

// summarySections are the headings a summary must contain, in order.
var summarySections = []string{
	"## Goal & original constraints",
	"## Decisions made (and why)",
	"## Files & resources touched (current state)",
	"## Work completed",
	"## Open todos / next step",
	"## Errors & dead ends (don't retry)",
	"## Key facts/identifiers",
}

const summaryInstructionFmt = `COMPACTION REQUEST. The conversation above is about to be replaced by your summary: the next model call will see only the summary, the original task and the latest message. Write that summary now. Do not call tools and do not continue the task.

Use exactly these markdown sections, in this order:
%s

If an earlier "[Conversation summary #…]" message appears above, carry forward everything in it that is still relevant. Keep exact names, file paths, identifiers, numbers, commands and error messages. Write "none" under a section with nothing to report. Stay under %d tokens.`

// compactionMode returns the configured mode; anything but "truncate" is
// "summary", the default.
func (r *Router) compactionMode() string {
	if r.cfg.ContextCompactionMode == compactionModeTruncate {
		return compactionModeTruncate
	}
	return compactionModeSummary
}

// compactStoredConversation compacts the stored conversation (r.priorMessages
// + r.messages) when a request to provider would exceed its trigger, and keeps
// the result, so later turns build on the compacted history instead of
// re-cutting the full one on every request. preamble is the request's injected
// context and turnInput the client's not-yet-stored input. In a continuation
// the turn in progress is part of the stored conversation. It reports whether
// it compacted.
func (r *Router) compactStoredConversation(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest, preamble, turnInput []Message, isContinuation bool) bool {
	b := r.contextBudgetFor(provider, chatReq.Tools)
	preambleTokens := estimateTokens(preamble)
	turnTokens := estimateTokens(turnInput)

	// One request is in flight at a time (streamingActive), so the snapshot
	// cannot change before it is replaced below.
	r.mu.Lock()
	stored := slices.Concat(r.priorMessages, r.messages)
	lastCompacted := r.lastCompactedTokens
	r.mu.Unlock()
	before := estimateTokens(stored)
	if len(stored) == 0 || preambleTokens+before+turnTokens <= b.trigger {
		return false
	}
	// A conversation that has barely grown since the last compaction cannot
	// be made materially smaller (on a small window the pinned task, summary
	// and tail alone can exceed the trigger), so compacting again would repeat
	// a summary call on every request. fitRequestToProvider still keeps each
	// request within the window.
	if lastCompacted > 0 && before-lastCompacted < r.tailBudget(provider) {
		slog.Debug("conversation over the compaction trigger but barely grown since the last compaction; not compacting again",
			"run", r.cfg.RunName, "tokens", before, "lastCompacted", lastCompacted)
		return false
	}

	mode := r.compactionMode()
	r.emitCompactionEvent(map[string]any{
		"type": "compaction.started", "provider": provider.Name, "mode": mode,
		"before": preambleTokens + before + turnTokens, "trigger": b.trigger,
	})
	started := time.Now()

	var (
		compacted []Message
		summary   string
		reason    string
	)
	if mode == compactionModeSummary {
		var err error
		compacted, summary, err = r.summarizeConversation(ctx, r.summaryProvider(provider), chatReq, preamble, stored, isContinuation)
		if err != nil {
			reason = err.Error()
			slog.Warn("conversation summary failed; falling back to dropping the oldest messages",
				"run", r.cfg.RunName, "provider", provider.Name, "err", err)
		}
	}
	if compacted == nil {
		room := b.target - preambleTokens - turnTokens
		if room > 0 {
			compacted = truncateHistory(stored, room)
		}
	}
	after := estimateTokens(compacted)
	if compacted == nil || after >= before {
		// The new input alone leaves no room (fitRequestToProvider shrinks it
		// per request), or nothing could be cut.
		r.emitCompactionEvent(map[string]any{
			"type": "truncation", "mode": compactionSkipped, "provider": provider.Name,
			"before": preambleTokens + before + turnTokens, "trigger": b.trigger, "reason": reason,
		})
		return false
	}

	archiveKey := r.archiveConversation(ctx, stored)
	r.mu.Lock()
	r.priorMessages = compacted
	r.messages = nil
	r.liveBufferTokens = after
	r.lastCompactedTokens = after
	r.mu.Unlock()

	result := mode
	if mode == compactionModeSummary && summary == "" {
		result = compactionDegraded
	}
	slog.Warn("compacted stored conversation",
		"run", r.cfg.RunName,
		"provider", provider.Name,
		"mode", result,
		"beforeTokens", before,
		"afterTokens", after,
		"trigger", b.trigger,
		"target", b.target,
		"preambleTokens", preambleTokens,
		"turnInputTokens", turnTokens,
		"durationMs", time.Since(started).Milliseconds(),
		"archive", archiveKey,
		"reason", reason,
		"breakdown", tokenBreakdown(stored),
	)
	// The `truncation` type is what trace-stream clients already render as a
	// "context compacted" note; mode says how it was done.
	ev := map[string]any{
		"type": "truncation", "mode": result, "provider": provider.Name,
		"before": preambleTokens + before + turnTokens, "after": preambleTokens + after + turnTokens,
		"trigger": b.trigger, "durationMs": time.Since(started).Milliseconds(),
	}
	if result != compactionModeSummary {
		ev["budget"] = b.target
	}
	if reason != "" {
		ev["reason"] = reason
	}
	r.emitCompactionEvent(ev)
	if summary != "" {
		r.retainCompactionSummary(summary)
	}
	return true
}

// summaryProvider returns the provider that writes compaction summaries: the
// Agent's summaryModelSelectorRef provider when it is one of the run's
// providers, else the provider the request was routed to.
func (r *Router) summaryProvider(requested *ProviderConfig) *ProviderConfig {
	name := r.cfg.EpisodicMemory.SummaryProviderName
	if name == "" || name == requested.Name {
		return requested
	}
	for i := range r.cfg.Providers {
		if r.cfg.Providers[i].Name == name {
			return &r.cfg.Providers[i]
		}
	}
	slog.Warn("summary provider is not one of the run's providers; summarizing with the request's provider",
		"summaryProvider", name, "provider", requested.Name, "run", r.cfg.RunName)
	return requested
}

// summarizeConversation asks provider to summarize stored and returns the
// compacted conversation: the pinned original task, the summary and, in a
// continuation, the latest user message plus the last few tool-call groups.
func (r *Router) summarizeConversation(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest, preamble, stored []Message, isContinuation bool) ([]Message, string, error) {
	pinned, toSummarize := pinnedTask(stored)
	var tail []Message
	if isContinuation {
		// A single long turn: the original task is the latest user message,
		// which the tail already keeps, so do not pin it twice.
		if pinned != nil && !slices.ContainsFunc(stored, isPinnedTask) && countUserMessages(stored) == 1 {
			pinned = nil
		}
		toSummarize, tail = splitTurnTail(toSummarize, r.tailBudget(provider))
	}
	if len(toSummarize) == 0 {
		return nil, "", errors.New("nothing to summarize")
	}

	maxTokens := summaryMaxTokens
	if provider.MaxOutputTokens > 0 {
		maxTokens = min(maxTokens, provider.MaxOutputTokens)
	}
	// The summary request reuses the current request's prefix (preamble,
	// tools) so providers with prompt caching serve it from cache. It must
	// still fit the window with room for the summary; if not, the oldest part
	// is dropped first and the summary covers the rest.
	replaced := len(toSummarize)
	b := r.contextBudgetFor(provider, chatReq.Tools)
	room := b.hardLimit - b.toolTokens - maxTokens - estimateTokens(preamble) - 1024
	if room <= 0 {
		// truncateHistory is a no-op on a non-positive budget, so the full
		// conversation would be sent to a window that cannot hold it: two
		// rejected calls before the fallback. Fail before any call instead.
		return nil, "", fmt.Errorf("no room for a summary request on provider %s: %d-token window, %d tool tokens, %d-token preamble, %d-token summary",
			provider.Name, b.hardLimit, b.toolTokens, estimateTokens(preamble), maxTokens)
	}
	if estimateTokens(toSummarize) > room {
		toSummarize = truncateHistory(toSummarize, room)
		if estimateTokens(toSummarize) > room {
			return nil, "", fmt.Errorf("conversation does not fit a summary request on provider %s (%d-token budget)", provider.Name, room)
		}
	}
	instruction := fmt.Sprintf(summaryInstructionFmt, strings.Join(summarySections, "\n"), maxTokens)
	req := ChatCompletionRequest{
		Model:     chatReq.Model,
		Messages:  slices.Concat(preamble, toSummarize, []Message{{Role: "user", Content: instruction}}),
		Tools:     chatReq.Tools,
		MaxTokens: maxTokens,
	}
	// tool_choice "none" keeps OpenAI-compatible providers from answering with
	// a tool call. The native Anthropic path (forwardToAnthropic) never sends a
	// tools array, so no tool_choice mapping is needed there.
	if len(req.Tools) > 0 {
		req.ToolChoice = "none"
	}

	var lastErr error
	for attempt := 1; attempt <= summaryAttempts; attempt++ {
		summary, err := r.requestSummary(ctx, provider, req)
		if err == nil {
			n := lastSummaryNumber(stored) + 1
			msg := Message{Role: "system", Content: fmt.Sprintf(summaryOpenFmt, n, replaced) + summary}
			out := make([]Message, 0, 2+len(tail))
			if pinned != nil {
				out = append(out, *pinned)
			}
			return append(append(out, msg), tail...), summary, nil
		}
		lastErr = err
		slog.Warn("conversation summary attempt failed", "run", r.cfg.RunName, "attempt", attempt, "err", err)
	}
	return nil, "", lastErr
}

// requestSummary sends one summary request and validates the answer.
func (r *Router) requestSummary(ctx context.Context, provider *ProviderConfig, req ChatCompletionRequest) (string, error) {
	body, err := r.forwardToProvider(ctx, provider, req)
	if err != nil {
		return "", err
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decoding summary response: %w", err)
	}
	r.updateSpend(resp.Usage, provider)
	if len(resp.Choices) == 0 {
		return "", errors.New("summary response has no choices")
	}
	summary := strings.TrimSpace(messageText(resp.Choices[0].Message))
	if missing := missingSummarySections(summary); len(missing) > 0 {
		return "", fmt.Errorf("summary is missing sections %q (finish_reason %q)", missing, resp.Choices[0].FinishReason)
	}
	return summary, nil
}

// missingSummarySections lists the required headings summary lacks.
func missingSummarySections(summary string) []string {
	var missing []string
	for _, s := range summarySections {
		// Match on the heading's first words so small wording drift in the
		// parenthesised part still passes.
		key := strings.ToLower(strings.SplitN(s, " (", 2)[0])
		if !strings.Contains(strings.ToLower(summary), key) {
			missing = append(missing, s)
		}
	}
	return missing
}

// countUserMessages counts the user messages in msgs.
func countUserMessages(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// isPinnedTask reports whether m is the pinned original-task message.
func isPinnedTask(m Message) bool {
	return m.Role == "system" && strings.HasPrefix(messageText(m), pinnedTaskOpen)
}

// pinnedTask returns the message pinning the conversation's original task (an
// existing pinned message, else one built from the first user message) and the
// conversation without an existing pinned message. It returns nil when there
// is no user message with text. A freshly built pin leaves the first user
// message in the returned conversation on purpose: it is summarized too, so
// the summary's Goal section can restate it alongside later context.
func pinnedTask(stored []Message) (*Message, []Message) {
	for i, m := range stored {
		if isPinnedTask(m) {
			return &stored[i], slices.Delete(slices.Clone(stored), i, i+1)
		}
	}
	for _, m := range stored {
		if m.Role != "user" {
			continue
		}
		text := canonicalUserText(m.Content)
		if text == "" {
			return nil, stored
		}
		return &Message{Role: "system", Content: pinnedTaskOpen + elideMiddle(text, pinnedTaskMaxTokens)}, stored
	}
	return nil, stored
}

// elideMiddle keeps text whole when it is within maxTokens, else its first and
// last parts with a marker in between.
func elideMiddle(text string, maxTokens int) string {
	if countTokens(text) <= maxTokens {
		return text
	}
	keep := maxTokens * 4 / 2 // chars per side at ~4 chars per token
	if len(text) <= 2*keep {
		return text
	}
	head, tail := text[:keep], text[len(text)-keep:]
	for len(head) > 0 && !utf8.RuneStart(head[len(head)-1]) {
		head = head[:len(head)-1]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return head + fmt.Sprintf("\n\n[... %d characters elided by compaction; the summary covers them ...]\n\n", len(text)-len(head)-len(tail)) + tail
}

// splitTurnTail splits off the turn in progress: the latest user message and
// the last tailToolGroups assistant groups after it (an assistant message plus
// its tool results), dropping the oldest of those groups while they exceed
// maxTokens. The rest is returned for summarizing; the groups between the
// user message and the kept ones are summarized too. A latest user message
// over pinnedTaskMaxTokens keeps only its start and end in the tail and goes
// to the summary whole: kept verbatim, it alone could hold the compacted
// conversation over the trigger, so every continuation would re-compact.
func splitTurnTail(msgs []Message, maxTokens int) (rest, tail []Message) {
	u := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			u = i
			break
		}
	}
	if u < 0 {
		return msgs, nil
	}
	var groupStarts []int
	for i := u + 1; i < len(msgs); i++ {
		if msgs[i].Role == "assistant" {
			groupStarts = append(groupStarts, i)
		}
	}
	keepFrom := len(msgs)
	for i := max(0, len(groupStarts)-tailToolGroups); i < len(groupStarts); i++ {
		if estimateTokens(msgs[groupStarts[i]:]) <= maxTokens {
			keepFrom = groupStarts[i]
			break
		}
	}
	latest, summarizeLatest := msgs[u], false
	if text, ok := latest.Content.(string); ok && countTokens(text) > pinnedTaskMaxTokens {
		latest.Content = elideMiddle(text, pinnedTaskMaxTokens)
		summarizeLatest = true
	}
	restEnd := u
	if summarizeLatest {
		restEnd = u + 1
	}
	rest = slices.Concat(msgs[:restEnd], msgs[u+1:keepFrom])
	tail = slices.Concat([]Message{latest}, msgs[keepFrom:])
	return rest, tail
}

// tailBudget is the most tokens the verbatim tool-call tail may keep.
func (r *Router) tailBudget(provider *ProviderConfig) int {
	window := provider.ContextWindow
	if window <= 0 {
		window = maxContextWindow(r.cfg.Providers)
	}
	return int(float64(window) * tailMaxWindowFraction)
}

// lastSummaryNumber returns the highest compaction-summary number in msgs.
func lastSummaryNumber(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		var k int
		if _, err := fmt.Sscanf(messageText(m), summaryOpenPrefix+"%d", &k); err == nil && k > n {
			n = k
		}
	}
	return n
}

// archiveConversation saves the conversation as it was before compaction
// (zstd, checkpoint TTL), so a recall tool can bring back what the summary
// replaced (#48). It returns the key, or "" when nothing was saved.
func (r *Router) archiveConversation(ctx context.Context, msgs []Message) string {
	if r.store == nil || r.cfg.RunName == "" {
		return ""
	}
	raw := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		if b, err := json.Marshal(m); err == nil {
			raw = append(raw, b)
		}
	}
	key := fmt.Sprintf("agentorca/runs/%s/archive/%d", r.cfg.RunName, time.Now().UnixMilli())
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := r.store.SaveMessages(saveCtx, key, raw, r.checkpointTTL); err != nil {
		slog.Warn("archiving pre-compaction conversation failed", "run", r.cfg.RunName, "err", err)
		return ""
	}
	return key
}

// retainCompactionSummary saves the summary to hindsight in the background;
// a failure never affects compaction.
func (r *Router) retainCompactionSummary(summary string) {
	if !r.cfg.Hindsight.Enabled || r.cfg.Hindsight.URL == "" {
		return
	}
	go func() {
		if err := r.hindsightRetain(r.cfg.RunName, "Conversation compaction summary:\n"+summary); err != nil {
			slog.Warn("hindsight retain of compaction summary failed", "run", r.cfg.RunName, "err", err)
		}
	}()
}

// emitCompactionEvent writes a compaction trace event for trace-stream clients.
func (r *Router) emitCompactionEvent(ev map[string]any) {
	if b, err := json.Marshal(ev); err == nil {
		r.emitTraceEventBoth(string(b))
	}
}
