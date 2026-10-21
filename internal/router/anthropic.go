package router

import (
	"encoding/json"
	"net/http"
)

const (
	anthropicMessagesURL = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion  = "2023-06-01"
	// anthropicWorkspaceHeader names the workspace a request is billed to.
	// Anthropic rejects calls (HTTP 400) from API keys that are not scoped to
	// a single workspace unless it is present.
	anthropicWorkspaceHeader = "anthropic-workspace-id"
)

// setAnthropicHeaders sets the auth, version, and (when configured) workspace
// headers shared by the streaming and non-streaming Messages API calls.
func setAnthropicHeaders(req *http.Request, provider *ProviderConfig, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	if provider.WorkspaceID != "" {
		req.Header.Set(anthropicWorkspaceHeader, provider.WorkspaceID)
	}
}

// toAnthropicMessages converts OpenAI-format messages to the Anthropic Messages
// format. System messages are returned separately (Anthropic takes them through a
// dedicated field); assistant tool_calls become tool_use blocks and tool results
// become tool_result blocks in a user message.
func toAnthropicMessages(msgs []Message) (systemParts []string, out []map[string]any) {
	for _, msg := range msgs {
		if msg.Role == "system" {
			if text, ok := msg.Content.(string); ok {
				systemParts = append(systemParts, text)
			}
			continue
		}

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			var content []map[string]any
			if text, ok := msg.Content.(string); ok && text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": json.RawMessage(args),
				})
			}
			out = append(out, map[string]any{"role": "assistant", "content": content})
			continue
		}

		if msg.Role == "tool" {
			resultContent, _ := msg.Content.(string)
			out = append(out, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     resultContent,
				}},
			})
			continue
		}

		out = append(out, map[string]any{"role": msg.Role, "content": msg.Content})
	}
	return systemParts, out
}

// toAnthropicTools converts OpenAI tool definitions to Anthropic's format.
func toAnthropicTools(tools []map[string]any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		at := map[string]any{"name": fn["name"]}
		if desc, ok := fn["description"]; ok {
			at["description"] = desc
		}
		if params, ok := fn["parameters"]; ok {
			at["input_schema"] = params
		} else {
			at["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, at)
	}
	return out
}
