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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/evals/gate"
	"github.com/heddles/agent-orca/internal/evals/runner"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeGateStore adds the gate tables to the fake experiment store. The rules it
// follows (the latest complete gate experiment counts) are the real store's,
// tested against PostgreSQL in internal/postgresql.
type fakeGateStore struct {
	*fakeExperimentStore
	gates          map[string]*postgresql.EvalGate
	gradersChanged bool
	failRead       bool
}

func newFakeGateStore() *fakeGateStore {
	return &fakeGateStore{fakeExperimentStore: newFakeExperimentStore(newFakeEvalStore()), gates: map[string]*postgresql.EvalGate{}}
}

func gateKey(ns, agent string) string { return ns + "/" + agent }

func (f *fakeGateStore) GetEvalGate(ctx context.Context, ns, agent string) (postgresql.EvalGate, error) {
	if f.failRead {
		return postgresql.EvalGate{}, errors.New("postgres is down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.gates[gateKey(ns, agent)]
	if !ok || !f.visible(ctx, g.TenantID) {
		return postgresql.EvalGate{}, postgresql.ErrEvalNotFound
	}
	return *g, nil
}

func (f *fakeGateStore) PutEvalGate(ctx context.Context, g postgresql.EvalGate) (postgresql.EvalGate, *postgresql.EvalGate, error) {
	g.Normalize()
	if err := g.Validate(); err != nil {
		return g, nil, err
	}
	d, err := f.GetEvalDataset(ctx, g.DatasetID)
	if err != nil {
		return g, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var old *postgresql.EvalGate
	if prev, ok := f.gates[gateKey(g.Namespace, g.Agent)]; ok {
		cp := *prev
		old = &cp
		g.ID = prev.ID
	} else {
		f.next++
		g.ID = f.next
	}
	g.TenantID = d.TenantID
	f.gates[gateKey(g.Namespace, g.Agent)] = &g
	return g, old, nil
}

func (f *fakeGateStore) DeleteEvalGate(ctx context.Context, ns, agent string) (postgresql.EvalGate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.gates[gateKey(ns, agent)]
	if !ok || !f.visible(ctx, g.TenantID) {
		return postgresql.EvalGate{}, postgresql.ErrEvalNotFound
	}
	delete(f.gates, gateKey(ns, agent))
	return *g, nil
}

func (f *fakeGateStore) LatestGateExperiment(ctx context.Context, ns, agent string) (postgresql.GateEvidence, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *postgresql.EvalExperiment
	for _, e := range f.exps {
		if e.Namespace == ns && e.Purpose == postgresql.PurposeGate && e.Status == postgresql.ExperimentComplete &&
			len(e.Arms) > 1 && e.Arms[1].Agent == agent && f.visible(ctx, e.TenantID) && (best == nil || e.ID > best.ID) {
			best = e
		}
	}
	if best == nil {
		return postgresql.GateEvidence{}, false, nil
	}
	return postgresql.GateEvidence{Experiment: *best, GradersChanged: f.gradersChanged}, true, nil
}

// gateFixture is the builder fixture with a UI-owned agent, a dataset of 40
// cases at version 7, an enabled gate, and the fake gate store.
type gateFixture struct {
	*builderFixture
	gates *fakeGateStore
	hash  string
	ds    postgresql.EvalDataset
}

const gateAgent = "pub-agent"

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	mine := map[string]string{tenancy.LabelTenantID: isoA}
	f := &gateFixture{builderFixture: newBuilderFixture(t, uiAgent(gateAgent, mine, tenancy.OwnerUI))}
	f.gates = newFakeGateStore()
	f.s.evalGates = f.gates
	f.s.evalExperiments = f.gates
	f.ds = postgresql.EvalDataset{ID: 1, TenantID: isoA, Namespace: sharedNS, Name: "support-core", Version: 7, CaseCount: 40}
	f.gates.datasets[1] = &f.ds
	var agent agentorcav1alpha1.Agent
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: gateAgent}, &agent); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.hash, err = runner.SpecHash(agent.Spec); err != nil {
		t.Fatal(err)
	}
	f.gates.gates[gateKey(sharedNS, gateAgent)] = &postgresql.EvalGate{
		ID: 90, TenantID: isoA, Namespace: sharedNS, Agent: gateAgent, DatasetID: 1, BaselineRevision: 1,
		Margin: 0.05, MinCases: 30, BudgetUSD: 5, Enabled: true, UpdatedBy: "root",
	}
	return f
}

