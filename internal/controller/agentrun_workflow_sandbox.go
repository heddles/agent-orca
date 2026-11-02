/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/podbuilder"
)

// authorizeWorkflowSandbox prevents an arbitrary AgentRun from executing in a
// sandbox it merely names. The workflow's own status must link this run and the
// same fresh or shared sandbox; continuations qualify after the workflow follows
// their continuationRunRef. Until then the run waits and cannot start a job.
func (r *AgentRunReconciler) authorizeWorkflowSandbox(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	ref := run.Spec.WorkflowSandbox
	workflowName := run.Labels[labelWorkflow]
	if workflowName == "" {
		return fmt.Errorf("AgentRun %s has a workflow sandbox but no workflow label", run.Name)
	}
	var wf agentorcav1alpha1.AgentWorkflow
	if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: workflowName}, &wf); err != nil {
		return fmt.Errorf("reading workflow %s for sandbox authorization: %w", workflowName, err)
	}
	if wf.Spec.Sandbox == nil {
		return fmt.Errorf("workflow %s has no sandbox", workflowName)
	}
	statuses := append(append([]agentorcav1alpha1.WorkflowStepStatus{}, wf.Status.Steps...), wf.Status.DynamicStepStatuses...)
	for _, ss := range statuses {
		if ss.AgentRunRef != run.Name {
			continue
		}
		expected := wf.Status.Sandbox
		fresh := ss.Sandbox != nil
		if fresh {
			expected = ss.Sandbox
		}
		if expected == nil || expected.Name == "" || expected.Name != ref.Name || fresh != ref.Fresh {
			return fmt.Errorf("workflow step %s does not authorize sandbox %s for AgentRun %s", ss.Name, ref.Name, run.Name)
		}
		return nil
	}
	return fmt.Errorf("workflow %s has not linked AgentRun %s to a sandbox step", workflowName, run.Name)
}

// ensureWorkflowAgentJob starts an AgentRun's agent in the workflow sandbox.
// The workflow already created and checked out that sandbox and merged its
// policy; this run supplies only its own router, token and process environment.
func (r *AgentRunReconciler) ensureWorkflowAgentJob(
	ctx context.Context,
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	tokenSecretName string,
	routerBaseURL string,
) (*osv1.Sandbox, error) {
	if err := r.authorizeWorkflowSandbox(ctx, run); err != nil {
		return nil, err
	}
	ref := run.Spec.WorkflowSandbox
	sb, err := r.Sandboxes.Get(ctx, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("reading workflow sandbox %s: %w", ref.Name, err)
	}
	if sb.Status.Phase != osv1.SandboxReady {
		return nil, fmt.Errorf("workflow sandbox %s is not ready (phase %s)", ref.Name, sb.Status.Phase)
	}
	if err := r.ensureRouterSandboxIngress(ctx, run, sb); err != nil {
		return nil, err
	}

	env := map[string]string{}
	for _, e := range agentRunEnv(run, agent) {
		env[e.Name] = e.Value
	}
	for _, e := range podbuilder.FrameworkEnvVars(agent.Spec.Runtime.Framework, routerBaseURL) {
		env[e.Name] = e.Value
	}
	var tokenSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: run.Namespace}, &tokenSecret); err != nil {
		return nil, fmt.Errorf("reading token secret: %w", err)
	}
	for k, v := range tokenSecret.Data {
		env[k] = string(v)
	}
	maps.Copy(env, tokenSecret.StringData)
	// Idempotently ensures the providers the workflow attached at creation and
	// returns this Agent's non-secret credential descriptors for its process.
	if _, _, err := r.ensureSandboxTools(ctx, run, agent, env); err != nil {
		return nil, err
	}
	command := slices.Concat(agent.Spec.Runtime.Command, agent.Spec.Runtime.Args)
	if err := openshell.StartCommandJob(ctx, r.Sandboxes, ref.Name, openshell.WorkflowJobName(run.Namespace, run.Name), command, nil, env, workflowCheckoutDir); err != nil {
		return nil, fmt.Errorf("starting agent job in workflow sandbox %s: %w", ref.Name, err)
	}
	return sb, nil
}

// checkWorkflowAgentProgress polls the detached agent job, not the sandbox's
// deliberately idle main process. Short calls and deterministic job IDs let a
// restarted operator recover without holding an Exec stream open.
func (r *AgentRunReconciler) checkWorkflowAgentProgress(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if err := r.authorizeWorkflowSandbox(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	ref := run.Spec.WorkflowSandbox
	sb, err := r.Sandboxes.Get(ctx, ref.Name)
	if err != nil {
		if osv1.IsNotFound(err) {
			return r.handlePodFailure(ctx, run, agent, "workflow sandbox not found")
		}
		logger.Error(err, "reading workflow sandbox", "sandbox", ref.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if err := r.ensureRouterSandboxIngress(ctx, run, sb); err != nil {
		return ctrl.Result{}, err
	}
	r.recordSandboxDecisions(ctx, run, sb)
	r.reconcileEgressApprovals(ctx, run, agent, sb)
	if outcome, reason := openshell.Observe(sb); outcome != openshell.OutcomeRunning {
		if reason == "" {
			reason = workflowIdleExit
		}
		return r.handlePodFailure(ctx, run, agent, reason)
	}

	st, err := openshell.PollJob(ctx, r.Sandboxes, ref.Name, openshell.WorkflowJobName(run.Namespace, run.Name))
	if err != nil {
		logger.Error(err, "polling workflow agent job", "sandbox", ref.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	switch st.Phase {
	case openshell.JobRunning:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	case openshell.JobExited:
		if st.ExitCode == 0 {
			return r.handleRunSuccess(ctx, run, func(context.Context) string { return st.Output })
		}
		reason := fmt.Sprintf("agent exited with code %d", st.ExitCode)
		if out := strings.TrimSpace(st.Output); out != "" {
			reason += "\n[agent] " + truncate(lastLines(out, 10), 1024)
		}
		return r.handlePodFailure(ctx, run, agent, reason)
	case openshell.JobLost:
		return r.handlePodFailure(ctx, run, agent, "the agent process was lost before it reported an exit code")
	case openshell.JobMissing:
		return r.handlePodFailure(ctx, run, agent, "the agent job is missing from the workflow sandbox")
	default:
		return ctrl.Result{}, fmt.Errorf("unknown agent job phase %q", st.Phase)
	}
}
