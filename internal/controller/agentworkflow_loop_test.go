package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func loopSteps() []agentorcav1alpha1.WorkflowStep {
	return []agentorcav1alpha1.WorkflowStep{
		{Name: "plan", AgentRef: "a", Input: "x"},
		{Name: "build", AgentRef: "a", Input: "feedback: {{steps.check.output}}", DependsOn: []string{"plan"}},
		{Name: "check", AgentRef: "a", Input: "x", DependsOn: []string{"build"},
			Loop: &agentorcav1alpha1.WorkflowLoop{From: "build", Until: `steps["check"].output.startsWith("PASS")`, MaxIterations: 3}},
		{Name: "ship", AgentRef: "a", Input: "x", DependsOn: []string{"check"}},
	}
}

func TestLoopBodyIsFromThroughGate(t *testing.T) {
	steps := loopSteps()
	body := loopBody(steps, &steps[2])
	if len(body) != 2 || !body["build"] || !body["check"] {
		t.Fatalf("body = %v, want build and check", body)
	}
	steps[2].Loop.From = "ship"
	if loopBody(steps, &steps[2]) != nil {
		t.Fatal("a from that is not an ancestor of the gate must give no body")
	}
}

func TestEvalLoopCondition(t *testing.T) {
	idx := map[string]*agentorcav1alpha1.WorkflowStepStatus{
		"review": {Name: "review", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded, Output: "VERDICT: APPROVE\nlooks fine"},
		"sec":    {Name: "sec", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded, Output: "VERDICT: FAIL x"},
	}
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`steps["review"].output.startsWith("VERDICT: APPROVE")`, true},
		{`steps["review"].output.contains("looks fine")`, true},
		{`steps["review"].output.contains("REQUEST_CHANGES")`, false},
		{`!steps["review"].output.contains("REQUEST_CHANGES")`, true},
		{`steps["review"].output.startsWith("VERDICT: APPROVE") && steps["sec"].output.startsWith("VERDICT: PASS")`, false},
		{`steps["review"].output.startsWith("VERDICT: APPROVE") && !steps["sec"].output.contains("PASS")`, true},
		{`steps["review"].phase == "Succeeded"`, true},
		{`steps["review"].phase != "Succeeded"`, false},
		{`steps["review"].output.contains("say \"hi\"")`, false},
	} {
		got, err := evalLoopCondition(tc.expr, idx)
		if err != nil || got != tc.want {
			t.Errorf("%s = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}
	for _, bad := range []string{`steps["x"].output`, `output.contains("a")`, `steps["x"].phase.contains("a")`, `a || b`} {
		if _, err := evalLoopCondition(bad, idx); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestValidateWorkflowLoops(t *testing.T) {
	if p := validateWorkflowLoops(loopSteps()); len(p) != 0 {
		t.Fatalf("valid loop rejected: %v", p)
	}
	for name, mutate := range map[string]func([]agentorcav1alpha1.WorkflowStep){
		"unknown step in until": func(s []agentorcav1alpha1.WorkflowStep) { s[2].Loop.Until = `steps["nope"].output.contains("a")` },
		"from not an ancestor":  func(s []agentorcav1alpha1.WorkflowStep) { s[2].Loop.From = "ship" },
		"approval in the body": func(s []agentorcav1alpha1.WorkflowStep) {
			s[1] = agentorcav1alpha1.WorkflowStep{Name: "build", Type: agentorcav1alpha1.WorkflowStepTypeApproval,
				Approval: &agentorcav1alpha1.ApprovalStep{Message: "m"}, DependsOn: []string{"plan"}}
		},
		"nested gate": func(s []agentorcav1alpha1.WorkflowStep) {
			s[1].Loop = &agentorcav1alpha1.WorkflowLoop{From: "build", Until: `steps["build"].output.contains("a")`}
		},
	} {
		steps := loopSteps()
		mutate(steps)
		if p := validateWorkflowLoops(steps); len(p) == 0 {
			t.Errorf("%s: expected a problem", name)
		}
	}
}

func TestIterStepName(t *testing.T) {
	if iterStepName("review", 0) != "review" || iterStepName("review", 2) != "review-i2" {
		t.Fatal("iteration names changed")
	}
}

func TestLoopRerunsBodyUntilGateSatisfied(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentorcav1alpha1.AddToScheme(scheme)
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{
			Steps:     loopSteps(),
			BudgetCap: &agentorcav1alpha1.WorkflowBudgetCap{Total: "5"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf).
		WithStatusSubresource(&agentorcav1alpha1.AgentWorkflow{}, &agentorcav1alpha1.AgentRun{}).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: scheme}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "wf", Namespace: "default"}}); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(run, out, spend string) {
		t.Helper()
		var ar agentorcav1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKey{Name: run, Namespace: "default"}, &ar); err != nil {
			t.Fatalf("run %s: %v", run, err)
		}
		ar.Status.Phase, ar.Status.Output, ar.Status.SpendUSD = agentorcav1alpha1.AgentRunPhaseSucceeded, out, spend
		if err := c.Status().Update(ctx, &ar); err != nil {
			t.Fatal(err)
		}
	}
	state := func(step string) agentorcav1alpha1.WorkflowStepStatus {
		t.Helper()
		var got agentorcav1alpha1.AgentWorkflow
		if err := c.Get(ctx, client.ObjectKey{Name: "wf", Namespace: "default"}, &got); err != nil {
			t.Fatal(err)
		}
		for _, s := range got.Status.Steps {
			if s.Name == step {
				return s
			}
		}
		t.Fatalf("no step %s", step)
		return agentorcav1alpha1.WorkflowStepStatus{}
	}

	reconcile() // starts plan
	finish("wf-plan", "plan", "0.1")
	reconcile() // plan done
	reconcile() // build starts
	finish("wf-build", "built v1", "0.2")
	reconcile()
	reconcile() // check starts
	finish("wf-check", "FAIL: tests broken", "0.3")
	reconcile() // gate unsatisfied: body reset

	if b, ch := state("build"), state("check"); b.Phase != agentorcav1alpha1.WorkflowStepPhasePending || b.Iteration != 1 || ch.Iteration != 1 {
		t.Fatalf("body not reset for pass 1: build=%+v check=%+v", b, ch)
	}
	if got := state("check").Output; got != "FAIL: tests broken" {
		t.Fatalf("feedback output lost on reset: %q", got)
	}
	if got := state("plan").Phase; got != agentorcav1alpha1.WorkflowStepPhaseSucceeded {
		t.Fatalf("a step outside the body must not rerun, plan is %s", got)
	}

	reconcile() // build pass 1 starts, under a new run name
	var run agentorcav1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKey{Name: "wf-build-i1", Namespace: "default"}, &run); err != nil {
		t.Fatalf("pass 1 must use its own AgentRun: %v", err)
	}
	if !strings.Contains(run.Spec.Input, "FAIL: tests broken") {
		t.Fatalf("pass 1 input lacks the previous feedback: %q", run.Spec.Input)
	}
	finish("wf-build-i1", "built v2", "0.2")
	reconcile()
	reconcile()
	finish("wf-check-i1", "PASS: all green", "0.3")
	reconcile() // gate satisfied
	reconcile() // ship starts
	finish("wf-ship", "shipped", "0.1")
	reconcile()

	var done agentorcav1alpha1.AgentWorkflow
	if err := c.Get(ctx, client.ObjectKey{Name: "wf", Namespace: "default"}, &done); err != nil {
		t.Fatal(err)
	}
	if done.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseSucceeded {
		t.Fatalf("phase = %s, conditions %v", done.Status.Phase, done.Status.Conditions)
	}
	if done.Status.TotalSpendUSD != "1.200000" {
		t.Fatalf("total spend = %q, want every pass counted (1.2)", done.Status.TotalSpendUSD)
	}
}