// gateRows are n cases of the baseline and candidate arms: the candidate passes
// all, the baseline the first half (or the candidate only the first half when
// regress is set, and the baseline all).
func gateRows(regress bool) []compare.Row {
	const n = 40
	var rows []compare.Row
	for c := 1; c <= n; c++ {
		for _, arm := range []string{gateBaselineArm, gateCandidateArm} {
			pass := c <= n/2
			switch {
			case regress && arm == gateBaselineArm, !regress && arm == gateCandidateArm:
				pass = true
			}
			score := 0.0
			if pass {
				score = 1
			}
			rows = append(rows, compare.Row{CaseID: int64(c), Arm: arm, Phase: compare.PhaseSucceeded, CostComplete: true,
				Grades: []compare.Grade{{GraderID: 1, Score: score, Pass: pass, Status: compare.GradeOK, Weight: 1, Required: true}}})
		}
	}
	return rows
}

// evaluated records a finished gate experiment of the agent.
func (f *gateFixture) evaluated(rows []compare.Row, hash string, version int) int64 {
	f.gates.next++
	id := f.gates.next
	f.gates.exps[id] = &postgresql.EvalExperiment{
		ID: id, TenantID: isoA, Namespace: sharedNS, DatasetID: 1, DatasetVersion: version, Status: postgresql.ExperimentComplete,
		Purpose: postgresql.PurposeGate,
		Arms:    []postgresql.EvalArm{{Label: gateBaselineArm, Agent: gateAgent, Revision: 1}, {Label: gateCandidateArm, Agent: gateAgent, Hash: hash}},
	}
	f.gates.rows = map[int64][]compare.Row{}
	f.gates.rows[id] = rows
	return id
}

// serveAll sends a request through the builder and gate routes.
func (f *gateFixture) serveAll(id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	mux := http.NewServeMux()
	f.s.registerBuilderRoutes(mux)
	f.s.registerGateRoutes(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func (f *gateFixture) publish(role tenancy.Role, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.serveAll(f.as(role), http.MethodPost, "/api/agents/"+sharedNS+"/"+gateAgent+"/publish", body)
}

type gateBody struct {
	Error string `json:"error"`
	Gate  struct {
		Status       string `json:"status"`
		Reason       string `json:"reason"`
		ExperimentID int64  `json:"experimentId"`
		Required     struct {
			MinCases int     `json:"minCases"`
			Margin   float64 `json:"margin"`
		} `json:"required"`
		Best *struct {
			Cases    int     `json:"cases"`
			PassRate float64 `json:"passRate"`
		} `json:"best"`
	} `json:"gate"`
}

func (f *gateFixture) overrides() []postgresql.AuditEvent {
	var out []postgresql.AuditEvent
	for _, e := range f.audit.events {
		if e.Action == postgresql.AuditOverride {
			out = append(out, e)
		}
	}
	return out
}

const overrideBody = `{"overrideGate":{"reason":"urgent fix for the refund bug, reviewed by hand"}}`

func TestPublishGate_OffByDefaultAndWhenDisabled(t *testing.T) {
	// No gate configured at all: the store is wired but has no row.
	f := newGateFixture(t)
	delete(f.gates.gates, gateKey(sharedNS, gateAgent))
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Fatalf("no gate: publish = %d %s", rec.Code, rec.Body)
	}
	// A gate that is switched off does not block.
	f = newGateFixture(t)
	f.gates.gates[gateKey(sharedNS, gateAgent)].Enabled = false
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Fatalf("disabled gate: publish = %d %s", rec.Code, rec.Body)
	}
	// No gate store at all (no PostgreSQL): publishing is as it always was.
	f = newGateFixture(t)
	f.s.evalGates = nil
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Fatalf("no store: publish = %d %s", rec.Code, rec.Body)
	}
	if len(f.overrides()) != 0 {
		t.Errorf("override audited without a gate: %+v", f.overrides())
	}
}

