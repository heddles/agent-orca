package postgresql

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRoutingEventsTenantIsolationAgainstPostgres proves that every read path
// of routing_events stays inside the caller's tenant when two tenants share a
// namespace, that the filters narrow within the tenant, and that the retention
// sweep (platform scope) still deletes across tenants under row-level security.
func TestRoutingEventsTenantIsolationAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := context.Background()
	a, b, plat := WithTenant(ctx, tenantA), WithTenant(ctx, tenantB), WithPlatform(ctx)
	now := time.Now().UTC()

	ev := func(id, agent, selector, errClass, fallback string, shadow bool, age time.Duration, cost float64) RoutingEvent {
		return RoutingEvent{DecisionID: id, Namespace: "shared", RunName: "r-" + id, Agent: agent, Selector: selector,
			Provider: "p", Model: "m", ErrorClass: errClass, FallbackFrom: fallback, IsShadow: shadow,
			CostUSD: cost, CreatedAt: now.Add(-age)}
	}
	if err := s.UpsertRoutingEvents(a, []RoutingEvent{
		ev("a-ok", "bot", "sel-1", "", "", false, time.Minute, 1),
		ev("a-err", "bot", "sel-1", "timeout", "", false, 2*time.Minute, 2),
		ev("a-fb", "other", "sel-2", "", "strong", false, 3*time.Minute, 3),
		ev("a-shadow", "bot", "sel-1", "", "", true, 4*time.Minute, 4),
		ev("a-old", "bot", "sel-1", "", "", false, 10*24*time.Hour, 5),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRoutingEvents(b, []RoutingEvent{ev("b-ok", "bot", "sel-1", "", "", false, 90*time.Second, 7)}); err != nil {
		t.Fatal(err)
	}
	ids := func(c context.Context, q RoutingEventQuery) []string {
		t.Helper()
		got, err := s.ListRoutingEvents(c, q)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(got))
		for i, e := range got {
			out[i] = e.DecisionID
		}
		return out
	}
	eq := func(name string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", name, got, want)
				return
			}
		}
	}

	// Cross-tenant reads return nothing, in the shared namespace, on every path.
	eq("B list", ids(b, RoutingEventQuery{Namespaces: []string{"shared"}}), "b-ok")
	eq("B asks for A's agent", ids(b, RoutingEventQuery{Agent: "other"}))
	eq("A list", ids(a, RoutingEventQuery{}), "a-ok", "a-err", "a-fb", "a-old")
	if got, _ := s.RoutingEventsForRun(b, "shared", "r-a-ok", 0); len(got) != 0 {
		t.Errorf("B reads A's run events: %+v", got)
	}
	if got, err := s.CostByModel(b, []string{"shared"}, now.Add(-time.Hour)); err != nil || len(got) != 1 || got[0].CostUSD != 7 {
		t.Errorf("B cost by model = %+v, %v; want only its own 7", got, err)
	}
	if got, err := s.CostByModel(a, nil, now.Add(-time.Hour)); err != nil || len(got) != 1 || got[0].CostUSD != 6 {
		t.Errorf("A cost by model = %+v, %v; want 6 (shadow and old excluded)", got, err)
	}
	eq("platform sees both tenants", ids(plat, RoutingEventQuery{Since: now.Add(-time.Hour)}), "a-ok", "b-ok", "a-err", "a-fb")
	if _, err := s.ListRoutingEvents(ctx, RoutingEventQuery{}); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("a call without a scope listed routing events: %v", err)
	}

	// Filters narrow within the tenant.
	eq("agent", ids(a, RoutingEventQuery{Agent: "other"}), "a-fb")
	eq("selector", ids(a, RoutingEventQuery{Selector: "sel-2"}), "a-fb")
	eq("outcome ok", ids(a, RoutingEventQuery{Outcome: RoutingOutcomeOK}), "a-ok", "a-old")
	eq("outcome error", ids(a, RoutingEventQuery{Outcome: RoutingOutcomeError}), "a-err")
	eq("outcome fallback", ids(a, RoutingEventQuery{Outcome: RoutingOutcomeFallback}), "a-fb")
	eq("shadow", ids(a, RoutingEventQuery{IncludeShadow: true, Since: now.Add(-time.Hour)}), "a-ok", "a-err", "a-fb", "a-shadow")
	eq("window", ids(a, RoutingEventQuery{Since: now.Add(-150 * time.Second), Until: now.Add(-30 * time.Second)}), "a-ok", "a-err")
	eq("namespace", ids(a, RoutingEventQuery{Namespaces: []string{"elsewhere"}}))
	eq("limit", ids(a, RoutingEventQuery{Limit: 2}), "a-ok", "a-err")
	if _, err := s.ListRoutingEvents(a, RoutingEventQuery{Outcome: "bogus"}); !errors.Is(err, ErrInvalidRoutingQuery) {
		t.Errorf("unknown outcome: %v", err)
	}

	// Retention runs in the operator's platform scope against a role RLS applies
	// to: it deletes the old rows of every tenant and keeps the recent ones.
	if err := s.UpsertRoutingEvents(b, []RoutingEvent{ev("b-old", "bot", "sel-1", "", "", false, 9*24*time.Hour, 1)}); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.AsPlatform().RunRoutingEventsRetention(rctx, 7*24*time.Hour, time.Hour) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := s.ListRoutingEvents(plat, RoutingEventQuery{Since: now.Add(-30 * 24 * time.Hour)}); len(got) == 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eq("after retention, A", ids(a, RoutingEventQuery{}), "a-ok", "a-err", "a-fb")
	eq("after retention, B", ids(b, RoutingEventQuery{}), "b-ok")
}
