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
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

func containerStep(name, image string, deps ...string) agentorcav1alpha1.WorkflowStep {
	return agentorcav1alpha1.WorkflowStep{
		Name: name, Type: agentorcav1alpha1.WorkflowStepTypeContainer, DependsOn: deps,
		Container: &agentorcav1alpha1.ContainerStep{Image: image, Command: []string{"/bin/run"}},
	}
}

func approvalStepDef(message string, deps ...string) agentorcav1alpha1.WorkflowStep {
	return agentorcav1alpha1.WorkflowStep{
		Name: "gate", Type: agentorcav1alpha1.WorkflowStepTypeApproval, DependsOn: deps,
		Approval: &agentorcav1alpha1.ApprovalStep{Message: message, ApproverGroups: []string{"release-managers"}},
	}
}

// stepsReconciler returns a reconciler with the pieces step types need: pods and
// network policies in the scheme, a fake clientset for logs, an approval store.
func stepsReconciler(t *testing.T, objs ...client.Object) (*AgentWorkflowReconciler, client.Client, *memApprovals) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agentorcav1alpha1.AgentWorkflow{}, &corev1.Pod{}).WithObjects(objs...).Build()
	k8s := newFakeClientsetWithLogs()
	store := &memApprovals{}
	return &AgentWorkflowReconciler{Client: cl, Scheme: scheme, K8s: k8s, Approvals: store}, cl, store
}

func stepWorkflow(steps ...agentorcav1alpha1.WorkflowStep) *agentorcav1alpha1.AgentWorkflow {
	return &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default", UID: "wf-uid-1"},
		Spec:       agentorcav1alpha1.AgentWorkflowSpec{Steps: steps},
	}
}

func stepOf(t *testing.T, wf *agentorcav1alpha1.AgentWorkflow, name string) *agentorcav1alpha1.WorkflowStepStatus {
	t.Helper()
	for i := range wf.Status.Steps {
		if wf.Status.Steps[i].Name == name {
			return &wf.Status.Steps[i]
		}
	}
	t.Fatalf("no status for step %q", name)
	return nil
}

func setPod(t *testing.T, cl client.Client, name string, mutate func(p *corev1.Pod)) {
	t.Helper()
	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	mutate(&pod)
	// Metadata and status are separate writes: persist both.
	if err := cl.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	mutate(&pod)
	if err := cl.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
}

func podExists(cl client.Client, name string) bool {
	var pod corev1.Pod
	return cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pod) == nil
}

// lockedDownPod builds the pod of a container step with an env var and a 90s limit.
func lockedDownPod() *corev1.Pod {
	wf := stepWorkflow(containerStep("build", "registry.example.com/tools/builder:1"))
	wf.Spec.Steps[0].Container.Env = []agentorcav1alpha1.StepEnvVar{{Name: "TARGET", Value: "linux"}}
	return buildStepPod(wf, &wf.Spec.Steps[0], 0, wf.Spec.Steps[0].Container.Env, 90*time.Second)
}

func TestContainerStepPodHasNoAmbientAuthority(t *testing.T) {
	pod := lockedDownPod()
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
		t.Errorf("pod security context = %+v, want non-root user 65532", sc)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp = %+v", sc.SeccompProfile)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the pod mounts a service account token: a job could call the Kubernetes API")
	}
	if pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks {
		t.Error("service links enabled: they leak the cluster's service names into the job")
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Error("the pod uses host namespaces")
	}
}

func TestContainerStepContainerIsLockedDown(t *testing.T) {
	csc := lockedDownPod().Spec.Containers[0].SecurityContext
	if csc == nil || csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation ||
		csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem ||
		csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security context = %+v, want no escalation, read-only root, all capabilities dropped", csc)
	}
	if csc.Privileged != nil && *csc.Privileged {
		t.Error("privileged container")
	}
}