func TestPublishGate_Missing(t *testing.T) {
	f := newGateFixture(t)
	rec := f.publish(tenancy.RoleBuilder, ``)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	b := decode[gateBody](t, rec)
	if b.Gate.Status != gate.StatusMissing || b.Gate.Required.MinCases != 30 || b.Gate.Reason == "" || !strings.Contains(b.Error, "gate") {
		t.Errorf("body = %+v", b)
	}
	if len(f.audit.events) != 0 {
		t.Errorf("a blocked publish left audit rows: %+v", f.audit.events)
	}
	var deps agentorcav1alpha1.AgentDeploymentList
	if err := f.c.List(context.Background(), &deps, client.InNamespace(sharedNS)); err != nil || countRunning(deps.Items, gateAgent) != 0 {
		t.Errorf("a deployment was made: %v %v", deps.Items, err)
	}
}

func TestPublishGate_Failed(t *testing.T) {
	f := newGateFixture(t)
	id := f.evaluated(gateRows(true), f.hash, 7)
	rec := f.publish(tenancy.RoleBuilder, ``)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	b := decode[gateBody](t, rec)
	if b.Gate.Status != gate.StatusFailed || b.Gate.ExperimentID != id || b.Gate.Best == nil || b.Gate.Best.Cases != 40 || b.Gate.Best.PassRate != 0.5 {
		t.Errorf("body = %+v", b)
	}
}

func TestPublishGate_Passed(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), f.hash, 7)
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	if len(f.overrides()) != 0 {
		t.Errorf("a passing publish was audited as an override: %+v", f.overrides())
	}
}

func TestPublishGate_DatasetEditStalesAPass(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), f.hash, 7)
	f.ds.Version = 8 // a case was edited or removed
	rec := f.publish(tenancy.RoleBuilder, ``)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	if b := decode[gateBody](t, rec); b.Gate.Status != gate.StatusStale || !strings.Contains(b.Gate.Reason, "dataset changed") {
		t.Errorf("body = %+v", b)
	}
	// Running the evaluation again on the new version makes it pass.
	f.evaluated(gateRows(false), f.hash, 8)
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Fatalf("after a new evaluation: publish = %d %s", rec.Code, rec.Body)
	}
}

func TestPublishGate_GraderChangeStalesAPass(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), f.hash, 7)
	f.gates.gradersChanged = true
	rec := f.publish(tenancy.RoleBuilder, ``)
	if rec.Code != http.StatusConflict || decode[gateBody](t, rec).Gate.Status != gate.StatusStale {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
}

func TestPublishGate_DifferentRevisionHashFails(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), strings.Repeat("ab", 32), 7)
	rec := f.publish(tenancy.RoleBuilder, ``)
	if rec.Code != http.StatusConflict {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	if b := decode[gateBody](t, rec); b.Gate.Status != gate.StatusStale || !strings.Contains(b.Gate.Reason, "different version of the agent") {
		t.Errorf("body = %+v", b)
	}
}

func TestPublishGate_TheLatestExperimentCountsNotTheBest(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), f.hash, 7) // a pass
	f.evaluated(gateRows(true), f.hash, 7)  // then a fail on the same version
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusConflict {
		t.Fatalf("publish = %d %s: the earlier pass must not count", rec.Code, rec.Body)
	}
}

func TestPublishGate_TooFewCasesAndOptionalBars(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), f.hash, 7)
	g := f.gates.gates[gateKey(sharedNS, gateAgent)]
	g.MinCases = 100
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "at least 100") {
		t.Errorf("min cases: %d %s", rec.Code, rec.Body)
	}
	g.MinCases = 30
	floor := 1.1
	g.MinPassRate = &floor
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusConflict {
		t.Errorf("min pass rate: %d %s", rec.Code, rec.Body)
	}
	g.MinPassRate = nil
	none := 0
	g.MaxRegressions = &none
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Errorf("no regressions allowed and none happened: %d %s", rec.Code, rec.Body)
	}
}

func TestPublishGate_FailsClosedWhenItCannotBeRead(t *testing.T) {
	f := newGateFixture(t)
	f.gates.failRead = true
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
}

