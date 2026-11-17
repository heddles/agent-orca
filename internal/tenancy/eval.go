package tenancy

import "strconv"

// Labels of evaluation experiments (docs/evaluations.md). The experiment runner
// puts them on the AgentRuns and on the hidden Agents and ModelSelectors it
// makes for an experiment. Every path that reports on production traffic (costs,
// run history, the fleet view, shadow sampling) leaves objects with LabelEval
// out.
const (
	// LabelEval is "true" on every AgentRun of an experiment.
	LabelEval = "agentorca.io/eval"
	// LabelEvalExperiment is the experiment's ID, on runs and on materialised objects.
	LabelEvalExperiment = "agentorca.io/eval-experiment"
	// LabelEvalArm is the arm's label, on runs and on materialised objects.
	LabelEvalArm = "agentorca.io/eval-arm"
	// LabelEvalCase is the case ID, on runs.
	LabelEvalCase = "agentorca.io/eval-case"
	// LabelEvalRepeat is the repeat number, on runs.
	LabelEvalRepeat = "agentorca.io/eval-repeat"
	// LabelEvalValue is the value of LabelEval.
	LabelEvalValue = "true"
)

// IsEval reports whether labels mark an object as part of an evaluation experiment.
func IsEval(labels map[string]string) bool {
	return labels[LabelEval] == LabelEvalValue || labels[LabelEvalExperiment] != ""
}

// ExperimentLabel formats an experiment ID for LabelEvalExperiment.
func ExperimentLabel(id int64) string { return strconv.FormatInt(id, 10) }
