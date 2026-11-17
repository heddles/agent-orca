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

package graders

import (
	"math"
	"unicode/utf8"
)

// The graders that are not pure functions: a model judge and a human reviewer.
// This file holds only their config. They are run by the experiment runner
// (internal/evals/runner), with the judge client in internal/evals/judge.

// Judge modes of the llm grader.
const (
	// ModeReference grades the output against the case's reference.
	ModeReference = "reference"
	// ModeRubric grades the output by the rubric alone.
	ModeRubric = "rubric"
	// ModePairwise compares two arms' outputs of the same case. It needs an
	// experiment with exactly two arms.
	ModePairwise = "pairwise"
)

// Human review scales.
const (
	ScalePassFail  = "passfail"
	ScaleOneToFive = "1to5"
)

// Limits and defaults of the judged graders.
const (
	MaxRubricChars      = 4000
	MaxInstructionChars = 4000
	MaxProviderRefLen   = 253
	DefaultJudgeTokens  = 512
	MaxJudgeTokens      = 2048
	MaxJudgeRepeats     = 3
	// DefaultLLMPassAt is the score an llm grade needs to pass.
	DefaultLLMPassAt = 0.7
	// DefaultHumanPassAt is the score a 1 to 5 label needs to pass: 4 of 5.
	DefaultHumanPassAt = 0.75
)

// LLMConfig is the config of an llm grader.
type LLMConfig struct {
	// ProviderRef names the ModelProvider that judges, in the experiment's
	// workspace or a platform one.
	ProviderRef string `json:"providerRef"`
	// Rubric is what to grade by, at most MaxRubricChars characters. Required for
	// the rubric and pairwise modes; the reference mode has a default.
	Rubric string `json:"rubric,omitempty"`
	// Mode is reference, rubric (the default) or pairwise.
	Mode string `json:"mode,omitempty"`
	// PassAt is the score that passes, default DefaultLLMPassAt.
	PassAt *float64 `json:"passAt,omitempty"`
	// MaxTokens bounds the judge's reply, default DefaultJudgeTokens.
	MaxTokens int `json:"maxTokens,omitempty"`
	// JudgeRepeats is how many times to ask (1 to 3); the median score counts.
	JudgeRepeats int `json:"judgeRepeats,omitempty"`
}

// PassScore is the score that passes.
func (c LLMConfig) PassScore() float64 {
	if c.PassAt != nil {
		return *c.PassAt
	}
	return DefaultLLMPassAt
}

// HumanConfig is the config of a human grader.
type HumanConfig struct {
	// Scale is passfail (the default) or 1to5.
	Scale string `json:"scale,omitempty"`
	// Instructions tell the reviewer what to look for.
	Instructions string `json:"instructions,omitempty"`
	// PassAt is the score a 1 to 5 label needs to pass, default DefaultHumanPassAt.
	PassAt *float64 `json:"passAt,omitempty"`
	// Sample is how many results of an experiment to send to review, chosen at
	// random; 0 sends them all.
	Sample int `json:"sample,omitempty"`
}

// PassScore is the score a label needs to pass.
func (c HumanConfig) PassScore() float64 {
	if c.PassAt != nil {
		return *c.PassAt
	}
	return DefaultHumanPassAt
}

func validPassAt(p *float64) bool {
	return p == nil || (!math.IsNaN(*p) && *p > 0 && *p <= 1)
}

// ParseLLM checks an llm grader's config and fills in its defaults.
func ParseLLM(raw []byte) (LLMConfig, error) {
	var c LLMConfig
	if err := decodeConfig(raw, &c); err != nil {
		return c, err
	}
	if c.ProviderRef == "" || len(c.ProviderRef) > MaxProviderRefLen {
		return c, configErr("providerRef is required and at most %d bytes", MaxProviderRefLen)
	}
	if utf8.RuneCountInString(c.Rubric) > MaxRubricChars {
		return c, configErr("rubric is over %d characters", MaxRubricChars)
	}
	switch c.Mode {
	case "":
		c.Mode = ModeRubric
	case ModeReference, ModeRubric, ModePairwise:
	default:
		return c, configErr("unknown mode %q (reference, rubric, pairwise)", c.Mode)
	}
	if c.Rubric == "" && c.Mode != ModeReference {
		return c, configErr("a %s grader needs a rubric", c.Mode)
	}
	if !validPassAt(c.PassAt) {
		return c, configErr("passAt must be above 0 and at most 1")
	}
	switch {
	case c.MaxTokens == 0:
		c.MaxTokens = DefaultJudgeTokens
	case c.MaxTokens < 16 || c.MaxTokens > MaxJudgeTokens:
		return c, configErr("maxTokens must be between 16 and %d", MaxJudgeTokens)
	}
	switch {
	case c.JudgeRepeats == 0:
		c.JudgeRepeats = 1
	case c.JudgeRepeats < 1 || c.JudgeRepeats > MaxJudgeRepeats:
		return c, configErr("judgeRepeats must be between 1 and %d", MaxJudgeRepeats)
	}
	return c, nil
}

// ParseHuman checks a human grader's config and fills in its defaults.
func ParseHuman(raw []byte) (HumanConfig, error) {
	var c HumanConfig
	if err := decodeConfig(raw, &c); err != nil {
		return c, err
	}
	switch c.Scale {
	case "":
		c.Scale = ScalePassFail
	case ScalePassFail, ScaleOneToFive:
	default:
		return c, configErr("unknown scale %q (passfail, 1to5)", c.Scale)
	}
	if utf8.RuneCountInString(c.Instructions) > MaxInstructionChars {
		return c, configErr("instructions are over %d characters", MaxInstructionChars)
	}
	if !validPassAt(c.PassAt) {
		return c, configErr("passAt must be above 0 and at most 1")
	}
	if c.Sample < 0 || c.Sample > 5000 {
		return c, configErr("sample must be between 0 and 5000")
	}
	return c, nil
}

// ScoreOf maps a human label to a score in 0 to 1: pass is 1 and fail 0 on the
// passfail scale; a 1 to 5 label n is (n-1)/4. ok is false for a label that is
// not on the scale.
func (c HumanConfig) ScoreOf(pass *bool, stars int) (score float64, ok bool) {
	switch c.Scale {
	case ScaleOneToFive:
		if stars < 1 || stars > 5 {
			return 0, false
		}
		return float64(stars-1) / 4, true
	default:
		if pass == nil {
			return 0, false
		}
		if *pass {
			return 1, true
		}
		return 0, true
	}
}
