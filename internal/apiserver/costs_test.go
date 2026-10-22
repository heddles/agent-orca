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

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// fakeCostArchive serves canned archived rows and records the query it got.
type fakeCostArchive struct {
	rows []postgresql.CostRow
	err  error
	got  postgresql.CostQuery
	n    int
}

func (f *fakeCostArchive) QueryCostRows(_ context.Context, q postgresql.CostQuery) ([]postgresql.CostRow, error) {
	f.n++
	f.got = q
	return f.rows, f.err
}

func liveRun(name, ns, agent string, phase agentorcav1alpha1.AgentRunPhase, spend string, done time.Time) *agentorcav1alpha1.AgentRun {
	ct := metav1.NewTime(done)
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: ct},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: agent},
		Status: agentorcav1alpha1.AgentRunStatus{
			Phase: phase, SpendUSD: spend, RoutingDecisions: []agentorcav1alpha1.RoutingDecision{{Model: "gpt-4o"}},
		},
	}
	if postgresql.IsTerminalPhase(phase) {
		run.Status.CompletionTime = &ct
	}
	return run
}

func incompleteRun(r *agentorcav1alpha1.AgentRun) *agentorcav1alpha1.AgentRun {
	r.Status.Conditions = []metav1.Condition{{Type: "CostCapture", Status: metav1.ConditionFalse, Reason: "Incomplete"}}
	return r
}

func costsServer(t *testing.T, arch *fakeCostArchive, runs ...*agentorcav1alpha1.AgentRun) *UIServer {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t))
	for _, r := range runs {
		b = b.WithObjects(r)
	}
	s := &UIServer{crdClient: b.Build()}
	if arch != nil {
		s.costArchive = arch
	}
	return s
}

func getCosts(t *testing.T, s *UIServer, query string, ctx context.Context) costResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/costs"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleCosts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v\n%s", err, rec.Body.String())
	}
	return out
}

var costDay1 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
var costDay2 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// Regression for #77: byModel and byDay were always empty.
func TestHandleCosts_PopulatesByModelAndByDay(t *testing.T) {
	s := costsServer(t, nil,
		liveRun("a", "ns", "agent-1", agentorcav1alpha1.AgentRunPhaseSucceeded, "0.5", costDay1),
		liveRun("b", "ns", "agent-1", agentorcav1alpha1.AgentRunPhaseSucceeded, "0.25", costDay2),
	)
	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.TotalUSD != usd(0.75) {
		t.Fatalf("total = %s, want 0.7500", got.TotalUSD)
	}
	if got.ByModel["gpt-4o"] != usd(0.75) {
		t.Fatalf("byModel = %v, want gpt-4o 0.7500", got.ByModel)
	}
	if len(got.ByDay) != 2 || got.ByDay[0].Date != "2026-10-20" || got.ByDay[1].Date != "2026-10-21" {
		t.Fatalf("byDay = %+v, want two ascending days", got.ByDay)
	}
	if got.ByDay[0].USD != usd(0.5) || got.ByDay[0].ByOutcome["Succeeded"] != usd(0.5) {
		t.Fatalf("day 1 = %+v", got.ByDay[0])
	}
}

// Regression for #77: runs whose CRD was garbage-collected vanished from totals.
func TestHandleCosts_IncludesArchivedRuns(t *testing.T) {
	arch := &fakeCostArchive{rows: []postgresql.CostRow{
		{Name: "gone", Namespace: "ns", AgentRef: "agent-2", Phase: "Succeeded", SpendUSD: "2.0", Model: "claude", When: costDay1},
	}}
	s := costsServer(t, arch, liveRun("live", "ns", "agent-1", agentorcav1alpha1.AgentRunPhaseSucceeded, "1.0", costDay2))

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.TotalUSD != usd(3) {
		t.Fatalf("total = %s, want live 1.0 + archived 2.0", got.TotalUSD)
	}
	if got.ByAgent["agent-2"] != usd(2) || got.ByModel["claude"] != usd(2) {
		t.Fatalf("archived run not attributed: agent=%v model=%v", got.ByAgent, got.ByModel)
	}
	if arch.got.Namespaces[0] != "ns" {
		t.Fatalf("archive not scoped to the namespace: %+v", arch.got)
	}
}

// A finished run is both a live CRD and an archive row; it must count once.
func TestHandleCosts_DoesNotDoubleCountLiveAndArchived(t *testing.T) {
	arch := &fakeCostArchive{rows: []postgresql.CostRow{
		{Name: "r1", Namespace: "ns", AgentRef: "a", Phase: "Succeeded", SpendUSD: "9.0", When: costDay1},
	}}
	s := costsServer(t, arch, liveRun("r1", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "1.0", costDay1))

	got := getCosts(t, s, "", context.Background())

	if got.TotalUSD != usd(1) {
		t.Fatalf("total = %s, want 1.0000 (live copy wins, archive row ignored)", got.TotalUSD)
	}
}

