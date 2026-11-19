package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeExperimentStore keeps experiments per tenant, as the store's scope does.
// The rules it follows (the active-experiment cap, cancel) are the real
// store's, which is tested against PostgreSQL in internal/postgresql.
type fakeExperimentStore struct {
	*fakeEvalStore
	exps     map[int64]*postgresql.EvalExperiment
	results  map[int64][]postgresql.EvalResult
	rows     map[int64][]compare.Row
	spend    postgresql.EvalSpend
	gotSpend postgresql.EvalSpendQuery
}

func newFakeExperimentStore(base *fakeEvalStore) *fakeExperimentStore {
	return &fakeExperimentStore{fakeEvalStore: base, exps: map[int64]*postgresql.EvalExperiment{}, results: map[int64][]postgresql.EvalResult{}}
}

func (f *fakeExperimentStore) CreateEvalExperiment(ctx context.Context, e postgresql.EvalExperiment, l postgresql.EvalExperimentLimits) (postgresql.EvalExperiment, error) {
	e.Normalize()
	if err := e.Validate(l); err != nil {
		return e, err
	}
	d, err := f.GetEvalDataset(ctx, e.DatasetID)
	if err != nil {
		return e, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	active := 0
	for _, o := range f.exps {
		if o.TenantID == d.TenantID && (o.Status == postgresql.ExperimentQueued || o.Status == postgresql.ExperimentRunning) {
			active++
		}
	}
	if active >= max(l.MaxActivePerTenant, 2) {
		return e, postgresql.ErrTooManyExperiments
	}
	f.next++
	e.ID, e.TenantID, e.DatasetVersion, e.Status, e.Namespace, e.Cases = f.next, d.TenantID, d.Version, postgresql.ExperimentQueued, d.Namespace, d.CaseCount
	f.exps[e.ID] = &e
	return e, nil
}

func (f *fakeExperimentStore) GetEvalExperiment(ctx context.Context, id int64) (postgresql.EvalExperiment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.exps[id]
	if !ok || !f.visible(ctx, e.TenantID) {
		return postgresql.EvalExperiment{}, postgresql.ErrEvalNotFound
	}
	return *e, nil
}

func (f *fakeExperimentStore) ListEvalExperiments(ctx context.Context, q postgresql.EvalExperimentQuery) ([]postgresql.EvalExperiment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []postgresql.EvalExperiment{}
	for id := f.next; id > 0; id-- {
		e, ok := f.exps[id]
		if !ok || !f.visible(ctx, e.TenantID) || (q.Before > 0 && id >= q.Before) {
			continue
		}
		if q.Namespaces != nil && !contains(q.Namespaces, e.Namespace) {
			continue
		}
		out = append(out, *e)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

func (f *fakeExperimentStore) RequestEvalCancel(ctx context.Context, id int64) (postgresql.EvalExperiment, error) {
	e, err := f.GetEvalExperiment(ctx, id)
	if err != nil {
		return e, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.exps[id]
	switch x.Status {
	case postgresql.ExperimentQueued:
		x.Status = postgresql.ExperimentCancelled
	case postgresql.ExperimentRunning:
		x.CancelRequested = true
	default:
		return *x, postgresql.ErrExperimentFinished
	}
	return *x, nil
}

func (f *fakeExperimentStore) ListEvalExperimentResults(ctx context.Context, id int64, q postgresql.EvalExperimentResultQuery) ([]postgresql.EvalResult, error) {
	if _, err := f.GetEvalExperiment(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []postgresql.EvalResult
	for _, r := range f.results[id] {
		if r.ID > q.After && (q.Arm == "" || q.Arm == r.Arm) {
			out = append(out, r)
		}
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeExperimentStore) QueryEvalSpend(_ context.Context, q postgresql.EvalSpendQuery) (postgresql.EvalSpend, error) {
	f.gotSpend = q
	return f.spend, nil
}

// ── fixture ─────────────────────────────────────────────────────────────────

type expFixture struct {
	*evalFixture
	exps *fakeExperimentStore
}

func newExpFixture(t *testing.T) *expFixture {
	t.Helper()
	f := newEvalFixture(t)
	exps := newFakeExperimentStore(f.store)
	f.s.evalExperiments = exps
	own := map[string]string{tenancy.LabelTenantID: isoA}
	for _, o := range []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: helperAgent, Namespace: sharedNS, Labels: own},
			Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", Tools: []string{"search"}}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: sharedNS, Labels: own}},
		&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: sharedNS, Labels: own}},
		&agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "cheap", Namespace: sharedNS, Labels: own}},
	} {
		if err := f.s.crdClient.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	return &expFixture{evalFixture: f, exps: exps}
}

// graded makes a dataset with one case and one grader.
func (f *expFixture) graded(name string) postgresql.EvalDataset {
	f.t.Helper()
	d := f.dataset(name)
	if rec := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID), `{"input":"secret question","expected":"secret answer"}`); rec.Code != http.StatusCreated {
		f.t.Fatalf("add case = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(f.builder(), http.MethodPut, fmt.Sprintf("/api/evals/datasets/%d/graders/has-answer", d.ID), containsRefund); rec.Code != http.StatusCreated {
		f.t.Fatalf("add grader = %d %s", rec.Code, rec.Body)
	}
	return d
}

