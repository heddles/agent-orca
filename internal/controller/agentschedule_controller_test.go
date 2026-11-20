package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const schedTenant = "tnt_0198aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee"

// schedEnv is a reconciler on a fake client with a clock the test moves.
type schedEnv struct {
	t   *testing.T
	cl  client.Client
	r   *AgentScheduleReconciler
	now time.Time
}

func newSchedEnv(t *testing.T, start time.Time, objs ...client.Object) *schedEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agentorcav1alpha1.AgentSchedule{}, &agentorcav1alpha1.AgentRun{}).
		WithObjects(objs...).Build()
	e := &schedEnv{t: t, cl: cl, now: start}
	e.r = &AgentScheduleReconciler{Client: cl, Scheme: scheme, Now: func() time.Time { return e.now }}
	return e
}

func schedAgent() *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "digest-agent", Namespace: "team-a", Labels: map[string]string{tenancy.LabelTenantID: schedTenant}},
	}
}

func hourly(mods ...func(*agentorcav1alpha1.AgentSchedule)) *agentorcav1alpha1.AgentSchedule {
	s := &agentorcav1alpha1.AgentSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "digest", Namespace: "team-a", UID: types.UID("sched-uid"), Generation: 1,
			Labels: map[string]string{tenancy.LabelTenantID: schedTenant},
		},
		Spec: agentorcav1alpha1.AgentScheduleSpec{
			AgentRef: "digest-agent", Schedule: "0 * * * *", TimeZone: "UTC",
			Input:  "Digest for {{schedule.date}} ({{schedule.name}})",
			Budget: &agentorcav1alpha1.RunBudget{MaxUSD: "1.00"},
		},
	}
	for _, m := range mods {
		m(s)
	}
	return s
}

func (e *schedEnv) reconcile() ctrl.Result {
	e.t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "digest"}})
	if err != nil {
		e.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (e *schedEnv) schedule() *agentorcav1alpha1.AgentSchedule {
	e.t.Helper()
	var s agentorcav1alpha1.AgentSchedule
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "digest"}, &s); err != nil {
		e.t.Fatal(err)
	}
	return &s
}

func (e *schedEnv) runs() []agentorcav1alpha1.AgentRun {
	e.t.Helper()
	var l agentorcav1alpha1.AgentRunList
	if err := e.cl.List(context.Background(), &l, client.InNamespace("team-a")); err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func (e *schedEnv) finish(name string, phase agentorcav1alpha1.AgentRunPhase, at time.Time) {
	e.t.Helper()
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		e.t.Fatal(err)
	}
	ct := metav1.NewTime(at)
	run.Status.Phase = phase
	run.Status.CompletionTime = &ct
	if err := e.cl.Status().Update(context.Background(), &run); err != nil {
		e.t.Fatal(err)
	}
}

func utc(h, m int) time.Time { return time.Date(2026, 11, 16, h, m, 0, 0, time.UTC) }

func TestScheduleFirstReconcileSetsCursorAndStartsNothing(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent())
	res := e.reconcile()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs on first reconcile, want 0 (nothing fires retroactively)", n)
	}
	s := e.schedule()
	if s.Status.CursorTime == nil || !s.Status.CursorTime.Time.Equal(utc(9, 30)) {
		t.Fatalf("cursor = %v", s.Status.CursorTime)
	}
	if s.Status.NextScheduleTime == nil || !s.Status.NextScheduleTime.Time.Equal(utc(10, 0)) {
		t.Fatalf("next = %v", s.Status.NextScheduleTime)
	}
	if res.RequeueAfter != 30*time.Minute {
		t.Fatalf("requeue = %s", res.RequeueAfter)
	}
	if !meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionReady) {
		t.Fatalf("not Ready: %+v", s.Status.Conditions)
	}
}

