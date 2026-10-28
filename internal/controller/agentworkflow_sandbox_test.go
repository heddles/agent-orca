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
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/githubapp"
	"github.com/heddles/agent-orca/internal/openshell"
)

const testCheckoutSHA = "0123456789abcdef0123456789abcdef01234567"

type workflowJob struct {
	phase    openshell.JobPhase
	exitCode int
	output   string
}

// workflowJobs simulates the job files in a sandbox. The scripts themselves are
// exercised against a real shell in internal/openshell/job_test.go; this fake
// lets controller tests move a job through its durable states explicitly.
type workflowJobs struct {
	jobs    map[string]*workflowJob
	scripts map[string]string
	starts  map[string]int
	execErr error
}

func newWorkflowJobs() *workflowJobs {
	return &workflowJobs{jobs: map[string]*workflowJob{}, scripts: map[string]string{}, starts: map[string]int{}}
}

func workflowJobKey(sandbox, id string) string { return sandbox + "/" + id }

func (j *workflowJobs) exec(_ context.Context, sandbox string, command []string, _ openshell.ExecOptions) (*openshell.ExecResult, error) {
	if j.execErr != nil {
		return nil, j.execErr
	}
	if len(command) < 5 {
		return nil, fmt.Errorf("unexpected exec: %v", command)
	}
	id := command[4]
	key := workflowJobKey(sandbox, id)
	switch {
	case strings.Contains(command[2], `mkdir -p "$dir"`):
		j.starts[key]++
		raw, err := base64.StdEncoding.DecodeString(command[5])
		if err != nil {
			return nil, err
		}
		j.scripts[key] = string(raw)
		if _, exists := j.jobs[key]; !exists {
			j.jobs[key] = &workflowJob{phase: openshell.JobRunning}
		}
		return &openshell.ExecResult{}, nil
	case strings.Contains(command[2], `echo "STATE $state"`):
		job := j.jobs[key]
		state := "missing"
		output := ""
		if job != nil {
			state = string(job.phase)
			if job.phase == openshell.JobExited {
				state += fmt.Sprintf(" %d", job.exitCode)
			}
			output = job.output
		}
		return &openshell.ExecResult{Stdout: []byte("STATE " + state + "\n---\n" + output)}, nil
	case strings.Contains(command[2], "kill -TERM"):
		if job := j.jobs[key]; job != nil && job.phase == openshell.JobRunning {
			job.phase, job.exitCode = openshell.JobExited, 143
		}
		return &openshell.ExecResult{}, nil
	default:
		return nil, fmt.Errorf("unexpected job exec: %v", command)
	}
}

func (j *workflowJobs) complete(sandbox, id string, code int, output string) {
	j.jobs[workflowJobKey(sandbox, id)] = &workflowJob{phase: openshell.JobExited, exitCode: code, output: output}
}

func (j *workflowJobs) script(sandbox, id string) string {
	return j.scripts[workflowJobKey(sandbox, id)]
}

func sandboxContainerStep(name, image string) agentorcav1alpha1.WorkflowStep {
	return agentorcav1alpha1.WorkflowStep{
		Name: name, Type: agentorcav1alpha1.WorkflowStepTypeContainer,
		Container: &agentorcav1alpha1.ContainerStep{
			Image: image, Command: []string{"sh", "-c", `printf 'checks passed\n'`},
			Env: []agentorcav1alpha1.StepEnvVar{{Name: "STATIC", Value: "a value; not shell"}},
		},
	}
}

func workflowWithSandbox(steps ...agentorcav1alpha1.WorkflowStep) *agentorcav1alpha1.AgentWorkflow {
	return &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wf", Namespace: "default", UID: "wf-sandbox-uid",
			Finalizers: []string{workflowSandboxFinalizer},
		},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{
			Sandbox: &agentorcav1alpha1.WorkflowSandboxSpec{
				Image: "ghcr.io/heddles/dev-sandbox:1",
				Repo:  "heddles/agent-orca", Ref: "main", Branch: "factory/wf",
			},
			Steps: steps,
		},
	}
}

