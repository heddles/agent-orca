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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	objSchema    = `{"schema":{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}}`
	fencedOutput = "Here you go:\n```json\n{\"id\": 7, \"tags\": [\"a\",\"b\"]}\n```\nbye"
)

func spec(kind, cfg string) Spec { return Spec{Kind: kind, Config: json.RawMessage(cfg)} }

func TestGrade(t *testing.T) {
	big := strings.Repeat("x", MaxTextBytes+1)
	tests := []struct {
		name      string
		spec      Spec
		in        Input
		pass      bool
		score     float64
		notGraded bool
		detail    string // substring, "" to skip
	}{
		{"exact equal", spec(KindExact, `{}`), Input{Output: "a", Reference: "a"}, true, 1, false, ""},
		{"exact differs", spec(KindExact, `{}`), Input{Output: "a ", Reference: "a"}, false, 0, false, "differs"},
		{"exact normalised", spec(KindExact, `{"normalize":["trim","collapseWhitespace","lowercase"]}`),
			Input{Output: "  Hello \n  World ", Reference: "hello world"}, true, 1, false, ""},
		{"exact no reference", spec(KindExact, `{}`), Input{Output: "a"}, false, 0, true, "no reference"},
		{"exact value overrides", spec(KindExact, `{"value":"yes"}`), Input{Output: "yes", Reference: "no"}, true, 1, false, ""},
		{"exact huge output", spec(KindExact, `{}`), Input{Output: big, Reference: big}, false, 0, false, "limit"},
		{"exact unicode lowercase", spec(KindExact, `{"normalize":["lowercase"]}`), Input{Output: "ÄÖ", Reference: "äö"}, true, 1, false, ""},
		{"contains hit", spec(KindContains, `{"value":"cat"}`), Input{Output: "a cat sat"}, true, 1, false, ""},
		{"contains miss", spec(KindContains, `{"value":"dog"}`), Input{Output: "a cat sat"}, false, 0, false, ""},
		{"contains case sensitive", spec(KindContains, `{"value":"CAT"}`), Input{Output: "a cat"}, false, 0, false, ""},
		{"contains case insensitive", spec(KindContains, `{"value":"CAT","caseInsensitive":true}`), Input{Output: "a cat"}, true, 1, false, ""},
		{"contains empty output", spec(KindContains, `{"value":"x"}`), Input{}, false, 0, false, ""},
		{"contains huge output", spec(KindContains, `{"value":"x"}`), Input{Output: big}, false, 0, false, "limit"},
		{"not contains ok", spec(KindNotContains, `{"value":"secret"}`), Input{Output: "fine"}, true, 1, false, ""},
		{"not contains hit", spec(KindNotContains, `{"value":"secret"}`), Input{Output: "the Secret"}, true, 1, false, ""},
		{"not contains hit ci", spec(KindNotContains, `{"value":"secret","caseInsensitive":true}`), Input{Output: "the Secret"}, false, 0, false, "must not"},
		{"not contains huge output fails", spec(KindNotContains, `{"value":"zzz"}`), Input{Output: big}, false, 0, false, "limit"},
		{"regex match", spec(KindRegex, `{"pattern":"^\\d{3}-\\d{4}$"}`), Input{Output: "555-1234"}, true, 1, false, ""},
		{"regex no match", spec(KindRegex, `{"pattern":"^\\d{3}$"}`), Input{Output: "55"}, false, 0, false, ""},
		{"regex mustMatch false ok", spec(KindRegex, `{"pattern":"error","mustMatch":false}`), Input{Output: "all good"}, true, 1, false, ""},
		{"regex mustMatch false hit", spec(KindRegex, `{"pattern":"error","mustMatch":false}`), Input{Output: "an error"}, false, 0, false, "must not"},
		{"regex pathological is linear", spec(KindRegex, `{"pattern":"(a+)+$"}`), Input{Output: strings.Repeat("a", 100000) + "b"}, false, 0, false, ""},
		{"regex huge output", spec(KindRegex, `{"pattern":"a"}`), Input{Output: big}, false, 0, false, "limit"},
		{"schema ok", spec(KindJSONSchema, objSchema), Input{Output: `{"id": 3}`}, true, 1, false, ""},
		{"schema fenced", spec(KindJSONSchema, objSchema), Input{Output: fencedOutput}, true, 1, false, ""},
		{"schema wrong type", spec(KindJSONSchema, objSchema), Input{Output: `{"id": "x"}`}, false, 0, false, "schema"},
		{"schema missing field", spec(KindJSONSchema, objSchema), Input{Output: `{}`}, false, 0, false, "schema"},
		{"schema not json", spec(KindJSONSchema, objSchema), Input{Output: "just words"}, false, 0, false, "not JSON"},
		{"schema empty output", spec(KindJSONSchema, objSchema), Input{}, false, 0, false, "not JSON"},
		{"schema trailing text", spec(KindJSONSchema, objSchema), Input{Output: `{"id":1} extra`}, false, 0, false, "not JSON"},
		{"schema integer as float", spec(KindJSONSchema, objSchema), Input{Output: `{"id": 4.0}`}, true, 1, false, ""},
		{"schema second fence", spec(KindJSONSchema, objSchema), Input{Output: "```\nnot json\n```\n```json\n{\"id\":1}\n```"}, true, 1, false, ""},
		{"schema unterminated fence", spec(KindJSONSchema, objSchema), Input{Output: "```json\n{\"id\":1}"}, false, 0, false, "not JSON"},
		{"field equals string", spec(KindJSONField, `{"path":"user.name","value":"ada"}`), Input{Output: `{"user":{"name":"ada"}}`}, true, 1, false, ""},
		{"field equals number", spec(KindJSONField, `{"path":"n","value":2}`), Input{Output: `{"n":2.0}`}, true, 1, false, ""},
		{"field equals array index", spec(KindJSONField, `{"path":"tags.1","value":"b"}`), Input{Output: fencedOutput}, true, 1, false, ""},
		{"field equals null", spec(KindJSONField, `{"path":"x","value":null}`), Input{Output: `{"x":null}`}, true, 1, false, ""},
		{"field equals object", spec(KindJSONField, `{"path":"o","value":{"a":1}}`), Input{Output: `{"o":{"a":1}}`}, true, 1, false, ""},
		{"field wrong value", spec(KindJSONField, `{"path":"n","value":3}`), Input{Output: `{"n":2}`}, false, 0, false, "expected"},
		{"field type differs", spec(KindJSONField, `{"path":"n","value":"2"}`), Input{Output: `{"n":2}`}, false, 0, false, "expected"},
		{"field missing path", spec(KindJSONField, `{"path":"a.b","value":1}`), Input{Output: `{"a":{}}`}, false, 0, false, "not in the output"},
		{"field index out of range", spec(KindJSONField, `{"path":"t.5","value":1}`), Input{Output: `{"t":[1]}`}, false, 0, false, "not in the output"},
		{"field negative index", spec(KindJSONField, `{"path":"t.-1","value":1}`), Input{Output: `{"t":[1]}`}, false, 0, false, "not in the output"},
		{"field through scalar", spec(KindJSONField, `{"path":"a.b","value":1}`), Input{Output: `{"a":5}`}, false, 0, false, "not in the output"},
		{"field not json", spec(KindJSONField, `{"path":"a","value":1}`), Input{Output: "no"}, false, 0, false, "not JSON"},
		{"tool called", spec(KindToolCalled, `{"tool":"search"}`), Input{Trace: Trace{ToolCalls: []string{"read", "search"}}}, true, 1, false, ""},
		{"tool not called when missing", spec(KindToolCalled, `{"tool":"search"}`), Input{Trace: Trace{ToolCalls: []string{"read"}}}, false, 0, false, ""},
		{"tool called empty trace", spec(KindToolCalled, `{"tool":"search"}`), Input{}, false, 0, false, ""},
		{"tool not called ok", spec(KindToolNotCalled, `{"tool":"delete"}`), Input{Trace: Trace{ToolCalls: []string{"read"}}}, true, 1, false, ""},
		{"tool not called empty trace", spec(KindToolNotCalled, `{"tool":"delete"}`), Input{}, true, 1, false, ""},
		{"tool not called violated", spec(KindToolNotCalled, `{"tool":"delete"}`), Input{Trace: Trace{ToolCalls: []string{"delete"}}}, false, 0, false, "must not"},
		{"tool names are exact", spec(KindToolNotCalled, `{"tool":"delete"}`), Input{Trace: Trace{ToolCalls: []string{"delete_all"}}}, true, 1, false, ""},
		{"cost within", spec(KindMaxCost, `{"usd":0.5}`), Input{Trace: Trace{CostUSD: 0.5}}, true, 1, false, ""},
		{"cost over", spec(KindMaxCost, `{"usd":0.5}`), Input{Trace: Trace{CostUSD: 0.51}}, false, 0, false, "over"},
		{"cost zero budget", spec(KindMaxCost, `{"usd":0}`), Input{}, true, 1, false, ""},
		{"cost unknown", spec(KindMaxCost, `{"usd":1}`), Input{Trace: Trace{CostUSD: -1}}, false, 0, false, "not known"},
		{"latency within", spec(KindMaxLatency, `{"ms":1000}`), Input{Trace: Trace{LatencyMs: 1000}}, true, 1, false, ""},
		{"latency over", spec(KindMaxLatency, `{"ms":1000}`), Input{Trace: Trace{LatencyMs: 1001}}, false, 0, false, "over"},
		{"latency unknown", spec(KindMaxLatency, `{"ms":1000}`), Input{Trace: Trace{LatencyMs: -1}}, false, 0, false, "not known"},
		{"bad config does not panic", spec(KindRegex, `{"pattern":"("}`), Input{Output: "x"}, false, 0, false, "invalid"},
		{"unknown kind does not panic", spec("nope", `{}`), Input{}, false, 0, false, "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Grade(tt.spec, tt.in)
			if got.Pass != tt.pass || got.Score != tt.score || got.NotGraded != tt.notGraded {
				t.Fatalf("got %+v, want pass=%v score=%v notGraded=%v", got, tt.pass, tt.score, tt.notGraded)
			}
			if tt.detail != "" && !strings.Contains(got.Detail, tt.detail) {
				t.Errorf("detail %q lacks %q", got.Detail, tt.detail)
			}
			if len(got.Detail) > MaxDetailBytes {
				t.Errorf("detail is %d bytes", len(got.Detail))
			}
			if got.Pass && got.Score != 1 || !got.Pass && got.Score != 0 {
				t.Errorf("pass %v with score %v", got.Pass, got.Score)
			}
		})
	}
}