func TestContainerStepPodContents(t *testing.T) {
	pod := lockedDownPod()
	c := pod.Spec.Containers[0]
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 90 {
		t.Errorf("activeDeadlineSeconds = %v, want 90", pod.Spec.ActiveDeadlineSeconds)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.Spec.RestartPolicy)
	}
	if c.Image != "registry.example.com/tools/builder:1" || len(c.Command) != 1 || c.WorkingDir != "/work" {
		t.Errorf("container = %+v", c)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["TARGET"] != "linux" || env["AGENTORC_WORKFLOW"] != "wf" || env["AGENTORC_STEP"] != "build" {
		t.Errorf("env = %v", env)
	}
	if lim := c.Resources.Limits; lim.Cpu().String() != "1" || lim.Memory().Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("default limits = %v, want 1 CPU / 512Mi", lim)
	}
	if pod.Labels[labelWorkflow] != "wf" || pod.Labels[labelStepPod] != "true" {
		t.Errorf("labels = %v: the deny-all policy selects on these", pod.Labels)
	}
}

func TestContainerStepOverrides(t *testing.T) {
	wf := stepWorkflow(containerStep("build", "img"))
	user := int64(1500)
	wf.Spec.Steps[0].Container.RunAsUser = &user
	wf.Spec.Steps[0].Container.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}
	pod := buildStepPod(wf, &wf.Spec.Steps[0], 0, nil, 0)
	if *pod.Spec.SecurityContext.RunAsUser != 1500 || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Errorf("security context = %+v", pod.Spec.SecurityContext)
	}
	if pod.Spec.Containers[0].Resources.Limits.Memory().Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("resources = %v", pod.Spec.Containers[0].Resources)
	}
	if *pod.Spec.ActiveDeadlineSeconds < 1 {
		t.Error("a zero timeout produced no deadline")
	}
}

func TestContainerStepRunsAndPassesItsOutputOn(t *testing.T) {
	plan := stepWorkflow(
		containerStep("gen", "img"),
		containerStep("use", "img", "gen"),
	)
	plan.Spec.Steps[1].Container.Env = []agentorcav1alpha1.StepEnvVar{{Name: "FROM_GEN", Value: "got: {{steps.gen.output}}"}}
	r, cl, _ := stepsReconciler(t, plan)

	reconcileWorkflow(t, r, "wf")
	wf := getWorkflow(t, cl, "wf")
	if wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseRunning || stepOf(t, wf, "gen").PodRef != "wf-gen" || stepOf(t, wf, "gen").Phase != agentorcav1alpha1.WorkflowStepPhaseRunning {
		t.Fatalf("workflow %s, gen = %+v", wf.Status.Phase, stepOf(t, wf, "gen"))
	}
	if !podExists(cl, "wf-gen") || podExists(cl, "wf-use") {
		t.Fatal("only the first step's pod should exist")
	}

	// The network policy denies everything for the workflow's step pods.
	var np networkingv1.NetworkPolicy
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "wf-steps-deny-all"}, &np); err != nil {
		t.Fatalf("no deny-all network policy: %v", err)
	}
	if len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 || len(np.Spec.PolicyTypes) != 2 ||
		np.Spec.PodSelector.MatchLabels[labelWorkflow] != "wf" || np.Spec.PodSelector.MatchLabels[labelStepPod] != "true" {
		t.Errorf("network policy = %+v: want ingress and egress both denied for the workflow's step pods", np.Spec)
	}

	// While the pod runs, nothing changes.
	reconcileWorkflow(t, r, "wf")
	if podExists(cl, "wf-use") {
		t.Fatal("the dependent step started before its dependency finished")
	}

	// The pod succeeds: output is captured, the pod removed, the next step starts.
	setPod(t, cl, "wf-gen", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	reconcileWorkflow(t, r, "wf")
	wf = getWorkflow(t, cl, "wf")
	gen := stepOf(t, wf, "gen")
	if gen.Phase != agentorcav1alpha1.WorkflowStepPhaseSucceeded || gen.Output != "fake logs" || gen.CompletionTime == nil {
		t.Errorf("gen = %+v", gen)
	}
	if podExists(cl, "wf-gen") {
		t.Error("the finished pod was left behind")
	}
	reconcileWorkflow(t, r, "wf")
	var use corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "wf-use"}, &use); err != nil {
		t.Fatalf("the next step did not start: %v", err)
	}
	var got string
	for _, e := range use.Spec.Containers[0].Env {
		if e.Name == "FROM_GEN" {
			got = e.Value
		}
	}
	if got != "got: fake logs" {
		t.Errorf("FROM_GEN = %q, want the previous step's output substituted", got)
	}

	// Finishing the last step finishes the workflow.
	setPod(t, cl, "wf-use", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	reconcileWorkflow(t, r, "wf")
	if got := getWorkflow(t, cl, "wf"); got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseSucceeded {
		t.Errorf("workflow = %s, want Succeeded", got.Status.Phase)
	}
}

