package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	tenA = "tnt_aaaa"
	tenB = "tnt_bbbb"
)

func run(name string, phase agentorcav1alpha1.AgentRunPhase, spend string, labels map[string]string) *agentorcav1alpha1.AgentRun {
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels, CreationTimestamp: metav1.NewTime(time.Now())},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: phase, SpendUSD: spend},
	}
}

func fakeKube(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestCountActiveRunsCountsTasksAndScheduledRuns(t *testing.T) {
	kube := fakeKube(t,
		run("task", agentorcav1alpha1.AgentRunPhaseRunning, "", map[string]string{"agentorca.io/external-task": "true", tenancy.LabelTenantID: tenA}),
		run("scheduled", agentorcav1alpha1.AgentRunPhasePending, "", map[string]string{schedule.LabelSchedule: "digest", tenancy.LabelTenantID: tenA}),
		run("scheduled-new", "", "", map[string]string{schedule.LabelSchedule: "digest", tenancy.LabelTenantID: tenA}),
		run("scheduled-done", agentorcav1alpha1.AgentRunPhaseSucceeded, "", map[string]string{schedule.LabelSchedule: "digest", tenancy.LabelTenantID: tenA}),
		run("both-labels", agentorcav1alpha1.AgentRunPhaseRunning, "", map[string]string{"agentorca.io/external-task": "true", schedule.LabelSchedule: "x", tenancy.LabelTenantID: tenA}),
		run("another-tenant", agentorcav1alpha1.AgentRunPhaseRunning, "", map[string]string{schedule.LabelSchedule: "digest", tenancy.LabelTenantID: tenB}),
		run("by-hand", agentorcav1alpha1.AgentRunPhaseRunning, "", map[string]string{tenancy.LabelTenantID: tenA}),
	)
	n, err := CountActiveRuns(context.Background(), kube, []string{"team-a"}, tenA)
	if err != nil {
		t.Fatal(err)
	}
	// task, scheduled, scheduled-new and both-labels (once). Not the finished
	// run, another tenant's run, or a run nobody labelled as a task or schedule.
	if n != 4 {
		t.Fatalf("count = %d, want 4", n)
	}
}

func TestCheck(t *testing.T) {
	kube := fakeKube(t,
		run("a", agentorcav1alpha1.AgentRunPhaseRunning, "3.00", map[string]string{schedule.LabelSchedule: "d", tenancy.LabelTenantID: tenA}),
		run("b", agentorcav1alpha1.AgentRunPhaseSucceeded, "2.50", map[string]string{tenancy.LabelTenantID: tenA}),
		run("other", agentorcav1alpha1.AgentRunPhaseSucceeded, "100", map[string]string{tenancy.LabelTenantID: tenB}),
	)
	base := Limits{TenantName: "acme", TenantID: tenA, Namespaces: []string{"team-a"}}
	for name, tc := range map[string]struct {
		mod  func(*Limits)
		code int
	}{
		"no limits":                  {func(*Limits) {}, 0},
		"under the concurrent cap":   {func(l *Limits) { l.ConcurrentRuns = 2 }, 0},
		"at the concurrent cap":      {func(l *Limits) { l.ConcurrentRuns = 1 }, 429},
		"under the daily budget":     {func(l *Limits) { l.BudgetPerDayUSD = "10" }, 0},
		"over the daily budget":      {func(l *Limits) { l.BudgetPerDayUSD = "5" }, 402},
		"another tenant's spend":     {func(l *Limits) { l.TenantID, l.BudgetPerDayUSD = tenB, "50" }, 402},
		"a zero budget means none":   {func(l *Limits) { l.BudgetPerDayUSD = "0" }, 0},
		"no namespaces cannot count": {func(l *Limits) { l.ConcurrentRuns, l.Namespaces = 1, nil }, 500},
	} {
		t.Run(name, func(t *testing.T) {
			l := base
			tc.mod(&l)
			err := Check(context.Background(), kube, l)
			var qe *Error
			switch {
			case tc.code == 0 && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.code != 0 && !errors.As(err, &qe):
				t.Fatalf("allowed, want code %d", tc.code)
			case tc.code != 0 && qe.Code != tc.code:
				t.Fatalf("code %d, want %d (%s)", qe.Code, tc.code, qe.Message)
			case tc.code == 500 && !qe.Transient():
				t.Fatal("a 500 must be transient")
			}
		})
	}
}

func TestFromTenantConfig(t *testing.T) {
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			TenantID: tenA, AllowedNamespaces: []string{"x", "y"}, BudgetPerDayUSD: "7",
			RateLimit: &agentorcav1alpha1.TenantRateLimit{ConcurrentRuns: 3},
		},
	}
	l := FromTenantConfig(tc)
	if l.TenantName != "acme" || l.TenantID != tenA || len(l.Namespaces) != 2 || l.ConcurrentRuns != 3 || l.BudgetPerDayUSD != "7" {
		t.Fatalf("limits = %+v", l)
	}
}
