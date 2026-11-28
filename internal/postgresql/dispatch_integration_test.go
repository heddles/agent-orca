package postgresql

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDispatchDecisionsAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)
	plat := WithPlatform(context.Background())
	since := time.Now().Add(-time.Hour)

	d := DispatchDecision{ID: "d1", TenantID: tenantA, Source: "api", Method: "decided", AgentNamespace: "ns", Agent: "triage",
		Confidence: 0.9, CandidateCount: 2, CostUSD: 0.25, Latency: 40 * time.Millisecond,
		Candidates: []DispatchCandidate{{Namespace: "ns", Agent: "triage", Probability: 0.9}}}
	if err := s.InsertDispatchDecision(a, d); err != nil {
		t.Fatal(err)
	}
	// A refusal names no agent and has no candidates.
	if err := s.InsertDispatchDecision(a, DispatchDecision{ID: "d2", TenantID: tenantA, Source: "api", Method: "refused", CostUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDispatchDecision(a, DispatchDecision{ID: "d3", TenantID: tenantB, Source: "api", Method: "only"}); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("writing another tenant's decision: %v", err)
	}
	if err := s.InsertDispatchDecision(context.Background(), d); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped write: %v", err)
	}
	if err := s.InsertDispatchDecision(a, DispatchDecision{ID: "d4", TenantID: tenantA, Source: "api", Method: "named"}); err == nil {
		t.Error("an unknown method was stored")
	}

	if got, err := s.DispatchSpendSince(a, tenantA, since); err != nil || got != 0.75 {
		t.Errorf("own spend = %v, %v", got, err)
	}
	// Another tenant sees none of it, even asking for it by ID.
	if got, err := s.DispatchSpendSince(b, tenantA, since); err != nil || got != 0 {
		t.Errorf("another tenant's view = %v, %v", got, err)
	}
	if got, err := s.DispatchSpendSince(plat, tenantA, since); err != nil || got != 0.75 {
		t.Errorf("platform view = %v, %v", got, err)
	}
	if got, err := s.DispatchSpendSince(a, tenantA, time.Now().Add(time.Hour)); err != nil || got != 0 {
		t.Errorf("spend after the window = %v, %v", got, err)
	}
	// The database enforces the tenant even without the query's filter.
	var visible int
	if err := raw.QueryRow(`SELECT count(*) FROM dispatch_decisions`).Scan(&visible); err != nil || visible != 0 {
		t.Errorf("rows visible with no scope set = %d, %v", visible, err)
	}

	if n, err := s.PruneDispatchDecisions(b, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("another tenant pruned %d, %v", n, err)
	}
	if n, err := s.PruneDispatchDecisions(plat, since); err != nil || n != 0 {
		t.Errorf("pruned recent rows: %d, %v", n, err)
	}
	if n, err := s.PruneDispatchDecisions(plat, time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Errorf("pruned %d, %v", n, err)
	}
}
