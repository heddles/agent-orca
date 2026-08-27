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

package mcp

import (
	"strings"
	"testing"
)

func TestParseToolResult_MalformedBareArrayIsError(t *testing.T) {
	// Reproduces the incident: a server returning a bare `[]` instead of a
	// proper `{"content":[...]}` object. Must surface as an error so the LLM
	// knows the tool failed (and can retry/clarify) instead of silently
	// receiving "[]" and bailing with a hallucinated fallback.
	got, err := parseToolResult("arxiv-search", []byte("[]"))
	if err == nil {
		t.Fatalf("expected an error for malformed MCP result, got result=%q", got)
	}
	if !strings.Contains(err.Error(), "malformed") || !strings.Contains(err.Error(), "arxiv-search") {
		t.Errorf("expected error to mention 'malformed' and the tool name; got: %v", err)
	}
}

func TestParseToolResult_TextContentReturned(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hello world"}]}`)
	got, err := parseToolResult("echo", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestParseToolResult_MultipleTextBlocksJoined(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`)
	got, _ := parseToolResult("echo", body)
	if got != "a\nb" {
		t.Errorf("got %q, want %q", got, "a\nb")
	}
}

func TestParseToolResult_IsErrorPropagated(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"boom"}],"isError":true}`)
	_, err := parseToolResult("echo", body)
	if err == nil || !strings.Contains(err.Error(), "MCP tool error") {
		t.Errorf("expected a tool error, got: %v", err)
	}
}

func TestParseToolResult_EmptyResultIsNotAnError(t *testing.T) {
	// A legitimately empty search result (`{"content":[]}`) is NOT a tool error
	// — it must still return an empty string so the LLM can react to "no results"
	// rather than being told the tool crashed.
	got, err := parseToolResult("web-search", []byte(`{"content":[]}`))
	if err != nil {
		t.Fatalf("empty-but-valid result should not be an error; got: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestParseToolResult_EmptyContentBlocksReturnedRaw(t *testing.T) {
	body := []byte(`{"content":[{"type":"blob"}]}`)
	got, err := parseToolResult("echo", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "type") || !strings.Contains(got, "blob") {
		t.Errorf("expected raw JSON payload when no text extractable; got: %q", got)
	}
}
