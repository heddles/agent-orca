package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func schedTenantConfig(mod func(*agentorcav1alpha1.TenantConfig)) *agentorcav1alpha1.TenantConfig {
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "agent-orca-system"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			TenantID: schedTenant, AuthMode: "issued", AllowedNamespaces: []string{"team-a"},
		},
	}
	if mod != nil {
		mod(tc)
	}
	return tc
}

type fakeAuditor struct {
	mu     sync.Mutex
	events []postgresql.AuditEvent
}

func (f *fakeAuditor) AppendAudit(_ context.Context, e postgresql.AuditEvent) (postgresql.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return e, nil
}

// startAt makes the schedule's first reconcile at 09:30 and its next tick 10:00.
func (e *schedEnv) tickAt10() {
	e.t.Helper()
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile()
}

func skipReasons(s *agentorcav1alpha1.AgentSchedule) []string {
	out := make([]string, 0, len(s.Status.RecentSkips))
	for _, k := range s.Status.RecentSkips {
		out = append(out, k.Reason)
	}
	return out
}

func TestScheduleAgentOutsideTheTenantAllowlistIsNotStarted(t *testing.T) {
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.AllowedAgents = []string{"some-other-agent"} })
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent(), tc)
	e.tickAt10()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs for an agent the tenant may not run", n)
	}
	s := e.schedule()
	if !meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionTargetMissing) {
		t.Fatalf("conditions = %+v", s.Status.Conditions)
	}
}

func TestScheduleAllowedAgentStarts(t *testing.T) {
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.AllowedAgents = []string{"digest-agent"} })
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent(), tc)
	e.tickAt10()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
}

func TestScheduleConcurrentRunQuotaRefusesTheTick(t *testing.T) {
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) {
		tc.Spec.RateLimit = &agentorcav1alpha1.TenantRateLimit{ConcurrentRuns: 1}
	})
	busy := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "task-1", Namespace: "team-a",
			Labels: map[string]string{"agentorca.io/external-task": "true", tenancy.LabelTenantID: schedTenant},
		},
		Spec:   agentorcav1alpha1.AgentRunSpec{AgentRef: "digest-agent", Input: "x"},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent(), tc, busy)
	e.tickAt10()
	for _, r := range e.runs() {
		if r.Name != "task-1" {
			t.Fatalf("a run %s was created over the tenant's concurrent-run cap", r.Name)
		}
	}
	s := e.schedule()
	if got := skipReasons(s); len(got) != 1 || got[0] != agentorcav1alpha1.ScheduleSkipQuota {
		t.Fatalf("skips = %v", got)
	}
}

func TestScheduleDailyTenantBudgetRefusesTheTick(t *testing.T) {
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.BudgetPerDayUSD = "5.00" })
	spent := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "yesterdays-big-spender", Namespace: "team-a", Labels: map[string]string{tenancy.LabelTenantID: schedTenant},
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec:   agentorcav1alpha1.AgentRunSpec{AgentRef: "digest-agent", Input: "x"},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "6.50"},
	}
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent(), tc, spent)
	e.tickAt10()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs, want only the pre-existing one", n)
	}
	if got := skipReasons(e.schedule()); len(got) != 1 || got[0] != agentorcav1alpha1.ScheduleSkipQuota {
		t.Fatalf("skips = %v", got)
	}
}

func TestScheduleQuotasOfAnotherTenantDoNotApply(t *testing.T) {
	// Two tenants share a namespace; the other one's runs hold its own slots.
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) {
		tc.Spec.RateLimit = &agentorcav1alpha1.TenantRateLimit{ConcurrentRuns: 1}
	})
	other := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "task-b", Namespace: "team-a",
			Labels: map[string]string{"agentorca.io/external-task": "true", tenancy.LabelTenantID: "tnt_other"},
		},
		Spec:   agentorcav1alpha1.AgentRunSpec{AgentRef: "x", Input: "x"},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
	e := newSchedEnv(t, utc(9, 30), hourly(), schedAgent(), tc, other)
	e.tickAt10()
	if n := len(e.runs()); n != 2 {
		t.Fatalf("%d runs, want the other tenant's plus this schedule's", n)
	}
}

func TestScheduleWithoutACeilingStartsNothing(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Budget = nil }), schedAgent(), schedTenantConfig(nil))
	e.tickAt10()
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs from a schedule with no budget on a tenant with no daily budget", n)
	}
	if got := skipReasons(e.schedule()); len(got) != 1 || got[0] != agentorcav1alpha1.ScheduleSkipInvalid {
		t.Fatalf("skips = %v", got)
	}
}

func TestScheduleTenantDailyBudgetIsACeiling(t *testing.T) {
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.BudgetPerDayUSD = "50" })
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Budget = nil }), schedAgent(), tc)
	e.tickAt10()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
}

