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

package decision

import (
	"context"
	"errors"
	"maps"
)

// Escalating asks First, and asks Second only about the choices First was unsure
// of or did not answer: a fast local decider in front of a model that is slower
// and costs money. A tenant opts in to it.
//
// Where Second gives no answer either, First's stands, so the caller's own
// threshold still sends an unsure request to the default agent.
type Escalating struct {
	First, Second Decider
	// Threshold is the confidence below which First's choice is asked again.
	Threshold float64
}

var _ Decider = Escalating{}

// Decide answers req. The Response names Second, and says it escalated, when
// any answer is Second's. Usage covers both.
func (e Escalating) Decide(ctx context.Context, req Request) (Response, error) {
	first, firstErr := e.First.Decide(ctx, req)
	again := req
	again.Questions = map[string]Question{}
	for id, q := range req.Questions {
		if q.Type != TypeChoice {
			continue
		}
		if a, ok := first.ChoiceAnswer(id, q); firstErr != nil || !ok || a.Confidence < e.Threshold {
			again.Questions[id] = q
		}
	}
	if len(again.Questions) == 0 || ctx.Err() != nil {
		return first, firstErr
	}

	second, secondErr := e.Second.Decide(ctx, again)
	out := first
	out.Usage = Usage{
		InputTokens:  first.Usage.InputTokens + second.Usage.InputTokens,
		OutputTokens: first.Usage.OutputTokens + second.Usage.OutputTokens,
		CostUSD:      first.Usage.CostUSD + second.Usage.CostUSD,
	}
	answers := map[string]Answer{}
	if firstErr == nil {
		maps.Copy(answers, first.Answers)
	}
	for id, q := range again.Questions {
		if a, ok := second.ChoiceAnswer(id, q); secondErr == nil && ok {
			answers[id] = a
			out.Escalated, out.Provider, out.Model = true, second.Provider, second.Model
		}
	}
	out.Answers = answers
	if firstErr != nil && !out.Escalated {
		return out, errors.Join(firstErr, secondErr)
	}
	return out, nil
}