func expBody(datasetID int64, arms string) string {
	return fmt.Sprintf(`{"datasetId":%d,"name":"v2 vs v1","arms":%s,"budgetUsd":2,"runBudgetUsd":0.5}`, datasetID, arms)
}

const helperAgent = "helper"

const twoModelArms = `[{"label":"base","agent":"helper"},{"label":"cheap","agent":"helper","modelProvider":"cheap"}]`

func (f *expFixture) create() (postgresql.EvalDataset, experimentView) {
	f.t.Helper()
	d := f.graded(fmt.Sprintf("ds-%d", len(f.exps.exps)))
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms))
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	return d, evalDecode[experimentView](f.t, rec)
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestExperiments_CreateIsAuditedWithoutText(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("support-core")
	before := f.auditCount()
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	got := evalDecode[experimentView](t, rec)
	if got.Status != postgresql.ExperimentQueued || got.DatasetVersion != 2 || got.Namespace != sharedNS || got.ResultsTotal != 2 ||
		got.Repeats != 1 || got.Concurrency != 4 || got.CreatedBy != builderUser || len(got.Arms) != 2 {
		t.Fatalf("created = %+v", got)
	}
	if f.auditCount() != before+1 {
		t.Fatalf("audit rows = %d, want %d", f.auditCount(), before+1)
	}
	e := f.audit.events[len(f.audit.events)-1]
	if e.Kind != kindEvalExperiment || e.Action != postgresql.AuditCreate || e.Namespace != sharedNS || e.Name != fmt.Sprint(got.ID) {
		t.Errorf("audit = %+v", e)
	}
	for _, text := range []string{"secret question", "secret answer", helperAgent} {
		if strings.Contains(e.Summary, text) {
			t.Errorf("audit summary %q holds %q", e.Summary, text)
		}
	}
	// Reading it back, listing it and its results.
	viewer := idFor(tenancy.RoleViewer, isoA)
	if rec := f.do(viewer, http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d", got.ID), ""); rec.Code != http.StatusOK {
		t.Errorf("get = %d", rec.Code)
	}
	list := evalDecode[struct{ Experiments []experimentView }](t, f.do(viewer, http.MethodGet, "/api/evals/experiments", ""))
	if len(list.Experiments) != 1 || list.Experiments[0].ID != got.ID {
		t.Errorf("list = %+v", list)
	}
	if rec := f.do(viewer, http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d/results", got.ID), ""); rec.Code != http.StatusOK {
		t.Errorf("results = %d %s", rec.Code, rec.Body)
	}
}