func TestScheduleBreakerStopsAfterConsecutiveFailuresAndResumes(t *testing.T) {
	e := newSchedEnv(t, utc(8, 30), hourly(), schedAgent())
	e.r.MaxConsecutiveFailures = 3
	e.reconcile()
	for i := range 3 {
		tick := utc(9+i, 0)
		e.now = tick.Add(time.Second)
		e.reconcile()
		e.finish(schedule.RunName("digest", tick), agentorcav1alpha1.AgentRunPhaseFailed, tick.Add(time.Minute))
		e.reconcile()
	}
	s := e.schedule()
	if s.Status.ConsecutiveFailures != 3 || !meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionDegraded) {
		t.Fatalf("status = %+v", s.Status)
	}
	if s.Status.NextScheduleTime != nil {
		t.Fatalf("a degraded schedule still has a next tick")
	}
	before := len(e.runs())
	e.now = utc(13, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != before {
		t.Fatalf("a degraded schedule started a run")
	}

	// A person resumes it: the API stamps the resume annotation.
	s = e.schedule()
	s.Annotations = map[string]string{schedule.AnnotationResumedAt: e.now.Add(time.Second).Format(time.RFC3339)}
	if err := e.cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	e.reconcile()
	s = e.schedule()
	if meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionDegraded) || s.Status.ConsecutiveFailures != 0 {
		t.Fatalf("not resumed: %+v", s.Status)
	}
	e.now = utc(14, 0).Add(time.Second)
	e.reconcile()
	if n := len(e.runs()); n != before+1 {
		t.Fatalf("%d runs after the resume, want %d", n, before+1)
	}
}

func TestScheduleBreakerIgnoresAStaleResumeAnnotation(t *testing.T) {
	e := newSchedEnv(t, utc(8, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Annotations = map[string]string{schedule.AnnotationResumedAt: utc(7, 0).Format(time.RFC3339)} // an old resume
	}), schedAgent())
	e.r.MaxConsecutiveFailures = 1
	e.reconcile()
	e.now = utc(9, 0).Add(time.Second)
	e.reconcile()
	e.finish(schedule.RunName("digest", utc(9, 0)), agentorcav1alpha1.AgentRunPhaseFailed, utc(9, 1))
	e.reconcile()
	e.reconcile()
	if !meta.IsStatusConditionTrue(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionDegraded) {
		t.Fatalf("an old resume annotation cleared the breaker")
	}
}

func TestScheduleSpendCapStopsTicksAuditsOnceAndRollsOver(t *testing.T) {
	aud := &fakeAuditor{}
	e := newSchedEnv(t, utc(8, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.Spend = &agentorcav1alpha1.ScheduleSpendCap{MaxUSDPerDay: "1.00"}
		s.Spec.ConcurrencyPolicy = agentorcav1alpha1.ScheduleConcurrencyAllow
	}), schedAgent())
	e.r.Audit = aud
	e.reconcile()
	e.now = utc(9, 0).Add(time.Second)
	e.reconcile() // starts the 09:00 run

	// It spends $1.20 and finishes: over the day's cap of $1.00.
	var run agentorcav1alpha1.AgentRun
	name := schedule.RunName("digest", utc(9, 0))
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		t.Fatal(err)
	}
	ct := metav1.NewTime(utc(9, 10))
	run.Status.Phase, run.Status.SpendUSD, run.Status.CompletionTime = agentorcav1alpha1.AgentRunPhaseSucceeded, "1.20", &ct
	if err := e.cl.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	e.now = utc(9, 15)
	e.reconcile()
	s := e.schedule()
	if !meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionBudgetExhausted) {
		t.Fatalf("conditions = %+v", s.Status.Conditions)
	}
	if s.Status.SpentTodayUSD != "1.2000" {
		t.Fatalf("spentTodayUSD = %q", s.Status.SpentTodayUSD)
	}

	e.now = utc(10, 0).Add(time.Second)
	res := e.reconcile()
	if n := len(e.runs()); n != 1 {
		t.Fatalf("%d runs: the 10:00 tick must be skipped over the cap", n)
	}
	if got := skipReasons(e.schedule()); len(got) != 1 || got[0] != agentorcav1alpha1.ScheduleSkipBudget {
		t.Fatalf("skips = %v", got)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Hour {
		t.Fatalf("requeue = %s", res.RequeueAfter)
	}
	e.reconcile()
	if len(aud.events) != 1 || aud.events[0].UserID != schedule.SystemUser || aud.events[0].Action != postgresql.AuditSuspend ||
		aud.events[0].TenantID != schedTenant || aud.events[0].Kind != "AgentSchedule" {
		t.Fatalf("audit events = %+v, want one system row", aud.events)
	}
	if s := e.schedule(); s.Spec.Suspend {
		t.Fatalf("the controller rewrote spec.suspend")
	}

	// The next UTC day: the window rolled, the cap no longer applies.
	e.now = time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC).Add(time.Second)
	e.reconcile()
	e.now = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC).Add(time.Second)
	e.reconcile()
	s = e.schedule()
	if meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionBudgetExhausted) {
		t.Fatalf("still exhausted after the window rolled: %+v", s.Status.Conditions)
	}
	// The cap rolled over: the 08:00 and 09:00 ticks of the new day both start.
	if n := len(e.runs()); n != 3 {
		t.Fatalf("%d runs on the next day, want 3", n)
	}
	if s.Status.SpentMonthUSD != "1.2000" {
		t.Fatalf("spentMonthUSD = %q: the month keeps counting", s.Status.SpentMonthUSD)
	}
}

