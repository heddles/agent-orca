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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newWorkflow creates and applies an AgentWorkflow to the API server.
func newWorkflow(ctx context.Context, name string, spec agentorcav1alpha1.AgentWorkflowSpec) *agentorcav1alpha1.AgentWorkflow {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
	Expect(k8sClient.Create(ctx, wf)).To(Succeed())
	return wf
}

// reconcileWF calls Reconcile once and returns the result.
func reconcileWF(ctx context.Context, name string) reconcile.Result {
	r := &AgentWorkflowReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

// getWF fetches the latest AgentWorkflow.
func getWF(ctx context.Context, name string) *agentorcav1alpha1.AgentWorkflow {
	wf := &agentorcav1alpha1.AgentWorkflow{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, wf)).To(Succeed())
	return wf
}

// getAR fetches the latest AgentRun (returns nil if not found).
func getAR(ctx context.Context, name string) *agentorcav1alpha1.AgentRun {
	ar := &agentorcav1alpha1.AgentRun{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, ar); err != nil {
		return nil
	}
	return ar
}

// succeedAR patches an AgentRun's status to Succeeded with the given output and spend.
func succeedAR(ctx context.Context, name, output, spend string) {
	ar := &agentorcav1alpha1.AgentRun{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, ar)).To(Succeed())
	ar.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	ar.Status.Output = output
	ar.Status.SpendUSD = spend
	Expect(k8sClient.Status().Update(ctx, ar)).To(Succeed())
}

// failAR patches an AgentRun's status to Failed.
func failAR(ctx context.Context, name, reason string) {
	ar := &agentorcav1alpha1.AgentRun{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, ar)).To(Succeed())
	ar.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	ar.Status.RawOutput = reason
	Expect(k8sClient.Status().Update(ctx, ar)).To(Succeed())
}

// stepStatus returns the WorkflowStepStatus for the named step, or nil.
func stepStatus(wf *agentorcav1alpha1.AgentWorkflow, name string) *agentorcav1alpha1.WorkflowStepStatus {
	for i := range wf.Status.Steps {
		if wf.Status.Steps[i].Name == name {
			return &wf.Status.Steps[i]
		}
	}
	return nil
}

// ── tests ─────────────────────────────────────────────────────────────────────