// CostCapture=Incomplete runs are untracked, not $0.
func TestHandleCosts_UntrackedRuns(t *testing.T) {
	arch := &fakeCostArchive{rows: []postgresql.CostRow{
		{Name: "arch-lost", Namespace: "ns", AgentRef: "a", Phase: "Failed", CostIncomplete: true, When: costDay1},
	}}
	s := costsServer(t, arch,
		incompleteRun(liveRun("live-lost", "ns", "a", agentorcav1alpha1.AgentRunPhaseFailed, "", costDay2)),
		liveRun("ok", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "1.0", costDay2),
	)

	got := getCosts(t, s, "", context.Background())

	if got.UntrackedCount != 2 {
		t.Fatalf("untrackedCount = %d, want 2 (one live, one archived)", got.UntrackedCount)
	}
	want := map[string]bool{"ns/live-lost": true, "ns/arch-lost": true}
	for _, k := range got.UntrackedSample {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("untrackedSample = %v, missing %v", got.UntrackedSample, want)
	}
	if got.AvgRunUSD != usd(1) {
		t.Fatalf("avgRunUSD = %s; untracked runs must not drag the mean toward $0", got.AvgRunUSD)
	}
}

func TestHandleCosts_UntrackedSampleIsCapped(t *testing.T) {
	runs := make([]*agentorcav1alpha1.AgentRun, 0, untrackedSampleSize+5)
	for i := range untrackedSampleSize + 5 {
		runs = append(runs, incompleteRun(liveRun("r"+string(rune('a'+i)), "ns", "a", agentorcav1alpha1.AgentRunPhaseFailed, "", costDay1)))
	}
	got := getCosts(t, costsServer(t, nil, runs...), "", context.Background())

	if got.UntrackedCount != untrackedSampleSize+5 || len(got.UntrackedSample) != untrackedSampleSize {
		t.Fatalf("count=%d sample=%d, want %d and a sample capped at %d",
			got.UntrackedCount, len(got.UntrackedSample), untrackedSampleSize+5, untrackedSampleSize)
	}
}

func TestHandleCosts_OutcomeInFlightAndFailedSpend(t *testing.T) {
	s := costsServer(t, nil,
		liveRun("ok", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "1.0", costDay1),
		liveRun("bad", "ns", "a", agentorcav1alpha1.AgentRunPhaseFailed, "0.5", costDay1),
		liveRun("run", "ns", "a", agentorcav1alpha1.AgentRunPhaseRunning, "0.25", costDay2),
	)
	got := getCosts(t, s, "", context.Background())

	if got.ByOutcome["Succeeded"] != usd(1) || got.ByOutcome["Failed"] != usd(0.5) || got.ByOutcome["Running"] != usd(0.25) {
		t.Fatalf("byOutcome = %v", got.ByOutcome)
	}
	if got.FailedRunUSD != usd(0.5) {
		t.Fatalf("failedRunUSD = %s, want 0.5000", got.FailedRunUSD)
	}
	if got.InFlightUSD != usd(0.25) {
		t.Fatalf("inFlightUSD = %s, want 0.2500", got.InFlightUSD)
	}
	if got.AvgRunUSD != usd(0.75) {
		t.Fatalf("avgRunUSD = %s, want (1.0+0.5)/2 over finished runs", got.AvgRunUSD)
	}
	if got.TotalUSD != usd(1.75) {
		t.Fatalf("total = %s, want 1.7500", got.TotalUSD)
	}
}

func TestHandleCosts_EmptyIsWellFormed(t *testing.T) {
	got := getCosts(t, costsServer(t, nil), "", context.Background())

	if got.TotalUSD != usd(0) || got.AvgRunUSD != usd(0) || got.UntrackedCount != 0 {
		t.Fatalf("unexpected empty response: %+v", got)
	}
	if got.ByDay == nil || got.UntrackedSample == nil || got.ByModel == nil || got.ByAgent == nil || got.ByOutcome == nil {
		t.Fatalf("empty response must serialise collections as {} / [] (not null): %+v", got)
	}
}

func TestHandleCosts_ArchiveFailureDegradesToLiveRuns(t *testing.T) {
	arch := &fakeCostArchive{err: errors.New("postgres down")}
	s := costsServer(t, arch, liveRun("a", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "1.0", costDay1))

	got := getCosts(t, s, "", context.Background())

	if got.TotalUSD != usd(1) {
		t.Fatalf("total = %s, want the live run still counted", got.TotalUSD)
	}
	if !got.ArchiveUnavailable {
		t.Fatal("archiveUnavailable must be set when the archive could not be read")
	}
}

