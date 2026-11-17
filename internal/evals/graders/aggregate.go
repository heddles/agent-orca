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

// CasePassMean is the weighted mean score the non-required graders must reach.
const CasePassMean = 0.5

// Graded is one grader's result with the weight and required flag of the grader.
type Graded struct {
	Result
	Weight   float64
	Required bool
}

// CasePass applies the case rule of docs/evaluations.md: every required grader
// passes, and the weighted mean score of the others is at least CasePassMean.
// A grader that was not graded is a failure when required and is left out of
// the mean otherwise. A case with nothing graded does not pass.
func CasePass(gs []Graded) bool {
	var sum, weights float64
	graded := false
	for _, g := range gs {
		if g.Required {
			if !g.Pass {
				return false
			}
			graded = true
			continue
		}
		if g.NotGraded {
			continue
		}
		graded = true
		w := g.Weight
		if w <= 0 {
			w = 1
		}
		sum += w * g.Score
		weights += w
	}
	if !graded {
		return false
	}
	return weights == 0 || sum/weights >= CasePassMean
}