func sandboxWorkflowReconciler(t *testing.T, wf *agentorcav1alpha1.AgentWorkflow, objs ...client.Object) (*AgentWorkflowReconciler, client.Client, *stubGateway, *workflowJobs) {
	t.Helper()
	objs = append([]client.Object{wf}, objs...)
	r, cl, _ := stepsReconciler(t, objs...)
	gw := newStubGateway()
	jobs := newWorkflowJobs()
	gw.execFn = jobs.exec
	r.Sandboxes = gw
	return r, cl, gw, jobs
}

func sharedSandboxName(wf *agentorcav1alpha1.AgentWorkflow) string {
	return openshell.WorkflowSandboxName(wf.Namespace, wf.Name)
}

func markSandboxReady(t *testing.T, gw *stubGateway, wf *agentorcav1alpha1.AgentWorkflow) {
	t.Helper()
	sb := gw.sandboxes[sharedSandboxName(wf)]
	if sb == nil {
		t.Fatal("the shared sandbox was not created")
	}
	sb.Status.Phase = osv1.SandboxReady
}

func TestWorkflowSandboxCheckoutAndContainerLifecycle(t *testing.T) {
	wf := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
	r, cl, gw, jobs := sandboxWorkflowReconciler(t, wf)

	reconcileWorkflow(t, r, "wf")
	got := getWorkflow(t, cl, "wf")
	name := sharedSandboxName(wf)
	if got.Status.Sandbox == nil {
		t.Fatal("sandbox status is nil")
	}
	if got.Status.Sandbox.Name != name || got.Status.Sandbox.ID != "id-"+name || got.Status.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseProvisioning {
		t.Fatalf("sandbox name=%q id=%q phase=%q", got.Status.Sandbox.Name, got.Status.Sandbox.ID, got.Status.Sandbox.Phase)
	}
	spec := gw.specs[name]
	if spec == nil || spec.Template.Image != wf.Spec.Sandbox.Image || len(spec.Command) == 0 {
		t.Fatalf("sandbox spec = %+v", spec)
	}
	if _, ok := spec.Policy.NetworkPolicies["agentorca-router"]; ok {
		t.Error("an idle workflow sandbox must not admit a router before an agent step")
	}

	markSandboxReady(t, gw, wf)
	reconcileWorkflow(t, r, "wf")
	if got := getWorkflow(t, cl, "wf"); got.Status.Sandbox.CheckoutSHA != "" || jobs.starts[workflowJobKey(name, workflowCheckoutJobID)] != 1 {
		t.Fatalf("checkout did not start once: sha=%q starts=%d", got.Status.Sandbox.CheckoutSHA, jobs.starts[workflowJobKey(name, workflowCheckoutJobID)])
	}
	checkout := jobs.script(name, workflowCheckoutJobID)
	for _, want := range []string{"https://github.com/heddles/agent-orca.git", "main", "factory/wf", workflowCheckoutDir} {
		if !strings.Contains(checkout, want) {
			t.Errorf("checkout script lacks %q:\n%s", want, checkout)
		}
	}

	// A new reconciler represents an operator restart. The checkout is already
	// in the sandbox, so recovery needs only its deterministic name and a poll.
	r2, _, _, _ := sandboxWorkflowReconciler(t, workflowWithSandbox())
	r2.Client, r2.Scheme, r2.Sandboxes = cl, cl.Scheme(), gw
	jobs.complete(name, workflowCheckoutJobID, 0, testCheckoutSHA+"\n")
	reconcileWorkflow(t, r2, "wf")
	got = getWorkflow(t, cl, "wf")
	if got.Status.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseReady || got.Status.Sandbox.CheckoutSHA != testCheckoutSHA {
		t.Fatalf("checkout phase=%q sha=%q", got.Status.Sandbox.Phase, got.Status.Sandbox.CheckoutSHA)
	}
	ss := stepOf(t, got, "test")
	if ss.Phase != agentorcav1alpha1.WorkflowStepPhaseRunning || ss.JobID == "" || ss.PodRef != "" {
		t.Fatalf("step = %+v, want a detached sandbox job and no pod", ss)
	}
	if podExists(cl, "wf-test") {
		t.Error("a sandboxed container step created a W2 pod")
	}
	stepScript := jobs.script(name, ss.JobID)
	for _, want := range []string{"AGENTORC_CHECKOUT_SHA='" + testCheckoutSHA + "'", "STATIC='a value; not shell'", "cd '" + workflowCheckoutDir + "'", "exec 'sh' '-c'"} {
		if !strings.Contains(stepScript, want) {
			t.Errorf("step script lacks %q:\n%s", want, stepScript)
		}
	}

	// A gateway outage while the job runs is retryable, not a failed step.
	gw.getErr = errors.New("gateway restarting")
	jobs.execErr = gw.getErr
	result := reconcileWorkflow(t, r2, "wf")
	if result.RequeueAfter != workflowSandboxRetry {
		t.Errorf("gateway outage requeue = %v, want %v", result.RequeueAfter, workflowSandboxRetry)
	}
	if ss := stepOf(t, getWorkflow(t, cl, "wf"), "test"); ss.Phase != agentorcav1alpha1.WorkflowStepPhaseRunning {
		t.Fatalf("a gateway outage failed the step: %+v", ss)
	}
	gw.getErr, jobs.execErr = nil, nil

	jobs.complete(name, ss.JobID, 0, "tests passed\n")
	reconcileWorkflow(t, r2, "wf")
	got = getWorkflow(t, cl, "wf")
	if got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseSucceeded || stepOf(t, got, "test").Output != "tests passed" {
		t.Fatalf("workflow = %+v, step = %+v", got.Status, stepOf(t, got, "test"))
	}

	// Terminal reconciliation collects the gateway-side resources that ownerRef
	// GC cannot reach.
	reconcileWorkflow(t, r2, "wf")
	if len(gw.deleted) == 0 || gw.deleted[len(gw.deleted)-1] != name {
		t.Errorf("deleted sandboxes = %v, want %s", gw.deleted, name)
	}
}

