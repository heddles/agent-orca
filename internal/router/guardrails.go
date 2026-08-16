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
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// GuardrailConfig is the serialized guardrail policy injected into the router config.
type GuardrailConfig struct {
	// OutputFilters are applied to LLM responses before returning to the agent.
	OutputFilters []FilterConfig `json:"outputFilters,omitempty"`
	// InputFilters are applied to user messages before sending to the LLM.
	InputFilters []FilterConfig `json:"inputFilters,omitempty"`
	// MaxOutputTokens limits output length. 0 = no limit.
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

// FilterConfig is a single filter rule in the serialized config.
type FilterConfig struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`   // "regex", "keyword-blocklist", "topic-validation"
	Action        string          `json:"action"` // "redact", "block", "warn"
	Patterns      []PatternConfig `json:"patterns,omitempty"`
	Keywords      []string        `json:"keywords,omitempty"`
	AllowedTopics []string        `json:"allowedTopics,omitempty"`
	BlockMessage  string          `json:"blockMessage,omitempty"`
	Replacement   string          `json:"replacement,omitempty"`
}

// PatternConfig is a named regex pattern.
type PatternConfig struct {
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement,omitempty"`
}

// FilterResult describes what happened when a filter was applied.
type FilterResult struct {
	// Blocked is true if the filter blocked the content entirely.
	Blocked bool
	// BlockMessage is the message to return when blocked.
	BlockMessage string
	// FilteredText is the text after redaction (if any).
	FilteredText string
	// Warnings are warning messages for "warn" action filters.
	Warnings []string
}

// GuardrailPipeline evaluates content against a set of filters.
type GuardrailPipeline struct {
	inputFilters  []Filter
	outputFilters []Filter
	maxTokens     int
}

// Filter is the interface all filter implementations must satisfy.
type Filter interface {
	// Apply processes the text and returns the result.
	Apply(text string) FilterResult
	// Name returns the filter's human-readable name.
	Name() string
}

// NewGuardrailPipeline builds a pipeline from the serialized config.
// Returns nil if the config has no filters.
func NewGuardrailPipeline(cfg *GuardrailConfig) *GuardrailPipeline {
	if cfg == nil {
		return nil
	}
	if len(cfg.InputFilters) == 0 && len(cfg.OutputFilters) == 0 && cfg.MaxOutputTokens == 0 {
		return nil
	}

	p := &GuardrailPipeline{
		maxTokens: cfg.MaxOutputTokens,
	}
	for _, fc := range cfg.InputFilters {
		if f := buildFilter(fc); f != nil {
			p.inputFilters = append(p.inputFilters, f)
		}
	}
	for _, fc := range cfg.OutputFilters {
		if f := buildFilter(fc); f != nil {
			p.outputFilters = append(p.outputFilters, f)
		}
	}
	return p
}

// ApplyInput runs input filters on user messages.
func (p *GuardrailPipeline) ApplyInput(text string) FilterResult {
	if p == nil {
		return FilterResult{FilteredText: text}
	}
	return applyFilters(p.inputFilters, text)
}

// ApplyOutput runs output filters on LLM responses.
func (p *GuardrailPipeline) ApplyOutput(text string) FilterResult {
	if p == nil {
		return FilterResult{FilteredText: text}
	}
	return applyFilters(p.outputFilters, text)
}

func applyFilters(filters []Filter, text string) FilterResult {
	result := FilterResult{FilteredText: text}
	for _, f := range filters {
		fr := f.Apply(result.FilteredText)
		if fr.Blocked {
			slog.Warn("Guardrail blocked content", "filter", f.Name())
			return fr
		}
		result.FilteredText = fr.FilteredText
		result.Warnings = append(result.Warnings, fr.Warnings...)
	}
	return result
}

func buildFilter(fc FilterConfig) Filter {
	switch fc.Type {
	case "regex":
		return newRegexFilter(fc)
	case "keyword-blocklist":
		return newKeywordFilter(fc)
	case "topic-validation":
		return newTopicFilter(fc)
	default:
		slog.Warn("Unknown guardrail filter type", "type", fc.Type, "name", fc.Name)
		return nil
	}
}