func TestLoopBreakStopsBeforePassesRunOut(t *testing.T) {
	steps := loopSteps()
	steps[2].Loop.Break = []string{`steps["check"].output.startsWith("BLOCKED")`}
	if p := validateWorkflowLoops(steps); len(p) != 0 {
		t.Fatalf("valid break rejected: %v", p)
	}
	steps[2].Loop.Break = append(steps[2].Loop.Break, `steps["nope"].output.contains("a")`)
	if p := validateWorkflowLoops(steps); len(p) == 0 {
		t.Fatal("a break naming an unknown step must be rejected")
	}
	steps[2].Loop.Break = steps[2].Loop.Break[:1]

	wf := &agentorcav1alpha1.AgentWorkflow{Spec: agentorcav1alpha1.AgentWorkflowSpec{Steps: steps}}
	idx := map[string]*agentorcav1alpha1.WorkflowStepStatus{}
	for _, s := range steps {
		idx[s.Name] = &agentorcav1alpha1.WorkflowStepStatus{Name: s.Name, Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded}
	}
	idx["check"].Output = "BLOCKED: the repository refuses the push"
	if advanceLoop(wf, &steps[2], idx) {
		t.Fatal("a blocker must not send the body back for another pass")
	}
	if idx["check"].Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.HasPrefix(idx["check"].FailureReason, loopFailurePrefix) {
		t.Fatalf("gate = %s %q", idx["check"].Phase, idx["check"].FailureReason)
	}
	if idx["build"].Iteration != 0 {
		t.Fatalf("body was reset to pass %d", idx["build"].Iteration)
	}
}