func TestScheduleSpendSurvivesPruning(t *testing.T) {
	zero := int32(0)
	e := newSchedEnv(t, utc(8, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.Spend = &agentorcav1alpha1.ScheduleSpendCap{MaxUSDPerDay: "10"}
		s.Spec.SuccessfulRunsHistoryLimit = &zero
	}), schedAgent())
	e.reconcile()
	e.now = utc(9, 0).Add(time.Second)
	e.reconcile()
	name := schedule.RunName("digest", utc(9, 0))
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		t.Fatal(err)
	}
	ct := metav1.NewTime(utc(9, 5))
	run.Status.Phase, run.Status.SpendUSD, run.Status.CompletionTime = agentorcav1alpha1.AgentRunPhaseSucceeded, "2.50", &ct
	if err := e.cl.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	e.now = utc(9, 10)
	e.reconcile() // counts it
	e.reconcile() // prunes it
	if n := len(e.runs()); n != 0 {
		t.Fatalf("%d runs, the history limit of 0 prunes it", n)
	}
	if got := e.schedule().Status.SpentTodayUSD; got != "2.5000" {
		t.Fatalf("spentTodayUSD = %q: pruning lost the run's cost", got)
	}
}

func TestScheduleReplaceCancelsTheActiveRunWithoutCountingAFailure(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.ConcurrencyPolicy = agentorcav1alpha1.ScheduleConcurrencyReplace
	}), schedAgent())
	e.r.MaxConsecutiveFailures = 1
	e.reconcile()
	e.now = utc(10, 0).Add(time.Second)
	e.reconcile()
	e.now = utc(11, 0).Add(time.Second)
	e.reconcile()

	runs := e.runs()
	if len(runs) != 2 {
		t.Fatalf("runs = %v, want the replaced run and its replacement", runNames(runs))
	}
	for _, r := range runs {
		switch r.Name {
		case schedule.RunName("digest", utc(10, 0)):
			if r.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed || r.Status.CompletionTime == nil ||
				r.Annotations[annotationScheduleReplaced] == "" || r.Status.LastRestartReason == "" {
				t.Errorf("the replaced run = %+v / %v", r.Status, r.Annotations)
			}
		case schedule.RunName("digest", utc(11, 0)):
		default:
			t.Errorf("unexpected run %s", r.Name)
		}
	}
	e.reconcile()
	s := e.schedule()
	if s.Status.ConsecutiveFailures != 0 || meta.IsStatusConditionTrue(s.Status.Conditions, agentorcav1alpha1.ScheduleConditionDegraded) {
		t.Fatalf("a replaced run counted as a failure: %+v", s.Status)
	}
	if len(s.Status.Active) != 1 || s.Status.Active[0].Name != schedule.RunName("digest", utc(11, 0)) {
		t.Fatalf("active = %+v", s.Status.Active)
	}
}

func templateWorkflow(mode agentorcav1alpha1.WorkflowMode) *agentorcav1alpha1.AgentWorkflow {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "report-template", Namespace: "team-a", Labels: map[string]string{tenancy.LabelTenantID: schedTenant}},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{
			Mode:       mode,
			Parameters: []agentorcav1alpha1.WorkflowParameter{{Name: "day"}},
			Steps:      []agentorcav1alpha1.WorkflowStep{{Name: "write", AgentRef: "digest-agent", Input: "report for {{params.day}}"}},
		},
	}
	wf.Spec.BudgetCap = &agentorcav1alpha1.WorkflowBudgetCap{Total: "3.00"}
	return wf
}

func workflowSchedule() *agentorcav1alpha1.AgentSchedule {
	return hourly(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.AgentRef, s.Spec.Input, s.Spec.Budget = "", "", nil
		s.Spec.WorkflowTemplateRef = "report-template"
		s.Spec.Parameters = map[string]string{"day": "{{schedule.date}} by {{schedule.name}}"}
	})
}

