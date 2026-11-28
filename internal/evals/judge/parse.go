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

package judge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRationaleBytes is the most rationale text kept (the grade column's limit).
const MaxRationaleBytes = 4096

// ErrInvalidReply is wrapped by every error about a judge reply that cannot be used.
var ErrInvalidReply = errors.New("the judge's reply is not usable")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidReply, fmt.Sprintf(format, a...))
}

// Verdict is a parsed single-output reply.
type Verdict struct {
	Rationale string
	Score     float64
	Pass      bool
}

// PairVerdict is a parsed pairwise reply: A, B or tie.
type PairVerdict struct {
	Rationale string
	Winner    string
}

// Pairwise winners.
const (
	WinnerA   = "A"
	WinnerB   = "B"
	WinnerTie = "tie"
)

// unwrap returns the one JSON object in a reply. The reply must be that object,
// optionally inside one code fence; prose before or after it is refused, since
// text around a verdict is where an injected second verdict hides.
func unwrap(reply string) ([]byte, error) {
	s := strings.TrimSpace(reply)
	if body, ok := strings.CutPrefix(s, "```"); ok {
		nl := strings.IndexByte(body, '\n')
		if nl < 0 || strings.TrimSpace(body[:nl]) != "" && !strings.EqualFold(strings.TrimSpace(body[:nl]), "json") {
			return nil, invalid("the reply is not a JSON object")
		}
		body = strings.TrimSpace(body[nl+1:])
		if !strings.HasSuffix(body, "```") {
			return nil, invalid("the code fence is not closed")
		}
		s = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, invalid("the reply is not a single JSON object")
	}
	return []byte(s), nil
}

// decodeStrict decodes one JSON object into v: no unknown fields, no second
// value, no duplicate key (a duplicate would let a later value hide the first).
func decodeStrict(raw []byte, v any) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("%v", err)
	}
	if dec.More() {
		return invalid("text after the JSON object")
	}
	return nil
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return invalid("%v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return invalid("the reply is not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return invalid("%v", err)
		}
		key, _ := k.(string)
		if seen[key] {
			return invalid("the field %q appears twice", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return invalid("%v", err)
		}
	}
	return nil
}

// CleanRationale makes a rationale safe to store and show: valid UTF-8, no
// control characters (a NUL cannot be stored at all), at most MaxRationaleBytes.
// It stays plain text; it is never rendered as Markdown or HTML.
func CleanRationale(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= MaxRationaleBytes {
		return s
	}
	cut := MaxRationaleBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ParseVerdict reads a single-output reply: {"rationale", "score", "pass"}. All
// three fields are required. The score must be a number from 0 to 1 and pass must
// agree with it (score >= passAt), or the reply is invalid: a reply that
// contradicts itself is not a verdict anyone gave.
func ParseVerdict(reply string, passAt float64) (Verdict, error) {
	raw, err := unwrap(reply)
	if err != nil {
		return Verdict{}, err
	}
	var in struct {
		Rationale *string  `json:"rationale"`
		Score     *float64 `json:"score"`
		Pass      *bool    `json:"pass"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return Verdict{}, err
	}
	switch {
	case in.Rationale == nil:
		return Verdict{}, invalid("the field rationale is missing")
	case in.Score == nil:
		return Verdict{}, invalid("the field score is missing")
	case in.Pass == nil:
		return Verdict{}, invalid("the field pass is missing")
	case math.IsNaN(*in.Score) || math.IsInf(*in.Score, 0) || *in.Score < 0 || *in.Score > 1:
		return Verdict{}, invalid("the score %v is outside 0 to 1", *in.Score)
	case *in.Pass != (*in.Score >= passAt):
		return Verdict{}, invalid("pass is %t but the score is %v and the pass mark %v", *in.Pass, *in.Score, passAt)
	}
	return Verdict{Rationale: CleanRationale(*in.Rationale), Score: *in.Score, Pass: *in.Pass}, nil
}

// ParsePair reads a pairwise reply: {"rationale", "winner"}.
func ParsePair(reply string) (PairVerdict, error) {
	raw, err := unwrap(reply)
	if err != nil {
		return PairVerdict{}, err
	}
	var in struct {
		Rationale *string `json:"rationale"`
		Winner    *string `json:"winner"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return PairVerdict{}, err
	}
	if in.Rationale == nil || in.Winner == nil {
		return PairVerdict{}, invalid("the fields rationale and winner are required")
	}
	var w string
	switch strings.TrimSpace(*in.Winner) {
	case "A", "a":
		w = WinnerA
	case "B", "b":
		w = WinnerB
	case "tie", "Tie", "TIE":
		w = WinnerTie
	default:
		return PairVerdict{}, invalid("the winner %q is not A, B or tie", *in.Winner)
	}
	return PairVerdict{Rationale: CleanRationale(*in.Rationale), Winner: w}, nil
}

// UnwrapObject returns the one JSON object in a model reply, with the same
// strictness as a judge's verdict: the reply is that object, optionally inside
// one code fence, and prose around it is refused. Other callers that read
// structured output from a model (the builder's draft) use it.
func UnwrapObject(reply string) ([]byte, error) { return unwrap(reply) }

// DecodeObject reads a model's reply as the one JSON object it must be, into v,
// refusing prose around it, unknown fields, duplicate keys and a second value.
// Other callers that ask a model for a decision use it.
func DecodeObject(reply string, v any) error {
	raw, err := unwrap(reply)
	if err != nil {
		return err
	}
	return decodeStrict(raw, v)
}