func TestApplyStepRetries(t *testing.T) {
	steps := loopSteps()
	failed := func(name string, iteration int32, reason string) agentorcav1alpha1.WorkflowStepStatus {
		return agentorcav1alpha1.WorkflowStepStatus{Name: name, Phase: agentorcav1alpha1.WorkflowStepPhaseFailed, Iteration: iteration, FailureReason: reason, SpendUSD: "0.5"}
	}
	newWF := func(retry string) *agentorcav1alpha1.AgentWorkflow {
		return &agentorcav1alpha1.AgentWorkflow{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{agentorcav1alpha1.RetryStepAnnotation: retry}},
			Spec:       agentorcav1alpha1.AgentWorkflowSpec{Steps: steps},
			Status: agentorcav1alpha1.AgentWorkflowStatus{
				Phase: agentorcav1alpha1.AgentWorkflowPhaseFailed,
				Steps: []agentorcav1alpha1.WorkflowStepStatus{
					{Name: "plan", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded},
					{Name: "build", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded, Iteration: 2, Generation: 2},
					failed("check", 2, "loop did not finish in 3 passes: x"),
					{Name: "ship", Phase: agentorcav1alpha1.WorkflowStepPhasePending},
				},
			},
		}
	}
	r := &AgentWorkflowReconciler{}
	byName := func(wf *agentorcav1alpha1.AgentWorkflow, name string) *agentorcav1alpha1.WorkflowStepStatus {
		for i := range wf.Status.Steps {
			if wf.Status.Steps[i].Name == name {
				return &wf.Status.Steps[i]
			}
		}
		t.Fatalf("no step %s", name)
		return nil
	}

	// A gate the loop failed restarts the whole body with a full set of passes.
	wf := newWF("check")
	if !r.applyStepRetries(context.Background(), wf) {
		t.Fatal("retry of a failed gate changed nothing")
	}
	if wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseRunning || wf.Status.CompletionTime != nil {
		t.Fatalf("workflow not revived: %s", wf.Status.Phase)
	}
	for _, name := range []string{"build", "check"} {
		s := byName(wf, name)
		if s.Phase != agentorcav1alpha1.WorkflowStepPhasePending || s.Iteration != 0 || s.NameGeneration() <= 2 {
			t.Errorf("%s = %+v, want Pending, pass 0, a generation past 2", name, *s)
		}
	}
	if byName(wf, "plan").Phase != agentorcav1alpha1.WorkflowStepPhaseSucceeded {
		t.Error("a step outside the loop must not rerun")
	}
	if got := byName(wf, "check").PriorSpendUSD; got != "0.500000" {
		t.Errorf("spend of the failed attempt = %q, want it carried", got)
	}

	// Any other failed step reruns alone, in the pass it failed in, under fresh names.
	wf = newWF("check")
	wf.Status.Steps[2].FailureReason = "the agent exited with code 1"
	if !r.applyStepRetries(context.Background(), wf) {
		t.Fatal("retry of a failed step changed nothing")
	}
	if s := byName(wf, "check"); s.Iteration != 2 || s.NameGeneration() != 3 || s.Phase != agentorcav1alpha1.WorkflowStepPhasePending {
		t.Errorf("check = %+v, want pass 2 under generation 3", *s)
	}
	if byName(wf, "build").Phase != agentorcav1alpha1.WorkflowStepPhaseSucceeded {
		t.Error("only the failed step should rerun")
	}
	if runName := workflowAgentRunName(wf, &steps[2], byName(wf, "check").NameGeneration()); runName != "-check-i3" {
		t.Errorf("run name = %q", runName)
	}

	// A step that did not fail, or does not exist, is left alone.
	wf = newWF("ship,nope")
	if r.applyStepRetries(context.Background(), wf) || wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseFailed {
		t.Error("retry of a step that did not fail must change nothing")
	}
}

