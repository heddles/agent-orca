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
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

var errWorkflowAgentUnusable = errors.New("workflow agent unusable")

func workflowAgentRunName(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, iteration int32) string {
	return fmt.Sprintf("%s-%s", wf.Name, iterStepName(step.Name, iteration))
}

// workflowRouterHost is the router Service hostname of one step's AgentRun, as
// seen from the sandbox. It is a literal name: OpenShell resolves policy hosts
// ahead of time, and a wildcard host has no mapping, so the supervisor refuses
// to resolve any name it matches (policy_dns_no_valid_address).
func (r *AgentWorkflowReconciler) workflowRouterHost(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, iteration int32) string {
	domain := r.ClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	return fmt.Sprintf("agentorca-router-%s.%s.svc.%s", workflowAgentRunName(wf, step, iteration), wf.Namespace, domain)
}

// workflowSharedRouterHosts lists the router hostnames of every agent step that
// runs in the shared sandbox. Dynamic (adaptive) steps and clarification
// continuations get generated run names and cannot be listed ahead of time.
func (r *AgentWorkflowReconciler) workflowSharedRouterHosts(wf *agentorcav1alpha1.AgentWorkflow) []string {
	var hosts []string
	for i := range wf.Spec.Steps {
		step := &wf.Spec.Steps[i]
		if stepType(step) == agentorcav1alpha1.WorkflowStepTypeAgent && step.Sandbox != agentorcav1alpha1.WorkflowStepSandboxFresh {
			for pass := int32(0); pass < workflowStepPasses(wf, step); pass++ {
				hosts = append(hosts, r.workflowRouterHost(wf, step, pass))
			}
		}
	}
	return hosts
}

func workflowAgentStep(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep) *agentorcav1alpha1.AgentRun {
	fresh := step != nil && step.Sandbox == agentorcav1alpha1.WorkflowStepSandboxFresh
	name := ""
	labels := map[string]string{labelWorkflow: wf.Name}
	if step != nil {
		name = workflowAgentRunName(wf, step, 0)
		labels[labelWorkflowStep] = step.Name
	}
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, Labels: labels},
		Spec:       agentorcav1alpha1.AgentRunSpec{WorkflowSandbox: &agentorcav1alpha1.WorkflowSandboxRef{Fresh: fresh}},
	}
}

func validateWorkflowAgent(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, agent *agentorcav1alpha1.Agent) error {
	if agent.Spec.Runtime.Sandbox == nil {
		return fmt.Errorf("%w: Agent %q has no spec.runtime.sandbox", errWorkflowAgentUnusable, agent.Name)
	}
	if agent.Spec.NetworkIsolation == nil || !agent.Spec.NetworkIsolation.SplitPod {
		return fmt.Errorf("%w: Agent %q needs spec.networkIsolation.splitPod=true", errWorkflowAgentUnusable, agent.Name)
	}
	if len(agent.Spec.Runtime.Command) == 0 {
		return fmt.Errorf("%w: Agent %q needs spec.runtime.command for sandbox exec", errWorkflowAgentUnusable, agent.Name)
	}
	if agent.Spec.Runtime.InputMode == "http" {
		return fmt.Errorf("%w: Agent %q uses inputMode=http; workflow sandboxes support env input only", errWorkflowAgentUnusable, agent.Name)
	}
	if step != nil && step.Sandbox != agentorcav1alpha1.WorkflowStepSandboxFresh && agent.Spec.Runtime.OCIRef != wf.Spec.Sandbox.Image {
		return fmt.Errorf("%w: shared Agent %q image %q must equal spec.sandbox.image %q", errWorkflowAgentUnusable, agent.Name, agent.Spec.Runtime.OCIRef, wf.Spec.Sandbox.Image)
	}
	return nil
}