// --- Regex Filter ---

type regexFilter struct {
	name     string
	action   string
	patterns []compiledPattern
	blockMsg string
}

type compiledPattern struct {
	name        string
	re          *regexp.Regexp
	replacement string
}

func newRegexFilter(fc FilterConfig) *regexFilter {
	f := &regexFilter{
		name:     fc.Name,
		action:   fc.Action,
		blockMsg: fc.BlockMessage,
	}
	for _, p := range fc.Patterns {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			slog.Warn("Invalid regex pattern in guardrail", "filter", fc.Name, "pattern", p.Name, "err", err)
			continue
		}
		f.patterns = append(f.patterns, compiledPattern{
			name:        p.Name,
			re:          re,
			replacement: p.Replacement,
		})
	}
	return f
}

func (f *regexFilter) Name() string { return f.name }

func (f *regexFilter) Apply(text string) FilterResult {
	result := FilterResult{FilteredText: text}
	for _, p := range f.patterns {
		if !p.re.MatchString(result.FilteredText) {
			continue
		}
		switch f.action {
		case "block": //nolint:goconst

			return FilterResult{Blocked: true, BlockMessage: f.blockMsg, FilteredText: text}
		case "redact":
			result.FilteredText = p.re.ReplaceAllString(result.FilteredText, p.replacement)
		case "warn": //nolint:goconst

			result.Warnings = append(result.Warnings, fmt.Sprintf("guardrail %q matched pattern %q", f.name, p.name))
		}
	}
	return result
}

// --- Keyword Blocklist Filter ---

type keywordFilter struct {
	name     string
	action   string
	keywords []string
	blockMsg string
}

func newKeywordFilter(fc FilterConfig) *keywordFilter {
	return &keywordFilter{
		name:     fc.Name,
		action:   fc.Action,
		keywords: fc.Keywords,
		blockMsg: fc.BlockMessage,
	}
}

func (f *keywordFilter) Name() string { return f.name }

func (f *keywordFilter) Apply(text string) FilterResult {
	lower := strings.ToLower(text)
	for _, kw := range f.keywords {
		if !strings.Contains(lower, strings.ToLower(kw)) {
			continue
		}
		switch f.action {
		case "block":
			return FilterResult{Blocked: true, BlockMessage: f.blockMsg, FilteredText: text}
		case "warn":
			return FilterResult{
				FilteredText: text,
				Warnings:     []string{fmt.Sprintf("guardrail %q matched keyword %q", f.name, kw)},
			}
		case "redact":
			// Case-insensitive replacement.
			re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(kw))
			if err == nil {
				text = re.ReplaceAllString(text, "[REDACTED]")
			}
		}
	}
	return FilterResult{FilteredText: text}
}

// --- Topic Validation Filter ---

type topicFilter struct {
	name          string
	action        string
	allowedTopics []string
	blockMsg      string
}

func newTopicFilter(fc FilterConfig) *topicFilter {
	return &topicFilter{
		name:          fc.Name,
		action:        fc.Action,
		allowedTopics: fc.AllowedTopics,
		blockMsg:      fc.BlockMessage,
	}
}

func (f *topicFilter) Name() string { return f.name }

// Apply checks if the text mentions any of the allowed topics.
// This is a simple keyword-overlap implementation for the prototype.
// A production version would use embedding similarity.
func (f *topicFilter) Apply(text string) FilterResult {
	lower := strings.ToLower(text)
	for _, topic := range f.allowedTopics {
		// Split topic into words and check if any appear in the text.
		words := strings.FieldsSeq(strings.ToLower(topic))
		for w := range words {
			if strings.Contains(lower, w) {
				return FilterResult{FilteredText: text}
			}
		}
	}

	// No topic match found.
	switch f.action {
	case "block":
		msg := f.blockMsg
		if msg == "" {
			msg = "Response does not match any allowed topic."
		}
		return FilterResult{Blocked: true, BlockMessage: msg, FilteredText: text}
	case "warn":
		return FilterResult{
			FilteredText: text,
			Warnings:     []string{fmt.Sprintf("guardrail %q: no topic match found", f.name)},
		}
	}
	return FilterResult{FilteredText: text}
}