var _ = Describe("AgentWorkflow Controller", func() {
	ctx := context.Background()

	// Use a unique suffix per test to avoid name collisions.
	var wfName string
	var testIdx int

	BeforeEach(func() {
		testIdx++
		wfName = fmt.Sprintf("wf-test-%d", testIdx)
	})

	AfterEach(func() {
		// Best-effort cleanup.
		wf := &agentorcav1alpha1.AgentWorkflow{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: wfName, Namespace: "default"}, wf); err == nil {
			_ = k8sClient.Delete(ctx, wf)
		}
	})

	// ── 1. Single step: Pending → Running ─────────────────────────────────────
	It("creates an AgentRun and sets step+workflow to Running on first reconcile", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "do the thing"},
			},
		})

		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseRunning))
		Expect(wf.Status.StartTime).NotTo(BeNil())

		ss := stepStatus(wf, "step1")
		Expect(ss).NotTo(BeNil())
		Expect(ss.Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(ss.AgentRunRef).To(Equal(wfName + "-step1"))
		Expect(ss.StartTime).NotTo(BeNil())

		ar := getAR(ctx, wfName+"-step1")
		Expect(ar).NotTo(BeNil())
		Expect(ar.Spec.AgentRef).To(Equal("my-agent"))
		Expect(ar.Spec.Input).To(Equal("do the thing"))

		// Owner reference is set so deletion cascades.
		Expect(ar.OwnerReferences).To(HaveLen(1))
		Expect(ar.OwnerReferences[0].Name).To(Equal(wfName))

		// Workflow-step labels are present.
		Expect(ar.Labels["agentorca.io/workflow"]).To(Equal(wfName))
		Expect(ar.Labels["agentorca.io/workflow-step"]).To(Equal("step1"))
	})

	// ── 2. Single step: full success path ─────────────────────────────────────
	It("transitions workflow to Succeeded when the single step AgentRun succeeds", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "summarise"},
			},
		})

		// First reconcile: start step1.
		reconcileWF(ctx, wfName)

		// Simulate AgentRun completing.
		succeedAR(ctx, wfName+"-step1", "great summary", "0.001234")

		// Second reconcile: observe completion.
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseSucceeded))
		Expect(wf.Status.CompletionTime).NotTo(BeNil())
		Expect(wf.Status.TotalSpendUSD).To(Equal("0.001234"))

		ss := stepStatus(wf, "step1")
		Expect(ss.Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseSucceeded))
		Expect(ss.Output).To(Equal("great summary"))
		Expect(ss.SpendUSD).To(Equal("0.001234"))
		Expect(ss.CompletionTime).NotTo(BeNil())
	})

	// ── 3. Linear chain with template variable resolution ─────────────────────
	It("threads step1 output into step2 input via {{steps.step1.output}}", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "research", AgentRef: "researcher", Input: "Research quantum computing."},
				{
					Name:      "analyze",
					AgentRef:  "analyst",
					DependsOn: []string{"research"},
					Input:     "Analyze this: {{steps.research.output}}",
				},
			},
		})

		// Reconcile #1: research starts, analyze stays Pending (dep not met).
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(stepStatus(wf, "research").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(stepStatus(wf, "analyze").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhasePending))
		Expect(getAR(ctx, wfName+"-analyze")).To(BeNil())

		// research step completes with output.
		succeedAR(ctx, wfName+"-research", "quantum computers are fast", "0.0005")

		// Reconcile #2: research → Succeeded, analyze → Running.
		reconcileWF(ctx, wfName)

		wf = getWF(ctx, wfName)
		Expect(stepStatus(wf, "research").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseSucceeded))
		Expect(stepStatus(wf, "analyze").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))

		// Template variable resolved in analyze's AgentRun input.
		ar := getAR(ctx, wfName+"-analyze")
		Expect(ar).NotTo(BeNil())
		Expect(ar.Spec.Input).To(Equal("Analyze this: quantum computers are fast"))

		// analyze completes, workflow succeeds.
		succeedAR(ctx, wfName+"-analyze", "top 3 implications", "0.0008")
		reconcileWF(ctx, wfName)

		wf = getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseSucceeded))
		Expect(wf.Status.TotalSpendUSD).To(Equal("0.001300"))
	})

	// ── 4. Fan-out: two parallel steps share a dependency ─────────────────────
	It("starts two parallel steps in the same reconcile once their shared dep succeeds", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "gather", AgentRef: "data-agent", Input: "gather data"},
				{Name: "emea", AgentRef: "analyst", DependsOn: []string{"gather"}, Input: "analyze EMEA"},
				{Name: "apac", AgentRef: "analyst", DependsOn: []string{"gather"}, Input: "analyze APAC"},
			},
		})

		// Reconcile #1: gather starts.
		reconcileWF(ctx, wfName)
		wf := getWF(ctx, wfName)
		Expect(stepStatus(wf, "gather").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(stepStatus(wf, "emea").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhasePending))
		Expect(stepStatus(wf, "apac").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhasePending))

		// gather succeeds.
		succeedAR(ctx, wfName+"-gather", "raw data", "0.001")

		// Reconcile #2: both parallel steps start in the same pass.
		reconcileWF(ctx, wfName)
		wf = getWF(ctx, wfName)
		Expect(stepStatus(wf, "emea").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(stepStatus(wf, "apac").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(getAR(ctx, wfName+"-emea")).NotTo(BeNil())
		Expect(getAR(ctx, wfName+"-apac")).NotTo(BeNil())
	})

	// ── 5. Step failure + onStepFailure=stop → workflow Failed ────────────────
	It("fails the workflow immediately when a step fails and onStepFailure=stop", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "task"},
			},
			// stop is the default; set it explicitly for clarity.
			OnStepFailure: "stop",
		})

		reconcileWF(ctx, wfName)
		failAR(ctx, wfName+"-step1", "OOM killed")
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseFailed))
		Expect(wf.Status.CompletionTime).NotTo(BeNil())

		ss := stepStatus(wf, "step1")
		Expect(ss.Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseFailed))
		Expect(ss.FailureReason).To(ContainSubstring("OOM killed"))
	})

	// ── 6. onStepFailure=continue → failed step Skipped, dependent runs ───────
	It("skips a failed step and continues with dependents when onStepFailure=continue", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			OnStepFailure: "continue",
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "optional", AgentRef: "enricher", Input: "enrich"},
				{
					Name:      "report",
					AgentRef:  "reporter",
					DependsOn: []string{"optional"},
					Input:     "Generate report. Enrichment (if any): {{steps.optional.output}}",
				},
			},
		})

		reconcileWF(ctx, wfName)
		failAR(ctx, wfName+"-optional", "external API down")

		// Reconcile: optional → Skipped (continue policy), report → Running.
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(stepStatus(wf, "optional").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseSkipped))
		Expect(stepStatus(wf, "report").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))

		// Workflow should still be Running (not Failed).
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseRunning))

		// Empty template var since optional was skipped (output="").
		ar := getAR(ctx, wfName+"-report")
		Expect(ar.Spec.Input).To(Equal("Generate report. Enrichment (if any): "))

		succeedAR(ctx, wfName+"-report", "final report", "0.002")
		reconcileWF(ctx, wfName)

		wf = getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseSucceeded))
	})

	// ── 7. CEL condition false → step Skipped ─────────────────────────────────
	It("skips a step whose condition evaluates to false", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "gate", AgentRef: "gater", Input: "gate"},
				{
					Name:      "conditional",
					AgentRef:  "writer",
					DependsOn: []string{"gate"},
					// Only run if gate failed — it won't, so this step gets skipped.
					Condition: `steps["gate"].phase == "Failed"`,
					Input:     "do conditional work",
				},
			},
		})

		reconcileWF(ctx, wfName)
		succeedAR(ctx, wfName+"-gate", "gate output", "0.0001")

		// gate → Succeeded, conditional → condition false → Skipped.
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(stepStatus(wf, "gate").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseSucceeded))
		Expect(stepStatus(wf, "conditional").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseSkipped))
		Expect(getAR(ctx, wfName+"-conditional")).To(BeNil())

		// All steps terminal → workflow Succeeded.
		reconcileWF(ctx, wfName)
		wf = getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseSucceeded))
	})

	// ── 8. CEL condition true → step runs ─────────────────────────────────────
	It("runs a step whose condition evaluates to true", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "gate", AgentRef: "gater", Input: "gate"},
				{
					Name:      "conditional",
					AgentRef:  "writer",
					DependsOn: []string{"gate"},
					Condition: `steps["gate"].phase == "Succeeded"`,
					Input:     "do conditional work",
				},
			},
		})

		reconcileWF(ctx, wfName)
		succeedAR(ctx, wfName+"-gate", "gate ok", "0.0001")
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(stepStatus(wf, "conditional").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(getAR(ctx, wfName+"-conditional")).NotTo(BeNil())
	})

	// ── 9. Budget cap exceeded → workflow Failed ──────────────────────────────
	It("fails the workflow when cumulative spend exceeds the budget cap", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			BudgetCap: &agentorcav1alpha1.WorkflowBudgetCap{Total: "0.001"},
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "expensive", AgentRef: "big-model", Input: "expensive task"},
			},
		})

		reconcileWF(ctx, wfName)
		// Spend 0.002 — exceeds the 0.001 cap.
		succeedAR(ctx, wfName+"-expensive", "output", "0.002000")
		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseFailed))
		Expect(wf.Status.TotalSpendUSD).NotTo(BeEmpty())
	})

	// ── 10. Workflow timeout → workflow Failed ────────────────────────────────
	It("fails the workflow when the wall-clock timeout is exceeded", func() {
		wf := newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Timeout: &metav1.Duration{Duration: 1 * time.Second},
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "slow", AgentRef: "slow-agent", Input: "slow task"},
			},
		})

		// Manually set StartTime in the past so timeout has already elapsed.
		past := metav1.NewTime(time.Now().Add(-10 * time.Second))
		wf.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseRunning
		wf.Status.StartTime = &past
		Expect(k8sClient.Status().Update(ctx, wf)).To(Succeed())

		reconcileWF(ctx, wfName)

		wf = getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseFailed))
		Expect(wf.Status.CompletionTime).NotTo(BeNil())
	})

	// ── 11. Terminal workflow is not re-reconciled ────────────────────────────
	It("returns immediately without changes when the workflow is already terminal", func() {
		wf := newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "task"},
			},
		})

		// Force terminal state directly.
		now := metav1.Now()
		wf.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseSucceeded
		wf.Status.CompletionTime = &now
		Expect(k8sClient.Status().Update(ctx, wf)).To(Succeed())

		result := reconcileWF(ctx, wfName)
		Expect(result.RequeueAfter).To(BeZero())

		// No AgentRun should have been created.
		Expect(getAR(ctx, wfName+"-step1")).To(BeNil())

		wf = getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseSucceeded))
	})

	// ── 12. AgentRun deleted externally → step Failed ────────────────────────
	It("marks a running step as Failed when its AgentRun is deleted externally", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "task"},
			},
		})

		reconcileWF(ctx, wfName)

		// Delete the AgentRun out-of-band.
		ar := getAR(ctx, wfName+"-step1")
		Expect(ar).NotTo(BeNil())
		Expect(k8sClient.Delete(ctx, ar)).To(Succeed())

		reconcileWF(ctx, wfName)

		wf := getWF(ctx, wfName)
		ss := stepStatus(wf, "step1")
		Expect(ss.Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseFailed))
		Expect(ss.FailureReason).To(ContainSubstring("deleted"))
	})

	// ── 13. Idempotency: reconcile on in-progress workflow is safe ────────────
	It("does not create duplicate AgentRuns when reconciled multiple times while Running", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "task"},
			},
		})

		reconcileWF(ctx, wfName)
		// Second reconcile while step is still Running.
		result := reconcileWF(ctx, wfName)
		// Should requeue after poll interval, not error.
		Expect(result.RequeueAfter).To(Equal(workflowPollInterval))

		// Still only one AgentRun.
		arList := &agentorcav1alpha1.AgentRunList{}
		Expect(k8sClient.List(ctx, arList)).To(Succeed())
		count := 0
		for _, ar := range arList.Items {
			if ar.Labels["agentorca.io/workflow"] == wfName {
				count++
			}
		}
		Expect(count).To(Equal(1))
	})

	// ── 14. Step-level timeout propagation ────────────────────────────────────
	It("passes step-level timeout to the created AgentRun", func() {
		stepTimeout := metav1.Duration{Duration: 5 * time.Minute}
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "step1", AgentRef: "my-agent", Input: "task", Timeout: &stepTimeout},
			},
		})

		reconcileWF(ctx, wfName)

		ar := getAR(ctx, wfName+"-step1")
		Expect(ar).NotTo(BeNil())
		Expect(ar.Spec.Timeout).NotTo(BeNil())
		Expect(ar.Spec.Timeout.Duration).To(Equal(5 * time.Minute))
	})

	// ── 15. Full fan-in pipeline ──────────────────────────────────────────────
	It("completes a gather → [emea, apac] → merge fan-in pipeline", func() {
		newWorkflow(ctx, wfName, agentorcav1alpha1.AgentWorkflowSpec{
			Steps: []agentorcav1alpha1.WorkflowStep{
				{Name: "gather", AgentRef: "data-agent", Input: "gather"},
				{
					Name: "emea", AgentRef: "analyst", DependsOn: []string{"gather"},
					Input: "EMEA from: {{steps.gather.output}}",
				},
				{
					Name: "apac", AgentRef: "analyst", DependsOn: []string{"gather"},
					Input: "APAC from: {{steps.gather.output}}",
				},
				{
					Name:      "merge",
					AgentRef:  "writer",
					DependsOn: []string{"emea", "apac"},
					Input:     "EMEA: {{steps.emea.output}} APAC: {{steps.apac.output}}",
				},
			},
		})

		// Phase 1: gather starts.
		reconcileWF(ctx, wfName)
		succeedAR(ctx, wfName+"-gather", "raw sales data", "0.001")

		// Phase 2: emea and apac start in parallel.
		reconcileWF(ctx, wfName)
		wf := getWF(ctx, wfName)
		Expect(stepStatus(wf, "emea").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(stepStatus(wf, "apac").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))
		Expect(stepStatus(wf, "merge").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhasePending))

		succeedAR(ctx, wfName+"-emea", "emea results", "0.001")
		succeedAR(ctx, wfName+"-apac", "apac results", "0.001")

		// Phase 3: merge starts with both outputs resolved.
		reconcileWF(ctx, wfName)
		wf = getWF(ctx, wfName)
		Expect(stepStatus(wf, "merge").Phase).To(Equal(agentorcav1alpha1.WorkflowStepPhaseRunning))

		ar := getAR(ctx, wfName+"-merge")
		Expect(ar.Spec.Input).To(ContainSubstring("EMEA: emea results"))
		Expect(ar.Spec.Input).To(ContainSubstring("APAC: apac results"))

		succeedAR(ctx, wfName+"-merge", "unified report", "0.002")

		// Phase 4: workflow Succeeded.
		reconcileWF(ctx, wfName)
		wf = getWF(ctx, wfName)
		Expect(wf.Status.Phase).To(Equal(agentorcav1alpha1.AgentWorkflowPhaseSucceeded))
		// Total spend across all 4 steps.
		Expect(wf.Status.TotalSpendUSD).To(Equal("0.005000"))
	})
})

