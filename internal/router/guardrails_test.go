/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package router

import (
	"strings"
	"testing"
)

// --- GuardrailPipeline nil-safety ---

func TestGuardrailPipeline_NilPipeline_PassesThrough(t *testing.T) {
	var p *GuardrailPipeline
	res := p.ApplyInput("hello")
	if res.Blocked {
		t.Error("nil pipeline: unexpected block")
	}
	if res.FilteredText != "hello" {
		t.Errorf("nil pipeline: text changed to %q", res.FilteredText)
	}
}

func TestNewGuardrailPipeline_NoFilters_ReturnsNil(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{})
	if p != nil {
		t.Error("empty config: expected nil pipeline")
	}
}

// --- Regex filter: redact action ---

func TestRegexFilter_Redact_ReplacesMatch(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{{
			Name:   "ssn-redact",
			Type:   "regex",
			Action: "redact",
			Patterns: []PatternConfig{{
				Name:        "ssn",
				Pattern:     `\d{3}-\d{2}-\d{4}`,
				Replacement: "[SSN]",
			}},
		}},
	})
	res := p.ApplyInput("My SSN is 123-45-6789.")
	if res.Blocked {
		t.Error("redact: unexpected block")
	}
	if strings.Contains(res.FilteredText, "123-45-6789") {
		t.Errorf("redact: SSN not replaced: %q", res.FilteredText)
	}
	if !strings.Contains(res.FilteredText, "[SSN]") {
		t.Errorf("redact: replacement not present: %q", res.FilteredText)
	}
}

// --- Regex filter: block action ---

func TestRegexFilter_Block_ReturnsBlockedResult(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{{
			Name:         "credit-card-block",
			Type:         "regex",
			Action:       "block",
			BlockMessage: "card numbers not allowed",
			Patterns: []PatternConfig{{
				Name:    "visa",
				Pattern: `\b4\d{15}\b`,
			}},
		}},
	})
	res := p.ApplyInput("Pay with 4111111111111111 please")
	if !res.Blocked {
		t.Error("block: expected Blocked=true")
	}
	if res.BlockMessage != "card numbers not allowed" {
		t.Errorf("block: message %q", res.BlockMessage)
	}
}

func TestRegexFilter_Block_NoMatch_PassesThrough(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{{
			Name:   "card-block",
			Type:   "regex",
			Action: "block",
			Patterns: []PatternConfig{{
				Name:    "visa",
				Pattern: `\b4\d{15}\b`,
			}},
		}},
	})
	res := p.ApplyInput("no card here")
	if res.Blocked {
		t.Error("no match: unexpected block")
	}
	if res.FilteredText != "no card here" {
		t.Errorf("no match: text changed: %q", res.FilteredText)
	}
}

// --- Regex filter: warn action ---

func TestRegexFilter_Warn_NoBlock(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{{
			Name:   "warn-filter",
			Type:   "regex",
			Action: "warn",
			Patterns: []PatternConfig{{
				Name:    "secret",
				Pattern: `(?i)secret`,
			}},
		}},
	})
	res := p.ApplyInput("this is a secret message")
	if res.Blocked {
		t.Error("warn: unexpected block")
	}
	if len(res.Warnings) == 0 {
		t.Error("warn: expected warning in result")
	}
}

// --- Keyword blocklist filter ---

func TestKeywordFilter_Block_CaseInsensitive(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{{
			Name:         "kw-block",
			Type:         "keyword-blocklist",
			Action:       "block",
			Keywords:     []string{"DROP TABLE"},
			BlockMessage: "sql injection blocked",
		}},
	})
	for _, input := range []string{"drop table users", "DROP TABLE users", "Drop Table users"} {
		res := p.ApplyInput(input)
		if !res.Blocked {
			t.Errorf("keyword block: input %q: expected Blocked=true", input)
		}
	}
}

func TestKeywordFilter_Redact_ReplacesKeyword(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{{
			Name:     "kw-redact",
			Type:     "keyword-blocklist",
			Action:   "redact",
			Keywords: []string{"password"},
		}},
	})
	res := p.ApplyInput("my Password is hunter2")
	if res.Blocked {
		t.Error("keyword redact: unexpected block")
	}
	if strings.Contains(strings.ToLower(res.FilteredText), "password") {
		t.Errorf("keyword redact: keyword not removed: %q", res.FilteredText)
	}
}