func TestExperiments_Roles(t *testing.T) {
	f := newExpFixture(t)
	d, e := f.create()
	body := expBody(d.ID, twoModelArms)
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		id := idFor(role, isoA)
		if rec := f.do(id, http.MethodPost, "/api/evals/experiments", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s creates: %d", role, rec.Code)
		}
		if rec := f.do(id, http.MethodPost, fmt.Sprintf("/api/evals/experiments/%d/cancel", e.ID), ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s cancels: %d", role, rec.Code)
		}
		for _, path := range []string{"/api/evals/experiments", fmt.Sprintf("/api/evals/experiments/%d", e.ID), fmt.Sprintf("/api/evals/experiments/%d/results", e.ID)} {
			if rec := f.do(id, http.MethodGet, path, ""); rec.Code != http.StatusOK {
				t.Errorf("%s reads %s: %d", role, path, rec.Code)
			}
		}
	}
	if rec := f.do(nil, http.MethodGet, "/api/evals/experiments", ""); rec.Code != http.StatusOK && rec.Code != http.StatusForbidden && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no identity: %d", rec.Code)
	}
}

func TestExperiments_CancelIsAudited(t *testing.T) {
	f := newExpFixture(t)
	_, e := f.create()
	before := f.auditCount()
	rec := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/experiments/%d/cancel", e.ID), "")
	if rec.Code != http.StatusOK || evalDecode[experimentView](t, rec).Status != postgresql.ExperimentCancelled {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	if f.auditCount() != before+1 || f.audit.events[len(f.audit.events)-1].Kind != kindEvalExperiment {
		t.Errorf("audit rows = %d", f.auditCount())
	}
	// A finished experiment cannot be cancelled again.
	if rec := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/experiments/%d/cancel", e.ID), ""); rec.Code != http.StatusConflict {
		t.Errorf("second cancel = %d", rec.Code)
	}
	// A running one is marked and the runner stops it.
	f.exps.exps[e.ID].Status = postgresql.ExperimentRunning
	if got := evalDecode[experimentView](t, f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/experiments/%d/cancel", e.ID), "")); !got.CancelRequested {
		t.Errorf("a running experiment was not marked: %+v", got)
	}
}

func TestExperiments_ForeignTenantSeesNothing(t *testing.T) {
	f := newExpFixture(t)
	d, e := f.create()
	b := idFor(tenancy.RoleAdmin, isoB)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d", e.ID), ""},
		{http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d/results", e.ID), ""},
		{http.MethodPost, fmt.Sprintf("/api/evals/experiments/%d/cancel", e.ID), ""},
		{http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms)},
	} {
		if rec := f.do(b, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("tenant B %s %s = %d", c.method, c.path, rec.Code)
		}
	}
	list := evalDecode[struct{ Experiments []experimentView }](t, f.do(b, http.MethodGet, "/api/evals/experiments", ""))
	if len(list.Experiments) != 0 {
		t.Errorf("tenant B lists %d", len(list.Experiments))
	}
	if got := f.exps.exps[e.ID]; got.Status != postgresql.ExperimentQueued {
		t.Errorf("tenant B changed the experiment: %+v", got)
	}
}

func TestExperiments_ANamespaceTheCallerMayNotUseIsNotFound(t *testing.T) {
	f := newExpFixture(t)
	_, e := f.create()
	f.exps.exps[e.ID].Namespace = "elsewhere"
	if rec := f.do(f.builder(), http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d", e.ID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("get = %d", rec.Code)
	}
	list := evalDecode[struct{ Experiments []experimentView }](t, f.do(f.builder(), http.MethodGet, "/api/evals/experiments", ""))
	if len(list.Experiments) != 0 {
		t.Errorf("list = %d", len(list.Experiments))
	}
}