func TestHandleCosts_FiltersAreForwardedToArchive(t *testing.T) {
	arch := &fakeCostArchive{}
	s := costsServer(t, arch)

	getCosts(t, s, "?namespace=ns&deployment=chat&run=r1&days=7", context.Background())

	if arch.got.Deployment != "chat" || arch.got.Run != "r1" || arch.got.Since == nil {
		t.Fatalf("archive query = %+v, want deployment, run and since forwarded", arch.got)
	}
	if time.Since(*arch.got.Since) < 6*24*time.Hour || time.Since(*arch.got.Since) > 8*24*time.Hour {
		t.Fatalf("since = %v, want ~7 days ago", arch.got.Since)
	}
}

func TestHandleCosts_DaysWindowDropsOldLiveRuns(t *testing.T) {
	now := time.Now()
	s := costsServer(t, nil,
		liveRun("old", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "5.0", now.AddDate(0, 0, -30)),
		liveRun("new", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "1.0", now.AddDate(0, 0, -1)),
	)
	got := getCosts(t, s, "?days=7", context.Background())

	if got.TotalUSD != usd(1) {
		t.Fatalf("total = %s, want only the run inside the window", got.TotalUSD)
	}
}

// Tenants only see their own namespaces, and a tenant with none must never turn
// into an unfiltered archive query (empty namespaces mean "all" to the store).
func TestHandleCosts_TenantScoping(t *testing.T) {
	arch := &fakeCostArchive{rows: []postgresql.CostRow{
		{Name: "x", Namespace: "mine", AgentRef: "a", Phase: "Succeeded", SpendUSD: "1.0", When: costDay1},
	}}
	s := costsServer(t, arch,
		liveRun("mine-run", "mine", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "2.0", costDay1),
		liveRun("other-run", "other", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "50.0", costDay1),
	)

	tenant := &TenantIdentity{TenantName: "t", Namespaces: []string{"mine"}}
	got := getCosts(t, s, "?namespace=other", context.WithValue(context.Background(), tenantIdentityKey, tenant))
	if got.TotalUSD != usd(3) {
		t.Fatalf("tenant total = %s, want 3.0000 (own live + archive only)", got.TotalUSD)
	}
	if len(arch.got.Namespaces) != 1 || arch.got.Namespaces[0] != "mine" {
		t.Fatalf("archive namespaces = %v, want the tenant's [mine] despite ?namespace=other", arch.got.Namespaces)
	}

	arch.n = 0
	none := &TenantIdentity{TenantName: "empty"}
	got = getCosts(t, s, "", context.WithValue(context.Background(), tenantIdentityKey, none))
	if arch.n != 0 {
		t.Fatal("a tenant without namespaces must not query the archive")
	}
	if got.TotalUSD != usd(0) {
		t.Fatalf("namespace-less tenant total = %s, want 0", got.TotalUSD)
	}
}

func TestAggregateCosts_UnknownModelAndAverage(t *testing.T) {
	got := aggregateCosts([]costEntry{
		{key: "a/1", agent: "x", phase: "Succeeded", spend: 1, terminal: true, when: costDay1},
		{key: "a/2", agent: "x", phase: "Succeeded", spend: 3, terminal: true, when: costDay1, model: "m"},
	})
	if got.ByModel[unknownModel] != usd(1) || got.ByModel["m"] != usd(3) {
		t.Fatalf("byModel = %v", got.ByModel)
	}
	if got.AvgRunUSD != usd(2) {
		t.Fatalf("avg = %s, want 2.0000", got.AvgRunUSD)
	}
}

// The UI declares byDay as Array<{date, usd}>; the wire keys are part of that
// contract.
func TestHandleCosts_ByDayWireShape(t *testing.T) {
	s := costsServer(t, nil, liveRun("a", "ns", "a", agentorcav1alpha1.AgentRunPhaseSucceeded, "0.5", costDay1))
	req := httptest.NewRequest(http.MethodGet, "/api/costs", nil)
	rec := httptest.NewRecorder()
	s.handleCosts(rec, req)

	var raw struct {
		ByDay []map[string]any `json:"byDay"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.ByDay) != 1 || raw.ByDay[0]["date"] != "2026-10-20" || raw.ByDay[0]["usd"] != "0.5000" {
		t.Fatalf("byDay wire shape = %v, want [{date, usd, byOutcome}]", raw.ByDay)
	}
}
