package apiserver

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	fleetTestSelector = "auto"
	fleetPath         = "/api/fleet"
)

func fleetCond(typ string, st metav1.ConditionStatus, msg string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: st, Message: msg}
}

func TestFleetHealthOf(t *testing.T) {
	agent := func(c ...metav1.Condition) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{Status: agentorcav1alpha1.AgentStatus{Conditions: c}}
	}
	dep := func(phase agentorcav1alpha1.AgentDeploymentPhase, failures int, c ...metav1.Condition) *agentorcav1alpha1.AgentDeployment {
		return &agentorcav1alpha1.AgentDeployment{Status: agentorcav1alpha1.AgentDeploymentStatus{Phase: phase, ConsecutiveFailures: failures, Conditions: c}}
	}
	running := agentorcav1alpha1.AgentDeploymentPhaseRunning
	tests := []struct {
		name string
		a    *agentorcav1alpha1.Agent
		d    *agentorcav1alpha1.AgentDeployment
		want string
	}{
		{"nothing reported", agent(), nil, healthUnknown},
		{"agent ready", agent(fleetCond("Ready", metav1.ConditionTrue, "")), nil, healthHealthy},
		{"agent not ready", agent(fleetCond("Ready", metav1.ConditionFalse, "no model")), nil, healthFailing},
		{"deployment running", agent(), dep(running, 0), healthHealthy},
		{"deployment failed", agent(), dep(agentorcav1alpha1.AgentDeploymentPhaseFailed, 0), healthFailing},
		{"deployment available false", agent(), dep(running, 0, fleetCond("Available", metav1.ConditionFalse, "")), healthFailing},
		{"deployment paused", agent(), dep(agentorcav1alpha1.AgentDeploymentPhasePaused, 0), healthDegraded},
		{"pod failures while running", agent(), dep(running, 2), healthDegraded},
		{"other condition false", agent(fleetCond("KnowledgeReady", metav1.ConditionFalse, "indexing")), dep(running, 0), healthDegraded},
		{"still creating", agent(), dep(agentorcav1alpha1.AgentDeploymentPhaseCreating, 0), healthUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetHealthOf(tc.a, tc.d)
			if got.Status != tc.want || got.Reason == "" {
				t.Fatalf("health = %+v, want status %s with a reason", got, tc.want)
			}
		})
	}
}

func TestPercentileNearestRank(t *testing.T) {
	if percentile95(nil) != nil {
		t.Fatal("no values must give nil")
	}
	vals := make([]float64, 20)
	for i := range vals {
		vals[i] = float64(20 - i) // unsorted: 20..1
	}
	if got := *percentile95(vals); got != 19 {
		t.Fatalf("p95 of 1..20 = %v, want 19", got)
	}
	if got := *percentile95([]float64{7}); got != 7 {
		t.Fatalf("p95 of one value = %v, want 7", got)
	}
}

func TestAggregateFleetRuns(t *testing.T) {
	now := time.Now()
	dur := func(s int) *time.Duration { d := time.Duration(s) * time.Second; return &d }
	live := []fleetRun{
		{key: "ns/a", agent: "ns/x", phase: "Succeeded", spend: 0.5, when: now, duration: dur(10)},
		{key: "ns/b", agent: "ns/x", phase: "Failed", spend: 0.25, when: now, duration: dur(30)},
		{key: "ns/c", agent: "ns/x", phase: "Running", spend: 0.25, when: now},
	}
	archived := []fleetRun{
		{key: "ns/a", agent: "ns/x", phase: "Succeeded", spend: 9, when: now, duration: dur(999)}, // duplicate of a live run
		{key: "ns/d", agent: "ns/x", phase: "Succeeded", spend: 1, when: now.Add(-time.Hour), duration: dur(20)},
		{key: "ns/old", agent: "ns/x", phase: "Failed", spend: 5, when: now.Add(-48 * time.Hour)}, // outside the window
	}
	by, total := aggregateFleetRuns(live, archived, now.Add(-24*time.Hour))
	st := by["ns/x"]
	if st.runs != 4 || st.finished != 3 || st.failed != 1 {
		t.Fatalf("counts = %+v, want 4 runs, 3 finished, 1 failed", st)
	}
	if total != 2 || st.spend != 2 {
		t.Fatalf("spend = %v / %v, want 2: the duplicate and the old run must not count", total, st.spend)
	}
	if got := *percentile95(st.durations); got != 30 {
		t.Fatalf("p95 = %v, want 30", got)
	}
}

