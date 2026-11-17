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

package compare

import "fmt"

// Verdict is the one-word answer to "is the candidate better than the baseline".
type Verdict string

// The verdicts. NotEnoughData and NoClearDifference are the grey chip.
const (
	VerdictBetter            Verdict = "better"
	VerdictWorse             Verdict = "worse"
	VerdictNoClearDifference Verdict = "no clear difference"
	VerdictNotEnoughData     Verdict = "not enough data"
)

// VerdictResult is a verdict with what it rests on.
type VerdictResult struct {
	Verdict Verdict `json:"verdict"`
	// Conclusive is true only for better and worse. When false the headline
	// should read "Not enough data to tell" and show the interval.
	Conclusive bool `json:"conclusive"`
	// NonInferior is true when there were enough cases and the interval's lower
	// bound is above minus the margin: the candidate is no worse than the
	// baseline by more than the margin. It can be true with any verdict but
	// worse, and is how "the cheaper model is no worse" is stated precisely.
	NonInferior bool    `json:"nonInferior"`
	Margin      float64 `json:"margin"`
	MinCases    int     `json:"minCases"`
	// Reason is one plain sentence saying why.
	Reason string `json:"reason"`
}

// Decide turns a paired pass-rate comparison into a verdict. The rules, in
// order:
//
//  1. Fewer than MinCases paired cases: not enough data, whatever the numbers.
//  2. The interval for the difference lies wholly above zero and McNemar's test
//     is significant at 1-confidence: better.
//  3. The interval lies wholly below zero and McNemar's test is significant:
//     worse.
//  4. Otherwise: no clear difference.
//
// Both the interval and the test must agree before better or worse is said,
// which is the cautious reading when they differ at the edge. The margin does
// not move these thresholds; it only sets NonInferior.
func Decide(d PassRateDiff, opts Options) (VerdictResult, error) {
	o, err := opts.WithDefaults()
	if err != nil {
		return VerdictResult{}, err
	}
	v := VerdictResult{Margin: o.Margin, MinCases: o.MinCases}
	if d.N < o.MinCases {
		v.Verdict = VerdictNotEnoughData
		v.Reason = fmt.Sprintf("%d paired cases; at least %d are needed to say anything", d.N, o.MinCases)
		return v, nil
	}
	alpha := 1 - o.Confidence
	significant := d.McNemar.P < alpha
	v.NonInferior = d.CI.Lo > -o.Margin
	switch {
	case d.CI.Lo > 0 && significant:
		v.Verdict, v.Conclusive = VerdictBetter, true
		v.Reason = fmt.Sprintf("the candidate passes %d cases the baseline fails and fails %d it passes; the difference is above zero", d.Counts.Improved, d.Counts.Regressed)
	case d.CI.Hi < 0 && significant:
		v.Verdict, v.Conclusive = VerdictWorse, true
		v.Reason = fmt.Sprintf("the candidate fails %d cases the baseline passes and passes %d it fails; the difference is below zero", d.Counts.Regressed, d.Counts.Improved)
	default:
		v.Verdict = VerdictNoClearDifference
		v.Reason = "the interval for the difference includes zero, so a real difference cannot be told from chance"
	}
	return v, nil
}