func TestWorkflowSandboxFailurePolicies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		onFailure  string
		wantPhase  agentorcav1alpha1.WorkflowStepPhase
		wantReason string
	}{
		{name: "stop", wantPhase: agentorcav1alpha1.WorkflowStepPhaseFailed, wantReason: "code 3"},
		{name: "continue", onFailure: onStepFailureContinue, wantPhase: agentorcav1alpha1.WorkflowStepPhaseSkipped, wantReason: "code 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
			wf.Spec.OnStepFailure = tc.onFailure
			r, cl, gw, jobs := sandboxWorkflowReconciler(t, wf)
			reconcileWorkflow(t, r, "wf")
			markSandboxReady(t, gw, wf)
			reconcileWorkflow(t, r, "wf")
			name := sharedSandboxName(wf)
			jobs.complete(name, workflowCheckoutJobID, 0, testCheckoutSHA)
			reconcileWorkflow(t, r, "wf")
			ss := stepOf(t, getWorkflow(t, cl, "wf"), "test")
			jobs.complete(name, ss.JobID, 3, "compiler output")
			reconcileWorkflow(t, r, "wf")
			got := getWorkflow(t, cl, "wf")
			ss = stepOf(t, got, "test")
			if ss.Phase != tc.wantPhase || !strings.Contains(ss.FailureReason, tc.wantReason) || ss.Output != "compiler output" {
				t.Errorf("step = %+v, want phase %s and %q", ss, tc.wantPhase, tc.wantReason)
			}
		})
	}
}