func TestPublishGate_OverrideAsNonAdminIsForbidden(t *testing.T) {
	f := newGateFixture(t)
	for _, role := range []tenancy.Role{tenancy.RoleBuilder} {
		if rec := f.publish(role, overrideBody); rec.Code != http.StatusForbidden {
			t.Errorf("%s override = %d %s", role, rec.Code, rec.Body)
		}
	}
	// Also when the gate would have passed: asking is not allowed either.
	f.evaluated(gateRows(false), f.hash, 7)
	if rec := f.publish(tenancy.RoleBuilder, overrideBody); rec.Code != http.StatusForbidden {
		t.Errorf("builder override of a passing gate = %d", rec.Code)
	}
	if len(f.audit.events) != 0 || len(f.overrides()) != 0 {
		t.Errorf("refused overrides left audit rows: %+v", f.audit.events)
	}
}

func TestPublishGate_OverrideIsAuditedWithTheNumbers(t *testing.T) {
	f := newGateFixture(t)
	id := f.evaluated(gateRows(true), f.hash, 7)
	rec := f.publish(tenancy.RoleAdmin, overrideBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("override = %d %s", rec.Code, rec.Body)
	}
	ov := f.overrides()
	if len(ov) != 1 {
		t.Fatalf("override events = %+v", f.audit.events)
	}
	e := ov[0]
	if e.Kind != kindAgent || e.Namespace != sharedNS || e.Name != gateAgent || e.UserID != builderUser || len(e.Summary) > postgresql.MaxAuditSummaryBytes {
		t.Errorf("event = %+v", e)
	}
	for _, want := range []string{"support-core v7", fmt.Sprintf("experiment %d", id), "50%", "100%", "urgent fix for the refund bug"} {
		if !strings.Contains(e.Summary, want) {
			t.Errorf("summary %q lacks %q", e.Summary, want)
		}
	}
	// The normal publish row is written too, after the override.
	last := f.audit.events[len(f.audit.events)-1]
	if len(f.audit.events) != 2 || last.Action != postgresql.AuditCreate || last.Kind != kindAgentDeployment {
		t.Errorf("events = %+v", f.audit.events)
	}
}

func TestPublishGate_OverrideReasonAndSwitch(t *testing.T) {
	f := newGateFixture(t)
	for _, body := range []string{`{"overrideGate":{"reason":"short"}}`, `{"overrideGate":{}}`, `{"overrideGate":{"reason":"` + strings.Repeat("x", 501) + `"}}`} {
		if rec := f.publish(tenancy.RoleAdmin, body); rec.Code != http.StatusBadRequest {
			t.Errorf("reason %.20q = %d", body, rec.Code)
		}
	}
	f.s.evalCfg.DisallowGateOverride = true
	if rec := f.publish(tenancy.RoleAdmin, overrideBody); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "turned off") {
		t.Errorf("override with the switch off = %d %s", rec.Code, rec.Body)
	}
	// An admin without overrideGate is gated like anyone.
	if rec := f.publish(tenancy.RoleAdmin, ``); rec.Code != http.StatusConflict {
		t.Errorf("admin without override = %d", rec.Code)
	}
	if len(f.overrides()) != 0 {
		t.Errorf("events = %+v", f.audit.events)
	}
}

func TestPublishGate_AnAdminOverrideOfAPassingGateIsNotAnOverride(t *testing.T) {
	f := newGateFixture(t)
	f.evaluated(gateRows(false), f.hash, 7)
	if rec := f.publish(tenancy.RoleAdmin, overrideBody); rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	if len(f.overrides()) != 0 {
		t.Errorf("a gate that passed was audited as overridden: %+v", f.overrides())
	}
}

func TestUnpublishIsNeverGated(t *testing.T) {
	f := newGateFixture(t)
	if rec := f.publish(tenancy.RoleAdmin, overrideBody); rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	rec := f.serveAll(f.as(tenancy.RoleBuilder), http.MethodDelete, "/api/agents/"+sharedNS+"/"+gateAgent+"/publish", ``)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Errorf("unpublish = %d %s", rec.Code, rec.Body)
	}
}

// revisionOfAgent answers GetRevision with the agent as saved: revision 1 only.
type revisionOfAgent struct {
	revisionStore
	agent *agentorcav1alpha1.Agent
}