func TestLoopFailsTheGateWhenPassesRunOut(t *testing.T) {
	steps := loopSteps()
	steps[2].Loop.MaxIterations = 2
	wf := &agentorcav1alpha1.AgentWorkflow{Spec: agentorcav1alpha1.AgentWorkflowSpec{Steps: steps}}
	idx := map[string]*agentorcav1alpha1.WorkflowStepStatus{}
	for _, s := range steps {
		idx[s.Name] = &agentorcav1alpha1.WorkflowStepStatus{Name: s.Name, Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded}
	}
	idx["check"].Output = "FAIL"
	if !advanceLoop(wf, &steps[2], idx) {
		t.Fatal("pass 1 should reset the body")
	}
	idx["check"].Phase = agentorcav1alpha1.WorkflowStepPhaseSucceeded
	if advanceLoop(wf, &steps[2], idx) {
		t.Fatal("pass limit reached: no further reset")
	}
	if idx["check"].Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(idx["check"].FailureReason, "did not finish in 2 passes") {
		t.Fatalf("gate = %s %q", idx["check"].Phase, idx["check"].FailureReason)
	}
}

func TestRetrySandboxProvisionsAFailedSharedSandboxAgain(t *testing.T) {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{agentorcav1alpha1.RetryStepAnnotation: RetrySandbox}},
		Spec:       agentorcav1alpha1.AgentWorkflowSpec{Sandbox: &agentorcav1alpha1.WorkflowSandboxSpec{Repo: "o/r"}, Steps: loopSteps()},
		Status: agentorcav1alpha1.AgentWorkflowStatus{
			Phase:   agentorcav1alpha1.AgentWorkflowPhaseFailed,
			Sandbox: &agentorcav1alpha1.WorkflowSandboxStatus{Name: "ao-x", Phase: agentorcav1alpha1.WorkflowSandboxPhaseFailed},
		},
	}
	r := &AgentWorkflowReconciler{}
	if !r.applyStepRetries(context.Background(), wf) {
		t.Fatal("a failed sandbox was not retried")
	}
	if wf.Status.Sandbox != nil || wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseRunning {
		t.Fatalf("sandbox = %v, phase = %s; want a sandbox to provision and a running workflow", wf.Status.Sandbox, wf.Status.Phase)
	}
}

func TestRefreshWorkflowSpendReadsRunsAgainAndCountsEarlierPasses(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentorcav1alpha1.AddToScheme(scheme)
	run := func(name, step, spend string) *agentorcav1alpha1.AgentRun {
		r := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			Labels: map[string]string{labelWorkflow: "wf", labelWorkflowStep: step}}}
		r.Status.SpendUSD = spend
		return r
	}
	// The step copied an empty spend when its run finished; the run settled later.
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Status: agentorcav1alpha1.AgentWorkflowStatus{Steps: []agentorcav1alpha1.WorkflowStepStatus{
			{Name: "plan", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded, AgentRunRef: "wf-plan"},
			{Name: "check", Phase: agentorcav1alpha1.WorkflowStepPhaseSucceeded, AgentRunRef: "wf-check-i1", Iteration: 1},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf,
		run("wf-plan", "plan", "0.25"), run("wf-check", "check", "0.5"), run("wf-check-i1", "check", "0.125")).Build()
	r := &AgentWorkflowReconciler{Client: c, Scheme: scheme}

	if !r.refreshWorkflowSpend(ctx, wf) {
		t.Fatal("spend that a run now holds was not picked up")
	}
	if wf.Status.Steps[0].SpendUSD != "0.25" || wf.Status.Steps[1].SpendUSD != "0.125" || wf.Status.Steps[1].PriorSpendUSD != "0.500000" {
		t.Fatalf("steps = %+v", wf.Status.Steps)
	}
	if wf.Status.TotalSpendUSD != "0.875000" {
		t.Fatalf("total = %q, want 0.875 across every run", wf.Status.TotalSpendUSD)
	}
	if r.refreshWorkflowSpend(ctx, wf) {
		t.Fatal("a second refresh with nothing new must report no change")
	}
}