func TestWorkflowFreshSandboxUsesTheWorkflowCommit(t *testing.T) {
	step := sandboxContainerStep("verify", "ghcr.io/heddles/verifier:2")
	step.Sandbox = agentorcav1alpha1.WorkflowStepSandboxFresh
	wf := workflowWithSandbox(step)
	r, cl, gw, jobs := sandboxWorkflowReconciler(t, wf)
	reconcileWorkflow(t, r, "wf")
	sharedName := sharedSandboxName(wf)
	markSandboxReady(t, gw, wf)
	reconcileWorkflow(t, r, "wf")
	jobs.complete(sharedName, workflowCheckoutJobID, 0, testCheckoutSHA)

	// Shared checkout completion creates the fresh verifier sandbox, not a job
	// in the shared writer sandbox.
	reconcileWorkflow(t, r, "wf")
	got := getWorkflow(t, cl, "wf")
	ss := stepOf(t, got, "verify")
	freshName := openshell.WorkflowStepSandboxName(wf.Namespace, wf.Name, "verify")
	if freshName == sharedName || ss.Sandbox == nil || ss.Sandbox.Name != freshName || ss.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseProvisioning {
		t.Fatalf("fresh sandbox status = %+v", ss.Sandbox)
	}
	fresh := gw.sandboxes[freshName]
	if fresh == nil || gw.specs[freshName].Template.Image != "ghcr.io/heddles/verifier:2" {
		t.Fatalf("fresh sandbox = %+v", fresh)
	}
	if _, running := jobs.jobs[workflowJobKey(sharedName, ss.JobID)]; running {
		t.Error("the fresh step started in the shared sandbox")
	}

	fresh.Status.Phase = osv1.SandboxReady
	reconcileWorkflow(t, r, "wf")
	if script := jobs.script(freshName, workflowCheckoutJobID); !strings.Contains(script, "CHECKOUT_REF='"+testCheckoutSHA+"'") {
		t.Errorf("fresh checkout is not pinned to the workflow commit:\n%s", script)
	}
	jobs.complete(freshName, workflowCheckoutJobID, 0, testCheckoutSHA)
	reconcileWorkflow(t, r, "wf")
	got = getWorkflow(t, cl, "wf")
	ss = stepOf(t, got, "verify")
	if ss.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseReady || ss.Sandbox.CheckoutSHA != testCheckoutSHA {
		t.Fatalf("fresh checkout status = %+v", ss.Sandbox)
	}
	if _, running := jobs.jobs[workflowJobKey(freshName, ss.JobID)]; !running {
		t.Fatal("the verifier job did not start in its fresh sandbox")
	}

	jobs.complete(freshName, ss.JobID, 0, "independent checks passed")
	reconcileWorkflow(t, r, "wf")
	got = getWorkflow(t, cl, "wf")
	if got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseSucceeded || stepOf(t, got, "verify").Output != "independent checks passed" {
		t.Fatalf("workflow phase=%q step=%+v", got.Status.Phase, stepOf(t, got, "verify"))
	}
	if _, exists := gw.sandboxes[freshName]; exists {
		t.Error("the fresh sandbox was not deleted after its output was captured")
	}
	if _, exists := gw.sandboxes[sharedName]; !exists {
		t.Error("the shared sandbox was deleted before terminal cleanup")
	}
}

func sandboxedWorkflowAgent(name, image string) *agentorcav1alpha1.Agent {
	agent := sandboxedAgent()
	agent.Name = name
	agent.Spec.Runtime.OCIRef = image
	agent.Spec.Runtime.Command = []string{"/usr/local/bin/coder-agent"}
	agent.Spec.Runtime.Args = []string{"--strict"}
	return agent
}

func TestWorkflowSharedAgentMergesPolicyAndCreatesSandboxRun(t *testing.T) {
	const image = "ghcr.io/acme/coder:1.0"
	agent := sandboxedWorkflowAgent("coder", image)
	agent.Spec.Runtime.Sandbox.Egress = []agentorcav1alpha1.SandboxEgressRule{{Host: "packages.example.com"}}
	step := agentorcav1alpha1.WorkflowStep{Name: "implement", AgentRef: "coder", Input: "fix the tests"}
	wf := workflowWithSandbox(step)
	wf.Spec.Sandbox.Image = image
	r, cl, gw, jobs := sandboxWorkflowReconciler(t, wf, agent)
	reconcileWorkflow(t, r, "wf")

	name := sharedSandboxName(wf)
	router := gw.specs[name].Policy.NetworkPolicies["agentorca-router"]
	if len(router.Endpoints) != 1 || router.Endpoints[0].Host != "agentorca-router-wf-implement.default.svc.cluster.local" {
		t.Errorf("router endpoints = %+v", router.Endpoints)
	}
	foundAgentEgress := false
	for _, rule := range gw.specs[name].Policy.NetworkPolicies {
		for _, ep := range rule.Endpoints {
			if ep.Host == "packages.example.com" {
				foundAgentEgress = true
			}
		}
	}
	if !foundAgentEgress {
		t.Error("the shared policy lost the Agent's egress")
	}

	markSandboxReady(t, gw, wf)
	reconcileWorkflow(t, r, "wf")
	jobs.complete(name, workflowCheckoutJobID, 0, testCheckoutSHA)
	reconcileWorkflow(t, r, "wf")
	got := getWorkflow(t, cl, "wf")
	ss := stepOf(t, got, "implement")
	runName := "wf-implement"
	if ss.AgentRunRef != runName || ss.JobID != openshell.WorkflowJobName("default", runName) {
		t.Fatalf("step = %+v", ss)
	}
	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: runName}, &run); err != nil {
		t.Fatal(err)
	}
	ref := run.Spec.WorkflowSandbox
	if ref == nil || ref.Name != name || ref.ID != "id-"+name || ref.Fresh || run.Spec.RestartPolicy == nil || run.Spec.RestartPolicy.MaxRetries != 0 {
		t.Errorf("AgentRun sandbox ref = %+v restart = %+v", ref, run.Spec.RestartPolicy)
	}
}