func (r revisionOfAgent) GetRevision(_ context.Context, _ postgresql.RevisionQuery, n int) (postgresql.Revision, error) {
	if n != 1 {
		return postgresql.Revision{}, postgresql.ErrRevisionNotFound
	}
	b, _ := json.Marshal(r.agent)
	return postgresql.Revision{Object: b, Action: postgresql.AuditCreate}, nil
}

func TestEvaluateQueuesAGateExperimentPinnedToTheAgentAsItIs(t *testing.T) {
	f := newGateFixture(t)
	var agent agentorcav1alpha1.Agent
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: gateAgent}, &agent); err != nil {
		t.Fatal(err)
	}
	f.s.revisions = revisionOfAgent{agent: &agent}
	if err := f.c.Create(context.Background(), refTargets(sharedNS)[0]); err != nil {
		t.Fatal(err)
	}
	f.gates.graders[1] = &postgresql.EvalGrader{ID: 1, TenantID: isoA, DatasetID: 1, Name: "has-answer", Kind: "contains", Config: []byte(`{"value":"x"}`), Weight: 1}
	path := "/api/agents/" + sharedNS + "/" + gateAgent + "/evaluate"
	if rec := f.serveAll(f.as(tenancy.RoleViewer), http.MethodPost, path, ``); rec.Code != http.StatusForbidden {
		t.Errorf("viewer = %d", rec.Code)
	}
	if rec := f.serveAll(f.as(tenancy.RoleBuilder), http.MethodGet, path, ``); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	before := len(f.audit.events)
	rec := f.serveAll(f.as(tenancy.RoleBuilder), http.MethodPost, path, ``)
	if rec.Code != http.StatusCreated {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	e := decode[experimentView](t, rec)
	if e.Purpose != postgresql.PurposeGate || len(e.Arms) != 2 || e.Arms[0].Label != gateBaselineArm || e.Arms[0].Revision != 1 ||
		e.Arms[1].Label != gateCandidateArm || e.Arms[1].Hash != f.hash || e.Arms[1].Revision != 0 || e.BudgetUSD != 5 || e.DatasetVersion != 7 {
		t.Errorf("experiment = %+v", e)
	}
	if len(f.audit.events) != before+1 || f.audit.events[before].Kind != kindEvalExperiment || !strings.Contains(f.audit.events[before].Summary, "publish gate") {
		t.Errorf("audit = %+v", f.audit.events)
	}
	// A baseline revision the agent does not have is refused before anything is queued.
	f.gates.gates[gateKey(sharedNS, gateAgent)].BaselineRevision = 9
	if rec := f.serveAll(f.as(tenancy.RoleBuilder), http.MethodPost, path, ``); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown baseline revision = %d %s", rec.Code, rec.Body)
	}
	// With no gate there is nothing to evaluate against.
	delete(f.gates.gates, gateKey(sharedNS, gateAgent))
	if rec := f.serveAll(f.as(tenancy.RoleBuilder), http.MethodPost, path, ``); rec.Code != http.StatusConflict {
		t.Errorf("no gate = %d %s", rec.Code, rec.Body)
	}
}

