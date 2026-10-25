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

// Workflow step types other than agent steps.
//
//   - container: a deterministic job, one container in its own pod, run to
//     completion. Exit 0 is success and the tail of the output is the step's
//     output. The pod gets no service account token, a read-only root
//     filesystem, a non-root user and a deny-all NetworkPolicy. The CNI programs
//     that policy a moment after the pod starts (measured at about two seconds
//     on k3s/flannel), so images must be trusted; governed network access and
//     untrusted code belong to the workflow-owned sandbox.
//   - approval: the workflow waits for a person to decide, through the approval
//     engine (inbox, notification, audit row). Rejected or undecided fails the step.
//
// Like agent steps, these only change the step's in-memory status; the
// reconcile loop persists it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

const (
	// maxStepOutputBytes caps a step's output (the tail of a container's logs).
	maxStepOutputBytes = 10 * 1024
	// stepLogTailLines is how many log lines are read from a finished container.
	stepLogTailLines = 400
	// defaultContainerStepTimeout applies when neither the step nor the workflow has a timeout.
	defaultContainerStepTimeout = 30 * time.Minute
	// defaultApprovalStepTimeout applies when an approval step sets no timeout.
	defaultApprovalStepTimeout = 24 * time.Hour
	// imagePullGrace is how long a container step may sit unable to start its image.
	imagePullGrace = 2 * time.Minute
	// stepPodUser is the user container steps run as unless the step sets one.
	stepPodUser int64 = 65532
	// maxApprovalMessage caps the message stored for the approver.
	maxApprovalMessage = 4000

	labelWorkflow     = "agentorca.io/workflow"
	labelWorkflowStep = "agentorca.io/workflow-step"
	labelStepPod      = "agentorca.io/step-pod"
)

// stepType returns a step's type; an unset type is an agent step.
func stepType(s *agentorcav1alpha1.WorkflowStep) agentorcav1alpha1.WorkflowStepType {
	if s.Type == "" {
		return agentorcav1alpha1.WorkflowStepTypeAgent
	}
	return s.Type
}

// markWorkflowStarted moves a workflow to Running when its first step launches.
func markWorkflowStarted(wf *agentorcav1alpha1.AgentWorkflow, now metav1.Time) {
	if wf.Status.Phase != agentorcav1alpha1.AgentWorkflowPhaseRunning {
		wf.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseRunning
		wf.Status.StartTime = &now
	}
}

// finishStep ends a step in-memory. A failure under onStepFailure: continue is
// recorded as Skipped so dependents can still be evaluated, as for agent steps.
func finishStep(wf *agentorcav1alpha1.AgentWorkflow, ss *agentorcav1alpha1.WorkflowStepStatus, succeeded bool, output, reason string) {
	now := metav1.Now()
	ss.CompletionTime = &now
	ss.Output = output
	switch {
	case succeeded:
		ss.Phase = agentorcav1alpha1.WorkflowStepPhaseSucceeded
	case wf.Spec.OnStepFailure == "continue":
		ss.Phase, ss.FailureReason = agentorcav1alpha1.WorkflowStepPhaseSkipped, reason
	default:
		ss.Phase, ss.FailureReason = agentorcav1alpha1.WorkflowStepPhaseFailed, reason
	}
}

// stepTimeout is the step's time limit: its own, else what is left of the
// workflow's, else def.
func stepTimeout(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, def time.Duration) time.Duration {
	if step.Timeout != nil && step.Timeout.Duration > 0 {
		return step.Timeout.Duration
	}
	if wf.Spec.Timeout != nil && wf.Status.StartTime != nil {
		if left := time.Until(wf.Status.StartTime.Add(wf.Spec.Timeout.Duration)); left > 0 {
			return left
		}
	}
	return def
}

func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "[output truncated]\n" + s[len(s)-max:]
}