func TestWorkflowFreshAgentChecksOutBeforeCreatingRun(t *testing.T) {
	const image = "ghcr.io/acme/verifier:2"
	agent := sandboxedWorkflowAgent("verifier", image)
	step := agentorcav1alpha1.WorkflowStep{Name: "verify", AgentRef: "verifier", Input: "check the evidence", Sandbox: agentorcav1alpha1.WorkflowStepSandboxFresh}
	wf := workflowWithSandbox(step)
	r, cl, gw, jobs := sandboxWorkflowReconciler(t, wf, agent)
	reconcileWorkflow(t, r, "wf")
	sharedName := sharedSandboxName(wf)
	markSandboxReady(t, gw, wf)
	reconcileWorkflow(t, r, "wf")
	jobs.complete(sharedName, workflowCheckoutJobID, 0, testCheckoutSHA)
	reconcileWorkflow(t, r, "wf")

	freshName := openshell.WorkflowStepSandboxName(wf.Namespace, wf.Name, "verify")
	if gw.specs[freshName].Template.Image != image {
		t.Errorf("fresh image = %q", gw.specs[freshName].Template.Image)
	}
	fresh := gw.sandboxes[freshName]
	fresh.Status.Phase = osv1.SandboxReady
	reconcileWorkflow(t, r, "wf")
	jobs.complete(freshName, workflowCheckoutJobID, 0, testCheckoutSHA)
	reconcileWorkflow(t, r, "wf")

	got := getWorkflow(t, cl, "wf")
	ss := stepOf(t, got, "verify")
	if ss.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseReady || ss.AgentRunRef != "wf-verify" {
		t.Fatalf("fresh agent step = %+v", ss)
	}
	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "wf-verify"}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Spec.WorkflowSandbox == nil || !run.Spec.WorkflowSandbox.Fresh || run.Spec.WorkflowSandbox.Name != freshName {
		t.Errorf("fresh AgentRun ref = %+v", run.Spec.WorkflowSandbox)
	}
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	run.Status.Output = "verified"
	if err := cl.Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	reconcileWorkflow(t, r, "wf")
	got = getWorkflow(t, cl, "wf")
	if got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseSucceeded || stepOf(t, got, "verify").Output != "verified" {
		t.Errorf("workflow phase=%q step=%+v", got.Status.Phase, stepOf(t, got, "verify"))
	}
	if _, exists := gw.sandboxes[freshName]; exists {
		t.Error("fresh agent sandbox was not deleted")
	}
}

func TestWorkflowSharedAgentRejectsDifferentImage(t *testing.T) {
	agent := sandboxedWorkflowAgent("coder", "ghcr.io/acme/other:1")
	wf := workflowWithSandbox(agentorcav1alpha1.WorkflowStep{Name: "implement", AgentRef: "coder", Input: "fix"})
	r, cl, gw, _ := sandboxWorkflowReconciler(t, wf, agent)
	reconcileWorkflow(t, r, "wf")
	got := getWorkflow(t, cl, "wf")
	if got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseFailed || got.Status.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseFailed {
		t.Fatalf("workflow phase=%q sandbox=%+v", got.Status.Phase, got.Status.Sandbox)
	}
	if len(gw.sandboxes) != 0 {
		t.Error("a sandbox was created for incompatible shared agents")
	}
}