func failedPod(code int32, reason string) func(p *corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "step", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason},
		}}}
	}
}

func TestFailedContainerStepFailsTheWorkflowWithTheExitCode(t *testing.T) {
	r, cl, _ := stepsReconciler(t, stepWorkflow(containerStep("a", "img")))
	reconcileWorkflow(t, r, "wf")
	setPod(t, cl, "wf-a", failedPod(2, "Error"))
	reconcileWorkflow(t, r, "wf")

	wf := getWorkflow(t, cl, "wf")
	a := stepOf(t, wf, "a")
	if a.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(a.FailureReason, "exited with code 2") || !strings.Contains(a.FailureReason, "fake logs") {
		t.Errorf("step = %+v", a)
	}
	if wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseFailed {
		t.Errorf("workflow = %s, want Failed (onStepFailure defaults to stop)", wf.Status.Phase)
	}
	if podExists(cl, "wf-a") {
		t.Error("the failed pod was left behind")
	}
}

func TestFailedContainerStepIsSkippedUnderContinue(t *testing.T) {
	wf := stepWorkflow(containerStep("a", "img"), containerStep("b", "img", "a"))
	wf.Spec.OnStepFailure = "continue"
	r, cl, _ := stepsReconciler(t, wf)
	reconcileWorkflow(t, r, "wf")
	setPod(t, cl, "wf-a", failedPod(1, "Error"))
	reconcileWorkflow(t, r, "wf")
	got := getWorkflow(t, cl, "wf")
	if a := stepOf(t, got, "a"); a.Phase != agentorcav1alpha1.WorkflowStepPhaseSkipped || a.FailureReason == "" {
		t.Errorf("a = %+v, want Skipped with the reason", a)
	}
	reconcileWorkflow(t, r, "wf")
	if !podExists(cl, "wf-b") {
		t.Error("the dependent step did not start under onStepFailure: continue")
	}
}

func TestContainerStepTimeoutAndOOMReasons(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(p *corev1.Pod)
		want   string
	}{
		"deadline exceeded": {func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed; p.Status.Reason = "DeadlineExceeded" }, "the step timed out"},
		"out of memory":     {failedPod(137, "OOMKilled"), "exited with code 137 (OOMKilled)"},
	} {
		r, cl, _ := stepsReconciler(t, stepWorkflow(containerStep("a", "img")))
		reconcileWorkflow(t, r, "wf")
		setPod(t, cl, "wf-a", tc.mutate)
		reconcileWorkflow(t, r, "wf")
		if a := stepOf(t, getWorkflow(t, cl, "wf"), "a"); !strings.Contains(a.FailureReason, tc.want) {
			t.Errorf("%s: reason %q, want it to contain %q", name, a.FailureReason, tc.want)
		}
	}
}

