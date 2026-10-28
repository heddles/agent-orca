package apiserver

import (
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// parseWorkflowRetryPath reads /api/workflows/{namespace}/{name}/steps/{step}/retry.
func parseWorkflowRetryPath(path string) (ns, name, step string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/workflows/"), "/")
	if len(parts) != 5 || parts[2] != "steps" || parts[4] != "retry" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[3], true
}

// handleRetryWorkflowStep asks the operator to run a failed step again by
// setting agentorca.io/retry-step. The operator does the reset, so a workflow
// that already ended is revived the same way as one still running.
func (s *UIServer) handleRetryWorkflowStep(w http.ResponseWriter, r *http.Request, ns, name, step string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var wf agentorcav1alpha1.AgentWorkflow
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, &wf); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	failed := false
	for _, ss := range append(append([]agentorcav1alpha1.WorkflowStepStatus{}, wf.Status.Steps...), wf.Status.DynamicStepStatuses...) {
		if ss.Name == step && ss.Phase == agentorcav1alpha1.WorkflowStepPhaseFailed {
			failed = true
		}
	}
	if step == "sandbox" && wf.Spec.Sandbox != nil && wf.Status.Sandbox != nil &&
		wf.Status.Sandbox.Phase == agentorcav1alpha1.WorkflowSandboxPhaseFailed {
		failed = true
	}
	if !failed {
		http.Error(w, fmt.Sprintf("step %q is not failed", step), http.StatusConflict)
		return
	}
	patch := client.MergeFrom(wf.DeepCopy())
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations[agentorcav1alpha1.RetryStepAnnotation] = step
	if err := s.crdClient.Patch(r.Context(), &wf, patch); err != nil {
		http.Error(w, fmt.Sprintf("requesting retry: %v", err), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "retrying", "step": step})
}