func TestGateConfig_AdminOnlyAuditedAndValidated(t *testing.T) {
	f := newGateFixture(t)
	delete(f.gates.gates, gateKey(sharedNS, gateAgent))
	f.gates.graders[1] = &postgresql.EvalGrader{ID: 1, TenantID: isoA, DatasetID: 1, Name: "has-answer", Kind: "contains", Weight: 1}
	path := "/api/evals/gates/" + sharedNS + "/" + gateAgent
	body := `{"datasetId":1,"baselineRevision":2,"minCases":40}`
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder} {
		if rec := f.serveAll(f.as(role), http.MethodPut, path, body); rec.Code != http.StatusForbidden {
			t.Errorf("%s PUT = %d", role, rec.Code)
		}
		if rec := f.serveAll(f.as(role), http.MethodDelete, path, ``); rec.Code != http.StatusForbidden {
			t.Errorf("%s DELETE = %d", role, rec.Code)
		}
	}
	for name, bad := range map[string]string{
		"no baseline":  `{"datasetId":1}`,
		"no dataset":   `{"baselineRevision":1}`,
		"huge margin":  `{"datasetId":1,"baselineRevision":1,"margin":1.5}`,
		"unknown":      `{"datasetId":1,"baselineRevision":1,"enable":true}`,
		"bad pass bar": `{"datasetId":1,"baselineRevision":1,"minPassRate":2}`,
	} {
		if rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodPut, path, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodPut, path, `{"datasetId":99,"baselineRevision":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown dataset = %d", rec.Code)
	}
	if len(f.audit.events) != 0 {
		t.Fatalf("refused calls were audited: %+v", f.audit.events)
	}
	rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodPut, path, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if g := decode[postgresql.EvalGate](t, rec); g.Margin != 0.05 || g.MinCases != 40 || !g.Enabled || g.BaselineRevision != 2 {
		t.Errorf("gate = %+v", g)
	}
	// Lowering it is audited with the old and new numbers.
	if rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodPut, path, `{"datasetId":1,"baselineRevision":2,"minCases":10}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT again = %d %s", rec.Code, rec.Body)
	}
	if len(f.audit.events) != 2 {
		t.Fatalf("events = %+v", f.audit.events)
	}
	e := f.audit.events[1]
	if e.Kind != kindEvalGate || e.Action != postgresql.AuditUpdate || e.Name != gateAgent || !strings.Contains(e.Summary, "min cases 40") || !strings.Contains(e.Summary, "min cases 10") {
		t.Errorf("update audit = %+v", e)
	}
	// Reading it back: a viewer sees the gate and where it stands.
	view := decode[gateView](t, f.serveAll(f.as(tenancy.RoleViewer), http.MethodGet, path, ``))
	if view.Gate == nil || view.Gate.MinCases != 10 || view.Decision == nil || view.Decision.Status != gate.StatusMissing || !view.OverrideAllowed {
		t.Errorf("view = %+v", view)
	}
	if rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodDelete, path, ``); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	if e := f.audit.events[2]; e.Action != postgresql.AuditDelete || e.Kind != kindEvalGate {
		t.Errorf("delete audit = %+v", e)
	}
	if view := decode[gateView](t, f.serveAll(f.as(tenancy.RoleViewer), http.MethodGet, path, ``)); view.Gate != nil {
		t.Errorf("view after delete = %+v", view)
	}
}

func TestGateConfig_RefusesAnLLMOnlyDataset(t *testing.T) {
	f := newGateFixture(t)
	f.gates.graders[1] = &postgresql.EvalGrader{ID: 1, TenantID: isoA, DatasetID: 1, Name: "judge", Kind: "llm", Weight: 1}
	rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodPut, "/api/evals/gates/"+sharedNS+"/"+gateAgent, `{"datasetId":1,"baselineRevision":1}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "only llm graders") {
		t.Errorf("llm-only = %d %s", rec.Code, rec.Body)
	}
	f.gates.graders[2] = &postgresql.EvalGrader{ID: 2, TenantID: isoA, DatasetID: 1, Name: "has-answer", Kind: "contains", Weight: 1}
	if rec := f.serveAll(f.as(tenancy.RoleAdmin), http.MethodPut, "/api/evals/gates/"+sharedNS+"/"+gateAgent, `{"datasetId":1,"baselineRevision":1}`); rec.Code != http.StatusOK {
		t.Errorf("with a deterministic grader = %d %s", rec.Code, rec.Body)
	}
}

func TestOverrideSummaryFitsTheAuditLimit(t *testing.T) {
	ds := postgresql.EvalDataset{Name: strings.Repeat("d", 200), Version: 3}
	reason := strings.Repeat("é", 500)
	s := overrideSummary(ds, gateDecisionFor(gate.StatusFailed), reason)
	if len(s) > postgresql.MaxAuditSummaryBytes || !strings.Contains(s, "reason: é") {
		t.Errorf("summary is %d bytes: %.80q", len(s), s)
	}
}

func gateDecisionFor(status string) gate.Decision {
	return gate.Decision{Status: status, Required: gate.Required{Margin: 0.05, MinCases: 30}, ExperimentID: 9}
}
