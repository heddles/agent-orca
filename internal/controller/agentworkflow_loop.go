package controller

import (
	"fmt"
	"regexp"
	"strings"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// iterStepName is the name a step's runs, pods, jobs and sandboxes carry: the step
// name on the first pass, then name-i1, name-i2 and so on. A loop pass must not
// reuse the names of the pass before it: a sandbox job that finished is still
// reported under its name, and a finished AgentRun still exists.
func iterStepName(name string, iteration int32) string {
	if iteration <= 0 {
		return name
	}
	return fmt.Sprintf("%s-i%d", name, iteration)
}

func loopMaxIterations(l *agentorcav1alpha1.WorkflowLoop) int32 { return l.Passes() }

// loopBody returns the names of the steps a loop on gate re-runs (see
// agentorcav1alpha1.LoopBody).
func loopBody(steps []agentorcav1alpha1.WorkflowStep, gate *agentorcav1alpha1.WorkflowStep) map[string]bool {
	return agentorcav1alpha1.LoopBody(steps, gate)
}

// validateWorkflowLoops checks every loop's shape. A loop body cannot contain an
// approval (its stored decision belongs to the step, not to a pass) or another
// gate (loops do not nest).
func validateWorkflowLoops(steps []agentorcav1alpha1.WorkflowStep) []string {
	var problems []string
	byName := make(map[string]*agentorcav1alpha1.WorkflowStep, len(steps))
	for i := range steps {
		byName[steps[i].Name] = &steps[i]
	}
	for i := range steps {
		gate := &steps[i]
		if gate.Loop == nil {
			continue
		}
		add := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("step %q loop: ", gate.Name)+fmt.Sprintf(format, args...))
		}
		if stepType(gate) == agentorcav1alpha1.WorkflowStepTypeApproval {
			add("an approval step cannot be a loop gate")
			continue
		}
		if err := checkCondition(gate.Loop.Until, byName); err != nil {
			add("until: %v", err)
		}
		for _, b := range gate.Loop.Break {
			if err := checkCondition(b, byName); err != nil {
				add("break: %v", err)
			}
		}
		body := loopBody(steps, gate)
		if body == nil {
			add("from %q must be this step or one of its dependencies", gate.Loop.From)
			continue
		}
		for name := range body {
			switch {
			case stepType(byName[name]) == agentorcav1alpha1.WorkflowStepTypeApproval:
				add("the body includes approval step %q", name)
			case name != gate.Name && byName[name].Loop != nil:
				add("the body includes %q, which is another loop's gate", name)
			}
		}
	}
	return problems
}

// condTerm is one term of a condition: [!]steps["x"].<field>[.method("arg")] [== "v"].
var condTerm = regexp.MustCompile(`^(!?)\s*steps\["([^"]+)"\]\.(output|phase)(?:\.(contains|startsWith)\("((?:[^"\\]|\\.)*)"\)|\s*(==|!=)\s*"((?:[^"\\]|\\.)*)")?$`)

type condTermParts struct {
	not          bool
	step, field  string
	method, arg  string // method call form
	cmp, cmpWith string // comparison form
}

func parseCondition(expr string) ([]condTermParts, error) {
	var terms []condTermParts
	for raw := range strings.SplitSeq(expr, "&&") {
		m := condTerm.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			return nil, fmt.Errorf("unsupported condition %q", strings.TrimSpace(raw))
		}
		t := condTermParts{not: m[1] == "!", step: m[2], field: m[3], method: m[4], arg: unescapeCond(m[5]), cmp: m[6], cmpWith: unescapeCond(m[7])}
		if t.method == "" && t.cmp == "" {
			return nil, fmt.Errorf("condition %q needs contains, startsWith, == or !=", strings.TrimSpace(raw))
		}
		if t.method != "" && t.field != "output" {
			return nil, fmt.Errorf("%s applies to output, not %s", t.method, t.field)
		}
		terms = append(terms, t)
	}
	return terms, nil
}

func unescapeCond(s string) string {
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s)
}