func TestScheduleTickCreatesExactlyOneRun(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent())
	e.reconcile()

	e.now = utc(10, 0).Add(2 * time.Second)
	e.reconcile()
	runs := e.runs()
	if len(runs) != 1 {
		t.Fatalf("%d runs after the tick, want 1", len(runs))
	}
	run := runs[0]
	if want := schedule.RunName("digest", utc(10, 0)); run.Name != want {
		t.Errorf("name = %q, want %q", run.Name, want)
	}
	if run.Labels[schedule.LabelSchedule] != "digest" || run.Labels[tenancy.LabelTenantID] != schedTenant {
		t.Errorf("labels = %v", run.Labels)
	}
	if run.Annotations[schedule.AnnotationCreatedBy] != "schedule:team-a/digest" ||
		run.Annotations[schedule.AnnotationScheduledFor] != "2026-11-16T10:00:00Z" {
		t.Errorf("annotations = %v", run.Annotations)
	}
	if !metav1.IsControlledBy(&run, e.schedule()) {
		t.Errorf("run is not controlled by the schedule: %v", run.OwnerReferences)
	}
	if run.Spec.AgentRef != "digest-agent" || run.Spec.Input != "Digest for 2026-11-16 (digest)" {
		t.Errorf("spec = %+v", run.Spec)
	}
	if run.Spec.Budget == nil || run.Spec.Budget.MaxUSD != "1.00" || run.Spec.Budget.OnExceeded != agentorcav1alpha1.RunBudgetFail {
		t.Errorf("budget = %+v (a scheduled run must fail at its cap, not pause)", run.Spec.Budget)
	}
	s := e.schedule()
	if s.Status.LastScheduleTime == nil || !s.Status.LastScheduleTime.Time.Equal(utc(10, 0)) || len(s.Status.Active) != 1 {
		t.Fatalf("status = %+v", s.Status)
	}

	// A second reconcile, and one a minute later, create nothing more.
	e.reconcile()
	e.now = e.now.Add(time.Minute)
	e.reconcile()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs after repeated reconciles, want 1", n)
	}
}

func TestScheduleDeterministicNameSurvivesLostStatus(t *testing.T) {
	// The run exists but the status never recorded it (a crash between the two
	// writes, or a second replica): the same tick must not create another run.
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent())
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile()

	s := e.schedule()
	s.Status.LastScheduleTime = nil
	s.Status.Active = nil
	if err := e.cl.Status().Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
	if s := e.schedule(); s.Status.LastScheduleTime == nil || len(s.Status.Active) != 1 {
		t.Fatalf("the existing run was not adopted: %+v", s.Status)
	}
}

func TestScheduleOnlyTheLatestMissedTickStarts(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		d := int64(3 * 3600)
		s.Spec.StartingDeadlineSeconds = &d
	}), schedAgent())
	e.reconcile()
	e.now = utc(13, 5) // 10:00, 11:00, 12:00 and 13:00 passed while nobody looked
	e.reconcile()
	runs := e.runs()
	if len(runs) != 1 || runs[0].Name != schedule.RunName("digest", utc(13, 0)) {
		t.Fatalf("runs = %v, want only the 13:00 tick", runNames(runs))
	}
}

func TestScheduleMissedBeyondDeadlineIsSkipped(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		d := int64(600)
		s.Spec.StartingDeadlineSeconds = &d
	}), schedAgent())
	e.reconcile()
	e.now = utc(10, 40)
	e.reconcile()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs, want 0: the 10:00 tick is 40 minutes late against a 10 minute deadline", n)
	}
	s := e.schedule()
	if len(s.Status.RecentSkips) != 1 || s.Status.RecentSkips[0].Reason != agentorcav1alpha1.ScheduleSkipMissed {
		t.Fatalf("skips = %+v", s.Status.RecentSkips)
	}
	// The next tick still starts normally.
	e.now = utc(11, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs at the next tick, want 1", n)
	}
}

func TestScheduleForbidSkipsWhileARunIsActive(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent())
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile() // starts the 10:00 run, which stays Pending

	e.now = utc(11, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs, want 1: Forbid must skip 11:00 while 10:00 is active", n)
	}
	s := e.schedule()
	if len(s.Status.RecentSkips) != 1 || s.Status.RecentSkips[0].Reason != agentorcav1alpha1.ScheduleSkipConcurrency {
		t.Fatalf("skips = %+v", s.Status.RecentSkips)
	}
	if !s.Status.LastScheduleTime.Time.Equal(utc(11, 0)) {
		t.Fatalf("lastScheduleTime = %v, a skipped tick still moves the cursor", s.Status.LastScheduleTime)
	}
}

func TestScheduleAllowStartsAlongsideAnActiveRun(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.ConcurrencyPolicy = agentorcav1alpha1.ScheduleConcurrencyAllow
	}), schedAgent())
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile()
	e.now = utc(11, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != 2 {
		t.Fatalf("%d runs, want 2", n)
	}
	if n := len(e.schedule().Status.Active); n != 2 {
		t.Fatalf("%d active, want 2", n)
	}
}

