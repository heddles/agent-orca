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
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/quota"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/state"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// ScheduleAuditor records the audit rows the controller writes on its own. A
// *postgresql.Store is one; nil disables auditing (no PostgreSQL).
type ScheduleAuditor interface {
	AppendAudit(ctx context.Context, e postgresql.AuditEvent) (postgresql.AuditEvent, error)
}

// AgentScheduleReconciler starts runs on a clock. It follows the Kubernetes
// CronJob algorithm: find the latest tick since the cursor, apply the
// concurrency policy, check the tenant's quotas, create one child with a name
// derived from the tick, then record it and sleep until the next tick. The
// deterministic name is the safety net against firing twice (a repeated
// reconcile, or a second replica when leader election is off); leader election
// only keeps the work single.
type AgentScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// StateStore carries the cancel signal for a run the Replace policy stops, as
	// the UI's cancel does. Optional.
	StateStore state.Store
	// Audit records the rows the controller writes itself. Optional.
	Audit ScheduleAuditor
	// MaxConsecutiveFailures is how many finished runs in a row may fail before
	// the schedule stops ticking; zero means schedule.DefaultMaxConsecutiveFailures.
	MaxConsecutiveFailures int
	// Notifier sends the result notifications of spec.result.notify. Nil turns
	// them off: no run is claimed for a notification.
	Notifier Notifier
	// UIURL is the UI's public base URL, for the link to a run in a
	// notification. Empty leaves the link out.
	UIURL string

	notifyWG sync.WaitGroup
}

// maxRequeue bounds the sleep between reconciles, so a clock step or a
// daylight-saving change is noticed within the hour.
const maxRequeue = time.Hour

// retryAfter is how soon a tick is retried when the cluster could not be read.
const retryAfter = 30 * time.Second

// maxRecentSkips is the length of status.recentSkips.
const maxRecentSkips = 10

// annotationScheduleReplaced marks a run the Replace policy cancelled: it ended
// Failed, but not because the work failed, so it does not feed the breaker.
const annotationScheduleReplaced = "agentorca.io/schedule-replaced"

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentschedules/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentworkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=tenantconfigs,verbs=get;list;watch

func (r *AgentScheduleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AgentScheduleReconciler) maxFailures() int32 {
	if r.MaxConsecutiveFailures > 0 {
		return int32(r.MaxConsecutiveFailures) //nolint:gosec // a small configured count
	}
	return schedule.DefaultMaxConsecutiveFailures
}

// childState is where a child run stands from the schedule's point of view.
type childState int

const (
	childActive childState = iota
	childSucceeded
	childFailed
)

// scheduleChild is a run or workflow the schedule created.
type scheduleChild struct {
	obj       client.Object
	ref       corev1.ObjectReference
	state     childState
	created   time.Time
	finished  time.Time
	accounted bool
	replaced  bool
	spend     float64
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

func workflowChildState(wf *agentorcav1alpha1.AgentWorkflow) childState {
	switch wf.Status.Phase {
	case agentorcav1alpha1.AgentWorkflowPhaseSucceeded:
		return childSucceeded
	case agentorcav1alpha1.AgentWorkflowPhaseFailed, agentorcav1alpha1.AgentWorkflowPhaseCancelled:
		return childFailed
	}
	return childActive
}

// listChildren returns the runs and workflows the schedule owns, oldest first.
func (r *AgentScheduleReconciler) listChildren(ctx context.Context, s *agentorcav1alpha1.AgentSchedule) ([]scheduleChild, error) {
	var out []scheduleChild
	sel := []client.ListOption{client.InNamespace(s.Namespace), client.MatchingLabels{schedule.LabelSchedule: s.Name}}
	var runs agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &runs, sel...); err != nil {
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
			replaced:  run.Annotations[annotationScheduleReplaced] != "",
			spend:     parseUSD(run.Status.SpendUSD),
		}
		if run.Status.CompletionTime != nil {
			c.finished = run.Status.CompletionTime.Time
		}
		out = append(out, c)
	}
	var wfs agentorcav1alpha1.AgentWorkflowList
	if err := r.List(ctx, &wfs, sel...); err != nil {
		return nil, fmt.Errorf("listing workflows: %w", err)
	}
	for i := range wfs.Items {
		wf := &wfs.Items[i]
		if !metav1.IsControlledBy(wf, s) {
			continue
		}
		c := scheduleChild{
			obj:       wf,
			ref:       corev1.ObjectReference{APIVersion: agentorcav1alpha1.GroupVersion.String(), Kind: "AgentWorkflow", Namespace: wf.Namespace, Name: wf.Name, UID: wf.UID},
			state:     workflowChildState(wf),
			created:   wf.CreationTimestamp.Time,
			accounted: wf.Annotations[schedule.AnnotationAccounted] != "",
			spend:     parseUSD(wf.Status.TotalSpendUSD),
		}
		if wf.Status.CompletionTime != nil {
			c.finished = wf.Status.CompletionTime.Time
		}
		out = append(out, c)
	}
	for i := range out {
		if out[i].state != childActive && out[i].finished.IsZero() {
			out[i].finished = out[i].created
		}
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
	st.Ledger = rollLedger(st.Ledger, now)
	counted := r.account(&s, children, now)
	children = r.prune(ctx, &s, children)
	st.Active = activeRefs(children)
	spentDay, spentMonth := r.spent(&s, children)

	tenant, err := r.tenantOf(ctx, &s)
	if err != nil {
		return ctrl.Result{}, err
	}
	exhausted := r.budgetState(ctx, &s, spentDay, spentMonth)
	r.breaker(&s)
	r.setNotifyCondition(ctx, &s, tenant)

	res := r.tick(ctx, &s, tenant, exhausted, now)

	if !equality.Semantic.DeepEqual(orig.Status, s.Status) {
		if err := r.Status().Patch(ctx, &s, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching AgentSchedule status: %w", err)
		}
	}
	// Mark the children counted only after the status that counts them is stored.
	// The same write claims the run's notification, before anything is sent, so
	// a run is announced at most once.
	var jobs []notifyJob
	for _, c := range counted {
		job, send := r.runNotice(&s, c)
		send = send && r.Notifier != nil
		claim := ""
		if send {
			claim = scheduleNotify(&s).Channel
		}
		if err := r.markAccounted(ctx, c, claim); err != nil {
			log.Error(err, "marking a run as counted", "run", c.ref.Name)
			continue
		}
		if send {
			jobs = append(jobs, job)
		}
	}
	jobs = append(jobs, r.scheduleNotices(&s, &orig.Status)...)
	r.dispatch(ctx, &s, tenant, jobs)
	if res.retry {
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}
	return ctrl.Result{RequeueAfter: res.after}, nil
}

// rollLedger returns the ledger moved to the UTC day and month of now: a window
// that has ended starts again at zero.
func rollLedger(l *agentorcav1alpha1.ScheduleSpendLedger, now time.Time) *agentorcav1alpha1.ScheduleSpendLedger {
	day, month := now.UTC().Format(time.DateOnly), now.UTC().Format("2006-01")
	if l == nil {
		l = &agentorcav1alpha1.ScheduleSpendLedger{}
	}
	out := l.DeepCopy()
	if out.Day != day {
		out.Day, out.DayUSD = day, ""
	}
	if out.Month != month {
		out.Month, out.MonthUSD = month, ""
	}
	return out
}

// account counts the children that finished since the last reconcile: the
// failure streak, the time of the last success, and the spend (in a ledger, so
// pruning a run later loses nothing). It returns the children it counted, so
// the caller can mark them once the status is stored.
func (r *AgentScheduleReconciler) account(s *agentorcav1alpha1.AgentSchedule, children []scheduleChild, now time.Time) []scheduleChild {
	var fresh []scheduleChild
	for _, c := range children {
		if c.state != childActive && !c.accounted {
			fresh = append(fresh, c)
		}
	}
	slices.SortFunc(fresh, func(a, b scheduleChild) int { return a.sortKey().Compare(b.sortKey()) })
	led := s.Status.Ledger
	day, month := now.UTC().Format(time.DateOnly), now.UTC().Format("2006-01")
	for _, c := range fresh {
		switch {
		case c.state == childSucceeded:
			s.Status.ConsecutiveFailures = 0
			t := metav1.NewTime(c.sortKey())
			s.Status.LastSuccessfulTime = &t
		case c.state == childFailed && !c.replaced:
			s.Status.ConsecutiveFailures++
		}
		// The spend belongs to the window the run finished in.
		fin := c.sortKey().UTC()
		if fin.Format(time.DateOnly) == day {
			led.DayUSD = formatUSD(parseUSD(led.DayUSD) + c.spend)
		}
		if fin.Format("2006-01") == month {
			led.MonthUSD = formatUSD(parseUSD(led.MonthUSD) + c.spend)
		}
	}
	return fresh
}

// spent returns what the schedule spent today and this month: the ledger of
// finished runs plus what the runs still going have reported so far.
func (r *AgentScheduleReconciler) spent(s *agentorcav1alpha1.AgentSchedule, children []scheduleChild) (day, month float64) {
	day, month = parseUSD(s.Status.Ledger.DayUSD), parseUSD(s.Status.Ledger.MonthUSD)
	for _, c := range children {
		if c.state == childActive {
			day += c.spend
			month += c.spend
		}
	}
	s.Status.SpentTodayUSD, s.Status.SpentMonthUSD = formatUSD(day), formatUSD(month)
	return day, month
}

// markAccounted records that c was counted. A non-empty notifyChannel also
// records that its notification was claimed.
func (r *AgentScheduleReconciler) markAccounted(ctx context.Context, c scheduleChild, notifyChannel string) error {
	patch := client.MergeFrom(c.obj.DeepCopyObject().(client.Object))
	a := c.obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[schedule.AnnotationAccounted] = annotationTrue
	if notifyChannel != "" {
		a[schedule.AnnotationNotified] = notifyChannel
	}
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
	gone := map[types.UID]bool{}
	for _, c := range finished {
		kept[c.state]++
		if kept[c.state] <= limits[c.state] {
			continue
		}
		if err := r.Delete(ctx, c.obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "pruning a finished child", "name", c.ref.Name)
			continue
		}
		gone[c.ref.UID] = true
	}
	if len(gone) == 0 {
		return children
	}
	left := children[:0:0]
	for _, c := range children {
		if !gone[c.ref.UID] {
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

// tenantOf returns the TenantConfig that owns the schedule, nil when none does.
func (r *AgentScheduleReconciler) tenantOf(ctx context.Context, s *agentorcav1alpha1.AgentSchedule) (*agentorcav1alpha1.TenantConfig, error) {
	var tcs agentorcav1alpha1.TenantConfigList
	if err := r.List(ctx, &tcs); err != nil {
		return nil, fmt.Errorf("listing TenantConfigs: %w", err)
	}
	return tenancy.TenantConfigFor(tcs.Items, s), nil
}

// budgetState sets the BudgetExhausted condition from the schedule's spend caps
// and says which window is over. It audits the moment a cap stops the schedule,
// because that changes behaviour a person relies on.
func (r *AgentScheduleReconciler) budgetState(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, day, month float64) exhaustedWindow {
	dayCap, monthCap := schedule.SpendCaps(&s.Spec)
	var w exhaustedWindow
	var msg string
	switch {
	case monthCap > 0 && month >= monthCap:
		w, msg = windowMonth, fmt.Sprintf("this month's spend $%s reached the cap of $%s; ticks are skipped until the month rolls over", formatUSD(month), s.Spec.Spend.MaxUSDPerMonth)
	case dayCap > 0 && day >= dayCap:
		w, msg = windowDay, fmt.Sprintf("today's spend $%s reached the cap of $%s; ticks are skipped until tomorrow (UTC)", formatUSD(day), s.Spec.Spend.MaxUSDPerDay)
	}
	was := meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionBudgetExhausted)
	status := metav1.ConditionFalse
	reason := "WithinBudget"
	if w != windowNone {
		status, reason = metav1.ConditionTrue, "SpendCapReached"
	}
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type: agentorcav1alpha1.ScheduleConditionBudgetExhausted, Status: status, Reason: reason, Message: msg, ObservedGeneration: s.Generation,
	})
	if w != windowNone && !was {
		if r.Recorder != nil {
			r.Recorder.Event(s, corev1.EventTypeWarning, "BudgetExhausted", msg)
		}
		r.audit(ctx, s, postgresql.AuditSuspend, "spend cap reached: "+msg)
	}
	return w
}

type exhaustedWindow int

const (
	windowNone exhaustedWindow = iota
	windowDay
	windowMonth
)

// end is when the window that stopped the schedule rolls over.
func (w exhaustedWindow) end(now time.Time) time.Time {
	n := now.UTC()
	switch w {
	case windowDay:
		return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
	case windowMonth:
		return time.Date(n.Year(), n.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

func (r *AgentScheduleReconciler) audit(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, action, summary string) {
	if r.Audit == nil {
		return
	}
	id, _ := tenancy.TenantOf(s)
	ev := postgresql.AuditEvent{
		TenantID: id, UserID: schedule.SystemUser, Action: action, Kind: "AgentSchedule",
		Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion, Summary: summary,
	}
	actx, cancel := context.WithTimeout(postgresql.WithPlatform(context.WithoutCancel(ctx)), 5*time.Second)
	defer cancel()
	if _, err := r.Audit.AppendAudit(actx, ev); err != nil {
		logf.FromContext(ctx).Error(err, "recording the schedule audit event", "schedule", s.Name)
	}
}

// breaker trips the Degraded condition after too many failures in a row, and
// clears it when a person resumes the schedule: the API stamps
// agentorca.io/schedule-resumed-at, which must be newer than the moment the
// schedule degraded. Editing the spec also clears it (see Reconcile).
func (r *AgentScheduleReconciler) breaker(s *agentorcav1alpha1.AgentSchedule) {
	st := &s.Status
	cur := meta.FindStatusCondition(st.Conditions, agentorcav1alpha1.ScheduleConditionDegraded)
	if cur != nil && cur.Status == metav1.ConditionTrue {
		if at, err := time.Parse(time.RFC3339, s.Annotations[schedule.AnnotationResumedAt]); err == nil && at.After(cur.LastTransitionTime.Time) {
			st.ConsecutiveFailures = 0
			t := metav1.NewTime(r.now())
			st.CursorTime = &t
			meta.SetStatusCondition(&st.Conditions, metav1.Condition{
				Type: agentorcav1alpha1.ScheduleConditionDegraded, Status: metav1.ConditionFalse, Reason: "Resumed",
				Message: "resumed by a person", ObservedGeneration: s.Generation,
			})
		}
		return
	}
	if st.ConsecutiveFailures >= r.maxFailures() {
		msg := fmt.Sprintf("the last %d runs failed; the schedule stopped until it is resumed", st.ConsecutiveFailures)
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: agentorcav1alpha1.ScheduleConditionDegraded, Status: metav1.ConditionTrue, Reason: "ConsecutiveFailures",
			Message: msg, ObservedGeneration: s.Generation, LastTransitionTime: metav1.NewTime(r.now()),
		})
		if r.Recorder != nil {
			r.Recorder.Event(s, corev1.EventTypeWarning, "Degraded", msg)
		}
	}
}

// tickResult is what tick decided about the next reconcile.
type tickResult struct {
	after time.Duration
	// retry means a tick could not be handled (the cluster could not be read)
	// and must be tried again soon with the cursor where it was.
	retry bool
}

// tick decides what the schedule does at now.
func (r *AgentScheduleReconciler) tick(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig, exhausted exhaustedWindow, now time.Time) tickResult {
	st := &s.Status
	setCond := func(t string, on bool, reason, msg string) {
		status := metav1.ConditionFalse
		if on {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: s.Generation,
		})
	}
	setCond(agentorcav1alpha1.ScheduleConditionSuspended, s.Spec.Suspend, "Spec", "spec.suspend")

	cs, err := r.parse(s)
	if err != nil {
		st.NextScheduleTime = nil
		setCond(agentorcav1alpha1.ScheduleConditionReady, false, "InvalidSchedule", err.Error())
		return tickResult{} // a spec change triggers the next reconcile
	}
	setCond(agentorcav1alpha1.ScheduleConditionReady, true, "Valid", "")

	if s.Spec.Suspend || meta.IsStatusConditionTrue(st.Conditions, agentorcav1alpha1.ScheduleConditionDegraded) {
		st.NextScheduleTime = nil
		return tickResult{}
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

	retry := false
	switch {
	case !plan.Missed.IsZero():
		r.skip(s, plan.Missed, agentorcav1alpha1.ScheduleSkipMissed,
			fmt.Sprintf("the operator did not start it within %s of its time", startingDeadline(s)))
		t := metav1.NewTime(now)
		st.CursorTime = &t
	case !plan.Due.IsZero():
		retry = r.fire(ctx, s, cs, tenant, exhausted, plan.Due)
	}
	setNext(plan.Next)
	wake := requeueIn(plan.Next, now)
	if end := exhausted.end(now); !end.IsZero() {
		wake = min(wake, max(end.Sub(now), time.Second))
	}
	return tickResult{after: wake, retry: retry}
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

func (r *AgentScheduleReconciler) setTargetMissing(s *agentorcav1alpha1.AgentSchedule, missing bool, msg string) {
	status, reason := metav1.ConditionFalse, "Found"
	if missing {
		status, reason = metav1.ConditionTrue, "TargetMissing"
	}
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type: agentorcav1alpha1.ScheduleConditionTargetMissing, Status: status, Reason: reason, Message: msg, ObservedGeneration: s.Generation,
	})
}

// fire handles one due tick. It returns true when the tick must be retried.
func (r *AgentScheduleReconciler) fire(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, cs *schedule.Cron, tenant *agentorcav1alpha1.TenantConfig, exhausted exhaustedWindow, tick time.Time) bool {
	log := logf.FromContext(ctx)
	st := &s.Status

	if exhausted != windowNone {
		r.skip(s, tick, agentorcav1alpha1.ScheduleSkipBudget, "the schedule's spend cap is reached")
		return false
	}

	// What the schedule starts must still exist and still be allowed: an agent
	// can leave the tenant's allowlist after the schedule was written.
	child, template, err := r.buildChild(ctx, s, cs, tick)
	if err != nil {
		var te *targetError
		if errors.As(err, &te) {
			r.setTargetMissing(s, true, te.Error())
			r.skip(s, tick, agentorcav1alpha1.ScheduleSkipTargetMissing, te.Error())
			return false
		}
		log.Error(err, "building the scheduled child")
		return true
	}
	if tenant != nil {
		for _, name := range scheduleAgents(&s.Spec, template) {
			if !tenancy.AgentAllowed(tenant.Spec.AllowedAgents, name) {
				msg := fmt.Sprintf("agent %q is not in the allowed agents of tenant %q", name, tenant.Name)
				r.setTargetMissing(s, true, msg)
				r.skip(s, tick, agentorcav1alpha1.ScheduleSkipTargetMissing, msg)
				return false
			}
		}
	}
	r.setTargetMissing(s, false, "")
	budget := ""
	if tenant != nil {
		budget = tenant.Spec.BudgetPerDayUSD
	}
	if !schedule.HasCeiling(&s.Spec, budget, template) {
		r.skip(s, tick, agentorcav1alpha1.ScheduleSkipInvalid, "the schedule has no spending ceiling: set spec.budget or spec.spend, or a daily budget on the tenant")
		return false
	}

	// Concurrency: runs of this schedule that are still going.
	if len(st.Active) > 0 {
		switch s.Spec.ConcurrencyPolicy {
		case agentorcav1alpha1.ScheduleConcurrencyAllow:
		case agentorcav1alpha1.ScheduleConcurrencyReplace:
			if err := r.cancelActive(ctx, s); err != nil {
				log.Error(err, "cancelling the active runs for Replace")
				return true
			}
		default:
			r.skip(s, tick, agentorcav1alpha1.ScheduleSkipConcurrency,
				fmt.Sprintf("the previous run %s is still going", st.Active[0].Name))
			return false
		}
	}

	// The tenant's quotas, on every tick.
	if tenant != nil {
		if err := quota.Check(ctx, r.Client, quota.FromTenantConfig(tenant)); err != nil {
			var qe *quota.Error
			if errors.As(err, &qe) && !qe.Transient() {
				r.skip(s, tick, agentorcav1alpha1.ScheduleSkipQuota, qe.Message)
				return false
			}
			log.Error(err, "checking the tenant's quotas")
			return true
		}
	}

	if err := r.Create(ctx, child); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// Leave the cursor where it is: the next attempt finds the same tick
			// and tries again with the same name.
			log.Error(err, "creating the scheduled child", "name", child.GetName())
			if r.Recorder != nil {
				r.Recorder.Eventf(s, corev1.EventTypeWarning, "CreateFailed", "creating %s: %v", child.GetName(), err)
			}
			return true
		}
		// Created by an earlier reconcile or another replica: the tick is handled.
	} else if r.Recorder != nil {
		r.Recorder.Eventf(s, corev1.EventTypeNormal, "Started", "started %s for %s", child.GetName(), tick.Format(time.RFC3339))
	}

	t := metav1.NewTime(tick)
	st.LastScheduleTime = &t
	kind := "AgentRun"
	if s.Spec.WorkflowTemplateRef != "" {
		kind = "AgentWorkflow"
	}
	ref := corev1.ObjectReference{APIVersion: agentorcav1alpha1.GroupVersion.String(), Kind: kind, Namespace: child.GetNamespace(), Name: child.GetName()}
	if !slices.ContainsFunc(st.Active, func(o corev1.ObjectReference) bool { return o.Name == ref.Name && o.Kind == ref.Kind }) {
		st.Active = append(st.Active, ref)
	}
	return false
}

// cancelActive stops the schedule's active runs the way the UI's stop does:
// the run is failed, and the cancel signal reaches the state store so a running
// agent stops and its sandbox is released. The run is marked as replaced, so
// the stop does not count as a failure.
func (r *AgentScheduleReconciler) cancelActive(ctx context.Context, s *agentorcav1alpha1.AgentSchedule) error {
	var runs agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &runs, client.InNamespace(s.Namespace), client.MatchingLabels{schedule.LabelSchedule: s.Name}); err != nil {
		return err
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if !metav1.IsControlledBy(run, s) || runChildState(run) != childActive {
			continue
		}
		patch := client.MergeFrom(run.DeepCopy())
		a := run.Annotations
		if a == nil {
			a = map[string]string{}
		}
		a[annotationScheduleReplaced] = annotationTrue
		run.Annotations = a
		if err := r.Patch(ctx, run, patch); err != nil {
			return client.IgnoreNotFound(err)
		}
		now := metav1.NewTime(r.now())
		statusPatch := client.MergeFrom(run.DeepCopy())
		run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
		run.Status.LastRestartReason = "replaced by schedule " + s.Name
		run.Status.CompletionTime = &now
		if err := r.Status().Patch(ctx, run, statusPatch); err != nil {
			return client.IgnoreNotFound(err)
		}
		if r.StateStore != nil {
			id, _ := tenancy.TenantOf(run)
			_ = state.ForTenantIfEnabled(r.StateStore, id).SignalCancel(ctx, run.Namespace, run.Name)
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(s, corev1.EventTypeNormal, "Replaced", "cancelled run %s: concurrencyPolicy is Replace", run.Name)
		}
	}
	s.Status.Active = nil
	return nil
}

// targetError means the schedule's target is not usable, as opposed to a
// failure to find out.
type targetError struct{ msg string }

func (e *targetError) Error() string { return e.msg }

// scheduleAgents lists the agents a schedule runs.
func scheduleAgents(spec *agentorcav1alpha1.AgentScheduleSpec, template *agentorcav1alpha1.AgentWorkflow) []string {
	if spec.AgentRef != "" {
		return []string{spec.AgentRef}
	}
	var out []string
	if template != nil {
		for _, st := range template.Spec.Steps {
			if st.AgentRef != "" && !slices.Contains(out, st.AgentRef) {
				out = append(out, st.AgentRef)
			}
		}
	}
	return out
}

// buildChild builds the AgentRun or AgentWorkflow for a tick. It fails with a
// *targetError when the agent or template is gone.
func (r *AgentScheduleReconciler) buildChild(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, cs *schedule.Cron, tick time.Time) (client.Object, *agentorcav1alpha1.AgentWorkflow, error) {
	local := tick.In(cs.Location())
	om := metav1.ObjectMeta{
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
	}
	// The tenant comes from the schedule, never from anything a person typed.
	for _, k := range []string{tenancy.LabelTenantID, tenancy.LabelTenantName} {
		if v := s.Labels[k]; v != "" {
			om.Labels[k] = v
		}
	}

	if s.Spec.WorkflowTemplateRef != "" {
		var tpl agentorcav1alpha1.AgentWorkflow
		if err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.WorkflowTemplateRef}, &tpl); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil, &targetError{fmt.Sprintf("workflow template %q was not found in namespace %q", s.Spec.WorkflowTemplateRef, s.Namespace)}
			}
			return nil, nil, fmt.Errorf("reading workflow template: %w", err)
		}
		if tpl.Spec.Mode != agentorcav1alpha1.WorkflowModeTemplate {
			return nil, nil, &targetError{fmt.Sprintf("AgentWorkflow %q is not a template (spec.mode: template)", tpl.Name)}
		}
		params := make(map[string]string, len(s.Spec.Parameters))
		for k, v := range s.Spec.Parameters {
			params[k] = schedule.Substitute(v, s.Name, local)
		}
		wf := &agentorcav1alpha1.AgentWorkflow{
			ObjectMeta: om,
			Spec: agentorcav1alpha1.AgentWorkflowSpec{
				FromTemplate: &agentorcav1alpha1.WorkflowFromTemplate{Name: tpl.Name, Parameters: params},
			},
		}
		if err := controllerutil.SetControllerReference(s, wf, r.Scheme); err != nil {
			return nil, nil, err
		}
		return wf, &tpl, nil
	}

	var agent agentorcav1alpha1.Agent
	if err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.AgentRef}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, &targetError{fmt.Sprintf("agent %q was not found in namespace %q", s.Spec.AgentRef, s.Namespace)}
		}
		return nil, nil, fmt.Errorf("reading agent %q: %w", s.Spec.AgentRef, err)
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: om,
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
	if err := controllerutil.SetControllerReference(s, run, r.Scheme); err != nil {
		return nil, nil, err
	}
	return run, nil, nil
}

// SetupWithManager registers the reconciler.
func (r *AgentScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("agentschedule") //nolint:staticcheck // same recorder API as the other controllers
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.AgentSchedule{}).
		Owns(&agentorcav1alpha1.AgentRun{}).
		Owns(&agentorcav1alpha1.AgentWorkflow{}).
		Named("agentschedule").
		Complete(r)
}
