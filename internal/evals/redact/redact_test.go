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

package redact

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinRules(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string
		wantRules []string
	}{
		{"nothing to redact", "Summarise the Q3 report for the sales team.", "Summarise the Q3 report for the sales team.", nil},
		{"email", "Mail ada.lovelace+test@example.co.uk now", "Mail [REDACTED_EMAIL] now", []string{"email"}},
		{"two emails", "a@b.io and c@d.io", "[REDACTED_EMAIL] and [REDACTED_EMAIL]", []string{"email"}},
		{"bearer token", "curl -H 'Authorization: Bearer abc123def456ghi789'", "curl -H 'Authorization: Bearer [REDACTED_TOKEN]'", []string{"bearer-token"}},
		{"bearer is case-insensitive", "bearer abcdefgh12345678", "Bearer [REDACTED_TOKEN]", []string{"bearer-token"}},
		{"short bearer word is prose", "Bearer of bad news", "Bearer of bad news", nil},
		{"openai key", "key sk-proj-AbCdEfGhIjKlMnOpQrStUv", "key [REDACTED_API_KEY]", []string{"api-key"}},
		{"anthropic key", "sk-ant-api03-abcdefghijklmnop_qrstuv", "[REDACTED_API_KEY]", []string{"api-key"}},
		{"github token", "ghp_" + strings.Repeat("a1", 18), "[REDACTED_API_KEY]", []string{"api-key"}},
		{"github fine grained", "github_pat_" + strings.Repeat("Z9", 20), "[REDACTED_API_KEY]", []string{"api-key"}},
		{"aws access key", "id AKIAIOSFODNN7EXAMPLE end", "id [REDACTED_API_KEY] end", []string{"api-key"}},
		{"google key", "AIza" + strings.Repeat("x", 35), "[REDACTED_API_KEY]", []string{"api-key"}},
		{"slack token", "xoxb-1234567890-abcdefghij", "[REDACTED_API_KEY]", []string{"api-key"}},
		{"stripe key", "sk_live_" + strings.Repeat("4", 24), "[REDACTED_API_KEY]", []string{"api-key"}},
		{"hugging face", "hf_" + strings.Repeat("q", 34), "[REDACTED_API_KEY]", []string{"api-key"}},
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk x", "token [REDACTED_TOKEN] x", []string{"jwt"}},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIE\nabc\n-----END RSA PRIVATE KEY-----\nrest", "[REDACTED_PRIVATE_KEY]\nrest", []string{"private-key"}},
		{"password assignment", "password=hunter22 user=bob", "password=[REDACTED_SECRET] user=bob", []string{"secret-assignment"}},
		{"json secret", `{"api_key": "abcd1234efgh"}`, `{"api_key": "[REDACTED_SECRET]"}`, []string{"secret-assignment"}},
		{"short value is kept", "token: abc", "token: abc", nil},
		{"url credentials", "postgres://admin:s3cret@db.internal:5432/app", "postgres://[REDACTED_CREDENTIALS]@db.internal:5432/app", []string{"url-credentials"}},
		{"visa card", "pay with 4111 1111 1111 1111 please", "pay with [REDACTED_CARD] please", []string{"card-number"}},
		{"card with dashes", "4111-1111-1111-1111", "[REDACTED_CARD]", []string{"card-number"}},
		{"digits failing luhn stay", "order 1234567890123456", "order 1234567890123456", nil},
		{"ssn", "SSN 123-45-6789.", "SSN [REDACTED_SSN].", []string{"ssn"}},
		{"several kinds", "mail a@b.io key sk-abcdefghijklmnopqrst", "mail [REDACTED_EMAIL] key [REDACTED_API_KEY]", []string{"api-key", "email"}},
		{"empty", "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.in, nil)
			if got.Text != tt.want {
				t.Errorf("Text = %q, want %q", got.Text, tt.want)
			}
			if !reflect.DeepEqual(got.Rules, tt.wantRules) {
				t.Errorf("Rules = %v, want %v", got.Rules, tt.wantRules)
			}
			if (got.Count > 0) != (tt.want != tt.in) {
				t.Errorf("Count = %d for changed=%v", got.Count, tt.want != tt.in)
			}
		})
	}
}

