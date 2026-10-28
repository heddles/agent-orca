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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// The AgentWorkflow CRD's admission rules for templates, checked against a real
// API server: the CEL rules and the schema are not exercised by fake clients.
var _ = Describe("AgentWorkflow template admission", func() {
	ctx := context.Background()

	create := func(name string, spec agentorcav1alpha1.AgentWorkflowSpec) (*agentorcav1alpha1.AgentWorkflow, error) {
		wf := &agentorcav1alpha1.AgentWorkflow{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
		err := k8sClient.Create(ctx, wf)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wf) })
		}
		return wf, err
	}
	oneStep := []agentorcav1alpha1.WorkflowStep{{Name: "a", AgentRef: "x", Input: "y"}}

	It("defaults mode to workflow and still requires steps", func() {
		wf, err := create("crd-plain", agentorcav1alpha1.AgentWorkflowSpec{Steps: oneStep})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wf), wf)).To(Succeed())
		Expect(wf.Spec.Mode).To(Equal(agentorcav1alpha1.WorkflowModeWorkflow))

		_, err = create("crd-empty", agentorcav1alpha1.AgentWorkflowSpec{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.steps is required unless spec.fromTemplate is set"))
	})

	It("accepts a template with typed parameters and an instance with no steps", func() {
		_, err := create("crd-template", agentorcav1alpha1.AgentWorkflowSpec{
			Mode:       agentorcav1alpha1.WorkflowModeTemplate,
			Parameters: []agentorcav1alpha1.WorkflowParameter{{Name: "repo", Required: true}, {Name: "n", Type: "integer"}},
			Steps:      oneStep,
		})
		Expect(err).NotTo(HaveOccurred())

		inst, err := create("crd-instance", agentorcav1alpha1.AgentWorkflowSpec{
			FromTemplate: &agentorcav1alpha1.WorkflowFromTemplate{Name: "crd-template", Parameters: map[string]string{"repo": "a/b"}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), inst)).To(Succeed())
		Expect(inst.Spec.Steps).To(BeEmpty())
	})

	It("rejects a template created from another template", func() {
		_, err := create("crd-chain", agentorcav1alpha1.AgentWorkflowSpec{
			Mode:         agentorcav1alpha1.WorkflowModeTemplate,
			Steps:        oneStep,
			FromTemplate: &agentorcav1alpha1.WorkflowFromTemplate{Name: "other"},
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("a template cannot be created from another template"))
	})

	It("rejects parameters on a workflow that is not a template", func() {
		_, err := create("crd-params", agentorcav1alpha1.AgentWorkflowSpec{
			Steps:      oneStep,
			Parameters: []agentorcav1alpha1.WorkflowParameter{{Name: "repo"}},
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.parameters is only for mode: template"))
	})

	It("rejects badly named parameters and unknown types", func() {
		for name, p := range map[string]agentorcav1alpha1.WorkflowParameter{
			"crd-badname": {Name: "1repo"},
			"crd-badtype": {Name: "repo", Type: "object"},
		} {
			_, err := create(name, agentorcav1alpha1.AgentWorkflowSpec{Mode: agentorcav1alpha1.WorkflowModeTemplate, Steps: oneStep, Parameters: []agentorcav1alpha1.WorkflowParameter{p}})
			Expect(err).To(HaveOccurred(), name)
		}
	})

	It("accepts the Template phase in status", func() {
		wf, err := create("crd-phase", agentorcav1alpha1.AgentWorkflowSpec{Mode: agentorcav1alpha1.WorkflowModeTemplate, Steps: oneStep})
		Expect(err).NotTo(HaveOccurred())
		wf.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseTemplate
		Expect(k8sClient.Status().Update(ctx, wf)).To(Succeed())
	})
})

// Step types and their admission rules.
var _ = Describe("AgentWorkflow step type admission", func() {
	ctx := context.Background()

	create := func(name string, steps ...agentorcav1alpha1.WorkflowStep) (*agentorcav1alpha1.AgentWorkflow, error) {
		wf := &agentorcav1alpha1.AgentWorkflow{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       agentorcav1alpha1.AgentWorkflowSpec{Steps: steps},
		}
		err := k8sClient.Create(ctx, wf)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wf) })
		}
		return wf, err
	}
	container := agentorcav1alpha1.WorkflowStepTypeContainer
	approval := agentorcav1alpha1.WorkflowStepTypeApproval

	It("defaults a step to an agent step and accepts the three types together", func() {
		wf, err := create("steps-mixed",
			agentorcav1alpha1.WorkflowStep{Name: "think", AgentRef: "planner", Input: "plan"},
			agentorcav1alpha1.WorkflowStep{Name: "test", Type: container, DependsOn: []string{"think"},
				Container: &agentorcav1alpha1.ContainerStep{Image: "busybox", Command: []string{"true"},
					Env: []agentorcav1alpha1.StepEnvVar{{Name: "PLAN", Value: "{{steps.think.output}}"}}}},
			agentorcav1alpha1.WorkflowStep{Name: "gate", Type: approval, DependsOn: []string{"test"},
				Approval: &agentorcav1alpha1.ApprovalStep{Message: "ship?", ApproverGroups: []string{"release"}}},
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wf), wf)).To(Succeed())
		Expect(wf.Spec.Steps[0].Type).To(Equal(agentorcav1alpha1.WorkflowStepTypeAgent))
	})

	DescribeTable("rejects steps that do not match their type",
		func(step agentorcav1alpha1.WorkflowStep, message string) {
			_, err := create("steps-bad", step)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("an agent step with no agentRef", agentorcav1alpha1.WorkflowStep{Name: "a", Input: "x"}, "an agent step needs agentRef and input"),
		Entry("an agent step with no input", agentorcav1alpha1.WorkflowStep{Name: "a", AgentRef: "x"}, "an agent step needs agentRef and input"),
		Entry("a container step with no container block",
			agentorcav1alpha1.WorkflowStep{Name: "a", Type: container}, "a container step needs a container block"),
		Entry("a container step that also names an agent",
			agentorcav1alpha1.WorkflowStep{Name: "a", Type: container, AgentRef: "x", Container: &agentorcav1alpha1.ContainerStep{Image: "i"}},
			"agentRef and input are only for agent steps"),
		Entry("a container block on an agent step",
			agentorcav1alpha1.WorkflowStep{Name: "a", AgentRef: "x", Input: "y", Container: &agentorcav1alpha1.ContainerStep{Image: "i"}},
			"the container block is only for container steps"),
		Entry("an approval step with no approval block",
			agentorcav1alpha1.WorkflowStep{Name: "a", Type: approval}, "an approval step needs an approval block"),
		Entry("an approval block on a container step",
			agentorcav1alpha1.WorkflowStep{Name: "a", Type: container, Container: &agentorcav1alpha1.ContainerStep{Image: "i"}, Approval: &agentorcav1alpha1.ApprovalStep{Message: "m"}},
			"the approval block is only for approval steps"),
	)

	It("rejects an unknown step type, root, a bad env name and an empty message or image", func() {
		bad := map[string]agentorcav1alpha1.WorkflowStep{
			"steps-type": {Name: "a", Type: "shell"},
			"steps-root": {Name: "a", Type: container, Container: &agentorcav1alpha1.ContainerStep{Image: "i", RunAsUser: new(int64)}},
			"steps-env": {Name: "a", Type: container, Container: &agentorcav1alpha1.ContainerStep{Image: "i",
				Env: []agentorcav1alpha1.StepEnvVar{{Name: "1BAD", Value: "x"}}}},
			"steps-msg":   {Name: "a", Type: approval, Approval: &agentorcav1alpha1.ApprovalStep{}},
			"steps-image": {Name: "a", Type: container, Container: &agentorcav1alpha1.ContainerStep{}},
		}
		for name, step := range bad {
			_, err := create(name, step)
			Expect(err).To(HaveOccurred(), name)
		}
	})

	It("accepts the WaitingForApproval step phase in status", func() {
		wf, err := create("steps-phase", agentorcav1alpha1.WorkflowStep{Name: "gate", Type: approval, Approval: &agentorcav1alpha1.ApprovalStep{Message: "ok?"}})
		Expect(err).NotTo(HaveOccurred())
		wf.Status.Steps = []agentorcav1alpha1.WorkflowStepStatus{{Name: "gate", Phase: agentorcav1alpha1.WorkflowStepPhaseWaitingForApproval, ApprovalID: "a1"}}
		Expect(k8sClient.Status().Update(ctx, wf)).To(Succeed())
	})
})