func checkCondition(expr string, steps map[string]*agentorcav1alpha1.WorkflowStep) error {
	terms, err := parseCondition(expr)
	if err != nil {
		return err
	}
	for _, t := range terms {
		if _, ok := steps[t.step]; !ok {
			return fmt.Errorf("step %q does not exist", t.step)
		}
	}
	return nil
}

// evalLoopCondition evaluates an until expression against the steps' status.
func evalLoopCondition(expr string, index map[string]*agentorcav1alpha1.WorkflowStepStatus) (bool, error) {
	terms, err := parseCondition(expr)
	if err != nil {
		return false, err
	}
	for _, t := range terms {
		ss := index[t.step]
		val := ""
		if ss != nil {
			val = ss.Output
			if t.field == "phase" {
				val = string(ss.Phase)
			}
		}
		var ok bool
		switch {
		case t.method == "contains":
			ok = strings.Contains(val, t.arg)
		case t.method == "startsWith":
			ok = strings.HasPrefix(strings.TrimSpace(val), t.arg)
		case t.cmp == "==":
			ok = val == t.cmpWith
		default:
			ok = val != t.cmpWith
		}
		if ok == t.not {
			return false, nil
		}
	}
	return true, nil
}

// advanceLoop runs when a gate step has just succeeded. It leaves the gate
// Succeeded when until holds. Otherwise it sends the body back for another pass,
// or, when the passes are used up, fails the gate. It reports whether the body
// was reset.
func advanceLoop(wf *agentorcav1alpha1.AgentWorkflow, gate *agentorcav1alpha1.WorkflowStep, index map[string]*agentorcav1alpha1.WorkflowStepStatus) (reset bool) {
	gs := index[gate.Name]
	done, err := evalLoopCondition(gate.Loop.Until, index)
	if err != nil {
		finishStep(wf, gs, false, gs.Output, fmt.Sprintf("loop until: %v", err))
		return false
	}
	if done {
		return false
	}
	for _, b := range gate.Loop.Break {
		stop, err := evalLoopCondition(b, index)
		if err != nil {
			finishStep(wf, gs, false, gs.Output, fmt.Sprintf("loop break: %v", err))
			return false
		}
		if stop {
			finishStep(wf, gs, false, gs.Output, fmt.Sprintf("loop stopped before its passes ran out, a blocker another pass cannot fix: %s", b))
			return false
		}
	}
	next := gs.Iteration + 1
	if next >= loopMaxIterations(gate.Loop) {
		finishStep(wf, gs, false, gs.Output, fmt.Sprintf("loop did not finish in %d passes: %s", loopMaxIterations(gate.Loop), gate.Loop.Until))
		return false
	}
	for name := range loopBody(wf.Spec.Steps, gate) {
		resetStepForPass(index[name], next)
	}
	return true
}

// resetStepForPass puts a step back to Pending for another attempt, a loop pass or
// a retry. Its output stays so the attempt can read what the last one produced.
// The new generation gives the attempt names its predecessor did not use.
func resetStepForPass(ss *agentorcav1alpha1.WorkflowStepStatus, iteration int32) {
	ss.Generation = ss.NameGeneration() + 1
	ss.PriorSpendUSD = addUSD(ss.PriorSpendUSD, ss.SpendUSD)
	ss.SpendUSD = ""
	ss.Phase = agentorcav1alpha1.WorkflowStepPhasePending
	ss.Iteration = iteration
	ss.AgentRunRef, ss.PodRef, ss.JobID, ss.ApprovalID = "", "", "", ""
	ss.Sandbox = nil
	ss.StartTime, ss.CompletionTime = nil, nil
	ss.FailureReason = ""
}

func addUSD(a, b string) string {
	total := parseFloatSafe(a) + parseFloatSafe(b)
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%.6f", total)
}

// workflowStepPasses is how many passes a step can have: 1, or the largest
// maxIterations of a loop whose body contains it.
func workflowStepPasses(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep) int32 {
	passes := int32(1)
	for i := range wf.Spec.Steps {
		gate := &wf.Spec.Steps[i]
		if gate.Loop != nil && loopBody(wf.Spec.Steps, gate)[step.Name] && loopMaxIterations(gate.Loop) > passes {
			passes = loopMaxIterations(gate.Loop)
		}
	}
	return passes
}