func fleetServer(t *testing.T) (*UIServer, *fakeApprovals) {
	t.Helper()
	now := metav1.NewTime(time.Now())
	earlier := metav1.NewTime(time.Now().Add(-30 * time.Second))
	run := func(ns, name, agent string, phase agentorcav1alpha1.AgentRunPhase, spend string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: agent},
			Status: agentorcav1alpha1.AgentRunStatus{
				Phase: phase, SpendUSD: spend, StartTime: &earlier, CompletionTime: &now,
			},
		}
	}
	agent := func(ns, name, owner string) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: map[string]string{
				tenancy.AnnotationOwner: owner, AnnotationMaintainer: "Support",
			}},
			Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: fleetTestSelector},
		}
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		agent(insightsNS, "helper", "ui"),
		agent("team-b", "secret", "git"),
		&agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "helper-chat", Namespace: insightsNS},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "helper"},
			Status:     agentorcav1alpha1.AgentDeploymentStatus{Phase: agentorcav1alpha1.AgentDeploymentPhaseRunning},
		},
		&agentorcav1alpha1.ModelSelector{
			ObjectMeta: metav1.ObjectMeta{Name: fleetTestSelector, Namespace: insightsNS},
			Spec:       agentorcav1alpha1.ModelSelectorSpec{BudgetCap: &agentorcav1alpha1.BudgetCap{PerDay: "2.00"}},
		},
		run(insightsNS, "r1", "helper", agentorcav1alpha1.AgentRunPhaseSucceeded, "0.30"),
		run(insightsNS, "r2", "helper", agentorcav1alpha1.AgentRunPhaseFailed, "0.10"),
		run("team-b", "r3", "secret", agentorcav1alpha1.AgentRunPhaseSucceeded, "50"),
	).Build()
	ap := &fakeApprovals{rows: map[string]postgresql.Approval{
		"1": {ID: "1", Namespace: insightsNS, RunName: "r1", Status: "pending"},
		"2": {ID: "2", Namespace: "team-b", RunName: "r3", Status: "pending"},
	}}
	return &UIServer{crdClient: cl, approvals: ap}, ap
}

func fleetGet(t *testing.T, s *UIServer, id *TenantIdentity, path string) (int, fleetResponse) {
	t.Helper()
	rec := agentsReq(s, id, http.MethodGet, path, "")
	var out fleetResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding %s: %v", rec.Body, err)
		}
	}
	return rec.Code, out
}

