package calibrate

import (
	"errors"
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestKappaKnownValues(t *testing.T) {
	cases := map[string]struct {
		c       Confusion
		kappa   float64
		defined bool
	}{
		// Textbook example: 20 yes/yes, 5 yes/no, 10 no/yes, 15 no/no: po 0.7, pe 0.5, kappa 0.4.
		"textbook":          {Confusion{BothPass: 20, LLMOnly: 5, HumanOnly: 10, BothFail: 15}, 0.4, true},
		"perfect":           {Confusion{BothPass: 12, BothFail: 8}, 1, true},
		"opposite":          {Confusion{LLMOnly: 10, HumanOnly: 10}, -1, true},
		"chance":            {Confusion{BothPass: 25, LLMOnly: 25, HumanOnly: 25, BothFail: 25}, 0, true},
		"both always pass":  {Confusion{BothPass: 30}, 0, false},
		"both always fail":  {Confusion{BothFail: 30}, 0, false},
		"empty":             {Confusion{}, 0, false},
		"judge always pass": {Confusion{BothPass: 15, LLMOnly: 15}, 0, true},
	}
	for name, c := range cases {
		k, ok := Kappa(c.c)
		if ok != c.defined || !near(k, c.kappa) {
			t.Errorf("%s: kappa = %v (defined %v), want %v (%v)", name, k, ok, c.kappa, c.defined)
		}
	}
}

func pairs(bothPass, bothFail, llmOnly, humanOnly int) []Pair {
	var out []Pair
	id := int64(0)
	add := func(n int, llm, human bool) {
		for range n {
			id++
			p := Pair{ResultID: id, CaseID: id, LLMPass: llm, HumanPass: human}
			if llm {
				p.LLMScore = 0.9
			}
			if human {
				p.HumanScore = 1
			}
			out = append(out, p)
		}
	}
	add(bothPass, true, true)
	add(bothFail, false, false)
	add(llmOnly, true, false)
	add(humanOnly, false, true)
	return out
}

func TestComputeReportsAgreementKappaAndDisagreements(t *testing.T) {
	r, err := Compute(pairs(20, 15, 5, 10))
	if err != nil {
		t.Fatal(err)
	}
	if r.Pairs != 50 || r.Agreement != 0.7 || r.Kappa != 0.4 || !r.KappaDefined || !r.Trustworthy {
		t.Errorf("report = %+v", r)
	}
	if r.Confusion != (Confusion{BothPass: 20, BothFail: 15, LLMOnly: 5, HumanOnly: 10}) {
		t.Errorf("confusion = %+v", r.Confusion)
	}
	if len(r.Disagreements) != 15 || r.DisagreementsCut {
		t.Errorf("disagreements = %d", len(r.Disagreements))
	}
	for _, d := range r.Disagreements {
		if d.LLMPass == d.HumanPass {
			t.Errorf("an agreement was listed: %+v", d)
		}
	}
}

func TestComputeMarksALowKappaNotTrustworthy(t *testing.T) {
	r, err := Compute(pairs(10, 10, 5, 5))
	if err != nil || r.Kappa >= TrustworthyKappa || r.Trustworthy || r.Agreement != 0.6667 {
		t.Errorf("low kappa: %+v, %v", r, err)
	}
	// Agreement of 100% on a single class says nothing about the judge.
	r, err = Compute(pairs(30, 0, 0, 0))
	if err != nil || r.Agreement != 1 || r.KappaDefined || r.Trustworthy || r.Kappa != 0 {
		t.Errorf("one class: %+v, %v", r, err)
	}
	r, _ = Compute(pairs(12, 8, 0, 0))
	if r.Kappa != 1 || !r.Trustworthy {
		t.Errorf("perfect: %+v", r)
	}
}

func TestComputeNeedsEnoughPairs(t *testing.T) {
	r, err := Compute(pairs(10, 9, 0, 0))
	if !errors.Is(err, ErrTooFewPairs) || r.Pairs != 19 || r.Disagreements == nil {
		t.Errorf("19 pairs: %+v, %v", r, err)
	}
	if _, err := Compute(pairs(10, 10, 0, 0)); err != nil {
		t.Errorf("20 pairs: %v", err)
	}
	if _, err := Compute(nil); !errors.Is(err, ErrTooFewPairs) {
		t.Errorf("no pairs: %v", err)
	}
}

func TestComputeListsTheWorstDisagreementsFirstAndBoundsThem(t *testing.T) {
	p := pairs(10, 10, 0, 0)
	for i := range 150 {
		p = append(p, Pair{ResultID: int64(1000 + i), LLMPass: true, HumanPass: false, LLMScore: 0.7 + float64(i)/1000, HumanScore: 0.5})
	}
	r, err := Compute(p)
	if err != nil || len(r.Disagreements) != MaxDisagreements || !r.DisagreementsCut {
		t.Fatalf("report = %d disagreements, cut %v, %v", len(r.Disagreements), r.DisagreementsCut, err)
	}
	if r.Disagreements[0].ResultID != 1149 {
		t.Errorf("the largest gap is first, got result %d", r.Disagreements[0].ResultID)
	}
}
