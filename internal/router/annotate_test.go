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
	"strings"
	"testing"
)

func TestAnnotateTrivialToolResult(t *testing.T) {
	tests := []struct {
		name  string
		input string
		empty bool // expect the recoverable hint (not the raw input)
	}{
		{name: "empty string", input: "", empty: true},
		{name: "bare [] array", input: "[]", empty: true},
		{name: "bare {} object", input: "{}", empty: true},
		{name: "whitespace-only []", input: "  []  ", empty: true},
		{name: "gtv-style empty object", input: `{"result":""}`, empty: false},
		{name: "real payload", input: `[{"title":"paper"}]`, empty: false},
		{name: "text payload", input: "the capital of Japan is Tokyo", empty: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := annotateTrivialToolResult("arxiv-search", tt.input)
			if tt.empty {
				if !strings.Contains(got, `"error"`) || !strings.Contains(got, "no results") || !strings.Contains(got, "arxiv-search") {
					t.Errorf("expected a no-results hint for %q, got: %s", tt.input, got)
				}
			} else {
				if got != tt.input {
					t.Errorf("expected non-trivial result unchanged, got %q (input %q)", got, tt.input)
				}
			}
		})
	}
}