func (e *schedEnv) workflows() []agentorcav1alpha1.AgentWorkflow {
	e.t.Helper()
	var l agentorcav1alpha1.AgentWorkflowList
	if err := e.cl.List(context.Background(), &l, client.InNamespace("team-a"), client.MatchingLabels{schedule.LabelSchedule: "digest"}); err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func TestScheduleWorkflowTemplateCreatesOneWorkflowPerTick(t *testing.T) {
	e := newSchedEnv(t, utc(9, 30), workflowSchedule(), schedAgent(), templateWorkflow(agentorcav1alpha1.WorkflowModeTemplate))
	e.tickAt10()
	e.reconcile()
	wfs := e.workflows()
	if len(wfs) != 1 {
		t.Fatalf("%d workflows, want 1", len(wfs))
	}
	wf := wfs[0]
	if wf.Name != schedule.RunName("digest", utc(10, 0)) || wf.Spec.FromTemplate == nil || wf.Spec.FromTemplate.Name != "report-template" {
		t.Fatalf("workflow = %+v", wf.Spec)
	}
	if got := wf.Spec.FromTemplate.Parameters["day"]; got != "2026-11-16 by digest" {
		t.Errorf("parameter day = %q", got)
	}
	if wf.Labels[schedule.LabelSchedule] != "digest" || wf.Labels[tenancy.LabelTenantID] != schedTenant ||
		wf.Annotations[schedule.AnnotationCreatedBy] != "schedule:team-a/digest" || !metav1.IsControlledBy(&wf, e.schedule()) {
		t.Errorf("metadata = %v %v %v", wf.Labels, wf.Annotations, wf.OwnerReferences)
	}
	if n := len(e.runs()); n != 0 {
		t.Fatalf("the schedule created %d AgentRuns directly for a template schedule", n)
	}
	if a := e.schedule().Status.Active; len(a) != 1 || a[0].Kind != "AgentWorkflow" {
		t.Fatalf("active = %+v", a)
	}
}

func TestScheduleWorkflowTemplateNeedsATemplate(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"missing":      {schedAgent()},
		"not template": {schedAgent(), templateWorkflow(agentorcav1alpha1.WorkflowModeWorkflow)},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSchedEnv(t, utc(9, 30), append(objs, workflowSchedule())...)
			e.tickAt10()
			if n := len(e.workflows()); n != 0 {
				t.Fatalf("%d workflows", n)
			}
			if !meta.IsStatusConditionTrue(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionTargetMissing) {
				t.Fatalf("conditions = %+v", e.schedule().Status.Conditions)
			}
		})
	}
}

func TestScheduleWorkflowTemplateAgentsMustBeAllowed(t *testing.T) {
	tc := schedTenantConfig(func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.AllowedAgents = []string{"nobody"} })
	e := newSchedEnv(t, utc(9, 30), workflowSchedule(), schedAgent(), templateWorkflow(agentorcav1alpha1.WorkflowModeTemplate), tc)
	e.tickAt10()
	if n := len(e.workflows()); n != 0 {
		t.Fatalf("%d workflows started with a step agent outside the allowlist", n)
	}
}

func TestScheduleWorkflowHistoryIsPrunedByTheScheduleLimits(t *testing.T) {
	one := int32(1)
	sched := workflowSchedule()
	sched.Spec.SuccessfulRunsHistoryLimit = &one
	sched.Spec.ConcurrencyPolicy = agentorcav1alpha1.ScheduleConcurrencyAllow
	e := newSchedEnv(t, utc(8, 30), sched, schedAgent(), templateWorkflow(agentorcav1alpha1.WorkflowModeTemplate))
	e.reconcile()
	for i := range 3 {
		tick := utc(9+i, 0)
		e.now = tick.Add(time.Second)
		e.reconcile()
		var wf agentorcav1alpha1.AgentWorkflow
		if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: schedule.RunName("digest", tick)}, &wf); err != nil {
			t.Fatal(err)
		}
		ct := metav1.NewTime(tick.Add(time.Minute))
		wf.Status.Phase, wf.Status.CompletionTime, wf.Status.TotalSpendUSD = agentorcav1alpha1.AgentWorkflowPhaseSucceeded, &ct, "0.50"
		if err := e.cl.Status().Update(context.Background(), &wf); err != nil {
			t.Fatal(err)
		}
		e.reconcile()
		e.reconcile()
	}
	wfs := e.workflows()
	if len(wfs) != 1 || wfs[0].Name != schedule.RunName("digest", utc(11, 0)) {
		names := make([]string, 0, len(wfs))
		for _, w := range wfs {
			names = append(names, w.Name)
		}
		t.Fatalf("workflows = %v, want only the newest", names)
	}
	if got := e.schedule().Status.SpentTodayUSD; got != "1.5000" {
		t.Fatalf("spentTodayUSD = %q, want the three workflows' $1.50", got)
	}
}