func TestRegexIsBounded(t *testing.T) {
	long := `{"pattern":"` + strings.Repeat("a", MaxPatternBytes+1) + `"}`
	if _, err := Normalize(spec(KindRegex, long)); !errors.Is(err, ErrConfig) {
		t.Errorf("a pattern over the cap: %v", err)
	}
	// Lookahead and backreferences are not RE2.
	for _, p := range []string{`(?=a)b`, `(a)\1`, `a{1001}`, `(`, ``} {
		cfg, _ := json.Marshal(map[string]string{"pattern": p})
		if _, err := Normalize(spec(KindRegex, string(cfg))); !errors.Is(err, ErrConfig) {
			t.Errorf("pattern %q was accepted", p)
		}
	}
	start := time.Now()
	r := Grade(spec(KindRegex, `{"pattern":"(x+x+)+y"}`), Input{Output: strings.Repeat("x", MaxTextBytes)})
	if r.Pass {
		t.Error("unexpected match")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a hostile pattern took %v", d)
	}
}

func TestSchemaCannotLoadOtherDocuments(t *testing.T) {
	for _, ref := range []string{`http://127.0.0.1:1/x.json`, `file:///etc/passwd`, `other.json`} {
		cfg := `{"schema":{"$ref":"` + ref + `"}}`
		if _, err := Normalize(spec(KindJSONSchema, cfg)); !errors.Is(err, ErrConfig) {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	// A local ref works.
	ok := `{"schema":{"$defs":{"n":{"type":"integer"}},"properties":{"a":{"$ref":"#/$defs/n"}}}}`
	if r := Grade(spec(KindJSONSchema, ok), Input{Output: `{"a":1}`}); !r.Pass {
		t.Errorf("local ref: %+v", r)
	}
	if r := Grade(spec(KindJSONSchema, ok), Input{Output: `{"a":"x"}`}); r.Pass {
		t.Error("local ref accepted a string")
	}
}

func TestNormalizeErrors(t *testing.T) {
	tests := []struct{ name, kind, cfg string }{
		{"unknown kind", "llm", `{}`},
		{"unknown field", KindContains, `{"value":"a","bogus":1}`},
		{"trailing text", KindContains, `{"value":"a"} {}`},
		{"not an object", KindContains, `[1]`},
		{"contains empty", KindContains, `{}`},
		{"exact bad step", KindExact, `{"normalize":["rot13"]}`},
		{"schema missing", KindJSONSchema, `{}`},
		{"schema not an object", KindJSONSchema, `{"schema":[1]}`},
		{"schema bad type", KindJSONSchema, `{"schema":{"type":"nope"}}`},
		{"schema too big", KindJSONSchema, `{"schema":{"description":"` + strings.Repeat("a", MaxSchemaBytes) + `"}}`},
		{"field no path", KindJSONField, `{"value":1}`},
		{"field no value", KindJSONField, `{"path":"a"}`},
		{"tool empty", KindToolCalled, `{}`},
		{"cost negative", KindMaxCost, `{"usd":-1}`},
		{"latency zero", KindMaxLatency, `{"ms":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Normalize(spec(tt.kind, tt.cfg)); !errors.Is(err, ErrConfig) {
				t.Errorf("err = %v, want ErrConfig", err)
			}
		})
	}
}

func TestNormalizeIsCanonical(t *testing.T) {
	a, err := Normalize(spec(KindContains, `{ "caseInsensitive": true,  "value": "x" }`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Normalize(spec(KindContains, string(a)))
	if string(a) != string(b) {
		t.Errorf("%s != %s", a, b)
	}
	// An empty config is fine for kinds that need none.
	if _, err := Normalize(Spec{Kind: KindExact}); err != nil {
		t.Errorf("exact without config: %v", err)
	}
}

func TestDetailIsClipped(t *testing.T) {
	long := strings.Repeat("é", MaxDetailBytes)
	if got := clip(long); len(got) > MaxDetailBytes || !strings.HasPrefix(long, got) {
		t.Errorf("clip gave %d bytes", len(got))
	}
}

func TestCasePass(t *testing.T) {
	g := func(pass bool, score, w float64, req, ng bool) Graded {
		return Graded{Result: Result{Pass: pass, Score: score, NotGraded: ng}, Weight: w, Required: req}
	}
	tests := []struct {
		name string
		in   []Graded
		want bool
	}{
		{"none", nil, false},
		{"required passes", []Graded{g(true, 1, 1, true, false)}, true},
		{"required fails", []Graded{g(false, 0, 1, true, false), g(true, 1, 1, false, false)}, false},
		{"required not graded", []Graded{g(false, 0, 1, true, true)}, false},
		{"mean at half", []Graded{g(true, 1, 1, false, false), g(false, 0, 1, false, false)}, true},
		{"mean below half", []Graded{g(true, 1, 1, false, false), g(false, 0, 2, false, false)}, false},
		{"not graded left out", []Graded{g(true, 1, 1, false, false), g(false, 0, 5, false, true)}, true},
		{"only not graded", []Graded{g(false, 0, 1, false, true)}, false},
		{"zero weight counts as one", []Graded{g(true, 1, 0, false, false)}, true},
	}
	for _, tt := range tests {
		if got := CasePass(tt.in); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

func FuzzGrade(f *testing.F) {
	f.Add(KindJSONSchema, objSchema, `{"id":1}`)
	f.Add(KindRegex, `{"pattern":"a+"}`, "aaa")
	f.Add(KindJSONField, `{"path":"a.0","value":1}`, "```json\n{\"a\":[1]}\n```")
	f.Fuzz(func(t *testing.T, kind, cfg, out string) {
		r := Grade(spec(kind, cfg), Input{Output: out})
		if len(r.Detail) > MaxDetailBytes || r.Score < 0 || r.Score > 1 {
			t.Fatalf("bad result %+v", r)
		}
	})
}