// ── unit tests for pure functions ─────────────────────────────────────────────

var _ = Describe("resolveTemplates", func() {
	It("replaces a single placeholder", func() {
		steps := []agentorcav1alpha1.WorkflowStepStatus{
			{Name: "step1", Output: "hello world"},
		}
		Expect(resolveTemplates("Input: {{steps.step1.output}}", steps)).To(Equal("Input: hello world"))
	})

	It("replaces multiple placeholders", func() {
		steps := []agentorcav1alpha1.WorkflowStepStatus{
			{Name: "a", Output: "AAA"},
			{Name: "b", Output: "BBB"},
		}
		Expect(resolveTemplates("{{steps.a.output}} and {{steps.b.output}}", steps)).To(Equal("AAA and BBB"))
	})

	It("leaves input unchanged when no placeholders match", func() {
		steps := []agentorcav1alpha1.WorkflowStepStatus{{Name: "x", Output: "X"}}
		input := "no placeholders here"
		Expect(resolveTemplates(input, steps)).To(Equal(input))
	})

	It("replaces a placeholder with empty string when step output is empty", func() {
		steps := []agentorcav1alpha1.WorkflowStepStatus{{Name: "empty", Output: ""}}
		Expect(resolveTemplates("prefix {{steps.empty.output}} suffix", steps)).To(Equal("prefix  suffix"))
	})
})

