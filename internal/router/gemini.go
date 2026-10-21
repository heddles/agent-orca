package router

import (
	"encoding/json"
	"strings"
)

// geminiSkipThoughtSignature is Google's documented placeholder for function
// calls that have no real thought signature (calls produced by another model,
// or injected by the router). It makes Gemini skip signature validation for
// that call instead of rejecting the request with HTTP 400.
// https://ai.google.dev/gemini-api/docs/thought-signatures
const geminiSkipThoughtSignature = "skip_thought_signature_validator"

func isGeminiProvider(p *ProviderConfig) bool {
	return strings.HasPrefix(p.LiteLLMModel, "gemini/")
}

// adaptToolCallsForProvider prepares the assistant tool calls in msgs for the
// target provider and returns the (possibly copied) slice; the input, which
// aliases the router's stored conversation, is never mutated.
//
//   - Every provider: empty tool-call arguments become "{}". Anthropic streams
//     no input for a tool that takes none, so the stored call has "" — invalid
//     JSON that Gemini rejects ("Request contains an invalid argument").
//   - Gemini: every tool call must carry a thought signature. Real signatures
//     (kept in ExtraContent when Gemini produced the call) pass through; calls
//     without one — made by a different model earlier in the conversation —
//     get the skip placeholder.
//   - Everything else: provider-specific ExtraContent is dropped so a Gemini
//     signature never leaks into another provider's request.
func adaptToolCallsForProvider(p *ProviderConfig, msgs []Message) []Message {
	gemini := isGeminiProvider(p)
	var out []Message // lazily copied on first change
	for i, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		var calls []ToolCall
		for j, tc := range m.ToolCalls {
			adapted := tc
			if strings.TrimSpace(adapted.Function.Arguments) == "" {
				adapted.Function.Arguments = "{}"
			}
			switch {
			case !gemini:
				adapted.ExtraContent = nil
			case !hasThoughtSignature(tc.ExtraContent):
				adapted.ExtraContent = json.RawMessage(`{"google":{"thought_signature":"` + geminiSkipThoughtSignature + `"}}`)
			}
			if adapted.Function.Arguments == tc.Function.Arguments && string(adapted.ExtraContent) == string(tc.ExtraContent) {
				continue
			}
			if calls == nil {
				calls = append([]ToolCall(nil), m.ToolCalls...)
			}
			calls[j] = adapted
		}
		if calls == nil {
			continue
		}
		if out == nil {
			out = append([]Message(nil), msgs...)
		}
		out[i].ToolCalls = calls
	}
	if out == nil {
		return msgs
	}
	return out
}

func hasThoughtSignature(extra json.RawMessage) bool {
	if len(extra) == 0 {
		return false
	}
	var v struct {
		Google struct {
			ThoughtSignature string `json:"thought_signature"`
		} `json:"google"`
	}
	return json.Unmarshal(extra, &v) == nil && v.Google.ThoughtSignature != ""
}
