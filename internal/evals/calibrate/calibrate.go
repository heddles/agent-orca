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

// Package calibrate measures how far an llm grader agrees with human labels
// (docs/evaluations.md). Given the results both graded, it reports the share of
// results the two agree on, Cohen's kappa, a confusion table and the
// disagreements, and marks a judge whose kappa is under TrustworthyKappa as not
// trustworthy. It is pure: no store, no model.
package calibrate

import (
	"errors"
	"math"
	"sort"
)

// Thresholds.
const (
	// MinPairs is the fewest results with both grades for a calibration to be reported.
	MinPairs = 20
	// TrustworthyKappa is the kappa under which a grader is "not trustworthy".
	TrustworthyKappa = 0.4
	// MaxDisagreements bounds the disagreements listed.
	MaxDisagreements = 100
)

// ErrTooFewPairs: fewer than MinPairs results have both grades.
var ErrTooFewPairs = errors.New("calibrate: too few results graded by both the judge and a human")

// Pair is one result's pass verdicts.
type Pair struct {
	ResultID   int64
	CaseID     int64
	LLMPass    bool
	HumanPass  bool
	LLMScore   float64
	HumanScore float64
}

// Confusion is the 2x2 table of judge against human.
type Confusion struct {
	// BothPass and BothFail are the agreements; LLMOnly is where the judge passed
	// and the human failed, HumanOnly the reverse.
	BothPass  int `json:"bothPass"`
	BothFail  int `json:"bothFail"`
	LLMOnly   int `json:"llmPassHumanFail"`
	HumanOnly int `json:"llmFailHumanPass"`
}

// Disagreement is a result the judge and the human graded differently, so the
// author can read both and fix the rubric.
type Disagreement struct {
	ResultID   int64   `json:"resultId"`
	CaseID     int64   `json:"caseId"`
	LLMScore   float64 `json:"llmScore"`
	HumanScore float64 `json:"humanScore"`
	LLMPass    bool    `json:"llmPass"`
	HumanPass  bool    `json:"humanPass"`
}

// Report is the calibration of one llm grader against one human grader.
type Report struct {
	Pairs     int       `json:"pairs"`
	Agreement float64   `json:"agreement"`
	Kappa     float64   `json:"kappa"`
	Confusion Confusion `json:"confusion"`
	// Trustworthy is false when Kappa is under TrustworthyKappa. A grader that is
	// not trustworthy cannot be part of a gate unless an admin accepts it.
	Trustworthy bool `json:"trustworthy"`
	// KappaDefined is false when kappa has no value: every grade of one side is the
	// same and the two agree by that alone (chance agreement is 1). Kappa is 0
	// then, and Trustworthy is false, since nothing shows the judge can tell good
	// from bad.
	KappaDefined  bool           `json:"kappaDefined"`
	Disagreements []Disagreement `json:"disagreements"`
	// DisagreementsCut is true when more than MaxDisagreements were found.
	DisagreementsCut bool `json:"disagreementsCut,omitempty"`
}

// Kappa is Cohen's kappa of two raters over a 2x2 table, and whether it is
// defined. kappa = (po - pe) / (1 - pe), where po is the observed agreement and pe
// the agreement expected by chance from each rater's own pass rate.
func Kappa(c Confusion) (kappa float64, defined bool) {
	n := float64(c.BothPass + c.BothFail + c.LLMOnly + c.HumanOnly)
	if n == 0 {
		return 0, false
	}
	po := float64(c.BothPass+c.BothFail) / n
	llmPass := float64(c.BothPass+c.LLMOnly) / n
	humanPass := float64(c.BothPass+c.HumanOnly) / n
	pe := llmPass*humanPass + (1-llmPass)*(1-humanPass)
	if pe >= 1 {
		return 0, false
	}
	return (po - pe) / (1 - pe), true
}

// Compute calibrates from pairs. It returns ErrTooFewPairs under MinPairs.
func Compute(pairs []Pair) (Report, error) {
	if len(pairs) < MinPairs {
		return Report{Pairs: len(pairs), Disagreements: []Disagreement{}}, ErrTooFewPairs
	}
	var c Confusion
	dis := []Disagreement{}
	for _, p := range pairs {
		switch {
		case p.LLMPass && p.HumanPass:
			c.BothPass++
		case !p.LLMPass && !p.HumanPass:
			c.BothFail++
		default:
			if p.LLMPass {
				c.LLMOnly++
			} else {
				c.HumanOnly++
			}
			dis = append(dis, Disagreement{ResultID: p.ResultID, CaseID: p.CaseID, LLMScore: p.LLMScore, HumanScore: p.HumanScore, LLMPass: p.LLMPass, HumanPass: p.HumanPass})
		}
	}
	k, defined := Kappa(c)
	r := Report{
		Pairs: len(pairs), Confusion: c, Kappa: round(k), KappaDefined: defined,
		Agreement:   round(float64(c.BothPass+c.BothFail) / float64(len(pairs))),
		Trustworthy: defined && round(k) >= TrustworthyKappa,
	}
	// The worst disagreements first: the biggest gap between the two scores.
	sort.SliceStable(dis, func(i, j int) bool {
		return math.Abs(dis[i].LLMScore-dis[i].HumanScore) > math.Abs(dis[j].LLMScore-dis[j].HumanScore)
	})
	if len(dis) > MaxDisagreements {
		dis, r.DisagreementsCut = dis[:MaxDisagreements], true
	}
	r.Disagreements = dis
	return r, nil
}

func round(v float64) float64 { return math.Round(v*10000) / 10000 }