func TestExperiments_MissingReferencesAre422WithTheTemplatePayload(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("needs")
	// The agent names a Tool the workspace does not have, and a model arm
	// names a provider it does not have.
	if err := f.s.crdClient.Create(context.Background(), &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "needy", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", Tools: []string{"ghost-tool"}},
	}); err != nil {
		t.Fatal(err)
	}
	arms := `[{"label":"base","agent":"helper"},{"label":"cand","agent":"needy","modelProvider":"nope"}]`
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, arms))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Error   string          `json:"error"`
		Missing []missingArmRef `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]missingArmRef{}
	for _, m := range got.Missing {
		byName[m.Kind+"/"+m.Name] = m
	}
	tool, provider := byName["Tool/ghost-tool"], byName["ModelProvider/nope"]
	if tool.Arm != "cand" || tool.Namespace != sharedNS || len(tool.Alternatives) != 1 || tool.Alternatives[0] != "search" ||
		provider.Arm != "cand" || len(provider.Alternatives) != 1 || provider.Alternatives[0] != "cheap" {
		t.Errorf("missing = %+v", got.Missing)
	}
	if len(f.exps.exps) != 0 {
		t.Error("an experiment with gaps was queued")
	}
	// An agent that does not exist, and one that is another tenant's, are the same 422.
	for _, arms := range []string{
		`[{"label":"base","agent":"helper"},{"label":"cand","agent":"ghost"}]`,
	} {
		if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, arms)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s", arms, rec.Code, rec.Body)
		}
	}
}

func TestExperiments_RevisionArmsNeedARevisionAndAMatchingHash(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("revs")
	// Revisions need PostgreSQL: without the store an arm that names one is refused.
	arms := `[{"label":"base","agent":"helper"},{"label":"old","agent":"helper","revision":3}]`
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, arms))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no revision store: %d %s", rec.Code, rec.Body)
	}
	revs := &fixedRevisions{}
	f.s.revisions = revs
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, arms)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown revision: %d %s", rec.Code, rec.Body)
	}
	revs.add(sharedNS, helperAgent, 3, `{"spec":{"modelSelectorRef":"default","tools":["search"],"systemPrompt":"old"}}`)
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, arms)); rec.Code != http.StatusCreated {
		t.Fatalf("known revision: %d %s", rec.Code, rec.Body)
	}
	stale := `[{"label":"base","agent":"helper"},{"label":"old","agent":"helper","revision":3,"hash":"` + strings.Repeat("a", 64) + `"}]`
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, stale)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("stale hash: %d %s", rec.Code, rec.Body)
	}
}

func TestExperiments_BadRequestsAndLimits(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("limits")
	for name, body := range map[string]string{
		"no dataset":    `{"arms":` + twoModelArms + `,"budgetUsd":1}`,
		"one arm":       expBody(d.ID, `[{"label":"a","agent":"helper"}]`),
		"no budget":     fmt.Sprintf(`{"datasetId":%d,"arms":%s}`, d.ID, twoModelArms),
		"over the cap":  fmt.Sprintf(`{"datasetId":%d,"arms":%s,"budgetUsd":26}`, d.ID, twoModelArms),
		"unknown field": fmt.Sprintf(`{"datasetId":%d,"arms":%s,"budgetUsd":1,"purpose":"gate"}`, d.ID, twoModelArms),
		"six repeats":   fmt.Sprintf(`{"datasetId":%d,"arms":%s,"budgetUsd":1,"repeats":6}`, d.ID, twoModelArms),
		"not json":      `{`,
	} {
		if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	// Arms with no label get arm1, arm2.
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, `[{"agent":"helper"},{"agent":"helper","modelProvider":"cheap"}]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unlabelled arms = %d %s", rec.Code, rec.Body)
	}
	if got := evalDecode[experimentView](t, rec); got.Arms[0].Label != "arm1" || got.Arms[1].Label != "arm2" {
		t.Errorf("arms = %+v", got.Arms)
	}
	// A dataset with no graders cannot be compared.
	bare := f.dataset("bare")
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(bare.ID, twoModelArms)); rec.Code != http.StatusConflict {
		t.Errorf("no graders = %d %s", rec.Code, rec.Body)
	}
	// Two experiments may run at once; the third is a 409.
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms)); rec.Code != http.StatusCreated {
		t.Fatalf("second = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms)); rec.Code != http.StatusConflict {
		t.Errorf("third = %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/api/evals/experiments/0", "/api/evals/experiments/x", "/api/evals/experiments/99", "/api/evals/experiments/1/nope"} {
		if rec := f.do(f.builder(), http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
}

func TestExperiments_503WhenOffOrWithoutAStore(t *testing.T) {
	f := newExpFixture(t)
	f.s.evalCfg.DisableExperiments = true
	if rec := f.do(f.builder(), http.MethodGet, "/api/evals/experiments", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("off: %d", rec.Code)
	}
	g := newEvalFixture(t)
	g.s.evals = nil
	if rec := g.do(g.builder(), http.MethodGet, "/api/evals/experiments", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: %d", rec.Code)
	}
}

// ── evaluation runs never reach the production views ────────────────────────

func evalLabelled(run *agentorcav1alpha1.AgentRun) *agentorcav1alpha1.AgentRun {
	run.Labels = map[string]string{tenancy.LabelEval: "true", tenancy.LabelEvalExperiment: "7", tenancy.LabelTenantID: isoA}
	return run
}

func TestEvalRunsAreLeftOutOfCosts(t *testing.T) {
	arch := &fakeCostArchive{}
	s := costsServer(t, arch,
		liveRun("prod", "ns", "agent-1", agentorcav1alpha1.AgentRunPhaseSucceeded, "0.5", costDay1),
		evalLabelled(liveRun("eval-1-0-1-0", "ns", "eval-7-base", agentorcav1alpha1.AgentRunPhaseSucceeded, "40", costDay1)),
		evalLabelled(liveRun("eval-1-0-1-1", "ns", "eval-7-base", agentorcav1alpha1.AgentRunPhaseFailed, "2", costDay2)),
	)
	exps := &fakeExperimentStore{spend: postgresql.EvalSpend{TotalUSD: 42, Experiments: 1}}
	s.evalSpend = exps
	got := getCosts(t, s, "?namespace=ns", context.Background())
	if got.TotalUSD != usd(0.5) || len(got.ByAgent) != 1 || got.ByAgent["agent-1"] != usd(0.5) || got.FailedRunUSD != usd(0) {
		t.Errorf("costs = %+v", got)
	}
	if arch.got.IncludeEval {
		t.Error("the archive query includes evaluation runs")
	}
	// The money is not lost: it has its own line.
	if got.Evaluations == nil || got.Evaluations.TotalUSD != usd(42) || got.Evaluations.Experiments != 1 {
		t.Errorf("evaluations line = %+v", got.Evaluations)
	}
	if exps.gotSpend.Namespaces == nil || exps.gotSpend.Namespaces[0] != "ns" {
		t.Errorf("evaluation spend query = %+v", exps.gotSpend)
	}
	// Asking for them puts them back, and the archive is asked for them too.
	all := getCosts(t, s, "?namespace=ns&includeEval=true", context.Background())
	if all.TotalUSD != usd(42.5) || !arch.got.IncludeEval {
		t.Errorf("with includeEval: total %s, archive %+v", all.TotalUSD, arch.got)
	}
}

func TestEvalRunsAreLeftOutOfTheRunList(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(
		liveRun("prod", "ns", "agent-1", agentorcav1alpha1.AgentRunPhaseSucceeded, "0.5", costDay1),
		evalLabelled(liveRun("eval-run", "ns", "eval-7-base", agentorcav1alpha1.AgentRunPhaseSucceeded, "1", costDay1)),
	).Build()
	s := &UIServer{crdClient: cl}
	names := func(query string) []string {
		req := httptest.NewRequest(http.MethodGet, "/api/runs"+query, nil)
		rec := httptest.NewRecorder()
		s.handleListRuns(rec, req)
		var out []struct{ Name string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		n := make([]string, 0, len(out))
		for _, r := range out {
			n = append(n, r.Name)
		}
		return n
	}
	if got := names(""); len(got) != 1 || got[0] != "prod" {
		t.Errorf("runs = %v", got)
	}
	if got := names("?includeEval=true"); len(got) != 2 {
		t.Errorf("with includeEval = %v", got)
	}
}

func TestWantsEvalReadsTheQueryParameter(t *testing.T) {
	for query, want := range map[string]bool{"": false, "?includeEval=true": true, "?includeEval=false": false, "?includeEval=1": false} {
		if got := wantsEval(httptest.NewRequest(http.MethodGet, "/api/runs/history"+query, nil)); got != want {
			t.Errorf("%q: %v", query, got)
		}
	}
}

// fixedRevisions is an in-memory revision store.
type fixedRevisions struct {
	rows map[string]postgresql.Revision
}

func (f *fixedRevisions) add(ns, name string, n int, object string) {
	if f.rows == nil {
		f.rows = map[string]postgresql.Revision{}
	}
	f.rows[fmt.Sprintf("%s/%s/%d", ns, name, n)] = postgresql.Revision{Revision: n, Action: postgresql.AuditUpdate, Object: json.RawMessage(object)}
}

func (f *fixedRevisions) AppendRevision(context.Context, postgresql.Revision, int) (postgresql.Revision, bool, error) {
	return postgresql.Revision{}, false, nil
}

func (f *fixedRevisions) ListRevisions(context.Context, postgresql.RevisionQuery) ([]postgresql.Revision, error) {
	return nil, nil
}

func (f *fixedRevisions) GetRevision(_ context.Context, q postgresql.RevisionQuery, n int) (postgresql.Revision, error) {
	if r, ok := f.rows[fmt.Sprintf("%s/%s/%d", q.Namespace, q.Name, n)]; ok {
		return r, nil
	}
	return postgresql.Revision{}, postgresql.ErrRevisionNotFound
}

func TestEvalRunsAreLeftOutOfTheFleet(t *testing.T) {
	s, _ := fleetServer(t)
	arch := &fakeCostArchive{}
	s.costArchive = arch
	ctx := context.Background()
	now := metav1.NewTime(time.Now())
	for _, o := range []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "eval-7-base", Namespace: insightsNS,
			Labels:      map[string]string{tenancy.LabelEvalExperiment: "7", tenancy.LabelEval: "true"},
			Annotations: map[string]string{tenancy.AnnotationOwner: "ui"}},
			Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: fleetTestSelector}},
		evalLabelled(&agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: "eval-run", Namespace: insightsNS},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: helperAgent},
			Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseFailed, SpendUSD: "99",
				StartTime: &now, CompletionTime: &now},
		}),
	} {
		if err := s.crdClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	id := &TenantIdentity{TenantID: "tnt_a", TenantName: "a", Namespaces: []string{insightsNS}, UserID: "ada", Roles: tenancy.RoleViewer.Expand()}
	code, got := fleetGet(t, s, id, fleetPath)
	if code != http.StatusOK || len(got.Agents) != 1 || got.Agents[0].Name != helperAgent {
		t.Fatalf("fleet = %d %+v", code, got.Agents)
	}
	row := got.Agents[0]
	if row.RunCount != 2 || *row.ErrorRate != 0.5 || got.TenantSpendUSD > 0.401 {
		t.Errorf("an evaluation run counted: runs %d, error rate %v, spend %v", row.RunCount, *row.ErrorRate, got.TenantSpendUSD)
	}
	if arch.n == 0 || arch.got.IncludeEval {
		t.Errorf("the fleet's archive query: %+v (%d calls)", arch.got, arch.n)
	}
}

// EvalSummaryRows returns the rows set in rows[id], as the real store reads them.
func (f *fakeExperimentStore) EvalSummaryRows(ctx context.Context, id int64) ([]compare.Row, error) {
	if _, err := f.GetEvalExperiment(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id], nil
}
