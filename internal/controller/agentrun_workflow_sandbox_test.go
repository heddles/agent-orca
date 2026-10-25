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
	"strings"
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
)

func externalWorkflowAgentRun(t *testing.T, gw *stubGateway, fresh bool) (*agentorcav1alpha1.AgentRun, *corev1.Secret, *agentorcav1alpha1.Agent, *agentorcav1alpha1.AgentWorkflow) {
	t.Helper()
	name := openshell.WorkflowStepSandboxName("default", "wf", "implement")
	sb, err := gw.Create(context.Background(), name, &osv1.SandboxSpec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sb.Status.Phase = osv1.SandboxReady
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run-1", Namespace: "default",
			Labels: map[string]string{labelWorkflow: "wf", labelWorkflowStep: "implement"},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef: "coder", Input: "line one\nline two",
			RestartPolicy:   &agentorcav1alpha1.AgentRunRestartPolicy{MaxRetries: 0},
			WorkflowSandbox: &agentorcav1alpha1.WorkflowSandboxRef{Name: name, ID: sb.ID, Fresh: fresh, ProviderOwner: "run-1"},
		},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning, SandboxName: name},
	}
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"}, Data: map[string][]byte{
		"OPENAI_BASE_URL": []byte("http://router/v1"), "OPENAI_API_KEY": []byte("run-token"),
	}}
	agent := sandboxedWorkflowAgent("coder", "ghcr.io/acme/coder:1.0")
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{
			Sandbox: &agentorcav1alpha1.WorkflowSandboxSpec{Image: agent.Spec.Runtime.OCIRef, Repo: "acme/widgets"},
			Steps:   []agentorcav1alpha1.WorkflowStep{{Name: "implement", AgentRef: "coder", Input: run.Spec.Input}},
		},
		Status: agentorcav1alpha1.AgentWorkflowStatus{
			Sandbox: &agentorcav1alpha1.WorkflowSandboxStatus{Name: name},
			Steps:   []agentorcav1alpha1.WorkflowStepStatus{{Name: "implement", AgentRunRef: run.Name}},
		},
	}
	if fresh {
		wf.Status.Sandbox.Name = openshell.WorkflowSandboxName("default", "wf")
		wf.Status.Steps[0].Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{Name: name}
	}
	return run, token, agent, wf
}

func TestEnsureWorkflowAgentJobUsesExistingCheckoutAndRunCredential(t *testing.T) {
	gw := newStubGateway()
	jobs := newWorkflowJobs()
	gw.execFn = jobs.exec
	run, token, agent, wf := externalWorkflowAgentRun(t, gw, false)
	r, cl := sandboxReconciler(t, gw, run, token, wf)

	if _, err := r.ensureWorkflowAgentJob(context.Background(), run, agent, "tok", "http://router:8080"); err != nil {
		t.Fatalf("ensureWorkflowAgentJob: %v", err)
	}
	if len(gw.sandboxes) != 1 {
		t.Fatalf("the AgentRun created another sandbox: %v", gw.sandboxes)
	}
	id := openshell.WorkflowJobName(run.Namespace, run.Name)
	script := jobs.script(run.Spec.WorkflowSandbox.Name, id)
	for _, want := range []string{
		"AGENTORC_RUN_ID='run-1'", "AGENTORC_INPUT='line one\nline two'", "OPENAI_API_KEY='run-token'",
		"OPENAI_BASE_URL='http://router/v1'", "cd '" + workflowCheckoutDir + "'", "exec '/usr/local/bin/coder-agent' '--strict'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("agent job script lacks %q:\n%s", want, script)
		}
	}
	var np networkingv1.NetworkPolicy
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "agentorca-router-run-1-sandbox"}, &np); err != nil {
		t.Fatalf("router ingress: %v", err)
	}
	from := np.Spec.Ingress[0].From[0]
	if from.PodSelector.MatchLabels[openshell.SandboxIDLabel] != run.Spec.WorkflowSandbox.ID {
		t.Errorf("router admits the wrong supervisor: %+v", from.PodSelector.MatchLabels)
	}
}

