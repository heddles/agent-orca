package v1alpha1

// Loop pass limits.
const (
	DefaultLoopIterations = 3
	MaxLoopIterations     = 10
)

// Passes is how many times the loop's body may run in total, with the default and
// the ceiling applied.
func (l *WorkflowLoop) Passes() int32 {
	switch {
	case l.MaxIterations <= 0:
		return DefaultLoopIterations
	case l.MaxIterations > MaxLoopIterations:
		return MaxLoopIterations
	}
	return l.MaxIterations
}

// LoopBody returns the names of the steps a loop on gate re-runs: From, the gate
// and every step that is both downstream of From and upstream of the gate. It is
// nil when the step has no loop or From is not the gate or one of its ancestors.
func LoopBody(steps []WorkflowStep, gate *WorkflowStep) map[string]bool {
	if gate.Loop == nil {
		return nil
	}
	byName := make(map[string]*WorkflowStep, len(steps))
	for i := range steps {
		byName[steps[i].Name] = &steps[i]
	}
	up := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if up[name] {
			return
		}
		up[name] = true
		if s := byName[name]; s != nil {
			for _, d := range s.DependsOn {
				walk(d)
			}
		}
	}
	walk(gate.Name)
	if !up[gate.Loop.From] {
		return nil
	}
	body := map[string]bool{gate.Loop.From: true}
	for changed := true; changed; {
		changed = false
		for i := range steps {
			s := &steps[i]
			if body[s.Name] || !up[s.Name] {
				continue
			}
			for _, d := range s.DependsOn {
				if body[d] {
					body[s.Name] = true
					changed = true
					break
				}
			}
		}
	}
	return body
}

// NameGeneration is the number a step's run, pod, job and sandbox names carry: 0
// on the first attempt, then one more for each loop pass or retry. Workflows
// written before Generation existed carry only Iteration.
func (s *WorkflowStepStatus) NameGeneration() int32 {
	return max(s.Generation, s.Iteration)
}

// RetryStepAnnotation asks the operator to run failed steps of a workflow again:
// a comma separated list of step names. The operator removes it once it has acted.
const RetryStepAnnotation = "agentorca.io/retry-step"
