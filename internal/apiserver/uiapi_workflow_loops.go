package apiserver

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Loop states the UI shows.
const (
	loopStatePending   = "pending"
	loopStateRunning   = "running"
	loopStateRepeating = "repeating"
	loopStateSatisfied = "satisfied"
	loopStateExhausted = "exhausted"
)

// workflowPassView is one run of a step. A step inside a loop has one per pass.
type workflowPassView struct {
	Pass        int32  `json:"pass"`
	AgentRunRef string `json:"agentRunRef"`
	Phase       string `json:"phase"`
	Output      string `json:"output,omitempty"`
	SpendUSD    string `json:"spendUSD,omitempty"`
}

// workflowLoopView is a loop as the UI shows it: which steps repeat, until what,
// and how far along it is.
type workflowLoopView struct {
	ID            string   `json:"id"`
	Gate          string   `json:"gate"`
	Steps         []string `json:"steps"`
	Until         string   `json:"until"`
	MaxIterations int32    `json:"maxIterations"`
	Pass          int32    `json:"pass"`
	State         string   `json:"state"`
}

const maxPassOutputBytes = 4000

// workflowLoopViews describes each loop of the workflow and maps every step in a
// loop body to the id of its loop.
func workflowLoopViews(wf *agentorcav1alpha1.AgentWorkflow) (loops []workflowLoopView, loopOfStep map[string]string) {
	loopOfStep = map[string]string{}
	statusByStep := make(map[string]*agentorcav1alpha1.WorkflowStepStatus, len(wf.Status.Steps))
	for i := range wf.Status.Steps {
		statusByStep[wf.Status.Steps[i].Name] = &wf.Status.Steps[i]
	}
	for i := range wf.Spec.Steps {
		gate := &wf.Spec.Steps[i]
		body := agentorcav1alpha1.LoopBody(wf.Spec.Steps, gate)
		if body == nil {
			continue
		}
		l := workflowLoopView{ID: gate.Name, Gate: gate.Name, Until: gate.Loop.Until, MaxIterations: gate.Loop.Passes(), State: loopStatePending}
		for _, st := range wf.Spec.Steps {
			if body[st.Name] {
				l.Steps = append(l.Steps, st.Name)
				loopOfStep[st.Name] = gate.Name
			}
		}
		if gs := statusByStep[gate.Name]; gs != nil {
			l.Pass = gs.Iteration + 1
			l.State = loopState(gs)
		}
		loops = append(loops, l)
	}
	return loops, loopOfStep
}

func loopState(gate *agentorcav1alpha1.WorkflowStepStatus) string {
	switch gate.Phase {
	case agentorcav1alpha1.WorkflowStepPhaseSucceeded:
		return loopStateSatisfied
	case agentorcav1alpha1.WorkflowStepPhaseFailed:
		return loopStateExhausted
	case agentorcav1alpha1.WorkflowStepPhasePending:
		if gate.Iteration > 0 {
			return loopStateRepeating
		}
		return loopStatePending
	}
	return loopStateRunning
}

// workflowPassHistory lists every run of every looped step. A run is named
// <workflow>-<step> on the first pass and <workflow>-<step>-i<N> after.
func (s *UIServer) workflowPassHistory(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, loopOfStep map[string]string) map[string][]workflowPassView {
	out := map[string][]workflowPassView{}
	if len(loopOfStep) == 0 {
		return out
	}
	var runs agentorcav1alpha1.AgentRunList
	if err := s.crdClient.List(ctx, &runs, client.InNamespace(wf.Namespace),
		client.MatchingLabels{"agentorca.io/workflow": wf.Name}); err != nil {
		return out
	}
	for _, run := range runs.Items {
		step := run.Labels["agentorca.io/workflow-step"]
		if _, looped := loopOfStep[step]; !looped {
			continue
		}
		pass := int32(0)
		if suffix, ok := strings.CutPrefix(run.Name, wf.Name+"-"+step+"-i"); ok {
			n, err := strconv.Atoi(suffix)
			if err != nil {
				continue
			}
			pass = int32(n)
		} else if run.Name != wf.Name+"-"+step {
			continue
		}
		output := run.Status.Output
		if len(output) > maxPassOutputBytes {
			output = output[:maxPassOutputBytes] + "…"
		}
		out[step] = append(out[step], workflowPassView{
			Pass: pass, AgentRunRef: run.Name, Phase: string(run.Status.Phase), Output: output, SpendUSD: run.Status.SpendUSD,
		})
	}
	for step := range out {
		ps := out[step]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Pass < ps[j].Pass })
	}
	return out
}
