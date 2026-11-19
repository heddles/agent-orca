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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// AgentScheduleReconciler starts runs on a clock. It follows the Kubernetes
// CronJob algorithm: find the latest tick since the cursor, apply the
// concurrency policy, create one child with a name derived from the tick, then
// record it and sleep until the next tick. The deterministic name is the safety
// net against firing twice (a repeated reconcile, or a second replica when
// leader election is off); leader election only keeps the work single.
type AgentScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// maxRequeue bounds the sleep between reconciles, so a clock step or a
// daylight-saving change is noticed within the hour.
const maxRequeue = time.Hour

// maxRecentSkips is the length of status.recentSkips.
const maxRecentSkips = 10

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentschedules/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentruns,verbs=get;list;watch;create;update;patch;delete

func (r *AgentScheduleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// childState is where a child run stands from the schedule's point of view.
type childState int

const (
	childActive childState = iota
	childSucceeded
	childFailed
)

// scheduleChild is a run the schedule created.
type scheduleChild struct {
	obj       client.Object
	ref       corev1.ObjectReference
	state     childState
	created   time.Time
	finished  time.Time
	accounted bool
}

func (c scheduleChild) sortKey() time.Time {
	if !c.finished.IsZero() {
		return c.finished
	}
	return c.created
}

func runChildState(run *agentorcav1alpha1.AgentRun) childState {
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseHandedOff:
		return childSucceeded
	case agentorcav1alpha1.AgentRunPhaseFailed:
		return childFailed
	}
	return childActive
}

// listChildren returns the runs the schedule owns, oldest first.
func (r *AgentScheduleReconciler) listChildren(ctx context.Context, s *agentorcav1alpha1.AgentSchedule) ([]scheduleChild, error) {
	var out []scheduleChild
	var runs agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &runs, client.InNamespace(s.Namespace), client.MatchingLabels{schedule.LabelSchedule: s.Name}); err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if !metav1.IsControlledBy(run, s) {
			continue
		}
		c := scheduleChild{
			obj:       run,
			ref:       corev1.ObjectReference{APIVersion: agentorcav1alpha1.GroupVersion.String(), Kind: "AgentRun", Namespace: run.Namespace, Name: run.Name, UID: run.UID},
			state:     runChildState(run),
			created:   run.CreationTimestamp.Time,
			accounted: run.Annotations[schedule.AnnotationAccounted] != "",
		}
		if run.Status.CompletionTime != nil {
			c.finished = run.Status.CompletionTime.Time
		} else if c.state != childActive {
			c.finished = c.created
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b scheduleChild) int {
		if c := a.created.Compare(b.created); c != 0 {
			return c
		}
		return compareStrings(a.ref.Name, b.ref.Name)
	})
	return out, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Reconcile implements reconcile.Reconciler.
func (r *AgentScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var s agentorcav1alpha1.AgentSchedule
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !s.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // children go with it through their owner reference
	}
	now := r.now()
	orig := s.DeepCopy()
	st := &s.Status

	// A change of spec resets the cursor: nothing fires retroactively, and a
	// resumed or edited schedule starts clean.
	if st.ObservedGeneration != s.Generation {
		st.ObservedGeneration = s.Generation
		t := metav1.NewTime(now)
		st.CursorTime = &t
		st.ConsecutiveFailures = 0
		meta.RemoveStatusCondition(&st.Conditions, agentorcav1alpha1.ScheduleConditionDegraded)
	}

	children, err := r.listChildren(ctx, &s)
	if err != nil {
		return ctrl.Result{}, err
	}
	accounted := r.account(&s, children)
	children = r.prune(ctx, &s, children)
	st.Active = activeRefs(children)

	result := r.tick(ctx, &s, now)

	if !equality.Semantic.DeepEqual(orig.Status, s.Status) {
		if err := r.Status().Patch(ctx, &s, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching AgentSchedule status: %w", err)
		}
	}
	// Mark the runs counted only after the status that counts them is stored.
	for _, c := range accounted {
		if err := r.markAccounted(ctx, c); err != nil {
			log.Error(err, "marking a run as counted", "run", c.ref.Name)
		}
	}
	return result, nil
}