func TestWorkflowAgentProgressReadsJobAndPreservesSharedSandbox(t *testing.T) {
	gw := newStubGateway()
	jobs := newWorkflowJobs()
	gw.execFn = jobs.exec
	run, token, agent, wf := externalWorkflowAgentRun(t, gw, false)
	r, cl := sandboxReconciler(t, gw, run, token, wf)
	if _, err := r.ensureWorkflowAgentJob(context.Background(), run, agent, "tok", "http://router:8080"); err != nil {
		t.Fatal(err)
	}
	id := openshell.WorkflowJobName(run.Namespace, run.Name)
	jobs.complete(run.Spec.WorkflowSandbox.Name, id, 0, "implementation complete")
	if _, err := r.checkWorkflowAgentProgress(context.Background(), run, agent); err != nil {
		t.Fatalf("checkWorkflowAgentProgress: %v", err)
	}
	got := fetchSandboxRun(t, cl, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded || got.Status.Output != "implementation complete" {
		t.Errorf("run phase=%q output=%q", got.Status.Phase, got.Status.Output)
	}
	r.deleteRunSandbox(context.Background(), got)
	if _, exists := gw.sandboxes[run.Spec.WorkflowSandbox.Name]; !exists {
		t.Error("an AgentRun deleted the workflow's shared sandbox")
	}
}

func TestWorkflowAgentFailureDoesNotRetrySharedJob(t *testing.T) {
	gw := newStubGateway()
	jobs := newWorkflowJobs()
	gw.execFn = jobs.exec
	run, token, agent, wf := externalWorkflowAgentRun(t, gw, false)
	r, cl := sandboxReconciler(t, gw, run, token, wf)
	if _, err := r.ensureWorkflowAgentJob(context.Background(), run, agent, "tok", "http://router:8080"); err != nil {
		t.Fatal(err)
	}
	id := openshell.WorkflowJobName(run.Namespace, run.Name)
	jobs.complete(run.Spec.WorkflowSandbox.Name, id, 2, "agent boom")
	if _, err := r.checkWorkflowAgentProgress(context.Background(), run, agent); err != nil {
		t.Fatalf("checkWorkflowAgentProgress: %v", err)
	}
	got := fetchSandboxRun(t, cl, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed || got.Status.RestartCount != 0 || !strings.Contains(got.Status.LastRestartReason, "code 2") {
		t.Errorf("run phase=%q restarts=%d reason=%q", got.Status.Phase, got.Status.RestartCount, got.Status.LastRestartReason)
	}
}

func TestWorkflowAgentCannotStealAnotherSandbox(t *testing.T) {
	gw := newStubGateway()
	jobs := newWorkflowJobs()
	gw.execFn = jobs.exec
	run, token, agent, wf := externalWorkflowAgentRun(t, gw, false)
	run.Spec.WorkflowSandbox.Name = "ao-someone-else"
	r, _ := sandboxReconciler(t, gw, run, token, wf)
	if _, err := r.ensureWorkflowAgentJob(context.Background(), run, agent, "tok", "http://router:8080"); err == nil || !strings.Contains(err.Error(), "does not authorize") {
		t.Fatalf("unlinked sandbox was accepted: %v", err)
	}
	if len(jobs.jobs) != 0 {
		t.Error("an unauthorized AgentRun started a sandbox job")
	}
}

func TestFreshWorkflowSandboxSurvivesUntilLastContinuation(t *testing.T) {
	gw := newStubGateway()
	run, _, _, wf := externalWorkflowAgentRun(t, gw, true)
	r, _ := sandboxReconciler(t, gw, run, wf)
	run.Status.ContinuationRunRef = "run-1-cont-a"
	r.deleteRunSandbox(context.Background(), run)
	name := run.Spec.WorkflowSandbox.Name
	if _, exists := gw.sandboxes[name]; !exists {
		t.Fatal("the original run deleted a fresh sandbox needed by its continuation")
	}
	continuation := run.DeepCopy()
	continuation.Name = run.Status.ContinuationRunRef
	continuation.Status.ContinuationRunRef = ""
	if got, want := openshell.WorkflowJobName(continuation.Namespace, continuation.Name), openshell.WorkflowJobName(run.Namespace, run.Name); got == want {
		t.Errorf("continuation job ID %q must differ from the original", got)
	}
	r.deleteRunSandbox(context.Background(), continuation)
	if _, exists := gw.sandboxes[name]; exists {
		t.Error("the final continuation did not delete its fresh sandbox")
	}
}
