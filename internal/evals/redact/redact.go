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

// Package redact removes secrets and personal data from text before it is
// stored in an evaluation dataset.
//
// Redaction is best effort: it catches well-known shapes (API keys, bearer
// tokens, private keys, e-mail addresses, card numbers) and whatever patterns a
// GuardrailPolicy already names, and nothing else. It does not understand
// names, addresses or free-text secrets, and callers must say so to users.
//
// Everything here is a pure function of its inputs. All patterns are RE2
// (Go regexp), so a hostile pattern cannot cause catastrophic backtracking.
package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Limits on caller-supplied patterns, so a guardrail policy cannot make
// redaction expensive.
const (
	MaxPatterns     = 100
	MaxPatternBytes = 1024
	// DefaultReplacement replaces a match when the pattern names no replacement.
	DefaultReplacement = "[REDACTED]"
)

// Pattern is a caller-supplied rule: the same shape as a GuardrailPolicy
// RegexPattern.
type Pattern struct {
	Name string
	// Pattern is a Go (RE2) regular expression.
	Pattern string
	// Replacement replaces each match; $1 style references work. Empty means
	// DefaultReplacement.
	Replacement string
}

// Rule is a compiled redaction rule.
type Rule struct {
	Name        string
	re          *regexp.Regexp
	replacement string
	// validate, when set, must accept a match for it to be replaced (a card
	// number must pass the Luhn check, or any long digit run would go).
	validate func(match string) bool
}

// Result is the outcome of applying rules to a text.
type Result struct {
	// Text is the redacted text.
	Text string
	// Count is the number of replacements made.
	Count int
	// Rules names the rules that matched, sorted and without duplicates. It
	// never contains matched text.
	Rules []string
}

const (
	nameAPIKey = "api-key"
	replKey    = "[REDACTED_API_KEY]"
)

// builtinSpecs are the always-on rules, most specific first so a key inside a
// URL or a key=value pair is replaced by the rule that names it best.
var builtinSpecs = []struct {
	name, pattern, replacement string
	validate                   func(string) bool
}{
	{"private-key", `-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`, "[REDACTED_PRIVATE_KEY]", nil},
	{"jwt", `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, "[REDACTED_TOKEN]", nil},
	{"bearer-token", `(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`, "Bearer [REDACTED_TOKEN]", nil},
	{nameAPIKey, `\bsk-[A-Za-z0-9_-]{16,}`, replKey, nil},
	{nameAPIKey, `\bgh[pousr]_[A-Za-z0-9]{30,}`, replKey, nil},
	{nameAPIKey, `\bgithub_pat_[A-Za-z0-9_]{30,}`, replKey, nil},
	{nameAPIKey, `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, replKey, nil},
	{nameAPIKey, `\bAIza[0-9A-Za-z_-]{35}`, replKey, nil},
	{nameAPIKey, `\bxox[abprs]-[A-Za-z0-9-]{10,}`, replKey, nil},
	{nameAPIKey, `\b[sr]k_(?:live|test)_[0-9A-Za-z]{16,}`, replKey, nil},
	{nameAPIKey, `\bhf_[A-Za-z0-9]{30,}`, replKey, nil},
	{"url-credentials", `(?i)\b([a-z][a-z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`, "${1}[REDACTED_CREDENTIALS]@", nil},
	{"secret-assignment", `(?i)\b(api[_-]?key|secret|token|password|passwd|pwd|access[_-]?key|client[_-]?secret)(["']?\s*[:=]\s*["']?)[^\s"',;]{6,}`, "${1}${2}[REDACTED_SECRET]", notRedacted},
	{"email", `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`, "[REDACTED_EMAIL]", nil},
	{"card-number", `\b\d(?:[ -]?\d){12,18}\b`, "[REDACTED_CARD]", luhnValid},
	{"ssn", `\b\d{3}-\d{2}-\d{4}\b`, "[REDACTED_SSN]", nil},
}

var builtinRules = func() []Rule {
	out := make([]Rule, len(builtinSpecs))
	for i, s := range builtinSpecs {
		out[i] = Rule{Name: s.name, re: regexp.MustCompile(s.pattern), replacement: s.replacement, validate: s.validate}
	}
	return out
}()

// Builtin returns the always-on rules.
func Builtin() []Rule {
	return append([]Rule(nil), builtinRules...)
}

// Compile turns caller patterns into rules. A pattern that is empty, too
// long or not a valid RE2 expression is skipped and reported in skipped as
// "name: reason"; the rest still apply, as the router does with a bad guardrail
// pattern. Patterns beyond MaxPatterns are skipped.
func Compile(patterns []Pattern) (rules []Rule, skipped []string) {
	for i, p := range patterns {
		label := p.Name
		if label == "" {
			label = fmt.Sprintf("pattern %d", i)
		}
		switch {
		case len(rules) >= MaxPatterns:
			skipped = append(skipped, fmt.Sprintf("%s: more than %d patterns", label, MaxPatterns))
			continue
		case p.Pattern == "":
			skipped = append(skipped, label+": empty pattern")
			continue
		case len(p.Pattern) > MaxPatternBytes:
			skipped = append(skipped, fmt.Sprintf("%s: pattern over %d bytes", label, MaxPatternBytes))
			continue
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			skipped = append(skipped, label+": invalid expression")
			continue
		}
		repl := p.Replacement
		if repl == "" {
			repl = DefaultReplacement
		}
		rules = append(rules, Rule{Name: label, re: re, replacement: repl})
	}
	return rules, skipped
}

// Keywords turns a keyword list into rules that replace each keyword,
// case-insensitively, with DefaultReplacement.
func Keywords(words []string) []Rule {
	var rules []Rule
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || len(w) > MaxPatternBytes || len(rules) >= MaxPatterns {
			continue
		}
		rules = append(rules, Rule{
			Name:        "keyword",
			re:          regexp.MustCompile("(?i)" + regexp.QuoteMeta(w)),
			replacement: DefaultReplacement,
		})
	}
	return rules
}

