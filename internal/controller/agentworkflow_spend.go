package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// refreshWorkflowSpend sets each step's spend from the workflow's AgentRuns and
// the workflow's total, and reports whether anything changed.
//
// A step copies its run's spend when the run finishes, but the router records the
// last model call's cost a moment after the agent reports its answer, so the
// copy can be short or empty. The run is right once it settles; this reads it
// again. The runs of earlier loop passes and retries are no longer linked from the
// step, so they are found by label: the newest run is the step's current spend and
// the rest are its prior spend.
func (r *AgentWorkflowReconciler) refreshWorkflowSpend(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) bool {
	var runs agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &runs, client.InNamespace(wf.Namespace), client.MatchingLabels{labelWorkflow: wf.Name}); err != nil {
		log.FromContext(ctx).Error(err, "listing workflow runs to refresh spend")
		return false
	}
	byRunName := map[string]string{}
	priorByStep := map[string]float64{}
	currentRun := map[string]string{}
	for i := range wf.Status.Steps {
		if ref := wf.Status.Steps[i].AgentRunRef; ref != "" {
			currentRun[wf.Status.Steps[i].Name] = ref
		}
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		byRunName[run.Name] = run.Status.SpendUSD
		step := run.Labels[labelWorkflowStep]
		if step != "" && currentRun[step] != run.Name {
			priorByStep[step] += parseFloatSafe(run.Status.SpendUSD)
		}
	}

	changed := false
	for i := range wf.Status.Steps {
		ss := &wf.Status.Steps[i]
		if spend, ok := byRunName[ss.AgentRunRef]; ok && ss.AgentRunRef != "" && spend != "" && spend != ss.SpendUSD {
			ss.SpendUSD, changed = spend, true
		}
		if p := priorByStep[ss.Name]; p > 0 {
			if prior := fmt.Sprintf("%.6f", p); prior != ss.PriorSpendUSD {
				ss.PriorSpendUSD, changed = prior, true
			}
		}
	}
	if total := r.sumSpend(append(append([]agentorcav1alpha1.WorkflowStepStatus{}, wf.Status.Steps...), wf.Status.DynamicStepStatuses...)); total != wf.Status.TotalSpendUSD {
		wf.Status.TotalSpendUSD, changed = total, true
	}
	return changed
}
