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

import (
	"errors"
	"reflect"
	"testing"
)

func TestPairsWinnersAndExclusions(t *testing.T) {
	row := func(arm string, c int64, pass bool, cost float64) Row {
		score := 0.0
		if pass {
			score = 1
		}
		return Row{CaseID: c, Arm: arm, Phase: PhaseSucceeded, CostUSD: cost, CostComplete: true,
			Grades: []Grade{{GraderID: 1, Score: score, Pass: pass, Status: GradeOK, Weight: 1, Required: true}}}
	}
	rows := []Row{
		row("a", 1, true, 0.1), row("b", 1, true, 0.01), // both pass: tie
		row("a", 2, true, 0.1), row("b", 2, false, 0.01), // baseline wins
		row("a", 3, false, 0.1), row("b", 3, true, 0.01), // candidate wins
		row("a", 4, false, 0.1), row("b", 4, false, 0.01), // both fail: tie
		row("a", 5, true, 0.1), row("b", 5, true, 0.01), // candidate grade pending: left out
		row("a", 6, true, 0.1), // no candidate result: left out
		row("c", 1, false, 0),  // another arm: ignored
	}
	rows[8].Grades[0].Status = GradePendingReview
	got, err := Pairs(rows, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	want := []PairedCase{
		{CaseID: 1, Winner: WinnerTie, BaselineCostUSD: 0.1, CandidateCostUSD: 0.01},
		{CaseID: 2, Winner: WinnerBaseline, BaselineCostUSD: 0.1, CandidateCostUSD: 0.01},
		{CaseID: 3, Winner: WinnerCandidate, BaselineCostUSD: 0.1, CandidateCostUSD: 0.01},
		{CaseID: 4, Winner: WinnerTie, BaselineCostUSD: 0.1, CandidateCostUSD: 0.01},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pairs = %+v\nwant   %+v", got, want)
	}
	if _, err := Pairs(rows, "x", "b"); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("no baseline: %v", err)
	}
	if _, err := Pairs(rows, "a", "a"); err == nil {
		t.Error("an arm paired with itself should be refused")
	}
}