func TestScheduleSuspendStartsNothingAndResumeDoesNotReplay(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Suspend = true }), schedAgent())
	e.reconcile()
	e.now = utc(12, 5)
	e.reconcile()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs while suspended", n)
	}
	s := e.schedule()
	if !meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionSuspended) || s.Status.NextScheduleTime != nil {
		t.Fatalf("status = %+v", s.Status)
	}

	// Resume: a spec change bumps the generation, which resets the cursor to now.
	s.Spec.Suspend = false
	s.Generation = 2
	if err := e.cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs right after resume: the skipped ticks must not be replayed", n)
	}
	e.now = utc(13, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs at the next tick after resume, want 1", n)
	}
}

func TestScheduleEditResetsCursor(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent())
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile()
	e.finish(schedule.RunName("digest", utc(10, 0)), agentorcav1alpha1.AgentRunPhaseSucceeded, utc(10, 5))

	// Edit at 12:30 although the cursor is at 10:00: 11:00 and 12:00 are not replayed.
	e.now = utc(12, 30)
	s := e.schedule()
	s.Spec.Input = "a different input"
	s.Generation = 2
	if err := e.cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	for _, r := range e.runs() {
		if r.Name != schedule.RunName("digest", utc(10, 0)) {
			t.Fatalf("unexpected run %s after an edit", r.Name)
		}
	}
}

func TestScheduleMissingAgentSkipsAndSetsCondition(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly()) // no agent
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs without an agent", n)
	}
	s := e.schedule()
	if !meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionTargetMissing) {
		t.Fatalf("conditions = %+v", s.Status.Conditions)
	}
	if len(s.Status.RecentSkips) != 1 || s.Status.RecentSkips[0].Reason != agentorcav1alpha1.ScheduleSkipTargetMissing {
		t.Fatalf("skips = %+v", s.Status.RecentSkips)
	}
}

func TestScheduleInvalidExpressionIsNotReady(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = "every day" }), schedAgent())
	res := e.reconcile()
	s := e.schedule()
	c := meta.FindStatusCondition(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "InvalidSchedule" {
		t.Fatalf("Ready = %+v", c)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("requeue = %s: a bad spec waits for an edit", res.RequeueAfter)
	}
}

func TestScheduleAccountsFailuresAndPrunesHistory(t *testing.T) {
	one := int32(1)
	e := newSchedEnv(t, utc(8, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.SuccessfulRunsHistoryLimit = &one
		s.Spec.FailedRunsHistoryLimit = &one
		s.Spec.ConcurrencyPolicy = agentorcav1alpha1.ScheduleConcurrencyAllow
	}), schedAgent())
	e.reconcile()
	outcomes := []agentorcav1alpha1.AgentRunPhase{
		agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseFailed,
		agentorcav1alpha1.AgentRunPhaseFailed, agentorcav1alpha1.AgentRunPhaseSucceeded,
		agentorcav1alpha1.AgentRunPhaseFailed,
	}
	for i, phase := range outcomes {
		tick := utc(9+i, 0)
		e.now = tick.Add(time.Second)
		e.reconcile()
		e.finish(schedule.RunName("digest", tick), phase, tick.Add(time.Minute))
		e.reconcile() // counts it
		e.reconcile() // prunes what the limits no longer keep
	}
	s := e.schedule()
	if s.Status.ConsecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1 (success, fail, fail, success, fail)", s.Status.ConsecutiveFailures)
	}
	if s.Status.LastSuccessfulTime == nil || !s.Status.LastSuccessfulTime.Time.Equal(utc(12, 1)) {
		t.Errorf("lastSuccessfulTime = %v", s.Status.LastSuccessfulTime)
	}
	var ok, failed int
	for _, r := range e.runs() {
		switch r.Status.Phase {
		case agentorcav1alpha1.AgentRunPhaseSucceeded:
			ok++
		case agentorcav1alpha1.AgentRunPhaseFailed:
			failed++
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("kept %d successful and %d failed runs, want 1 and 1 (%v)", ok, failed, runNames(e.runs()))
	}
	// The newest of each is the one that stays.
	for _, r := range e.runs() {
		if r.Name != schedule.RunName("digest", utc(12, 0)) && r.Name != schedule.RunName("digest", utc(13, 0)) {
			t.Errorf("kept %s, want the newest of each outcome", r.Name)
		}
	}
}

func TestScheduleDoesNotAdoptRunsItDoesNotOwn(t *testing.T) {
	stranger := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stranger", Namespace: "team-a", Labels: map[string]string{schedule.LabelSchedule: "digest"},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: "digest-agent", Input: "x"},
	}
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent(), stranger)
	e.reconcile()
	if n := len(e.schedule().Status.Active); n != 0 {
		t.Fatalf("a run with the label but another owner counted as active")
	}
}

func runNames(runs []agentorcav1alpha1.AgentRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.Name)
	}
	return out
}