var _ = Describe("AgentWorkflow sandbox admission", func() {
	ctx := context.Background()
	container := agentorcav1alpha1.WorkflowStepTypeContainer

	create := func(name string, spec agentorcav1alpha1.AgentWorkflowSpec) error {
		wf := &agentorcav1alpha1.AgentWorkflow{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
		err := k8sClient.Create(ctx, wf)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wf) })
		}
		return err
	}
	spec := func() agentorcav1alpha1.AgentWorkflowSpec {
		return agentorcav1alpha1.AgentWorkflowSpec{
			Sandbox: &agentorcav1alpha1.WorkflowSandboxSpec{Image: "ghcr.io/acme/dev:1", Repo: "acme/widgets", Ref: "main", Branch: "factory/ticket-1"},
			Steps: []agentorcav1alpha1.WorkflowStep{{Name: "test", Type: container, Sandbox: agentorcav1alpha1.WorkflowStepSandboxFresh,
				Container: &agentorcav1alpha1.ContainerStep{Image: "ghcr.io/acme/dev:1", Command: []string{"make", "test"}}}},
		}
	}

	It("accepts a workflow sandbox and records its status", func() {
		s := spec()
		Expect(create("sandbox-valid", s)).To(Succeed())
		wf := &agentorcav1alpha1.AgentWorkflow{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sandbox-valid"}, wf)).To(Succeed())
		wf.Status.Sandbox = &agentorcav1alpha1.WorkflowSandboxStatus{Name: "ao-1", ID: "sandbox-1", Phase: agentorcav1alpha1.WorkflowSandboxPhaseReady, CheckoutSHA: "0123456789abcdef0123456789abcdef01234567"}
		wf.Status.Steps = []agentorcav1alpha1.WorkflowStepStatus{{Name: "test", Phase: agentorcav1alpha1.WorkflowStepPhaseRunning, JobID: "job-1"}}
		Expect(k8sClient.Status().Update(ctx, wf)).To(Succeed())
	})

	It("rejects a malformed repository and a sandbox on an approval step", func() {
		s := spec()
		s.Sandbox.Repo = "https://github.com/acme/widgets"
		Expect(create("sandbox-bad-repo", s)).NotTo(Succeed())

		s = spec()
		s.Steps = []agentorcav1alpha1.WorkflowStep{{Name: "gate", Type: agentorcav1alpha1.WorkflowStepTypeApproval,
			Sandbox: agentorcav1alpha1.WorkflowStepSandboxShared, Approval: &agentorcav1alpha1.ApprovalStep{Message: "ship?"}}}
		err := create("sandbox-approval", s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sandbox is only for agent and container steps"))
	})
})