// account counts the children that finished since the last reconcile: it keeps
// the failure streak and the time of the last success. It returns the children
// it counted, so the caller can mark them once the status is stored.
func (r *AgentScheduleReconciler) account(s *agentorcav1alpha1.AgentSchedule, children []scheduleChild) []scheduleChild {
	var fresh []scheduleChild
	for _, c := range children {
		if c.state != childActive && !c.accounted {
			fresh = append(fresh, c)
		}
	}
	slices.SortFunc(fresh, func(a, b scheduleChild) int { return a.sortKey().Compare(b.sortKey()) })
	for _, c := range fresh {
		switch c.state {
		case childSucceeded:
			s.Status.ConsecutiveFailures = 0
			t := metav1.NewTime(c.sortKey())
			s.Status.LastSuccessfulTime = &t
		case childFailed:
			s.Status.ConsecutiveFailures++
		}
	}
	return fresh
}

func (r *AgentScheduleReconciler) markAccounted(ctx context.Context, c scheduleChild) error {
	patch := client.MergeFrom(c.obj.DeepCopyObject().(client.Object))
	a := c.obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[schedule.AnnotationAccounted] = annotationTrue
	c.obj.SetAnnotations(a)
	return client.IgnoreNotFound(r.Patch(ctx, c.obj, patch))
}

// prune deletes finished, counted children beyond the history limits, oldest
// first, and returns the children that are left.
func (r *AgentScheduleReconciler) prune(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, children []scheduleChild) []scheduleChild {
	log := logf.FromContext(ctx)
	limits := map[childState]int{
		childSucceeded: historyLimit(s.Spec.SuccessfulRunsHistoryLimit),
		childFailed:    historyLimit(s.Spec.FailedRunsHistoryLimit),
	}
	var finished []scheduleChild
	for _, c := range children {
		if c.state != childActive && c.accounted {
			finished = append(finished, c)
		}
	}
	slices.SortFunc(finished, func(a, b scheduleChild) int { return b.sortKey().Compare(a.sortKey()) }) // newest first
	kept := map[childState]int{}
	gone := map[string]bool{}
	for _, c := range finished {
		kept[c.state]++
		if kept[c.state] <= limits[c.state] {
			continue
		}
		if err := r.Delete(ctx, c.obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "pruning a finished run", "run", c.ref.Name)
			continue
		}
		gone[c.ref.Name] = true
	}
	if len(gone) == 0 {
		return children
	}
	left := children[:0:0]
	for _, c := range children {
		if !gone[c.ref.Name] {
			left = append(left, c)
		}
	}
	return left
}

func historyLimit(p *int32) int {
	if p == nil {
		return schedule.DefaultHistoryLimit
	}
	return int(*p)
}

func activeRefs(children []scheduleChild) []corev1.ObjectReference {
	var out []corev1.ObjectReference
	for _, c := range children {
		if c.state == childActive {
			out = append(out, c.ref)
		}
	}
	return out
}