// Apply runs rules over text in order and reports what it did. Each rule sees
// the output of the one before.
func Apply(text string, rules []Rule) Result {
	res := Result{Text: text}
	seen := map[string]bool{}
	for _, r := range rules {
		var n int
		res.Text, n = replaceAll(r, res.Text)
		if n > 0 {
			res.Count += n
			seen[r.Name] = true
		}
	}
	for name := range seen {
		res.Rules = append(res.Rules, name)
	}
	sort.Strings(res.Rules)
	return res
}

// Text applies the built-in rules followed by extra.
func Text(text string, extra []Rule) Result {
	return Apply(text, append(Builtin(), extra...))
}

// replaceAll applies one rule and returns the new text and the replacement count.
func replaceAll(r Rule, text string) (string, int) {
	locs := r.re.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text, 0
	}
	if r.validate == nil {
		return r.re.ReplaceAllString(text, r.replacement), len(locs)
	}
	var b []byte
	last, n := 0, 0
	for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
		if !r.validate(text[m[0]:m[1]]) {
			continue
		}
		b = append(b, text[last:m[0]]...)
		b = r.re.ExpandString(b, r.replacement, text, m)
		last = m[1]
		n++
	}
	if n == 0 {
		return text, 0
	}
	b = append(b, text[last:]...)
	return string(b), n
}

// notRedacted rejects a match whose value is already a placeholder, which keeps
// redaction idempotent.
func notRedacted(match string) bool { return !strings.Contains(match, "[REDACTED_") }

// luhnValid reports whether the digits in s (ignoring spaces and dashes) form a
// number of 13 to 19 digits that passes the Luhn check.
func luhnValid(s string) bool {
	var digits []int
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digits = append(digits, int(c-'0'))
		case c == ' ' || c == '-':
		default:
			return false
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	for i := range digits {
		d := digits[len(digits)-1-i]
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