func TestRedactionIsIdempotent(t *testing.T) {
	in := "a@b.io Bearer abcdefgh12345678 4111111111111111 password=hunter22"
	once := Text(in, nil)
	twice := Text(once.Text, nil)
	if twice.Text != once.Text || twice.Count != 0 {
		t.Errorf("second pass changed %q to %q (count %d)", once.Text, twice.Text, twice.Count)
	}
}

func TestCompileGuardrailPatterns(t *testing.T) {
	rules, skipped := Compile([]Pattern{
		{Name: "employee-id", Pattern: `EMP-\d{6}`, Replacement: "[EMPLOYEE]"},
		{Name: "no-replacement", Pattern: `project\s+x`},
		{Name: "group", Pattern: `(\w+)@internal`, Replacement: "$1@[host]"},
		{Name: "broken", Pattern: `(unclosed`},
		{Name: "backreference", Pattern: `(a)\1`},
		{Name: "empty", Pattern: ``},
		{Pattern: strings.Repeat("a", MaxPatternBytes+1)},
	})
	if len(rules) != 3 {
		t.Fatalf("compiled %d rules, want 3", len(rules))
	}
	wantSkipped := []string{"broken: invalid expression", "backreference: invalid expression", "empty: empty pattern",
		"pattern 6: pattern over 1024 bytes"}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Errorf("skipped = %v, want %v", skipped, wantSkipped)
	}
	got := Apply("EMP-123456 works on project   x with bob@internal", rules)
	if want := "[EMPLOYEE] works on [REDACTED] with bob@[host]"; got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
	if want := []string{"employee-id", "group", "no-replacement"}; !reflect.DeepEqual(got.Rules, want) {
		t.Errorf("Rules = %v, want %v", got.Rules, want)
	}
}

func TestCompileCapsThePatternCount(t *testing.T) {
	ps := make([]Pattern, 0, MaxPatterns+5)
	for range MaxPatterns + 5 {
		ps = append(ps, Pattern{Name: "p", Pattern: "x"})
	}
	rules, skipped := Compile(ps)
	if len(rules) != MaxPatterns || len(skipped) != 5 {
		t.Errorf("rules = %d, skipped = %d", len(rules), len(skipped))
	}
}

func TestKeywords(t *testing.T) {
	rules := Keywords([]string{"Project Falcon", "  ", "a.b"})
	got := Apply("project FALCON and a.b but not axb", rules)
	if want := "[REDACTED] and [REDACTED] but not axb"; got.Text != want || got.Count != 2 {
		t.Errorf("got %+v, want %q", got, want)
	}
}

func TestCustomRulesRunAfterBuiltins(t *testing.T) {
	rules, _ := Compile([]Pattern{{Name: "mark", Pattern: `REDACTED_EMAIL`, Replacement: "mail"}})
	if got := Text("a@b.io", rules); got.Text != "[mail]" {
		t.Errorf("Text = %q", got.Text)
	}
}

func TestResultNeverHoldsMatchedText(t *testing.T) {
	got := Text("mail secret.person@example.com", nil)
	if strings.Contains(strings.Join(got.Rules, " "), "secret.person") {
		t.Errorf("Rules leak the match: %v", got.Rules)
	}
}

func TestLuhn(t *testing.T) {
	for in, want := range map[string]bool{
		"4111111111111111":     true,
		"5500 0000 0000 0004":  true,
		"3782-822463-10005":    true,
		"4111111111111112":     false,
		"411111111111":         false, // 12 digits
		"41111111111111111111": false, // 20 digits
		"4111x111111111111":    false,
	} {
		if got := luhnValid(in); got != want {
			t.Errorf("luhnValid(%q) = %v, want %v", in, got, want)
		}
	}
}

// A long input with no match, and one made of near-misses, finishes: RE2 has
// no backtracking blow-up.
func TestLargeInputs(t *testing.T) {
	big := strings.Repeat("sk-short ghp_ AKIA ", 40000)
	if got := Text(big, nil); got.Count != 0 {
		t.Errorf("near-misses matched %d times", got.Count)
	}
	tail := strings.Repeat("a", 200000) + "@"
	_ = Text(tail, nil)
}
