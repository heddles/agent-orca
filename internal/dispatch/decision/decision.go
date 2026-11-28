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

// Package decision is the contract of a decision model: typed questions about a
// piece of input, answered with a choice and its probabilities instead of prose.
// Agent dispatch asks it which agent a request is for (plans/agent-dispatch.md).
//
// The shape follows the Decisions API of TypeSafe's jev, with one addition:
// Fit, how well the chosen option suits the input on its own. Probabilities only
// compare the options with each other, so they cannot say that none of them fits.
//
// Both deciders answer this contract: the in-cluster service orca-decide, over
// HTTP (Client, Handler), and a tenant's ModelSelector (Chooser).
package decision

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// Question and answer types.
const (
	// TypeChoice asks for one of the keys of Criteria.
	TypeChoice = "choice"
	// TypeRefusal answers a question the decider could not or would not answer.
	TypeRefusal = "refusal"
)

// Limits on a request. A decider may be shared by every tenant, so one request
// cannot be arbitrarily large.
const (
	MaxStateBytes     = 64 << 10
	MaxQuestions      = 4
	MaxOptions        = 128
	MaxCriterionBytes = 4 << 10
	MaxKeyBytes       = 253 + 1 + 253 // namespace/name
)

// Question is one thing to decide about the state.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	// Criteria describes each option of a choice, by option key.
	Criteria map[string]string `json:"criteria,omitempty"`
}

// Request asks Questions, by ID, about State.
type Request struct {
	// Tenant scopes whatever the decider caches. It is never sent to a model.
	Tenant string `json:"tenant,omitempty"`
	// State is the input being decided about. It is untrusted.
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is the answer to one question.
type Answer struct {
	// Type is the question's type, or TypeRefusal.
	Type   string `json:"type"`
	Choice string `json:"choice,omitempty"`
	// Probabilities compare the options with each other and sum to about 1.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence is how sure the decider is of Choice, from 0 to 1.
	Confidence float64 `json:"confidence"`
	// Fit is how well Choice suits the state on its own, from 0 to 1. Nil when
	// the decider cannot tell.
	Fit *float64 `json:"fit,omitempty"`
	// Reason says why a refusal was given. It is for logs, not for users.
	Reason string `json:"reason,omitempty"`
}

// Usage is what answering cost.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// Response holds one answer per question.
type Response struct {
	// Provider and Model name what decided, for the decision record.
	Provider string            `json:"provider,omitempty"`
	Model    string            `json:"model,omitempty"`
	Answers  map[string]Answer `json:"answers"`
	Usage    Usage             `json:"usage"`
}

// Decider answers decision requests. On error the Response still carries the
// usage of any call that was made and charged.
type Decider interface {
	Decide(ctx context.Context, req Request) (Response, error)
}

// ErrInvalidRequest is wrapped by every error about a malformed request.
var ErrInvalidRequest = errors.New("invalid decision request")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, a...))
}

// Validate checks a request against the limits.
func (r Request) Validate() error {
	switch {
	case r.State == "":
		return invalid("state is empty")
	case len(r.State) > MaxStateBytes:
		return invalid("state is over %d bytes", MaxStateBytes)
	case !utf8.ValidString(r.State):
		return invalid("state is not valid UTF-8")
	case len(r.Questions) == 0:
		return invalid("there are no questions")
	case len(r.Questions) > MaxQuestions:
		return invalid("more than %d questions", MaxQuestions)
	}
	for id, q := range r.Questions {
		if q.Type == "" {
			return invalid("question %q has no type", id)
		}
		if len(q.Instructions) > MaxCriterionBytes {
			return invalid("the instructions of question %q are over %d bytes", id, MaxCriterionBytes)
		}
		if q.Type != TypeChoice {
			continue
		}
		if len(q.Criteria) == 0 {
			return invalid("choice %q has no options", id)
		}
		if len(q.Criteria) > MaxOptions {
			return invalid("choice %q has more than %d options", id, MaxOptions)
		}
		for k, c := range q.Criteria {
			if k == "" || len(k) > MaxKeyBytes {
				return invalid("choice %q has an empty or oversized option key", id)
			}
			if len(c) > MaxCriterionBytes {
				return invalid("option %q of choice %q is over %d bytes", k, id, MaxCriterionBytes)
			}
		}
	}
	return nil
}

// Refusal is an answer that decides nothing.
func Refusal(reason string) Answer { return Answer{Type: TypeRefusal, Reason: reason} }

// ChoiceAnswer reads the answer to a choice question. It reports false, meaning
// "not answered", for anything but a well-formed choice of one of the question's
// own options: a missing answer, a refusal, another type, an option that was not
// offered, or a number outside 0 to 1. A decider is never trusted to name
// something the caller did not offer it.
func (r Response) ChoiceAnswer(id string, q Question) (Answer, bool) {
	a, ok := r.Answers[id]
	if !ok || a.Type != TypeChoice {
		return Answer{}, false
	}
	if _, offered := q.Criteria[a.Choice]; !offered {
		return Answer{}, false
	}
	if !unit(a.Confidence) || a.Fit != nil && !unit(*a.Fit) {
		return Answer{}, false
	}
	for k, p := range a.Probabilities {
		if _, offered := q.Criteria[k]; !offered || !unit(p) {
			return Answer{}, false
		}
	}
	return a, true
}

func unit(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }
