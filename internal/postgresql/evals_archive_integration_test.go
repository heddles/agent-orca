package postgresql

import (
	"context"
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func archivedRun(name string, labels map[string]string) *agentorcav1alpha1.AgentRun {
	l := map[string]string{tenancy.LabelTenantID: tenantA}
	maps.Copy(l, labels)
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: evalNSA, Labels: l},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "support", Input: "q"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "0.5"},
	}
}

// Runs of evaluation experiments are not production traffic: the history and
// cost queries leave them out unless asked.
func TestArchivedEvalRunsAreExcludedFromHistoryAndCosts(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	for _, run := range []*agentorcav1alpha1.AgentRun{
		archivedRun("prod-1", nil),
		archivedRun("prod-2", map[string]string{"agentorca.io/deployment": "chat"}),
		archivedRun("eval-run", map[string]string{tenancy.LabelEval: tenancy.LabelEvalValue, tenancy.LabelEvalExperiment: "7"}),
		archivedRun("eval-only-experiment-label", map[string]string{tenancy.LabelEvalExperiment: "7"}),
	} {
		if err := s.ArchiveRun(ctx, run, nil); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.QueryHistory(ctx, HistoryQuery{})
	if err != nil || page.Total != 2 {
		t.Fatalf("history total = %+v, %v", page, err)
	}
	for _, r := range page.Runs {
		if r.Name != "prod-1" && r.Name != "prod-2" {
			t.Errorf("history lists %s", r.Name)
		}
	}
	if page, _ := s.QueryHistory(ctx, HistoryQuery{IncludeEval: true}); page.Total != 4 {
		t.Errorf("with IncludeEval: %d, want 4", page.Total)
	}
	rows, err := s.QueryCostRows(ctx, CostQuery{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("cost rows = %d, %v", len(rows), err)
	}
	if rows, _ := s.QueryCostRows(ctx, CostQuery{IncludeEval: true}); len(rows) != 4 {
		t.Errorf("with IncludeEval: %d, want 4", len(rows))
	}
	if rows, _ := s.QueryCostRows(ctx, CostQuery{Deployment: "chat"}); len(rows) != 1 {
		t.Errorf("deployment filter: %d", len(rows))
	}
}