var _ = Describe("evalCondition", func() {
	makeIndex := func(name, phase, output string) map[string]*agentorcav1alpha1.WorkflowStepStatus { //nolint:unparam

		return map[string]*agentorcav1alpha1.WorkflowStepStatus{
			name: {Name: name, Phase: agentorcav1alpha1.WorkflowStepPhase(phase), Output: output},
		}
	}

	It("returns true for a matching phase condition", func() {
		ok, err := evalCondition(`steps["gate"].phase == "Succeeded"`, makeIndex("gate", "Succeeded", ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
	})

	It("returns false for a non-matching phase condition", func() {
		ok, err := evalCondition(`steps["gate"].phase == "Failed"`, makeIndex("gate", "Succeeded", ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("returns true for a matching output condition", func() {
		ok, err := evalCondition(`steps["s"].output == "yes"`, makeIndex("s", "Succeeded", "yes"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
	})

	It("returns false for a non-matching output condition", func() {
		ok, err := evalCondition(`steps["s"].output == "yes"`, makeIndex("s", "Succeeded", "no"))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("returns false when the referenced step is not in the index", func() {
		ok, err := evalCondition(`steps["missing"].phase == "Succeeded"`, makeIndex("other", "Succeeded", ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("returns an error for an unsupported expression but defaults to true", func() {
		// Unsupported expressions pass through as true to avoid silently skipping steps.
		ok, err := evalCondition(`some_unknown_expr`, makeIndex("s", "Succeeded", ""))
		Expect(err).To(HaveOccurred())
		Expect(ok).To(BeTrue())
	})
})

var _ = Describe("parseFloatSafe", func() {
	It("parses a valid float string", func() {
		Expect(parseFloatSafe("1.23")).To(BeNumerically("~", 1.23, 0.001))
	})
	It("returns 0 for an empty string", func() {
		Expect(parseFloatSafe("")).To(BeZero())
	})
	It("returns 0 for an invalid string", func() {
		Expect(parseFloatSafe("not-a-float")).To(BeZero())
	})
})
