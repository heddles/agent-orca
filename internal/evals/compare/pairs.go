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
	"fmt"
	"slices"
)

// Winner of one paired case, from the baseline's point of view.
type Winner string

// The winners. The baseline arm plays "served" in routing evidence, the
// candidate "shadow".
const (
	WinnerBaseline  Winner = "baseline"
	WinnerCandidate Winner = "candidate"
	WinnerTie       Winner = "tie"
)

// PairedCase is the outcome of one case in two arms.
type PairedCase struct {
	CaseID           int64
	Winner           Winner
	BaselineCostUSD  float64
	CandidateCostUSD float64
}

// Pairs reads, for every case with a usable result in both arms, who won: the
// arm that passed it when only one did, a tie when both passed or both failed.
// It uses the same case outcomes as Summarize (graders.CasePass, repeats folded
// by majority, results with a grade in error or pending review left out), so a
// routing finding built from it agrees with the experiment's own comparison.
// Cases are in ascending order.
func Pairs(rows []Row, baseline, candidate string) ([]PairedCase, error) {
	if baseline == candidate {
		return nil, invalidf("an arm cannot be paired with itself")
	}
	byArm := map[string]map[int64][]Row{baseline: {}, candidate: {}}
	for _, r := range rows {
		if err := validateRow(r); err != nil {
			return nil, err
		}
		if m, ok := byArm[r.Arm]; ok {
			m[r.CaseID] = append(m[r.CaseID], r)
		}
	}
	if len(byArm[baseline]) == 0 {
		return nil, fmt.Errorf("%w %q", ErrNoBaseline, baseline)
	}
	ids := make([]int64, 0, len(byArm[baseline]))
	for id := range byArm[baseline] {
		if _, ok := byArm[candidate][id]; ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out []PairedCase
	for _, id := range ids {
		b, err := foldCase(byArm[baseline][id])
		if err != nil {
			return nil, err
		}
		c, err := foldCase(byArm[candidate][id])
		if err != nil {
			return nil, err
		}
		if !b.usable || !c.usable {
			continue
		}
		w := WinnerTie
		switch {
		case b.pass && !c.pass:
			w = WinnerBaseline
		case c.pass && !b.pass:
			w = WinnerCandidate
		}
		out = append(out, PairedCase{CaseID: id, Winner: w, BaselineCostUSD: b.cost, CandidateCostUSD: c.cost})
	}
	return out, nil
}
