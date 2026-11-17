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

// Package graders holds the deterministic evaluation graders: pure functions
// from an agent's output and a trace summary to a pass or fail with a score.
// Nothing here calls a model, reads a file or opens a connection.
//
// A grader is a Spec: a kind and a JSON config. Normalize checks a config and
// returns its canonical form (for storing); Grade runs one.
//
// Bounds. Text over MaxTextBytes is never graded, it fails with a detail that
// says so (truncating would let a "not contains" or an "exact" pass on a prefix).
// A regex pattern is at most MaxPatternBytes and runs on Go's RE2 engine, whose
// matching time is linear in the input, so no pattern can blow up. A schema is
// at most MaxSchemaBytes and cannot load another document: every $ref must point
// inside the schema itself.
package graders

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Grader kinds.
const (
	KindExact         = "exact"
	KindContains      = "contains"
	KindNotContains   = "not_contains"
	KindRegex         = "regex"
	KindJSONSchema    = "json_schema"
	KindJSONField     = "json_field_equals"
	KindToolCalled    = "tool_called"
	KindToolNotCalled = "tool_not_called"
	KindMaxCost       = "max_cost"
	KindMaxLatency    = "max_latency"
	// KindLLM and KindHuman are not run by Grade: a model judge and a human
	// reviewer grade in the experiment runner. They are kinds here so their
	// config is checked and stored like any other.
	KindLLM   = "llm"
	KindHuman = "human"
)

// Kinds lists every kind, in documentation order.
var Kinds = []string{
	KindExact, KindContains, KindNotContains, KindRegex, KindJSONSchema, KindJSONField,
	KindToolCalled, KindToolNotCalled, KindMaxCost, KindMaxLatency, KindLLM, KindHuman,
}

// Limits.
const (
	MaxTextBytes    = 256 << 10 // output and reference text
	MaxPatternBytes = 1 << 10
	MaxSchemaBytes  = 64 << 10
	MaxValueBytes   = 4 << 10 // a "contains" value, a JSON field value, a path
	MaxDetailBytes  = 1 << 10
	maxToolNameLen  = 256
)

// Normalisation steps of the exact grader.
const (
	NormTrim               = "trim"
	NormCollapseWhitespace = "collapseWhitespace"
	NormLowercase          = "lowercase"
)

const (
	schemaURL    = "mem:///schema.json"
	msgNotJSON   = "the output is not JSON, and has no JSON code block"
	msgNeedValue = "value is required and at most %d bytes"
)

// ErrConfig is wrapped by every error about a grader's kind or config.
var ErrConfig = errors.New("invalid grader config")

// Spec is a grader: a kind and its config.
type Spec struct {
	Kind   string          `json:"kind"`
	Config json.RawMessage `json:"config"`
}

// Trace is what a grader may know about the run besides its output.
type Trace struct {
	// ToolCalls are the names of the tools the run called, in order, repeats kept.
	ToolCalls []string `json:"toolCalls,omitempty"`
	// CostUSD is the run's total cost.
	CostUSD float64 `json:"costUsd"`
	// LatencyMs is the run's wall-clock time.
	LatencyMs int64 `json:"latencyMs"`
}

// Input is what is graded.
type Input struct {
	// Output is the agent's final text.
	Output string
	// Reference is the case's reference, "" when it has none.
	Reference string
	Trace     Trace
}

// Result is one grader's verdict.
type Result struct {
	Pass  bool    `json:"pass"`
	Score float64 `json:"score"`
	// Detail says why, in at most MaxDetailBytes. It never repeats the output.
	Detail string `json:"detail"`
	// NotGraded is set when the grader needs something the input lacks (a
	// reference). Pass is false then: a case is never counted as a pass for it.
	NotGraded bool `json:"notGraded,omitempty"`
}

