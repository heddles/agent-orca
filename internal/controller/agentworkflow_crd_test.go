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
