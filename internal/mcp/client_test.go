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

func TestMatchGlob_SimpleWildcards(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"list_*", "list_repos", true},
		{"list_*", "get_repo", false},
		{"get_*", "get_repo", true},
		{"*", "anything", true},
		// ? matches a single character.
		{"search?repo", "search2repo", true},
		{"search?repo", "searchrepo", false},
		// A literal with no wildcards matches exactly.
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchDoublestar(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// Bare ** matches everything.
		{"**", "list_repos", true},
		// prefix/** matches the exact prefix and anything nested beneath it.
		{"internal/**", "internal", true},
		{"internal/**", "internal/subtool", true},
		// Prefix is segment-boundary aware: "internals" is NOT "internal/".
		{"internal/**", "internals", false},
		{"internal/**", "external/foo", false},
		// ** with no prefix only applies when there is no suffix to anchor.
		{"**/json", "list_repos", false},
	}
	for _, c := range cases {
		if got := matchDoublestar(c.pattern, c.name); got != c.want {
			t.Errorf("matchDoublestar(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchToolName(t *testing.T) {
	cases := []struct {
		name    string
		include []string
		exclude []string
		want    bool
	}{
		// No patterns => everything admitted.
		{"list_repos", nil, nil, true},
		{"anything", nil, nil, true},
		// Include-only.
		{"list_repos", []string{"list_*", "get_*"}, nil, true},
		{"get_repo", []string{"list_*", "get_*"}, nil, true},
		{"delete_repo", []string{"list_*", "get_*"}, nil, false},
		// Exclude-only.
		{"list_x", nil, []string{"internal_*"}, true},
		{"internal_y", nil, []string{"internal_*"}, false},
		// Exclude wins over include.
		{"search_public", []string{"search_*"}, []string{"search_internal*"}, true},
		{"search_internal", []string{"search_*"}, []string{"search_internal*"}, false},
		// ** patterns.
		{"internal/foo", nil, []string{"internal/**"}, false},
		{"internal/foo", []string{"internal/**"}, nil, true},
	}
	for _, c := range cases {
		if got := matchToolName(c.name, c.include, c.exclude); got != c.want {
			t.Errorf("matchToolName(%q, %v, %v) = %v, want %v", c.name, c.include, c.exclude, got, c.want)
		}
	}
}

func TestFilterTools(t *testing.T) {
	tools := []Tool{
		{Name: "list_repos"}, {Name: "get_repo"}, {Name: "delete_repo"}, {Name: "internal_do"},
	}
	available := []string{"list_repos", "get_repo", "delete_repo", "internal_do"}

	// No patterns => inputs unchanged (identity).
	got, gotNames := filterTools(tools, available, nil, nil)
	if &got[0] != &tools[0] {
		t.Errorf("expected unchanged slice when no patterns; got new slice")
	}
	if &gotNames[0] != &available[0] {
		t.Errorf("expected unchanged names when no patterns")
	}

	// include list_* + get_*, exclude internal_* => keep list_repos, get_repo.
	got, gotNames = filterTools(tools, available, []string{"list_*", "get_*"}, []string{"internal_*"})
	var gotNames2 []string
	for _, t := range got {
		gotNames2 = append(gotNames2, t.Name)
	}
	if len(got) != 2 || gotNames2[0] != "list_repos" || gotNames2[1] != "get_repo" {
		t.Errorf("filterTools kept = %v, want [list_repos get_repo]", gotNames2)
	}
	if len(gotNames) != 2 || gotNames[0] != "list_repos" || gotNames[1] != "get_repo" {
		t.Errorf("filterTools keptNames = %v, want [list_repos get_repo]", gotNames)
	}
}