// Per-kind configs. Unknown fields are refused.
type (
	exactConfig struct {
		Normalize []string `json:"normalize,omitempty"`
		// Value overrides the case reference.
		Value *string `json:"value,omitempty"`
	}
	containsConfig struct {
		Value           string `json:"value"`
		CaseInsensitive bool   `json:"caseInsensitive,omitempty"`
	}
	regexConfig struct {
		Pattern string `json:"pattern"`
		// MustMatch defaults to true. False makes the grader pass when the
		// pattern does not match.
		MustMatch *bool `json:"mustMatch,omitempty"`
	}
	schemaConfig struct {
		Schema json.RawMessage `json:"schema"`
	}
	fieldConfig struct {
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	toolConfig struct {
		Tool string `json:"tool"`
	}
	costConfig struct {
		USD float64 `json:"usd"`
	}
	latencyConfig struct {
		Ms int64 `json:"ms"`
	}
)

func decodeConfig(raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: text after the config", ErrConfig)
	}
	return nil
}

func configErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, a...))
}

// Normalize checks a spec and returns its config in canonical form.
func Normalize(s Spec) (json.RawMessage, error) {
	cfg, err := parseConfig(s)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return out, nil
}

// parseConfig decodes and checks a spec's config into its typed form.
func parseConfig(s Spec) (any, error) {
	switch s.Kind {
	case KindExact:
		var c exactConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		for _, n := range c.Normalize {
			if n != NormTrim && n != NormCollapseWhitespace && n != NormLowercase {
				return nil, configErr("unknown normalize step %q (trim, collapseWhitespace, lowercase)", n)
			}
		}
		if c.Value != nil && len(*c.Value) > MaxTextBytes {
			return nil, configErr("value is over %d bytes", MaxTextBytes)
		}
		return c, nil
	case KindContains, KindNotContains:
		var c containsConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if c.Value == "" || len(c.Value) > MaxValueBytes {
			return nil, configErr(msgNeedValue, MaxValueBytes)
		}
		return c, nil
	case KindRegex:
		var c regexConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if _, err := compileRegex(c.Pattern); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return parseJSONConfig(s)
	}
}

func parseJSONConfig(s Spec) (any, error) {
	switch s.Kind {
	case KindJSONSchema:
		var c schemaConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if _, err := compileSchema(c.Schema); err != nil {
			return nil, err
		}
		return c, nil
	case KindJSONField:
		var c fieldConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if c.Path == "" || len(c.Path) > MaxValueBytes {
			return nil, configErr("path is required and at most %d bytes", MaxValueBytes)
		}
		if len(c.Value) == 0 || len(c.Value) > MaxValueBytes {
			return nil, configErr(msgNeedValue, MaxValueBytes)
		}
		return c, nil
	default:
		return parseTraceConfig(s)
	}
}

func parseTraceConfig(s Spec) (any, error) {
	switch s.Kind {
	case KindToolCalled, KindToolNotCalled:
		var c toolConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if c.Tool == "" || len(c.Tool) > maxToolNameLen {
			return nil, configErr("tool is required and at most %d bytes", maxToolNameLen)
		}
		return c, nil
	case KindMaxCost:
		var c costConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if math.IsNaN(c.USD) || math.IsInf(c.USD, 0) || c.USD < 0 {
			return nil, configErr("usd must be zero or more")
		}
		return c, nil
	case KindMaxLatency:
		var c latencyConfig
		if err := decodeConfig(s.Config, &c); err != nil {
			return nil, err
		}
		if c.Ms <= 0 {
			return nil, configErr("ms must be above zero")
		}
		return c, nil
	case KindLLM:
		return ParseLLM(s.Config)
	case KindHuman:
		return ParseHuman(s.Config)
	default:
		return nil, configErr("unknown kind %q (one of %s)", s.Kind, strings.Join(Kinds, ", "))
	}
}

// Grade runs one grader. A spec that Normalize refuses gives a failing Result
// whose detail says so; Grade never panics and never returns an error, so a
// stored grader that has gone bad cannot stop a batch.
func Grade(s Spec, in Input) Result {
	cfg, err := parseConfig(s)
	if err != nil {
		return fail("grader config is invalid: %s", err.Error())
	}
	switch c := cfg.(type) {
	case exactConfig:
		return gradeExact(c, in)
	case containsConfig:
		return gradeContains(c, in, s.Kind == KindNotContains)
	case regexConfig:
		return gradeRegex(c, in)
	case schemaConfig:
		return gradeSchema(c, in)
	case fieldConfig:
		return gradeField(c, in)
	case toolConfig:
		return gradeTool(c, in, s.Kind == KindToolCalled)
	case costConfig:
		return gradeCost(c, in)
	case latencyConfig:
		return gradeLatency(c, in)
	case LLMConfig:
		return Result{NotGraded: true, Detail: "an llm grader grades in an experiment, by the judge"}
	case HumanConfig:
		return Result{NotGraded: true, Detail: "a human grader grades in an experiment's review queue"}
	}
	return fail("unknown grader")
}