func (r *AgentWorkflowReconciler) startContainerStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) error {
	now := metav1.Now()
	ss.StartTime = &now
	markWorkflowStarted(wf, now)
	c := step.Container
	if err := agentwebhook.CheckRegistryAllowlist(c.Image, r.AllowedRegistries); err != nil {
		finishStep(wf, ss, false, "", fmt.Sprintf("image rejected: %v", err))
		return nil
	}

	allStatuses := append(wf.Status.Steps, wf.Status.DynamicStepStatuses...)
	env := make([]agentorcav1alpha1.StepEnvVar, len(c.Env))
	for i, e := range c.Env {
		env[i] = agentorcav1alpha1.StepEnvVar{Name: e.Name, Value: resolveTemplates(e.Value, allStatuses)}
	}
	pod := buildStepPod(wf, step, env, stepTimeout(wf, step, defaultContainerStepTimeout))
	if err := controllerutil.SetControllerReference(wf, pod, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on step pod: %w", err)
	}
	if err := r.ensureStepNetworkPolicy(ctx, wf); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating pod for step %q: %w", step.Name, err)
	}
	ss.Phase = agentorcav1alpha1.WorkflowStepPhaseRunning
	ss.PodRef = pod.Name
	return nil
}

// buildStepPod builds the pod of a container step. It is pure so the security
// posture can be tested without a cluster.
func buildStepPod(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, env []agentorcav1alpha1.StepEnvVar, timeout time.Duration) *corev1.Pod {
	c := step.Container
	user := stepPodUser
	if c.RunAsUser != nil {
		user = *c.RunAsUser
	}
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	if c.Resources != nil {
		resources = *c.Resources
	}
	vars := make([]corev1.EnvVar, 0, 2+len(env))
	vars = append(vars,
		corev1.EnvVar{Name: "AGENTORC_WORKFLOW", Value: wf.Name},
		corev1.EnvVar{Name: "AGENTORC_STEP", Value: step.Name})
	for _, e := range env {
		vars = append(vars, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	deadline := max(int64(timeout.Seconds()), 1)
	no, yes := false, true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", wf.Name, step.Name),
			Namespace: wf.Namespace,
			Labels:    map[string]string{labelWorkflow: wf.Name, labelWorkflowStep: step.Name, labelStepPod: "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			ActiveDeadlineSeconds:        &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				RunAsUser:      &user,
				RunAsGroup:     &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:       "step",
				Image:      c.Image,
				Command:    c.Command,
				Args:       c.Args,
				Env:        vars,
				Resources:  resources,
				WorkingDir: "/work",
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "work", MountPath: "/work"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

// ensureStepNetworkPolicy denies all traffic to and from the workflow's step
// pods: a container step has no network.
func (r *AgentWorkflowReconciler) ensureStepNetworkPolicy(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: wf.Name + "-steps-deny-all", Namespace: wf.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelWorkflow: wf.Name, labelStepPod: "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	if err := controllerutil.SetControllerReference(wf, np, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on step network policy: %w", err)
	}
	if err := r.Create(ctx, np); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating step network policy: %w", err)
	}
	return nil
}

func (r *AgentWorkflowReconciler) checkContainerStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, ss *agentorcav1alpha1.WorkflowStepStatus) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: ss.PodRef, Namespace: wf.Namespace}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			finishStep(wf, ss, false, "", "the step's pod was deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		out := r.stepLogs(ctx, &pod)
		finishStep(wf, ss, true, out, "")
		r.deleteStepPod(ctx, &pod)
		return ctrl.Result{}, nil
	case corev1.PodFailed:
		out := r.stepLogs(ctx, &pod)
		finishStep(wf, ss, false, out, stepPodFailureReason(&pod, out))
		r.deleteStepPod(ctx, &pod)
		return ctrl.Result{}, nil
	}
	if why := stuckReason(&pod); why != "" {
		finishStep(wf, ss, false, "", why)
		r.deleteStepPod(ctx, &pod)
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
}

