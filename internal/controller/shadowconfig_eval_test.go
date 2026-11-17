package controller

import (
	"testing"

	"github.com/heddles/agent-orca/internal/tenancy"
)

// An evaluation run is never sampled for a routing benchmark, however the
// benchmark is set up: it would spend the benchmark's budget on traffic that is
// not production and count the call twice.
func TestResolveShadow_EvaluationRunsAreNeverSampled(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"eval label":       {tenancy.LabelEval: tenancy.LabelEvalValue},
		"experiment label": {tenancy.LabelEvalExperiment: "42"},
		"both":             {tenancy.LabelEval: tenancy.LabelEvalValue, tenancy.LabelEvalExperiment: "42", "team": "a"},
	} {
		got, err := shadowFixtureFor(t, shadowSource{RunLabels: labels}, nil)
		if err != nil || got.Config != nil || len(got.Aux) != 0 {
			t.Errorf("%s: shadow = %+v, %v; want none", name, got, err)
		}
	}
	// The same benchmark still samples a production run, and one labelled "false".
	for name, labels := range map[string]map[string]string{
		"no labels":  nil,
		"other":      {"team": "a"},
		"eval false": {tenancy.LabelEval: "false"},
	} {
		got, err := shadowFixtureFor(t, shadowSource{RunLabels: labels}, nil)
		if err != nil || got.Config == nil {
			t.Errorf("%s: production run was not sampled: %+v, %v", name, got, err)
		}
	}
}