func TestWorkflowSandboxClassAndGitCredentials(t *testing.T) {
	wf := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
	wf.Spec.Sandbox.ClassRef = "build"
	class := &agentorcav1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "build"},
		Spec: agentorcav1alpha1.SandboxClassSpec{
			Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce,
			Egress:      []agentorcav1alpha1.SandboxEgressRule{{Host: "packages.example.com"}},
			Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
			}},
			NodeSelector: map[string]string{"pool": "sandbox"},
		},
	}
	r, cl, gw, _ := sandboxWorkflowReconciler(t, wf, class)
	minter := &stubMinter{ttl: time.Hour}
	r.GitHubApp = minter
	reconcileWorkflow(t, r, "wf")

	name := sharedSandboxName(wf)
	spec := gw.specs[name]
	if spec == nil || spec.Template.Resources["limits"].(map[string]any)["cpu"] != "2" || spec.Template.DriverConfig == nil {
		t.Errorf("class resources or placement missing: %+v", spec.Template)
	}
	if len(minter.reqs) != 1 || !equalTokenRequest(minter.reqs[0], githubapp.TokenRequest{
		Repositories: []string{"agent-orca"}, Permissions: map[string]string{"contents": "read", "metadata": "read"},
	}) {
		t.Errorf("token requests = %+v", minter.reqs)
	}
	providerName := workflowGitProviderName(wf)
	if provider := gw.providers[providerName]; provider == nil || provider.Spec.Credentials[openshell.GitTokenEnvVar] != testGitToken {
		t.Errorf("git provider = %+v", gw.providers[providerName])
	}
	rule, ok := spec.Policy.NetworkPolicies["agentorca-git-github"]
	if !ok || len(rule.Endpoints) != 1 || rule.Endpoints[0].CredentialBinding == nil || rule.Endpoints[0].CredentialBinding.Provider != providerName {
		t.Fatalf("credential-bound git rule = %+v", rule)
	}
	if rule.Endpoints[0].Enforcement != osv1.NetworkEnforcementModeEnforce {
		t.Errorf("git enforcement = %q, want the class's enforce mode", rule.Endpoints[0].Enforcement)
	}
	foundClassEndpoint := false
	for _, rule := range spec.Policy.NetworkPolicies {
		for _, ep := range rule.Endpoints {
			if ep.Host == "packages.example.com" {
				foundClassEndpoint = true
			}
		}
	}
	if !foundClassEndpoint {
		t.Error("the SandboxClass egress was not applied")
	}
	if got := getWorkflow(t, cl, "wf"); got.Status.Sandbox.GitTokenExpiry == nil {
		t.Error("the token expiry was not recorded for refresh")
	}
}

func TestWorkflowSandboxPushMintsWriteToken(t *testing.T) {
	wf := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
	wf.Spec.Sandbox.Push = true
	r, _, _, _ := sandboxWorkflowReconciler(t, wf)
	minter := &stubMinter{ttl: time.Hour}
	r.GitHubApp = minter
	reconcileWorkflow(t, r, "wf")
	if len(minter.reqs) != 1 || minter.reqs[0].Permissions["contents"] != "write" {
		t.Errorf("token requests = %+v, want contents: write", minter.reqs)
	}
}

func equalTokenRequest(a, b githubapp.TokenRequest) bool {
	if strings.Join(a.Repositories, ",") != strings.Join(b.Repositories, ",") || len(a.Permissions) != len(b.Permissions) {
		return false
	}
	for k, v := range b.Permissions {
		if a.Permissions[k] != v {
			return false
		}
	}
	return true
}