func TestContainerStepImageNotAllowedNeverStarts(t *testing.T) {
	r, cl, _ := stepsReconciler(t, stepWorkflow(containerStep("a", "evil.example.com/miner:latest")))
	r.AllowedRegistries = []string{"ghcr.io/heddles"}
	reconcileWorkflow(t, r, "wf")
	wf := getWorkflow(t, cl, "wf")
	if a := stepOf(t, wf, "a"); a.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(a.FailureReason, "image rejected") {
		t.Errorf("a = %+v", a)
	}
	if podExists(cl, "wf-a") {
		t.Error("a pod was created for a disallowed image")
	}

	// An allowed image runs.
	r2, cl2, _ := stepsReconciler(t, stepWorkflow(containerStep("a", "ghcr.io/heddles/tool:1")))
	r2.AllowedRegistries = []string{"ghcr.io/heddles"}
	reconcileWorkflow(t, r2, "wf")
	if !podExists(cl2, "wf-a") {
		t.Error("an allowed image did not start")
	}
}

func TestContainerStepThatCannotPullItsImage(t *testing.T) {
	r, cl, _ := stepsReconciler(t, stepWorkflow(containerStep("a", "ghcr.io/heddles/nope:1")))
	reconcileWorkflow(t, r, "wf")
	waiting := func(reason string, age time.Duration) func(p *corev1.Pod) {
		return func(p *corev1.Pod) {
			p.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "step", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "pull access denied"}}}}
		}
	}
	// Within the grace period a slow pull is not a failure.
	setPod(t, cl, "wf-a", waiting("ImagePullBackOff", 10*time.Second))
	reconcileWorkflow(t, r, "wf")
	if a := stepOf(t, getWorkflow(t, cl, "wf"), "a"); a.Phase != agentorcav1alpha1.WorkflowStepPhaseRunning {
		t.Fatalf("a = %+v, want still Running during the grace period", a)
	}
	// Past it, the step fails with the reason.
	setPod(t, cl, "wf-a", waiting("ImagePullBackOff", 5*time.Minute))
	reconcileWorkflow(t, r, "wf")
	if a := stepOf(t, getWorkflow(t, cl, "wf"), "a"); a.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(a.FailureReason, "ImagePullBackOff") {
		t.Errorf("a = %+v", a)
	}
}

func TestContainerStepWhosePodIsGone(t *testing.T) {
	r, cl, _ := stepsReconciler(t, stepWorkflow(containerStep("a", "img")))
	reconcileWorkflow(t, r, "wf")
	if err := cl.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "wf-a", Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	reconcileWorkflow(t, r, "wf")
	if a := stepOf(t, getWorkflow(t, cl, "wf"), "a"); a.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(a.FailureReason, "pod was deleted") {
		t.Errorf("a = %+v", a)
	}
}

func TestTruncateTailKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("a", 20000) + "THE END"
	got := truncateTail(long)
	if !strings.HasSuffix(got, "THE END") || !strings.HasPrefix(got, "[output truncated]") || len(got) > maxStepOutputBytes+40 {
		t.Errorf("truncated to %d bytes, prefix %q, suffix %q", len(got), got[:20], got[len(got)-7:])
	}
	if truncateTail("short") != "short" {
		t.Error("a short output was changed")
	}
}

type notified struct{ got []postgresql.Approval }

func (n *notified) NotifyApproval(_ context.Context, a postgresql.Approval) error {
	n.got = append(n.got, a)
	return nil
}