// --- Topic validation filter ---

func TestTopicFilter_MatchedTopic_Allows(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		OutputFilters: []FilterConfig{{
			Name:          "topic-check",
			Type:          "topic-validation",
			Action:        "block",
			AllowedTopics: []string{"kubernetes", "cloud infrastructure"},
		}},
	})
	res := p.ApplyOutput("kubernetes deployments use pods")
	if res.Blocked {
		t.Error("topic match: unexpected block")
	}
}

func TestTopicFilter_NoMatch_Blocks(t *testing.T) {
	p := NewGuardrailPipeline(&GuardrailConfig{
		OutputFilters: []FilterConfig{{
			Name:          "topic-check",
			Type:          "topic-validation",
			Action:        "block",
			AllowedTopics: []string{"kubernetes", "cloud infrastructure"},
			BlockMessage:  "off-topic",
		}},
	})
	res := p.ApplyOutput("here is a recipe for pasta")
	if !res.Blocked {
		t.Error("topic mismatch: expected block")
	}
	if res.BlockMessage != "off-topic" {
		t.Errorf("topic mismatch: block message %q", res.BlockMessage)
	}
}

// --- PHI redaction: ehr-phi-redact policy patterns ---
//
// These tests exercise the exact regex patterns used in the GuardrailPolicy
// (ehr-phi-redact) to ensure PHI is scrubbed from both
// input messages (before reaching the LLM) and output responses.

var ehrPHIRedactConfig = &GuardrailConfig{
	InputFilters: []FilterConfig{
		{
			Name:   "phi-redact",
			Type:   "regex",
			Action: "redact",
			Patterns: []PatternConfig{
				{Name: "ssn", Pattern: `\b\d{3}-\d{2}-\d{4}\b`, Replacement: "[SSN REDACTED]"},
				{Name: "mrn", Pattern: `\bMRN[:\s#]*\d{4,10}\b`, Replacement: "[MRN REDACTED]"},
				{Name: "dob", Pattern: `\b(0?[1-9]|1[0-2])/(0?[1-9]|[12]\d|3[01])/(\d{2}|\d{4})\b`, Replacement: "[DOB REDACTED]"},
				{Name: "npi", Pattern: `\bNPI[:\s#]*\d{10}\b`, Replacement: "[NPI REDACTED]"},
			},
		},
	},
	OutputFilters: []FilterConfig{
		{
			Name:   "phi-redact",
			Type:   "regex",
			Action: "redact",
			Patterns: []PatternConfig{
				{Name: "ssn", Pattern: `\b\d{3}-\d{2}-\d{4}\b`, Replacement: "[SSN REDACTED]"},
				{Name: "mrn", Pattern: `\bMRN[:\s#]*\d{4,10}\b`, Replacement: "[MRN REDACTED]"},
				{Name: "dob", Pattern: `\b(0?[1-9]|1[0-2])/(0?[1-9]|[12]\d|3[01])/(\d{2}|\d{4})\b`, Replacement: "[DOB REDACTED]"},
				{Name: "npi", Pattern: `\bNPI[:\s#]*\d{10}\b`, Replacement: "[NPI REDACTED]"},
			},
		},
	},
}

func TestPHIRedact_SSN_Input(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	res := p.ApplyInput("Patient SSN is 123-45-6789, please advise.")
	if strings.Contains(res.FilteredText, "123-45-6789") {
		t.Errorf("SSN not redacted in input: %q", res.FilteredText)
	}
	if !strings.Contains(res.FilteredText, "[SSN REDACTED]") {
		t.Errorf("SSN replacement missing in input: %q", res.FilteredText)
	}
}

func TestPHIRedact_SSN_Output(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	res := p.ApplyOutput("Based on SSN 987-65-4321, the patient qualifies.")
	if strings.Contains(res.FilteredText, "987-65-4321") {
		t.Errorf("SSN not redacted in output: %q", res.FilteredText)
	}
	if !strings.Contains(res.FilteredText, "[SSN REDACTED]") {
		t.Errorf("SSN replacement missing in output: %q", res.FilteredText)
	}
}