// tick decides what the schedule does at now and returns when to look again.
func (r *AgentScheduleReconciler) tick(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, now time.Time) ctrl.Result {
	st := &s.Status
	cond := func(t string, on bool, reason, msg string) {
		status := metav1.ConditionFalse
		if on {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: s.Generation,
		})
	}
	cond(agentorcav1alpha1.ScheduleConditionSuspended, s.Spec.Suspend, "Spec", "spec.suspend")

	cs, err := r.parse(s)
	if err != nil {
		st.NextScheduleTime = nil
		cond(agentorcav1alpha1.ScheduleConditionReady, false, "InvalidSchedule", err.Error())
		return ctrl.Result{} // a spec change triggers the next reconcile
	}
	cond(agentorcav1alpha1.ScheduleConditionReady, true, "Valid", "")

	if s.Spec.Suspend {
		st.NextScheduleTime = nil
		return ctrl.Result{}
	}

	cursor := scheduleCursor(st, now)
	plan := cs.Plan(cursor, now, startingDeadline(s))
	setNext := func(next time.Time) {
		if next.IsZero() {
			st.NextScheduleTime = nil
			return
		}
		t := metav1.NewTime(next)
		st.NextScheduleTime = &t
	}

	switch {
	case !plan.Missed.IsZero():
		r.skip(s, plan.Missed, agentorcav1alpha1.ScheduleSkipMissed,
			fmt.Sprintf("the operator did not start it within %s of its time", startingDeadline(s)))
		t := metav1.NewTime(now)
		st.CursorTime = &t
		setNext(plan.Next)
	case !plan.Due.IsZero():
		r.fire(ctx, s, cs, plan.Due)
		setNext(plan.Next)
	default:
		setNext(plan.Next)
	}
	return ctrl.Result{RequeueAfter: requeueIn(plan.Next, now)}
}

func requeueIn(next, now time.Time) time.Duration {
	if next.IsZero() {
		return maxRequeue
	}
	return min(max(next.Sub(now), time.Second), maxRequeue)
}

func startingDeadline(s *agentorcav1alpha1.AgentSchedule) time.Duration {
	if s.Spec.StartingDeadlineSeconds == nil {
		return schedule.DefaultStartingDeadline
	}
	return time.Duration(*s.Spec.StartingDeadlineSeconds) * time.Second
}

// scheduleCursor is where tick evaluation starts: the later of the last tick
// handled and the cursor.
func scheduleCursor(st *agentorcav1alpha1.AgentScheduleStatus, now time.Time) time.Time {
	var c time.Time
	if st.CursorTime != nil {
		c = st.CursorTime.Time
	}
	if st.LastScheduleTime != nil && st.LastScheduleTime.After(c) {
		c = st.LastScheduleTime.Time
	}
	if c.IsZero() {
		return now
	}
	return c
}

func (r *AgentScheduleReconciler) parse(s *agentorcav1alpha1.AgentSchedule) (*schedule.Cron, error) {
	if len(s.Name) > schedule.MaxNameLength {
		return nil, fmt.Errorf("the schedule's name is longer than %d characters", schedule.MaxNameLength)
	}
	return schedule.Parse(s.Spec.Schedule, s.Spec.TimeZone)
}

// skip records a tick that started nothing and moves the cursor past it.
func (r *AgentScheduleReconciler) skip(s *agentorcav1alpha1.AgentSchedule, tick time.Time, reason, msg string) {
	st := &s.Status
	st.RecentSkips = append(st.RecentSkips, agentorcav1alpha1.ScheduleSkip{Time: metav1.NewTime(tick), Reason: reason, Message: msg})
	if n := len(st.RecentSkips); n > maxRecentSkips {
		st.RecentSkips = slices.Clone(st.RecentSkips[n-maxRecentSkips:])
	}
	t := metav1.NewTime(tick)
	st.LastScheduleTime = &t
	if r.Recorder != nil {
		r.Recorder.Eventf(s, corev1.EventTypeNormal, reason, "tick %s skipped: %s", tick.Format(time.RFC3339), msg)
	}
}

