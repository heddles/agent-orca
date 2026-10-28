package controller

import (
	"context"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// RetrySandbox is the name that asks for the workflow's shared sandbox to be
// provisioned again, for a workflow that failed before or without a failed step
// because its sandbox never came up or was lost.
const RetrySandbox = "sandbox"

// loopFailurePrefix starts the failure reason of a gate that its loop failed, as
// opposed to a gate whose own run failed.
const loopFailurePrefix = "loop "

// applyStepRetries runs the failed steps named by agentorcav1alpha1.RetryStepAnnotation again and
// reports whether it changed the workflow's status. A workflow that had already
// ended is brought back to Running. The caller persists the status, then calls
// clearRetryRequest.
//
// A step goes back to Pending under a new generation, so its names are fresh.
// A loop gate that the loop itself failed (out of passes, or stopped by break)
// restarts the whole loop body with a full set of passes; any other failed step
// reruns alone, in the pass it failed in.
func (r *AgentWorkflowReconciler) applyStepRetries(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) bool {
	raw := wf.Annotations[agentorcav1alpha1.RetryStepAnnotation]
	if raw == "" {
		return false
	}
	index := map[string]*agentorcav1alpha1.WorkflowStepStatus{}
	for i := range wf.Status.Steps {
		index[wf.Status.Steps[i].Name] = &wf.Status.Steps[i]
	}
	for i := range wf.Status.DynamicStepStatuses {
		index[wf.Status.DynamicStepStatuses[i].Name] = &wf.Status.DynamicStepStatuses[i]
	}
	changed := false
	for name := range strings.SplitSeq(raw, ",") {
		name = strings.TrimSpace(name)
		if name == RetrySandbox {
			if wf.Spec.Sandbox != nil && wf.Status.Sandbox != nil && wf.Status.Sandbox.Phase == agentorcav1alpha1.WorkflowSandboxPhaseFailed {
				wf.Status.Sandbox = nil
				changed = true
			}
			continue
		}
		ss := index[name]
		if ss == nil || ss.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed {
			continue
		}
		gate := findStep(wf, name)
		if gate != nil && gate.Loop != nil && strings.HasPrefix(ss.FailureReason, loopFailurePrefix) {
			for body := range loopBody(wf.Spec.Steps, gate) {
				r.deleteFreshStepSandbox(ctx, index[body])
				resetStepForPass(index[body], 0)
			}
		} else {
			r.deleteFreshStepSandbox(ctx, ss)
			resetStepForPass(ss, ss.Iteration)
		}
		changed = true
	}
	if !changed {
		return false
	}
	log.FromContext(ctx).Info("retrying failed steps", "steps", raw)
	if isWorkflowTerminal(wf.Status.Phase) {
		now := metav1.NewTime(time.Now())
		wf.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseRunning
		wf.Status.CompletionTime = nil
		// The workflow timeout counts from the retry, or a workflow that failed
		// by timing out would fail again at once.
		wf.Status.StartTime = &now
		// Finishing deleted the shared sandbox. Provision it again from the
		// workflow's repository, ref and branch.
		if wf.Spec.Sandbox != nil {
			wf.Status.Sandbox = nil
		}
	}
	return true
}

func findStep(wf *agentorcav1alpha1.AgentWorkflow, name string) *agentorcav1alpha1.WorkflowStep {
	for i := range wf.Spec.Steps {
		if wf.Spec.Steps[i].Name == name {
			return &wf.Spec.Steps[i]
		}
	}
	for i := range wf.Status.DynamicSteps {
		if wf.Status.DynamicSteps[i].Name == name {
			return &wf.Status.DynamicSteps[i]
		}
	}
	return nil
}

// clearRetryRequest removes the retry annotation, and the completion notice so a
// workflow that ends again is announced again.
func (r *AgentWorkflowReconciler) clearRetryRequest(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) error {
	patch := client.MergeFrom(wf.DeepCopy())
	delete(wf.Annotations, agentorcav1alpha1.RetryStepAnnotation)
	delete(wf.Annotations, completionNotifiedAnnotation)
	return r.Patch(ctx, wf, patch)
}
