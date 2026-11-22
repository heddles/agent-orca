package postgresql

import (
	"errors"
	"testing"
)

func TestEvalExperimentValidate(t *testing.T) {
	ok := EvalExperiment{
		Arms:      []EvalArm{{Label: "base", Agent: "support"}, {Label: "cand", Agent: "support", Revision: 3, ModelProvider: "cheap"}},
		BudgetUSD: 5, CreatedBy: "ada",
	}
	ok.Normalize()
	if err := ok.Validate(EvalExperimentLimits{}); err != nil {
		t.Fatalf("valid experiment refused: %v", err)
	}
	if ok.RunBudgetUSD != 1 || ok.Repeats != 1 || ok.Concurrency != 4 || ok.Purpose != PurposeCompare {
		t.Errorf("defaults = %+v", ok)
	}
	for name, mutate := range map[string]func(*EvalExperiment){
		"five arms":      func(e *EvalExperiment) { e.Arms = append(e.Arms, make([]EvalArm, 3)...) },
		"upper label":    func(e *EvalExperiment) { e.Arms[0].Label = "Base" },
		"long label":     func(e *EvalExperiment) { e.Arms[0].Label = "this-label-is-far-too-long" },
		"duplicate":      func(e *EvalExperiment) { e.Arms[1].Label = "base" },
		"bad hash":       func(e *EvalExperiment) { e.Arms[0].Hash = "xyz" },
		"negative rev":   func(e *EvalExperiment) { e.Arms[0].Revision = -1 },
		"repeats":        func(e *EvalExperiment) { e.Repeats = 6 },
		"concurrency":    func(e *EvalExperiment) { e.Concurrency = 17 },
		"budget":         func(e *EvalExperiment) { e.BudgetUSD = 100 },
		"run over total": func(e *EvalExperiment) { e.RunBudgetUSD = 6 },
		"purpose":        func(e *EvalExperiment) { e.Purpose = "x" },
		"timeout":        func(e *EvalExperiment) { e.TimeoutSeconds = 1 },
	} {
		e := ok
		e.Arms = append([]EvalArm(nil), ok.Arms...)
		mutate(&e)
		if err := e.Validate(EvalExperimentLimits{}); !errors.Is(err, ErrInvalidEval) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestEvalCheckExperimentValidate(t *testing.T) {
	spec := []byte(`{"systemPrompt":"hi"}`)
	ok := EvalExperiment{
		Purpose: PurposeCheck, MaxCases: 25, BudgetUSD: 0.25, CreatedBy: "ada",
		Arms: []EvalArm{{Label: "published", Agent: "support"}, {Label: "draft", Agent: "support", Spec: spec}},
	}
	ok.Normalize()
	if err := ok.Validate(EvalExperimentLimits{}); err != nil {
		t.Fatalf("valid check refused: %v", err)
	}
	one := ok
	one.Arms = ok.Arms[1:]
	if err := one.Validate(EvalExperimentLimits{}); err != nil {
		t.Errorf("a check with only the draft: %v", err)
	}
	for name, mutate := range map[string]func(*EvalExperiment){
		"no arms":            func(e *EvalExperiment) { e.Arms = nil },
		"three arms":         func(e *EvalExperiment) { e.Arms = append(e.Arms, EvalArm{Label: "c", Agent: "x"}) },
		"spec with revision": func(e *EvalExperiment) { e.Arms[1].Revision = 2 },
		"spec too big":       func(e *EvalExperiment) { e.Arms[1].Spec = make([]byte, MaxArmSpecBytes+1) },
		"spec on a compare":  func(e *EvalExperiment) { e.Purpose = PurposeCompare },
		"negative max":       func(e *EvalExperiment) { e.MaxCases = -1 },
	} {
		e := ok
		e.Arms = append([]EvalArm(nil), ok.Arms...)
		mutate(&e)
		if err := e.Validate(EvalExperimentLimits{}); !errors.Is(err, ErrInvalidEval) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// maxCases is for checks only.
	cmp := EvalExperiment{Arms: []EvalArm{{Label: "a", Agent: "x"}, {Label: "b", Agent: "x", Revision: 1}}, BudgetUSD: 1, CreatedBy: "ada", MaxCases: 5}
	cmp.Normalize()
	if err := cmp.Validate(EvalExperimentLimits{}); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("maxCases on a compare: %v", err)
	}
}