func TestApprovalStepWaitsThenContinuesOnApproval(t *testing.T) {
	wf := stepWorkflow(
		containerStep("test", "img"),
		approvalStepDef("Test report:\n{{steps.test.output}}\nShip it?", "test"),
		containerStep("ship", "img", "gate"),
	)
	wf.Spec.Steps[1].Approval.Timeout = &metav1.Duration{Duration: 2 * time.Hour}
	r, cl, store := stepsReconciler(t, wf)
	n := &notified{}
	r.ApprovalNotifier = n

	reconcileWorkflow(t, r, "wf")
	setPod(t, cl, "wf-test", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	reconcileWorkflow(t, r, "wf") // test finishes
	reconcileWorkflow(t, r, "wf") // gate opens

	got := getWorkflow(t, cl, "wf")
	gate := stepOf(t, got, "gate")
	if gate.Phase != agentorcav1alpha1.WorkflowStepPhaseWaitingForApproval || gate.ApprovalID == "" {
		t.Fatalf("gate = %+v", gate)
	}
	if len(store.rows) != 1 {
		t.Fatalf("%d approvals, want 1", len(store.rows))
	}
	a := store.rows[0]
	if a.Kind != postgresql.ApprovalKindWorkflow || a.SourceKey != "wf-uid-1/gate" || a.Namespace != "default" ||
		len(a.ApproverGroups) != 1 || a.ApproverGroups[0] != "release-managers" || a.Status != postgresql.ApprovalPending {
		t.Errorf("approval = %+v", a)
	}
	if time.Until(a.ExpiresAt) < time.Hour+50*time.Minute || time.Until(a.ExpiresAt) > 2*time.Hour {
		t.Errorf("expires in %v, want about the step's 2h", time.Until(a.ExpiresAt))
	}
	if !strings.Contains(string(a.Details), "Test report:\\nfake logs\\nShip it?") {
		t.Errorf("details = %s: the message should show the earlier step's output", a.Details)
	}
	if !strings.HasPrefix(a.Summary, "wf / gate: Test report:") || strings.Contains(a.Summary, "\n") {
		t.Errorf("summary = %q", a.Summary)
	}
	if len(n.got) != 1 {
		t.Errorf("%d notifications, want 1", len(n.got))
	}
	if podExists(cl, "wf-ship") {
		t.Fatal("the step after the gate started before approval")
	}

	// Still pending: the same single approval, no second notification, nothing starts.
	reconcileWorkflow(t, r, "wf")
	if len(store.rows) != 1 || len(n.got) != 1 || podExists(cl, "wf-ship") {
		t.Fatalf("%d approvals, %d notifications, ship started=%v", len(store.rows), len(n.got), podExists(cl, "wf-ship"))
	}

	// An approver approves: the gate passes and the next step starts.
	if _, err := store.DecideApproval(context.Background(), a.ID, postgresql.ApprovalApproved, "alice", "looks good"); err != nil {
		t.Fatal(err)
	}
	reconcileWorkflow(t, r, "wf")
	gate = stepOf(t, getWorkflow(t, cl, "wf"), "gate")
	if gate.Phase != agentorcav1alpha1.WorkflowStepPhaseSucceeded || gate.Output != "approved by alice: looks good" {
		t.Errorf("gate = %+v", gate)
	}
	reconcileWorkflow(t, r, "wf")
	if !podExists(cl, "wf-ship") {
		t.Error("the step after the gate did not start once approved")
	}
}

func TestApprovalStepRejectionAndTimeoutFailTheWorkflow(t *testing.T) {
	for name, tc := range map[string]struct {
		act  func(t *testing.T, s *memApprovals)
		want string
	}{
		"rejected": {func(t *testing.T, s *memApprovals) {
			if _, err := s.DecideApproval(context.Background(), s.rows[0].ID, postgresql.ApprovalRejected, "bob", "not on a Friday"); err != nil {
				t.Fatal(err)
			}
		}, "the approval was rejected by bob: not on a Friday"},
		"expired": {func(_ *testing.T, s *memApprovals) { s.rows[0].ExpiresAt = time.Now().Add(-time.Second) }, "the approval was not decided in time"},
	} {
		r, cl, store := stepsReconciler(t, stepWorkflow(approvalStepDef("ok?"), containerStep("after", "img", "gate")))
		reconcileWorkflow(t, r, "wf")
		tc.act(t, store)
		reconcileWorkflow(t, r, "wf")
		wf := getWorkflow(t, cl, "wf")
		if g := stepOf(t, wf, "gate"); g.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(g.FailureReason, tc.want) {
			t.Errorf("%s: gate = %+v, want reason containing %q", name, g, tc.want)
		}
		if wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseFailed || podExists(cl, "wf-after") {
			t.Errorf("%s: workflow %s, dependent started=%v", name, wf.Status.Phase, podExists(cl, "wf-after"))
		}
	}
}

func TestApprovalStepNeedsTheStore(t *testing.T) {
	r, cl, _ := stepsReconciler(t, stepWorkflow(approvalStepDef("ok?")))
	r.Approvals = nil
	reconcileWorkflow(t, r, "wf")
	wf := getWorkflow(t, cl, "wf")
	if g := stepOf(t, wf, "gate"); g.Phase != agentorcav1alpha1.WorkflowStepPhaseFailed || !strings.Contains(g.FailureReason, "PostgreSQL") {
		t.Errorf("gate = %+v, want a clear failure rather than waiting forever", g)
	}
}

func TestEndedWorkflowWithdrawsItsPendingApprovalsOnly(t *testing.T) {
	// One workflow waits at a gate while a parallel container step fails.
	wf := stepWorkflow(approvalStepDef("ok?"), containerStep("work", "img"))
	r, cl, store := stepsReconciler(t, wf)
	reconcileWorkflow(t, r, "wf")
	// Another workflow's approval, and a non-workflow one, must be left alone.
	if _, err := store.RequestApproval(context.Background(), postgresql.Approval{
		Kind: postgresql.ApprovalKindWorkflow, SourceKey: "other-uid/gate", Namespace: "default", Summary: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestApproval(context.Background(), postgresql.Approval{
		Kind: postgresql.ApprovalKindTool, SourceKey: "wf-uid-1/call", Namespace: "default", Summary: "tool"}); err != nil {
		t.Fatal(err)
	}

	setPod(t, cl, "wf-work", failedPod(1, "Error"))
	reconcileWorkflow(t, r, "wf") // the workflow fails
	if got := getWorkflow(t, cl, "wf"); got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseFailed {
		t.Fatalf("workflow = %s", got.Status.Phase)
	}
	reconcileWorkflow(t, r, "wf") // the next pass sees it ended

	status := map[string]string{}
	for _, a := range store.rows {
		status[a.SourceKey] = a.Status
	}
	if status["wf-uid-1/gate"] != postgresql.ApprovalWithdrawn {
		t.Errorf("the ended workflow's gate = %q, want withdrawn so no one is asked", status["wf-uid-1/gate"])
	}
	if status["other-uid/gate"] != postgresql.ApprovalPending || status["wf-uid-1/call"] != postgresql.ApprovalPending {
		t.Errorf("unrelated approvals were touched: %v", status)
	}
}

func TestStepShapeIsChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		step agentorcav1alpha1.WorkflowStep
		want string
	}{
		"agent without agentRef":      {agentorcav1alpha1.WorkflowStep{Name: "a"}, "needs agentRef"},
		"container without a block":   {agentorcav1alpha1.WorkflowStep{Name: "a", Type: agentorcav1alpha1.WorkflowStepTypeContainer}, "needs container.image"},
		"container with an agentRef":  {agentorcav1alpha1.WorkflowStep{Name: "a", Type: agentorcav1alpha1.WorkflowStepTypeContainer, AgentRef: "x", Container: &agentorcav1alpha1.ContainerStep{Image: "i"}}, "has agentRef, input"},
		"approval without a message":  {agentorcav1alpha1.WorkflowStep{Name: "a", Type: agentorcav1alpha1.WorkflowStepTypeApproval, Approval: &agentorcav1alpha1.ApprovalStep{}}, "needs approval.message"},
		"agent step with a container": {agentorcav1alpha1.WorkflowStep{Name: "a", AgentRef: "x", Container: &agentorcav1alpha1.ContainerStep{Image: "i"}}, "container or approval block"},
		"unknown type":                {agentorcav1alpha1.WorkflowStep{Name: "a", Type: "shell"}, "unknown type"},
	} {
		if p := validateStepShape(&tc.step); !hasProblem(p, tc.want) {
			t.Errorf("%s: problems %v, want one containing %q", name, p, tc.want)
		}
	}
	if p := validateStepShape(&agentorcav1alpha1.WorkflowStep{Name: "a", AgentRef: "x", Input: "do it"}); len(p) != 0 {
		t.Errorf("a valid agent step got problems: %v", p)
	}
}

// Parameters may fill a container's environment or an approval message, but never
// decide what a job runs or who approves.
func TestTemplateParametersInNewStepTypes(t *testing.T) {
	tmpl := func(mutate func(s *agentorcav1alpha1.AgentWorkflowSpec)) []string {
		spec := agentorcav1alpha1.AgentWorkflowSpec{
			Mode:       agentorcav1alpha1.WorkflowModeTemplate,
			Parameters: []agentorcav1alpha1.WorkflowParameter{{Name: "target", Required: true}},
			Steps: []agentorcav1alpha1.WorkflowStep{
				containerStep("build", "img"),
				approvalStepDef("Ship {{params.target}}? Built: {{steps.build.output}}", "build"),
			},
		}
		spec.Steps[0].Container.Env = []agentorcav1alpha1.StepEnvVar{{Name: "TARGET", Value: "{{params.target}}"}}
		mutate(&spec)
		return validateTemplate(&spec)
	}
	if p := tmpl(func(*agentorcav1alpha1.AgentWorkflowSpec) {}); len(p) != 0 {
		t.Fatalf("a valid template got problems: %v", p)
	}
	for name, tc := range map[string]struct {
		f    func(s *agentorcav1alpha1.AgentWorkflowSpec)
		want string
	}{
		"parameter in the image": {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[0].Container.Image = "{{params.target}}" }, "container.image may not use {{params"},
		"parameter in the command": {func(s *agentorcav1alpha1.AgentWorkflowSpec) {
			s.Steps[0].Container.Command = []string{"sh", "-c", "{{params.target}}"}
		}, "command or args may not use {{params"},
		"parameter in the args": {func(s *agentorcav1alpha1.AgentWorkflowSpec) {
			s.Steps[0].Container.Args = []string{"{{params.target}}"}
		}, "command or args may not use {{params"},
		"parameter as an env name": {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[0].Container.Env[0].Name = "{{params.target}}" }, "env name"},
		"parameter in approverGroups": {func(s *agentorcav1alpha1.AgentWorkflowSpec) {
			s.Steps[1].Approval.ApproverGroups = []string{"{{params.target}}"}
		}, "approverGroups may not use {{params"},
		"undeclared in an env value":   {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[0].Container.Env[0].Value = "{{params.nope}}" }, "{{params.nope}}, which is not declared"},
		"undeclared in the message":    {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[1].Approval.Message = "{{params.nope}}" }, "approval message uses {{params.nope}}"},
		"message reads a non-ancestor": {func(s *agentorcav1alpha1.AgentWorkflowSpec) { s.Steps[1].DependsOn = nil }, "not one of the step's dependencies"},
	} {
		if p := tmpl(tc.f); !hasProblem(p, tc.want) {
			t.Errorf("%s: problems %v, want one containing %q", name, p, tc.want)
		}
	}

	// Instantiating substitutes into the env value and the message.
	tm := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{
			Mode:       agentorcav1alpha1.WorkflowModeTemplate,
			Parameters: []agentorcav1alpha1.WorkflowParameter{{Name: "target", Required: true}},
			Steps:      []agentorcav1alpha1.WorkflowStep{containerStep("build", "img"), approvalStepDef("Ship {{params.target}}?", "build")},
		},
	}
	tm.Spec.Steps[0].Container.Env = []agentorcav1alpha1.StepEnvVar{{Name: "TARGET", Value: "{{params.target}}"}}
	spec, err := instantiateTemplate(tm, instance(map[string]string{"target": "linux"}))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Steps[0].Container.Env[0].Value != "linux" || spec.Steps[1].Approval.Message != "Ship linux?" {
		t.Errorf("not substituted: env %q, message %q", spec.Steps[0].Container.Env[0].Value, spec.Steps[1].Approval.Message)
	}
	if !strings.Contains(tm.Spec.Steps[0].Container.Env[0].Value, "{{params.target}}") {
		t.Error("instantiating rewrote the template's own container env")
	}
	// A hostile value in an env var or message cannot pull in another step's output.
	spec, err = instantiateTemplate(tm, instance(map[string]string{"target": "{{steps.build.output}}"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveTemplates(spec.Steps[1].Approval.Message, []agentorcav1alpha1.WorkflowStepStatus{{Name: "build", Output: "SECRET"}}); strings.Contains(got, "SECRET") {
		t.Errorf("a parameter pulled a step output into an approval message: %q", got)
	}
}

func TestDeletedNetworkPolicyIsRecreatedAndExistingOneTolerated(t *testing.T) {
	wf := stepWorkflow(containerStep("a", "img"))
	r, cl, _ := stepsReconciler(t, wf)
	if err := r.ensureStepNetworkPolicy(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureStepNetworkPolicy(context.Background(), wf); err != nil {
		t.Fatalf("a second call failed: %v", err)
	}
	var np networkingv1.NetworkPolicy
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "wf-steps-deny-all"}, &np); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(context.Background(), &np); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := r.ensureStepNetworkPolicy(context.Background(), wf); err != nil {
		t.Fatalf("recreating: %v", err)
	}
}

// A step that is still waiting must not stall the steps after it. An approval
// gate can wait for hours; the branch running alongside it has to keep moving.
func TestWaitingStepDoesNotStallParallelBranches(t *testing.T) {
	r, cl, _ := stepsReconciler(t, stepWorkflow(
		approvalStepDef("ship it?"),
		containerStep("a", "img"),
		containerStep("b", "img", "a"),
	))
	reconcileWorkflow(t, r, "wf")
	wf := getWorkflow(t, cl, "wf")
	if stepOf(t, wf, "gate").Phase != agentorcav1alpha1.WorkflowStepPhaseWaitingForApproval || stepOf(t, wf, "a").Phase != agentorcav1alpha1.WorkflowStepPhaseRunning {
		t.Fatalf("gate %s, a %s: both should have started together", stepOf(t, wf, "gate").Phase, stepOf(t, wf, "a").Phase)
	}

	// a finishes while the gate is still waiting: b, which comes later in the list, starts.
	setPod(t, cl, "wf-a", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	reconcileWorkflow(t, r, "wf")
	reconcileWorkflow(t, r, "wf")
	wf = getWorkflow(t, cl, "wf")
	if stepOf(t, wf, "a").Phase != agentorcav1alpha1.WorkflowStepPhaseSucceeded {
		t.Errorf("a = %s: its completion was not noticed while the gate waited", stepOf(t, wf, "a").Phase)
	}
	if !podExists(cl, "wf-b") {
		t.Error("b did not start while an earlier step was waiting")
	}
	if wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseRunning {
		t.Errorf("workflow = %s, want Running while the gate waits", wf.Status.Phase)
	}

	// A failure on the parallel branch is noticed too, and ends the workflow.
	setPod(t, cl, "wf-b", failedPod(1, "Error"))
	reconcileWorkflow(t, r, "wf")
	if got := getWorkflow(t, cl, "wf"); got.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseFailed {
		t.Errorf("workflow = %s, want Failed: b's failure was not noticed while the gate waited", got.Status.Phase)
	}
}