// ── results ────────────────────────────────────────────────────────────────

func pass(detail string) Result { return Result{Pass: true, Score: 1, Detail: clip(detail)} }

func fail(format string, a ...any) Result {
	return Result{Detail: clip(fmt.Sprintf(format, a...))}
}

func verdict(ok bool, passMsg, failMsg string) Result {
	if ok {
		return pass(passMsg)
	}
	return fail("%s", failMsg)
}

func clip(s string) string {
	if len(s) <= MaxDetailBytes {
		return s
	}
	cut := MaxDetailBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func tooBig(label string, n int) Result {
	return fail("%s is %d bytes, over the %d byte limit; not graded", label, n, MaxTextBytes)
}

// ── text graders ───────────────────────────────────────────────────────────

func gradeExact(c exactConfig, in Input) Result {
	ref := in.Reference
	if c.Value != nil {
		ref = *c.Value
	}
	if ref == "" {
		return Result{NotGraded: true, Detail: "the case has no reference to compare with"}
	}
	if len(in.Output) > MaxTextBytes {
		return tooBig("the output", len(in.Output))
	}
	if len(ref) > MaxTextBytes {
		return tooBig("the reference", len(ref))
	}
	got, want := applyNormalize(in.Output, c.Normalize), applyNormalize(ref, c.Normalize)
	return verdict(got == want, "the output equals the reference",
		fmt.Sprintf("the output differs from the reference (%d and %d bytes after normalising)", len(got), len(want)))
}

func applyNormalize(s string, steps []string) string {
	for _, n := range steps {
		switch n {
		case NormTrim:
			s = strings.TrimSpace(s)
		case NormCollapseWhitespace:
			s = strings.Join(strings.Fields(s), " ")
		case NormLowercase:
			s = strings.ToLower(s)
		}
	}
	return s
}

func gradeContains(c containsConfig, in Input, negate bool) Result {
	if len(in.Output) > MaxTextBytes {
		return tooBig("the output", len(in.Output))
	}
	hay, needle := in.Output, c.Value
	if c.CaseInsensitive {
		hay, needle = strings.ToLower(hay), strings.ToLower(needle)
	}
	found := strings.Contains(hay, needle)
	if negate {
		return verdict(!found, "the output does not contain the text", "the output contains the text it must not")
	}
	return verdict(found, "the output contains the text", "the output does not contain the text")
}

func compileRegex(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || len(pattern) > MaxPatternBytes {
		return nil, configErr("pattern is required and at most %d bytes", MaxPatternBytes)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, configErr("pattern does not compile: %v", err)
	}
	return re, nil
}

func gradeRegex(c regexConfig, in Input) Result {
	re, err := compileRegex(c.Pattern)
	if err != nil {
		return fail("%s", err.Error())
	}
	if len(in.Output) > MaxTextBytes {
		return tooBig("the output", len(in.Output))
	}
	found := re.MatchString(in.Output)
	if c.MustMatch != nil && !*c.MustMatch {
		return verdict(!found, "the output does not match the pattern", "the output matches the pattern it must not")
	}
	return verdict(found, "the output matches the pattern", "the output does not match the pattern")
}

// ── JSON graders ───────────────────────────────────────────────────────────

// extractJSON finds the JSON document in text: the whole text when it is JSON,
// else the first fenced code block that is. decode turns text into a value, or
// reports it is not JSON.
func extractJSON(text string, decode func(string) (any, bool)) (any, bool) {
	if v, ok := decode(strings.TrimSpace(text)); ok {
		return v, true
	}
	rest := text
	for {
		i := strings.Index(rest, "```")
		if i < 0 {
			return nil, false
		}
		rest = rest[i+3:]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, false
		}
		body := rest[nl+1:]
		block, after, found := strings.Cut(body, "```")
		if !found {
			return nil, false
		}
		if v, ok := decode(strings.TrimSpace(block)); ok {
			return v, true
		}
		rest = after
	}
}

