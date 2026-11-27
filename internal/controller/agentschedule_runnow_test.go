package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// runNow makes the run the API makes for Run now: labelled for the schedule,
// not owned by it, and asking to be reported.
func (e *schedEnv) runNow(name string, mod func(*agentorcav1alpha1.AgentRun)) {
	e.t.Helper()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "team-a",
			Labels:      map[string]string{schedule.LabelSchedule: schedName, tenancy.LabelTenantID: schedTenant},
			Annotations: map[string]string{schedule.AnnotationCreatedBy: "sam", schedule.AnnotationReport: schedule.ReportRequested},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: "digest-agent"},
	}
	if mod != nil {
		mod(run)
	}
	if err := e.cl.Create(context.Background(), run); err != nil {
		e.t.Fatal(err)
	}
}

func (e *schedEnv) run(name string) agentorcav1alpha1.AgentRun {
	e.t.Helper()
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		e.t.Fatal(err)
	}
	return run
}

func bothOutcomes() *agentorcav1alpha1.ScheduleNotify {
	return &agentorcav1alpha1.ScheduleNotify{Channel: "ops", On: []string{"Succeeded", "Failed"}}
}

func TestARunNowRunIsReportedOnceWhenItFinishes(t *testing.T) {
	e, f := notifyEnv(t, bothOutcomes())
	e.runNow("digest-m-abc12", nil)
	e.reconcile()
	e.settle()
	if got := f.all(); len(got) != 0 {
		t.Fatalf("reported before it finished: %+v", got)
	}
	e.finish("digest-m-abc12", agentorcav1alpha1.AgentRunPhaseSucceeded, utc(8, 31))
	for range 3 {
		e.reconcile()
	}
	e.settle()
	got := f.all()
	if len(got) != 1 || got[0].msg.Run != "digest-m-abc12" || got[0].msg.Outcome != "Succeeded" {
		t.Fatalf("notifications = %+v, want one for the run", got)
	}
	a := e.run("digest-m-abc12").Annotations
	if a[schedule.AnnotationNotified] != bothOutcomes().Destination() || a[schedule.AnnotationReport] != schedule.ReportHandled {
		t.Errorf("annotations = %v", a)
	}
}

func TestARunNowRunIsReportedButNotCounted(t *testing.T) {
	e, f := notifyEnv(t, bothOutcomes())
	e.runNow("digest-m-bad", nil)
	e.finish("digest-m-bad", agentorcav1alpha1.AgentRunPhaseFailed, utc(8, 31))
	run := e.run("digest-m-bad")
	run.Status.SpendUSD = "3.00"
	if err := e.cl.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.settle()
	if got := f.all(); len(got) != 1 || got[0].msg.Outcome != "Failed" {
		t.Fatalf("notifications = %+v", got)
	}
	st := e.schedule().Status
	if st.ConsecutiveFailures != 0 || st.SpentTodayUSD != "0.0000" || len(st.Active) != 0 {
		t.Errorf("a Run now run was counted: %+v", st)
	}
}

func TestOnlyRunsThatAskAndMatchTheScheduleAreReported(t *testing.T) {
	for name, mod := range map[string]func(*agentorcav1alpha1.AgentRun){
		"made before this feature": func(r *agentorcav1alpha1.AgentRun) { delete(r.Annotations, schedule.AnnotationReport) },
		"another agent":            func(r *agentorcav1alpha1.AgentRun) { r.Spec.AgentRef = "other-agent" },
		"another tenant":           func(r *agentorcav1alpha1.AgentRun) { r.Labels[tenancy.LabelTenantID] = "tnt_other" },
		"another schedule":         func(r *agentorcav1alpha1.AgentRun) { r.Labels[schedule.LabelSchedule] = "weekly" },
	} {
		t.Run(name, func(t *testing.T) {
			e, f := notifyEnv(t, bothOutcomes())
			e.runNow("digest-m-x", mod)
			e.finish("digest-m-x", agentorcav1alpha1.AgentRunPhaseSucceeded, utc(8, 31))
			e.reconcile()
			e.settle()
			if got := f.all(); len(got) != 0 {
				t.Errorf("reported: %+v", got)
			}
		})
	}
}

// A later change to the events must not send reports about old runs.
func TestARunNowRunThatWasNotWantedIsNotReportedLater(t *testing.T) {
	e, f := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops"}) // Failed only
	e.runNow("digest-m-ok", nil)
	e.finish("digest-m-ok", agentorcav1alpha1.AgentRunPhaseSucceeded, utc(8, 31))
	e.reconcile()
	s := e.schedule()
	s.Spec.Result.Notify.On = []string{"Succeeded", "Failed"}
	if err := e.cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.settle()
	if got := f.all(); len(got) != 0 {
		t.Errorf("reported: %+v", got)
	}
}

func TestARunNowRunIsReportedOnceWhenTheCacheLags(t *testing.T) {
	e, f := notifyEnv(t, bothOutcomes())
	real := e.r.Client
	lag := &lagClient{Client: real}
	e.r.Client, e.r.APIReader = lag, real
	e.runNow("digest-m-lag", nil)
	e.finish("digest-m-lag", agentorcav1alpha1.AgentRunPhaseSucceeded, utc(8, 31))
	var snap agentorcav1alpha1.AgentRunList
	if err := real.List(context.Background(), &snap, client.InNamespace("team-a")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	lag.frozen = &snap
	e.reconcile()
	e.settle()
	if got := f.all(); len(got) != 1 {
		t.Fatalf("%d notifications, want exactly 1: %+v", len(got), got)
	}
}

// Nothing owns a Run now run, so its schedule must be woken by the label.
func TestAFinishedRunNowRunWakesItsSchedule(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "digest-m-1", Namespace: "team-a", CreationTimestamp: metav1.NewTime(time.Unix(0, 0)),
		Labels:      map[string]string{schedule.LabelSchedule: schedName},
		Annotations: map[string]string{schedule.AnnotationReport: schedule.ReportRequested},
	}}
	got := scheduleOfRequestedReport(context.Background(), run)
	if len(got) != 1 || got[0].Name != schedName || got[0].Namespace != "team-a" {
		t.Errorf("requests = %v", got)
	}
	run.Annotations[schedule.AnnotationReport] = schedule.ReportHandled
	if got := scheduleOfRequestedReport(context.Background(), run); len(got) != 0 {
		t.Errorf("requests for a handled run = %v", got)
	}
}