func (r *AgentWorkflowReconciler) workflowAgent(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, agentRef string) (*agentorcav1alpha1.Agent, error) {
	var agent agentorcav1alpha1.Agent
	if err := r.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: agentRef}, &agent); err != nil {
		return nil, fmt.Errorf("reading Agent %q for the workflow sandbox: %w", agentRef, err)
	}
	if err := validateWorkflowAgent(wf, step, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// workflowPolicyAgents returns the shared agents whose policies must be known
// before sandbox creation. Dynamic steps are limited to allowedAgentRefs so a
// proposal cannot introduce an agent whose tools were never merged.
func workflowPolicyAgents(wf *agentorcav1alpha1.AgentWorkflow) map[string]*agentorcav1alpha1.WorkflowStep {
	out := map[string]*agentorcav1alpha1.WorkflowStep{}
	for i := range wf.Spec.Steps {
		step := &wf.Spec.Steps[i]
		if stepType(step) == agentorcav1alpha1.WorkflowStepTypeAgent && step.Sandbox != agentorcav1alpha1.WorkflowStepSandboxFresh {
			out[step.AgentRef] = step
		}
	}
	if wf.Spec.AdaptivePolicy != nil {
		for _, ref := range wf.Spec.AdaptivePolicy.AllowedAgentRefs {
			if _, exists := out[ref]; !exists {
				out[ref] = nil
			}
		}
	}
	return out
}

func workflowAgentPolicyIncluded(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep) bool {
	_, ok := workflowPolicyAgents(wf)[step.AgentRef]
	return ok
}

// mergeWorkflowAgentPolicies folds every declared shared Agent's class, inline
// egress and tools into the workflow sandbox. Roles deliberately share this
// union for the life of the sandbox; their routers and tokens remain per-run.
func (r *AgentWorkflowReconciler) mergeWorkflowAgentPolicies(
	ctx context.Context,
	wf *agentorcav1alpha1.AgentWorkflow,
	base *agentorcav1alpha1.Agent,
) ([]openshell.ToolEndpoint, []string, error) {
	var tools []openshell.ToolEndpoint
	var providers []string
	policyAgents := workflowPolicyAgents(wf)
	for _, ref := range slices.Sorted(maps.Keys(policyAgents)) {
		step := policyAgents[ref]
		agent, err := r.workflowAgent(ctx, wf, step, ref)
		if err != nil {
			return nil, nil, err
		}
		class, err := loadSandboxClass(ctx, r.Client, wf.Namespace, agent.Spec.Runtime.Sandbox.ClassRef)
		if err != nil {
			return nil, nil, err
		}
		agent, _ = openshell.ApplyClass(agent, class, openshell.Placement{})
		base.Spec.Runtime.Sandbox = openshell.MergeSandboxSpecs(base.Spec.Runtime.Sandbox, agent.Spec.Runtime.Sandbox)

		run := workflowAgentStep(wf, step)
		run.Spec.AgentRef = ref
		if step == nil {
			// Dynamic runs have no name yet. Shared provider naming is scoped to
			// the workflow, so the placeholder step is sufficient here.
			run = workflowAgentStep(wf, &agentorcav1alpha1.WorkflowStep{Name: "dynamic", AgentRef: ref})
		}
		eps, used, err := ensureSandboxTools(ctx, r.Client, r.Sandboxes, run, agent, map[string]string{})
		if err != nil {
			return nil, nil, fmt.Errorf("preparing tools of Agent %q: %w", ref, err)
		}
		for _, ep := range eps {
			if !slices.Contains(tools, ep) {
				tools = append(tools, ep)
			}
		}
		for _, provider := range used {
			if !slices.Contains(providers, provider) {
				providers = append(providers, provider)
			}
		}
	}
	return tools, providers, nil
}

// freshAgentSandboxSpec builds a dedicated Agent image with the union of the
// workflow and Agent classes. Resources default from the workflow class and
// may be overridden by the Agent; the Agent class has the final placement.
func (r *AgentWorkflowReconciler) freshAgentSandboxSpec(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, iteration int32) (*osv1.SandboxSpec, error) {
	agent, err := r.workflowAgent(ctx, wf, step, step.AgentRef)
	if err != nil {
		return nil, err
	}
	workflowClass, err := loadSandboxClass(ctx, r.Client, wf.Namespace, wf.Spec.Sandbox.ClassRef)
	if err != nil {
		return nil, err
	}
	agentClass, err := loadSandboxClass(ctx, r.Client, wf.Namespace, agent.Spec.Runtime.Sandbox.ClassRef)
	if err != nil {
		return nil, err
	}
	agent, placement := openshell.ApplyClass(agent, workflowClass, r.SandboxPlacement)
	agent, placement = openshell.ApplyClass(agent, agentClass, placement)

	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	run := workflowAgentStep(wf, step)
	tools, providers, err := ensureSandboxTools(ctx, r.Client, r.Sandboxes, run, agent, env)
	if err != nil {
		return nil, err
	}
	var git *openshell.GitAccess
	if r.GitHubApp != nil {
		git = &openshell.GitAccess{Repositories: []string{wf.Spec.Sandbox.Repo}, Provider: workflowGitProviderName(wf)}
		maps.Copy(env, openshell.GitEnv())
		providers = append(providers, git.Provider)
	}
	// The workflow, not OpenShell's main process, owns the agent lifecycle.
	agent.Spec.Runtime.Command = []string{"sh", "-c", `while :; do sleep 3600; done`}
	agent.Spec.Runtime.Args = nil
	return openshell.BuildSpec(openshell.SpecInput{
		Agent: agent, RouterHost: r.workflowRouterHost(wf, step, iteration), Env: env,
		Tools: tools, Providers: providers, Placement: placement, Git: git,
	}), nil
}

func (r *AgentWorkflowReconciler) startFreshSandboxAgentStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) error {
	agent, err := r.workflowAgent(ctx, wf, step, step.AgentRef)
	if err != nil {
		if errors.Is(err, errWorkflowAgentUnusable) || apierrors.IsNotFound(err) {
			finishStep(wf, ss, false, "", err.Error())
			return nil
		}
		return err
	}
	if err := agentwebhook.CheckRegistryAllowlist(agent.Spec.Runtime.OCIRef, r.AllowedRegistries); err != nil {
		finishStep(wf, ss, false, "", fmt.Sprintf("image rejected: %v", err))
		return nil
	}
	spec, err := r.freshAgentSandboxSpec(ctx, wf, step, ss.NameGeneration())
	if err != nil {
		if errors.Is(err, errSandboxClassUnusable) || errors.Is(err, errWorkflowAgentUnusable) {
			finishStep(wf, ss, false, "", err.Error())
			return nil
		}
		return err
	}
	name := openshell.WorkflowStepSandboxName(wf.Namespace, wf.Name, iterStepName(step.Name, ss.NameGeneration()))
	labels := workflowSandboxLabels(wf)
	labels[labelWorkflowStep] = step.Name
	sb, err := r.Sandboxes.Create(ctx, name, spec, labels)
	if err != nil {
		if osv1.IsInvalidArgument(err) || osv1.IsPermissionDenied(err) {
			finishStep(wf, ss, false, "", fmt.Sprintf("creating fresh agent sandbox: %v", err))
			return nil
		}
		return fmt.Errorf("creating fresh agent sandbox for step %q: %w", step.Name, err)
	}
	now := metav1.Now()
	ss.Phase = agentorcav1alpha1.WorkflowStepPhaseRunning
	ss.JobID = workflowStepJobID(wf, step, ss.NameGeneration())
	ss.StartTime = &now
	ss.Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{Name: name, ID: sb.ID, Phase: agentorcav1alpha1.WorkflowSandboxPhaseProvisioning}
	markWorkflowStarted(wf, now)
	return nil
}