// ExtractJSON returns the JSON document in text: the whole text when it is JSON,
// else the first fenced code block that is.
func ExtractJSON(text string) (any, bool) {
	return extractJSON(text, func(s string) (any, bool) {
		if s == "" {
			return nil, false
		}
		dec := json.NewDecoder(strings.NewReader(s))
		var v any
		if err := dec.Decode(&v); err != nil || dec.More() {
			return nil, false
		}
		return v, true
	})
}

// noLoader refuses to load anything: a schema is graded on its own.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("a schema cannot load %q: references must stay inside the schema", url)
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 || len(raw) > MaxSchemaBytes {
		return nil, configErr("schema is required and at most %d bytes", MaxSchemaBytes)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, configErr("schema is not JSON: %v", err)
	}
	if _, isObj := doc.(map[string]any); !isObj {
		return nil, configErr("schema must be a JSON object")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, configErr("schema: %v", err)
	}
	s, err := c.Compile(schemaURL)
	if err != nil {
		return nil, configErr("schema does not compile: %v", err)
	}
	return s, nil
}

func gradeSchema(c schemaConfig, in Input) Result {
	if len(in.Output) > MaxTextBytes {
		return tooBig("the output", len(in.Output))
	}
	schema, err := compileSchema(c.Schema)
	if err != nil {
		return fail("%s", err.Error())
	}
	doc, ok := extractJSON(in.Output, func(s string) (any, bool) {
		if s == "" {
			return nil, false
		}
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
		return v, err == nil
	})
	if !ok {
		return fail(msgNotJSON)
	}
	if err := schema.Validate(doc); err != nil {
		return fail("the output does not match the schema: %s", firstLine(err.Error()))
	}
	return pass("the output is JSON that matches the schema")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func gradeField(c fieldConfig, in Input) Result {
	if len(in.Output) > MaxTextBytes {
		return tooBig("the output", len(in.Output))
	}
	doc, ok := ExtractJSON(in.Output)
	if !ok {
		return fail(msgNotJSON)
	}
	got, found := lookupPath(doc, c.Path)
	if !found {
		return fail("the path %q is not in the output", c.Path)
	}
	var want any
	if err := json.Unmarshal(c.Value, &want); err != nil {
		return fail("the expected value is not JSON")
	}
	return verdict(reflect.DeepEqual(got, want), fmt.Sprintf("%q has the expected value", c.Path),
		fmt.Sprintf("%q does not have the expected value", c.Path))
}

// lookupPath follows a dotted path (object keys and array indexes) in a decoded
// JSON value. A key that holds a dot cannot be addressed; that is a limit.
func lookupPath(v any, path string) (any, bool) {
	for seg := range strings.SplitSeq(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// ── trace graders ──────────────────────────────────────────────────────────

func gradeTool(c toolConfig, in Input, wantCalled bool) Result {
	called := slices.Contains(in.Trace.ToolCalls, c.Tool)
	if wantCalled {
		return verdict(called, fmt.Sprintf("the run called %q", c.Tool), fmt.Sprintf("the run did not call %q", c.Tool))
	}
	return verdict(!called, fmt.Sprintf("the run did not call %q", c.Tool), fmt.Sprintf("the run called %q, which it must not", c.Tool))
}

func gradeCost(c costConfig, in Input) Result {
	got := in.Trace.CostUSD
	if math.IsNaN(got) || got < 0 {
		return fail("the run's cost is not known")
	}
	return verdict(got <= c.USD, fmt.Sprintf("cost $%.6f is within $%.6f", got, c.USD),
		fmt.Sprintf("cost $%.6f is over $%.6f", got, c.USD))
}

func gradeLatency(c latencyConfig, in Input) Result {
	got := in.Trace.LatencyMs
	if got < 0 {
		return fail("the run's latency is not known")
	}
	return verdict(got <= c.Ms, fmt.Sprintf("latency %d ms is within %d ms", got, c.Ms),
		fmt.Sprintf("latency %d ms is over %d ms", got, c.Ms))
}