func TestWorkflowSandboxDeletionRunsFinalizerCleanup(t *testing.T) {
	wf := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
	r, cl, gw, _ := sandboxWorkflowReconciler(t, wf)
	reconcileWorkflow(t, r, "wf")
	name := sharedSandboxName(wf)
	if err := cl.Delete(context.Background(), getWorkflow(t, cl, "wf")); err != nil {
		t.Fatal(err)
	}
	reconcileWorkflow(t, r, "wf")
	if len(gw.deleted) != 1 || gw.deleted[0] != name {
		t.Errorf("deleted = %v, want %s", gw.deleted, name)
	}
	var gone agentorcav1alpha1.AgentWorkflow
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "wf"}, &gone); !apierrors.IsNotFound(err) {
		t.Errorf("workflow still exists after finalizer cleanup: %v", err)
	}
}

func TestWorkflowSandboxValidation(t *testing.T) {
	valid := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1")).Spec
	if problems := validateWorkflowSandbox(&valid); len(problems) != 0 {
		t.Fatalf("valid sandbox got problems: %v", problems)
	}
	for name, tc := range map[string]struct {
		mutate func(*agentorcav1alpha1.AgentWorkflowSpec)
		want   string
	}{
		"placement without a sandbox": {func(s *agentorcav1alpha1.AgentWorkflowSpec) {
			s.Sandbox = nil
			s.Steps[0].Sandbox = agentorcav1alpha1.WorkflowStepSandboxFresh
		}, "spec.sandbox is not configured"},
		"different shared image": {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[0].Container.Image = "other" }, "must equal spec.sandbox.image"},
		"no exec command":        {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[0].Container.Command = nil }, "needs container.command"},
		"pod-only resources": {func(s *agentorcav1alpha1.AgentWorkflowSpec) {
			s.Steps[0].Container.Resources = &corev1.ResourceRequirements{}
		}, "apply only to pod steps"},
		"bad repository":            {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Sandbox.Repo = "https://github.com/a/b" }, "owner/name"},
		"undeclared dynamic agents": {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.AdaptivePolicy = &agentorcav1alpha1.AdaptivePolicy{} }, "allowedAgentRefs"},
		"approval placement": {func(s *agentorcav1alpha1.AgentWorkflowSpec) {
			s.Steps = []agentorcav1alpha1.WorkflowStep{{Name: "gate", Type: agentorcav1alpha1.WorkflowStepTypeApproval, Sandbox: agentorcav1alpha1.WorkflowStepSandboxShared, Approval: &agentorcav1alpha1.ApprovalStep{Message: "ok"}}}
		}, "cannot set sandbox"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := *valid.DeepCopy()
			tc.mutate(&spec)
			if problems := validateWorkflowSandbox(&spec); !hasProblem(problems, tc.want) {
				t.Errorf("problems = %v, want one containing %q", problems, tc.want)
			}
		})
	}
}

type recordingTextNotifier struct{ texts []string }

func (n *recordingTextNotifier) NotifyText(_ context.Context, text string) error {
	n.texts = append(n.texts, text)
	return nil
}

func TestWorkflowCompletionNotifiesOnceWhenOptedIn(t *testing.T) {
	optedIn := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
	optedIn.Annotations = map[string]string{notifyOnCompletionAnnotation: "true"}
	optedIn.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseFailed
	optedIn.Status.Steps = []agentorcav1alpha1.WorkflowStepStatus{
		{Name: "test", Phase: agentorcav1alpha1.WorkflowStepPhaseFailed, FailureReason: "exit 2"},
	}
	r, _, _, _ := sandboxWorkflowReconciler(t, optedIn)
	notifier := &recordingTextNotifier{}
	r.CompletionNotifier = notifier
	reconcileWorkflow(t, r, "wf")
	reconcileWorkflow(t, r, "wf")
	if len(notifier.texts) != 1 || !strings.Contains(notifier.texts[0], "Step test failed: exit 2") {
		t.Errorf("notifications = %q, want one naming the failed step", notifier.texts)
	}

	quiet := workflowWithSandbox(sandboxContainerStep("test", "ghcr.io/heddles/dev-sandbox:1"))
	quiet.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseSucceeded
	r, _, _, _ = sandboxWorkflowReconciler(t, quiet)
	notifier = &recordingTextNotifier{}
	r.CompletionNotifier = notifier
	reconcileWorkflow(t, r, "wf")
	if len(notifier.texts) != 0 {
		t.Errorf("a workflow without the annotation was announced: %q", notifier.texts)
	}
}