// stuckReason reports a pod that cannot start its container (a bad image, a bad
// config), after a grace period so a slow pull is not mistaken for a failure.
func stuckReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "InvalidImageName", "CreateContainerConfigError":
			return fmt.Sprintf("the container cannot start: %s: %s", w.Reason, w.Message)
		case "ImagePullBackOff", "ErrImagePull", "ErrImageNeverPull", "CreateContainerError":
			if time.Since(pod.CreationTimestamp.Time) > imagePullGrace {
				return fmt.Sprintf("the container did not start within %s: %s: %s", imagePullGrace, w.Reason, w.Message)
			}
		}
	}
	return ""
}

func stepPodFailureReason(pod *corev1.Pod, logs string) string {
	reason := "the step's pod failed"
	if pod.Status.Reason != "" {
		reason = "the step's pod failed: " + pod.Status.Reason
		if pod.Status.Reason == "DeadlineExceeded" {
			reason = "the step timed out"
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			reason = fmt.Sprintf("the container exited with code %d", t.ExitCode)
			if t.Reason != "" && t.Reason != "Error" {
				reason += " (" + t.Reason + ")"
			}
		}
	}
	if tail := strings.TrimSpace(logs); tail != "" {
		if len(tail) > 500 {
			tail = "…" + tail[len(tail)-500:]
		}
		reason += ": " + tail
	}
	return reason
}

func (r *AgentWorkflowReconciler) stepLogs(ctx context.Context, pod *corev1.Pod) string {
	if r.K8s == nil {
		return ""
	}
	tail := int64(stepLogTailLines)
	raw, err := r.K8s.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "step", TailLines: &tail}).DoRaw(ctx)
	if err != nil {
		log.FromContext(ctx).Info("reading a step pod's logs", "pod", pod.Name, "err", err.Error())
		return ""
	}
	return truncateTail(strings.TrimRight(string(raw), "\n"), maxStepOutputBytes)
}

func (r *AgentWorkflowReconciler) deleteStepPod(ctx context.Context, pod *corev1.Pod) {
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		log.FromContext(ctx).Info("deleting a finished step pod", "pod", pod.Name, "err", err.Error())
	}
}

// approvalDetails is stored with an approval step's request, for the approver.
type approvalDetails struct {
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	Message  string `json:"message"`
}

func approvalSourcePrefix(wf *agentorcav1alpha1.AgentWorkflow) string { return string(wf.UID) + "/" }

// approvalRequest builds the request an approval step makes. It is the same
// every time, so asking again returns the stored row.
func (r *AgentWorkflowReconciler) approvalRequest(wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, started time.Time) postgresql.Approval {
	allStatuses := append(wf.Status.Steps, wf.Status.DynamicStepStatuses...)
	msg := resolveTemplates(step.Approval.Message, allStatuses)
	if len(msg) > maxApprovalMessage {
		msg = msg[:maxApprovalMessage] + "…"
	}
	details, _ := json.Marshal(approvalDetails{Workflow: wf.Name, Step: step.Name, Message: msg})
	timeout := defaultApprovalStepTimeout
	if step.Approval.Timeout != nil && step.Approval.Timeout.Duration > 0 {
		timeout = step.Approval.Timeout.Duration
	}
	first := msg
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > 120 {
		first = first[:120] + "…"
	}
	return postgresql.Approval{
		Tenant:         wf.Labels["agentorca.io/tenant"],
		Namespace:      wf.Namespace,
		Kind:           postgresql.ApprovalKindWorkflow,
		SourceKey:      approvalSourcePrefix(wf) + step.Name,
		Summary:        fmt.Sprintf("%s / %s: %s", wf.Name, step.Name, first),
		Details:        details,
		ApproverGroups: step.Approval.ApproverGroups,
		ExpiresAt:      started.Add(timeout),
	}
}