func TestFleetIsTenantScopedAndAggregates(t *testing.T) {
	s, ap := fleetServer(t)
	id := &TenantIdentity{
		TenantID: "tnt_a", TenantName: "a", Namespaces: []string{insightsNS}, UserID: "ada",
		Roles: tenancy.RoleViewer.Expand(), BudgetPerDayUSD: "10",
	}
	code, got := fleetGet(t, s, id, fleetPath)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "helper" {
		t.Fatalf("only the caller's agent must show: %+v", got.Agents)
	}
	row := got.Agents[0]
	if row.RunCount != 2 || row.ErrorRate == nil || *row.ErrorRate != 0.5 {
		t.Fatalf("runs/error rate = %d / %v, want 2 / 0.5", row.RunCount, row.ErrorRate)
	}
	if row.P95DurationSeconds == nil || *row.P95DurationSeconds < 29 || *row.P95DurationSeconds > 31 {
		t.Fatalf("p95 = %v, want about 30s", row.P95DurationSeconds)
	}
	if row.Spend.USD < 0.399 || row.Spend.USD > 0.401 || got.TenantSpendUSD > 0.401 {
		t.Fatalf("spend = %v tenant %v, want 0.40 and no leak from team-b", row.Spend.USD, got.TenantSpendUSD)
	}
	if row.Spend.AgentBudgetUSD == nil || *row.Spend.AgentBudgetUSD != 2 || row.Spend.TenantBudgetUSD == nil || *row.Spend.TenantBudgetUSD != 10 {
		t.Fatalf("budgets = %v / %v, want 2 and 10", row.Spend.AgentBudgetUSD, row.Spend.TenantBudgetUSD)
	}
	if row.PendingApprovals != 1 || row.Owner != "ui" || !row.Published || row.Maintainer != "Support" || row.Health.Status != healthHealthy {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.LastEvalScore != nil {
		t.Fatalf("lastEvalScore must be null until evaluations exist")
	}
	if len(ap.lastList.Namespaces) != 1 || ap.lastList.Namespaces[0] != insightsNS || ap.lastList.Status != fleetPendingStatus {
		t.Fatalf("approvals query must be tenant scoped: %+v", ap.lastList)
	}

	// The seven day window scales budgets to seven days.
	_, week := fleetGet(t, s, id, fleetPath+"?window=7d")
	if week.Window != fleetWindow7d || *week.Agents[0].Spend.AgentBudgetUSD != 14 {
		t.Fatalf("7d window = %s, agent budget %v, want 14", week.Window, week.Agents[0].Spend.AgentBudgetUSD)
	}
	if code, _ := fleetGet(t, s, id, fleetPath+"?window=1y"); code != http.StatusBadRequest {
		t.Fatalf("unknown window: status %d, want 400", code)
	}
}

func TestFleetWithoutTenantSeesEverythingAndUsesArchive(t *testing.T) {
	s, _ := fleetServer(t)
	started := time.Now().Add(-time.Hour)
	done := started.Add(time.Minute)
	arch := &fakeCostArchive{rows: []postgresql.CostRow{
		{Name: "old", Namespace: insightsNS, AgentRef: "helper", Phase: "Failed", SpendUSD: "1", When: done, Started: &started, Completed: &done},
	}}
	s.costArchive = arch
	code, got := fleetGet(t, s, nil, fleetPath)
	if code != http.StatusOK || len(got.Agents) != 2 {
		t.Fatalf("status %d agents %d, want both agents", code, len(got.Agents))
	}
	if arch.got.Since == nil || len(arch.got.Namespaces) != 0 {
		t.Fatalf("archive query = %+v, want a window and no namespace filter", arch.got)
	}
	helper := got.Agents[0]
	if helper.RunCount != 3 || *helper.P95DurationSeconds != 60 {
		t.Fatalf("archived run must be counted: runs %d p95 %v", helper.RunCount, helper.P95DurationSeconds)
	}
}

func TestFleetTenantArchiveQueryIsScoped(t *testing.T) {
	s, _ := fleetServer(t)
	arch := &fakeCostArchive{}
	s.costArchive = arch
	id := &TenantIdentity{TenantID: "tnt_a", Namespaces: []string{insightsNS}, Roles: tenancy.RoleViewer.Expand()}
	fleetGet(t, s, id, fleetPath)
	if len(arch.got.Namespaces) != 1 || arch.got.Namespaces[0] != insightsNS {
		t.Fatalf("archive namespaces = %v, want the tenant's", arch.got.Namespaces)
	}
	other := &TenantIdentity{TenantID: "tnt_c", Roles: tenancy.RoleViewer.Expand()}
	arch.n = 0
	fleetGet(t, s, other, fleetPath)
	if arch.n != 0 {
		t.Fatal("a tenant without namespaces must not reach the archive")
	}
}