func TestPHIRedact_MRN(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	cases := []string{
		"MRN: 1234567",
		"MRN #9876543",
		"MRN1234567",
	}
	for _, input := range cases {
		res := p.ApplyInput("Patient record " + input + " needs review.")
		if strings.Contains(res.FilteredText, "MRN: 1234567") ||
			strings.Contains(res.FilteredText, "MRN #9876543") ||
			strings.Contains(res.FilteredText, "MRN1234567") {
			t.Errorf("MRN not redacted for input %q: got %q", input, res.FilteredText)
		}
		if !strings.Contains(res.FilteredText, "[MRN REDACTED]") {
			t.Errorf("MRN replacement missing for input %q: got %q", input, res.FilteredText)
		}
	}
}

func TestPHIRedact_DOB(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	cases := []string{
		"DOB 3/15/1985",
		"born 12/01/72",
		"dob: 1/1/2000",
	}
	for _, c := range cases {
		res := p.ApplyInput("Patient " + c + " presents with chest pain.")
		if res.Blocked {
			t.Errorf("DOB: unexpected block for %q", c)
		}
		if !strings.Contains(res.FilteredText, "[DOB REDACTED]") {
			t.Errorf("DOB not redacted for %q: got %q", c, res.FilteredText)
		}
	}
}

func TestPHIRedact_NPI(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	cases := []string{
		"NPI: 1234567890",
		"NPI #0987654321",
		"NPI1234567890",
	}
	for _, input := range cases {
		res := p.ApplyInput("Ordering provider " + input)
		if !strings.Contains(res.FilteredText, "[NPI REDACTED]") {
			t.Errorf("NPI not redacted for %q: got %q", input, res.FilteredText)
		}
	}
}

func TestPHIRedact_MultiplePHIInSingleMessage(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	input := "Patient MRN: 1234567, DOB 4/22/1978, SSN 111-22-3333, ordering NPI: 9876543210."
	res := p.ApplyInput(input)
	if res.Blocked {
		t.Error("multi-PHI: unexpected block")
	}
	for _, phi := range []string{"1234567", "4/22/1978", "111-22-3333", "9876543210"} {
		if strings.Contains(res.FilteredText, phi) {
			t.Errorf("multi-PHI: raw value %q not redacted: %q", phi, res.FilteredText)
		}
	}
}

func TestPHIRedact_NoFalsePositives(t *testing.T) {
	p := NewGuardrailPipeline(ehrPHIRedactConfig)
	// Phone numbers, zip codes, and version strings should not be redacted.
	benign := []string{
		"Call us at 555-867-5309", // phone — not SSN format
		"Zip code 94105",          // 5-digit zip
		"Version 1.2.3",           // semver
		"Patient is 45 years old", // age
	}
	for _, text := range benign {
		res := p.ApplyInput(text)
		if res.FilteredText != text {
			t.Errorf("false positive: %q became %q", text, res.FilteredText)
		}
	}
}

// --- Pipeline chaining: multiple filters ---

func TestPipeline_MultipleInputFilters_AppliedInOrder(t *testing.T) {
	// First filter redacts SSNs, second blocks if "DROP" is present.
	p := NewGuardrailPipeline(&GuardrailConfig{
		InputFilters: []FilterConfig{
			{
				Name:   "ssn-redact",
				Type:   "regex",
				Action: "redact",
				Patterns: []PatternConfig{{
					Name:        "ssn",
					Pattern:     `\d{3}-\d{2}-\d{4}`,
					Replacement: "[SSN]",
				}},
			},
			{
				Name:         "sql-block",
				Type:         "keyword-blocklist",
				Action:       "block",
				Keywords:     []string{"DROP"},
				BlockMessage: "blocked",
			},
		},
	})

	// SSN only — should redact and allow.
	res := p.ApplyInput("ssn: 123-45-6789")
	if res.Blocked {
		t.Error("chained: SSN only: unexpected block")
	}
	if !strings.Contains(res.FilteredText, "[SSN]") {
		t.Errorf("chained: SSN not redacted: %q", res.FilteredText)
	}

	// Both SSN and DROP — redact first, then block.
	res2 := p.ApplyInput("ssn: 123-45-6789 and DROP TABLE")
	if !res2.Blocked {
		t.Error("chained: SSN+DROP: expected block")
	}
}