// fire handles one due tick: concurrency policy, then the run.
func (r *AgentScheduleReconciler) fire(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, cs *schedule.Cron, tick time.Time) {
	log := logf.FromContext(ctx)
	st := &s.Status

	// Replace arrives with the tenancy stage; until then it behaves as Forbid.
	if s.Spec.ConcurrencyPolicy != agentorcav1alpha1.ScheduleConcurrencyAllow && len(st.Active) > 0 {
		r.skip(s, tick, agentorcav1alpha1.ScheduleSkipConcurrency,
			fmt.Sprintf("the previous run %s is still going", st.Active[0].Name))
		return
	}

	run, err := r.buildRun(ctx, s, cs, tick)
	if err != nil {
		cond := metav1.Condition{
			Type: agentorcav1alpha1.ScheduleConditionTargetMissing, Status: metav1.ConditionTrue,
			Reason: "TargetMissing", Message: err.Error(), ObservedGeneration: s.Generation,
		}
		meta.SetStatusCondition(&st.Conditions, cond)
		r.skip(s, tick, agentorcav1alpha1.ScheduleSkipTargetMissing, err.Error())
		return
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: agentorcav1alpha1.ScheduleConditionTargetMissing, Status: metav1.ConditionFalse,
		Reason: "Found", ObservedGeneration: s.Generation,
	})

	if err := r.Create(ctx, run); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// Leave the cursor where it is: the next reconcile finds the same tick
			// and tries again with the same name.
			log.Error(err, "creating the scheduled run", "run", run.Name)
			if r.Recorder != nil {
				r.Recorder.Eventf(s, corev1.EventTypeWarning, "CreateFailed", "creating run %s: %v", run.Name, err)
			}
			return
		}
		// Created by an earlier reconcile or another replica: the tick is handled.
	} else if r.Recorder != nil {
		r.Recorder.Eventf(s, corev1.EventTypeNormal, "Started", "started run %s for %s", run.Name, tick.Format(time.RFC3339))
	}

	t := metav1.NewTime(tick)
	st.LastScheduleTime = &t
	ref := corev1.ObjectReference{APIVersion: agentorcav1alpha1.GroupVersion.String(), Kind: "AgentRun", Namespace: run.Namespace, Name: run.Name}
	if !slices.ContainsFunc(st.Active, func(o corev1.ObjectReference) bool { return o.Name == ref.Name }) {
		st.Active = append(st.Active, ref)
	}
}

// buildRun builds the AgentRun for a tick. It fails when the agent is gone.
func (r *AgentScheduleReconciler) buildRun(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, cs *schedule.Cron, tick time.Time) (*agentorcav1alpha1.AgentRun, error) {
	var agent agentorcav1alpha1.Agent
	if err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.AgentRef}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("agent %q was not found in namespace %q", s.Spec.AgentRef, s.Namespace)
		}
		return nil, fmt.Errorf("reading agent %q: %w", s.Spec.AgentRef, err)
	}
	local := tick.In(cs.Location())
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedule.RunName(s.Name, tick),
			Namespace: s.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
				schedule.LabelSchedule:  s.Name,
			},
			Annotations: map[string]string{
				schedule.AnnotationCreatedBy:    schedule.Actor(s.Namespace, s.Name),
				schedule.AnnotationScheduledFor: local.Format(time.RFC3339),
			},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef:   s.Spec.AgentRef,
			Input:      schedule.Substitute(s.Spec.Input, s.Name, local),
			Timeout:    s.Spec.Timeout.DeepCopy(),
			Safeguards: s.Spec.Safeguards.DeepCopy(),
		},
	}
	if b := s.Spec.Budget; b != nil {
		run.Spec.Budget = b.DeepCopy()
		if run.Spec.Budget.OnExceeded == "" {
			run.Spec.Budget.OnExceeded = agentorcav1alpha1.RunBudgetFail
		}
	}
	if res := s.Spec.Result; res != nil {
		run.Spec.Egress = res.Egress.DeepCopy()
		run.Spec.Callbacks = res.Callbacks.DeepCopy()
	}
	// The tenant comes from the schedule, never from anything a person typed.
	for _, k := range []string{tenancy.LabelTenantID, tenancy.LabelTenantName} {
		if v := s.Labels[k]; v != "" {
			run.Labels[k] = v
		}
	}
	if err := controllerutil.SetControllerReference(s, run, r.Scheme); err != nil {
		return nil, err
	}
	return run, nil
}

// SetupWithManager registers the reconciler.
func (r *AgentScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("agentschedule") //nolint:staticcheck // same recorder API as the other controllers
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.AgentSchedule{}).
		Owns(&agentorcav1alpha1.AgentRun{}).
		Named("agentschedule").
		Complete(r)
}