func (r *AgentWorkflowReconciler) checkFreshSandboxAgentStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) (ctrl.Result, error) {
	if ss.StartTime != nil && time.Since(ss.StartTime.Time) >= stepTimeout(wf, step, 5*time.Minute) {
		r.deleteFreshStepSandbox(ctx, ss)
		finishStep(wf, ss, false, "", "the step timed out")
		return ctrl.Result{}, nil
	}
	ready, result, failure, err := r.ensureFreshStepSandbox(ctx, wf, step, ss)
	if err != nil {
		return result, err
	}
	if failure != "" {
		ss.Sandbox.Phase = agentorcav1alpha1.WorkflowSandboxPhaseFailed
		r.deleteFreshStepSandbox(ctx, ss)
		finishStep(wf, ss, false, "", failure)
		return ctrl.Result{}, nil
	}
	if !ready {
		return result, nil
	}
	if err := r.startAgentStep(ctx, wf, step, ss); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
}

func (r *AgentWorkflowReconciler) checkSandboxAgentStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) (ctrl.Result, error) {
	if ss.Sandbox != nil && ss.Sandbox.Phase != agentorcav1alpha1.WorkflowSandboxPhaseReady {
		return r.checkFreshSandboxAgentStep(ctx, wf, step, ss)
	}
	result, err := r.checkStepRun(ctx, wf, step, ss)
	if err == nil && result.RequeueAfter == 0 && ss.Sandbox != nil &&
		(ss.Phase == agentorcav1alpha1.WorkflowStepPhaseSucceeded || ss.Phase == agentorcav1alpha1.WorkflowStepPhaseFailed || ss.Phase == agentorcav1alpha1.WorkflowStepPhaseSkipped) {
		r.deleteFreshStepSandbox(ctx, ss)
	}
	return result, err
}