func (r *AgentWorkflowReconciler) startApprovalStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) error {
	now := metav1.Now()
	ss.StartTime = &now
	markWorkflowStarted(wf, now)
	if r.Approvals == nil {
		finishStep(wf, ss, false, "", "approval steps need the PostgreSQL store, which is not configured")
		return nil
	}
	stored, err := r.Approvals.RequestApproval(ctx, r.approvalRequest(wf, step, now.Time))
	if err != nil {
		return fmt.Errorf("opening the approval for step %q: %w", step.Name, err)
	}
	if stored.Created && stored.Status == postgresql.ApprovalPending && r.ApprovalNotifier != nil {
		if err := r.ApprovalNotifier.NotifyApproval(ctx, stored); err != nil {
			log.FromContext(ctx).Error(err, "notifying approvers", "approval", stored.ID)
		}
	}
	ss.Phase = agentorcav1alpha1.WorkflowStepPhaseWaitingForApproval
	ss.ApprovalID = stored.ID
	return nil
}

func (r *AgentWorkflowReconciler) checkApprovalStep(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, step *agentorcav1alpha1.WorkflowStep, ss *agentorcav1alpha1.WorkflowStepStatus) (ctrl.Result, error) {
	if r.Approvals == nil {
		finishStep(wf, ss, false, "", "approval steps need the PostgreSQL store, which is not configured")
		return ctrl.Result{}, nil
	}
	started := time.Now()
	if ss.StartTime != nil {
		started = ss.StartTime.Time
	}
	// The request is identical to the one that opened the approval, so this
	// returns the stored row (with its current status).
	stored, err := r.Approvals.RequestApproval(ctx, r.approvalRequest(wf, step, started))
	if err != nil {
		log.FromContext(ctx).Error(err, "reading the approval", "step", step.Name)
		return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
	}
	if stored.Status == postgresql.ApprovalPending && time.Now().After(stored.ExpiresAt) {
		if decided, err := r.Approvals.DecideApproval(ctx, stored.ID, postgresql.ApprovalExpired, "", "no decision within the approval timeout"); err == nil {
			stored = decided
		}
	}
	switch stored.Status {
	case postgresql.ApprovalPending:
		return ctrl.Result{RequeueAfter: workflowPollInterval}, nil
	case postgresql.ApprovalApproved:
		out := "approved"
		if stored.DecidedBy != "" {
			out += " by " + stored.DecidedBy
		}
		if stored.Reason != "" {
			out += ": " + stored.Reason
		}
		finishStep(wf, ss, true, out, "")
	default: // rejected, expired or withdrawn
		why := stored.Status
		if stored.Status == postgresql.ApprovalExpired {
			why = "not decided in time"
		}
		if stored.DecidedBy != "" {
			why += " by " + stored.DecidedBy
		}
		if stored.Reason != "" {
			why += ": " + stored.Reason
		}
		finishStep(wf, ss, false, "", "the approval was "+why)
	}
	return ctrl.Result{}, nil
}

// withdrawWorkflowApprovals withdraws the pending approvals of a workflow's
// approval steps once the workflow has ended, so no one is asked to decide
// something that no longer matters.
func (r *AgentWorkflowReconciler) withdrawWorkflowApprovals(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) {
	if r.Approvals == nil || wf.UID == "" || !hasApprovalSteps(wf) {
		return
	}
	if err := r.Approvals.WithdrawBySourcePrefix(ctx, postgresql.ApprovalKindWorkflow, approvalSourcePrefix(wf), "the workflow ended"); err != nil {
		log.FromContext(ctx).Error(err, "withdrawing the workflow's pending approvals")
	}
}

func hasApprovalSteps(wf *agentorcav1alpha1.AgentWorkflow) bool {
	for i := range wf.Spec.Steps {
		if stepType(&wf.Spec.Steps[i]) == agentorcav1alpha1.WorkflowStepTypeApproval {
			return true
		}
	}
	return false
}
