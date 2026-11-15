package router

import (
	"encoding/json"
	"strings"
)

// systemMessageSeparator joins the parts merged into the single system message.
const systemMessageSeparator = "\n\n"

// normalizeSystemMessages returns msgs with every "system" and "developer"
// message merged into one leading system message.
//
// Several OpenAI-compatible servers (mlx_lm.server with Qwen-style chat
// templates, vLLM with some templates) reject any request whose system message
// is not the first and only one: "System message must be at the beginning".
// The router assembles many system parts (system prompt, built-in tool hint,
// long-term memory, hindsight recall, RAG trust boundary, compaction pin and
// summary, truncation note, client system messages), so the seam right before
// an OpenAI-compatible body is marshalled collapses them.
//
// Parts keep their original relative order, so the stable ones the preamble
// places first (system prompt, hints) still precede the volatile ones (memory,
// recall) and provider prefix caching keeps working. A system message that
// appears after a non-system message (a later-turn client system message, a
// compaction summary) is folded into the head as well: moving it forward
// changes where its text sits but never drops it, and a template that accepts
// only a leading system message cannot represent it anywhere else.
//
// Empty system content is skipped. Non-text content parts are dropped; only
// text is kept. When there is no system text the input is returned as is. The
// input slice is never modified, so the stored conversation is unaffected.
func normalizeSystemMessages(msgs []Message) []Message {
	var parts []string
	hasSystem := false
	for _, m := range msgs {
		if m.Role != roleSystem && m.Role != roleDeveloper {
			continue
		}
		hasSystem = true
		if text := systemContentText(m.Content); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	if !hasSystem {
		return msgs
	}
	out := make([]Message, 0, len(msgs)+1)
	if len(parts) > 0 {
		out = append(out, Message{Role: "system", Content: strings.Join(parts, systemMessageSeparator)})
	}
	for _, m := range msgs {
		if m.Role != roleSystem && m.Role != roleDeveloper {
			out = append(out, m)
		}
	}
	return out
}

// systemContentText extracts the text of a message content that is either a
// string or an array of content parts.
func systemContentText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if (p.Type == "" || p.Type == partTypeText) && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, systemMessageSeparator)
}

// Message roles and content part types the router matches on.
const (
	roleSystem    = "system"
	roleDeveloper = "developer"
	partTypeText  = "text"
)
