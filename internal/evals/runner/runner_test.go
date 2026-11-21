package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	tenant   = "tnt_01935c7e-0000-7000-8000-00000000000a"
	otherTen = "tnt_01935c7e-0000-7000-8000-00000000000b"
	ns       = "team-a"
	user     = "ada"
)

func scopeOf(ctx context.Context) string {
	s, _ := postgresql.ScopeFrom(ctx)
	return s
}

// fakeStore is an in-memory Store. It insists on the scopes the real runner
// owes the database: the platform scope to claim, the experiment's own tenant
// for everything else.
type fakeStore struct {
	t  *testing.T
	mu sync.Mutex

	exps      map[int64]*postgresql.EvalExperiment
	cases     []postgresql.EvalCase
	graders   []postgresql.EvalGrader
	results   map[int64]map[slot]postgresql.EvalResult
	grades    map[int64]map[slot][]postgresql.EvalGrade
	revisions map[string]postgresql.Revision
	finished  map[int64]json.RawMessage

	// archives are the archived runs, by "namespace/name", for the trace.
	archives map[string]*postgresql.RunArchive
	getRuns  int
	// resultIDs numbers results, in the order they were recorded.
	resultIDs map[int64]map[int64]slot
	nextID    int64
	canary    map[int64]float64
	pending   map[int64]int
	purges    []time.Time
}

func newFakeStore(t *testing.T) *fakeStore {
	return &fakeStore{t: t, exps: map[int64]*postgresql.EvalExperiment{}, results: map[int64]map[slot]postgresql.EvalResult{},
		grades: map[int64]map[slot][]postgresql.EvalGrade{}, revisions: map[string]postgresql.Revision{}, finished: map[int64]json.RawMessage{},
		archives: map[string]*postgresql.RunArchive{}, resultIDs: map[int64]map[int64]slot{}, canary: map[int64]float64{}, pending: map[int64]int{}}
}

func (f *fakeStore) wantScope(ctx context.Context, id int64) {
	f.t.Helper()
	f.mu.Lock()
	e := f.exps[id]
	f.mu.Unlock()
	if e == nil {
		return
	}
	if got := scopeOf(ctx); got != e.TenantID {
		f.t.Errorf("a call about experiment %d ran in scope %q, want its tenant %q", id, got, e.TenantID)
	}
}

func (f *fakeStore) add(e postgresql.EvalExperiment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.Status == "" {
		e.Status = postgresql.ExperimentQueued
	}
	f.exps[e.ID] = &e
}

func (f *fakeStore) exp(id int64) postgresql.EvalExperiment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.exps[id]
}

func (f *fakeStore) ClaimEvalExperiments(ctx context.Context, _ string, _ time.Duration, _ int) ([]postgresql.EvalExperiment, error) {
	if got := scopeOf(ctx); got != postgresql.PlatformScope {
		f.t.Errorf("claim ran in scope %q, want the platform", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []postgresql.EvalExperiment
	for _, e := range f.exps {
		if e.Status == postgresql.ExperimentQueued || e.Status == postgresql.ExperimentRunning || e.Status == postgresql.ExperimentReview {
			c := *e
			c.ResultsDone = len(f.results[e.ID])
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) ActiveEvalExperiments(_ context.Context, ids []int64) (map[int64]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]bool{}
	for _, id := range ids {
		if e := f.exps[id]; e != nil && (e.Status == postgresql.ExperimentQueued || e.Status == postgresql.ExperimentRunning) {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeStore) StartEvalExperiment(ctx context.Context, id int64, arms []postgresql.EvalArm) error {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.exps[id]
	e.Status, e.Arms = postgresql.ExperimentRunning, arms
	return nil
}

func (f *fakeStore) ListEvalExperimentCases(ctx context.Context, id int64) ([]postgresql.EvalCase, error) {
	f.wantScope(ctx, id)
	return f.cases, nil
}

func (f *fakeStore) ListEvalExperimentResultKeys(ctx context.Context, id int64) ([]postgresql.EvalResultKey, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]postgresql.EvalResultKey, 0, len(f.results[id]))
	for s, r := range f.results[id] {
		out = append(out, postgresql.EvalResultKey{CaseID: s.caseID, Arm: s.arm, Repeat: s.repeat, Phase: r.Phase, CostUSD: r.CostUSD})
	}
	return out, nil
}

func (f *fakeStore) RecordEvalRunResult(ctx context.Context, id int64, r postgresql.EvalResult, grades []postgresql.EvalGrade) (bool, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	s := slot{*r.CaseID, r.Arm, r.Repeat}
	if f.results[id] == nil {
		f.results[id], f.grades[id] = map[slot]postgresql.EvalResult{}, map[slot][]postgresql.EvalGrade{}
	}
	if _, ok := f.results[id][s]; ok {
		return false, nil
	}
	f.results[id][s], f.grades[id][s] = r, grades
	f.nextID++
	if f.resultIDs[id] == nil {
		f.resultIDs[id] = map[int64]slot{}
	}
	f.resultIDs[id][f.nextID] = s
	f.exps[id].SpentUSD += r.CostUSD
	for _, g := range grades {
		f.exps[id].SpentUSD += g.JudgeCostUSD
	}
	return true, nil
}

func (f *fakeStore) FinishEvalExperiment(ctx context.Context, id int64, status string, summary json.RawMessage, errText string) error {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.exps[id]
	if e.Status != postgresql.ExperimentQueued && e.Status != postgresql.ExperimentRunning && e.Status != postgresql.ExperimentReview {
		return postgresql.ErrExperimentFinished
	}
	e.Status, e.Error, e.Summary = status, errText, summary
	f.finished[id] = summary
	return nil
}

func (f *fakeStore) EvalSummaryRows(ctx context.Context, id int64) ([]compare.Row, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]compare.Row, 0, len(f.results[id]))
	for s, r := range f.results[id] {
		row := compare.Row{CaseID: s.caseID, Arm: s.arm, Repeat: s.repeat, Phase: r.Phase, CostUSD: r.CostUSD, CostComplete: !r.CostIncomplete}
		for _, g := range f.grades[id][s] {
			status := g.Status
			if status == "" {
				status = compare.GradeOK
			}
			row.Grades = append(row.Grades, compare.Grade{GraderID: g.GraderID, Score: g.Score, Pass: g.Pass, Status: status, Weight: 1})
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.CaseID < b.CaseID || a.CaseID == b.CaseID && (a.Arm < b.Arm || a.Arm == b.Arm && a.Repeat < b.Repeat)
	})
	return out, nil
}

func (f *fakeStore) ListEvalGraders(ctx context.Context, datasetID int64) ([]postgresql.EvalGrader, error) {
	return f.graders, nil
}

func (f *fakeStore) GetRun(_ context.Context, namespace, name string) (*postgresql.RunArchive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getRuns++
	return f.archives[namespace+"/"+name], nil
}

func (f *fakeStore) ListEvalExperimentResults(ctx context.Context, id int64, q postgresql.EvalExperimentResultQuery) ([]postgresql.EvalResult, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []postgresql.EvalResult
	for rid, s := range f.resultIDs[id] {
		if rid <= q.After {
			continue
		}
		r := f.results[id][s]
		r.ID, r.Grades = rid, append([]postgresql.EvalGrade(nil), f.grades[id][s]...)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeStore) SaveEvalGrades(ctx context.Context, id, resultID int64, grades []postgresql.EvalGrade) error {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.resultIDs[id][resultID]
	if !ok {
		return postgresql.ErrEvalNotFound
	}
	f.grades[id][s] = append(f.grades[id][s], grades...)
	for _, g := range grades {
		f.exps[id].SpentUSD += g.JudgeCostUSD
	}
	return nil
}

func (f *fakeStore) PurgeEvalResultsBefore(ctx context.Context, cutoff time.Time) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges = append(f.purges, cutoff)
	return 1, 2, nil
}

func (f *fakeStore) AddEvalSpend(ctx context.Context, id int64, usd float64) error {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exps[id].SpentUSD += usd
	f.canary[id] += usd
	return nil
}

func (f *fakeStore) EvalJudgeSpendSince(context.Context, time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total float64
	for id, c := range f.canary {
		total += c
		for _, gs := range f.grades[id] {
			for _, g := range gs {
				total += g.JudgeCostUSD
			}
		}
	}
	return total, nil
}

func (f *fakeStore) EvalGraderStats(ctx context.Context, id int64) ([]postgresql.EvalGraderStat, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	byID := map[int64]*postgresql.EvalGraderStat{}
	for _, gs := range f.grades[id] {
		for _, g := range gs {
			st := byID[g.GraderID]
			if st == nil {
				st = &postgresql.EvalGraderStat{Grader: fmt.Sprint("grader-", g.GraderID)}
				byID[g.GraderID] = st
			}
			switch g.Status {
			case postgresql.GradePendingReview:
				st.Pending++
			case postgresql.GradeInvalid:
				st.Invalid++
			case postgresql.GradeError:
				st.Errors++
			default:
				st.Graded++
				if g.Pass {
					st.Passed++
				}
			}
			st.JudgeCostUSD += g.JudgeCostUSD
		}
	}
	out := make([]postgresql.EvalGraderStat, 0, len(byID))
	for _, st := range byID {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Grader < out[j].Grader })
	return out, nil
}

func (f *fakeStore) PrepareEvalReview(ctx context.Context, id int64) (int, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.pending[id]; ok {
		return n, nil
	}
	n := 0
	for _, gs := range f.grades[id] {
		for _, g := range gs {
			if g.Status == postgresql.GradePendingReview {
				n++
			}
		}
	}
	f.pending[id] = n
	return n, nil
}

func (f *fakeStore) AwaitEvalReview(ctx context.Context, id int64, summary json.RawMessage) error {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exps[id].Status, f.exps[id].Summary = postgresql.ExperimentReview, summary
	return nil
}

func (f *fakeStore) PendingEvalReviews(ctx context.Context, id int64) (int, error) {
	f.wantScope(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending[id], nil
}

// label records n human labels: the items they cover are no longer pending.
func (f *fakeStore) label(id int64, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending[id] -= n
}

func (f *fakeStore) GetRevision(_ context.Context, q postgresql.RevisionQuery, n int) (postgresql.Revision, error) {
	if rev, ok := f.revisions[fmt.Sprintf("%s/%s/%d", q.Namespace, q.Name, n)]; ok {
		return rev, nil
	}
	return postgresql.Revision{}, postgresql.ErrRevisionNotFound
}

// env is a runner over a fake store and a fake cluster.
type env struct {
	t     *testing.T
	store *fakeStore
	kube  client.Client
	r     *Runner
	now   time.Time
}

type opts struct {
	lim    Limiter
	tcs    []agentorcav1alpha1.TenantConfig
	extras []client.Object
}

func labelled(tenantID string) map[string]string {
	return map[string]string{tenancy.LabelTenantID: tenantID}
}

func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 5+len(o.tcs)+len(o.extras))
	objs = append(objs,
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: ns, Labels: labelled(tenant)},
			Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", SystemPrompt: "live prompt"}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns, Labels: labelled(tenant)},
			Spec: agentorcav1alpha1.ModelSelectorSpec{Providers: []agentorcav1alpha1.ProviderWeight{{Name: "big", Weight: 100}}}},
		&agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: ns, Labels: labelled(tenant)}},
		&agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "cheap", Namespace: ns, Labels: labelled(tenant)}},
		&agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: agentorcav1alpha1.TenantConfigSpec{TenantID: tenant}},
	)
	for i := range o.tcs {
		objs = append(objs, &o.tcs[i])
	}
	objs = append(objs, o.extras...)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	store := newFakeStore(t)
	for i := int64(1); i <= 2; i++ {
		store.cases = append(store.cases, postgresql.EvalCase{ID: 100 + i, DatasetID: 7, Input: fmt.Sprintf("question %d", i), Reference: "ok"})
	}
	e := &env{t: t, store: store, kube: kube, now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	e.r = New(store, kube, Config{Owner: "test", Limiter: o.lim, Now: func() time.Time { return e.now }})
	return e
}

func baseExperiment(id int64) postgresql.EvalExperiment {
	return postgresql.EvalExperiment{
		ID: id, TenantID: tenant, Namespace: ns, DatasetID: 7, DatasetVersion: 3, Repeats: 1,
		Arms:      []postgresql.EvalArm{{Label: "base", Agent: "support"}, {Label: "cand", Agent: "support", ModelProvider: "cheap"}},
		BudgetUSD: 10, RunBudgetUSD: 1, TimeoutSeconds: 120, Concurrency: 4, Purpose: postgresql.PurposeCompare, CreatedBy: user,
	}
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.r.Tick(context.Background()); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

func (e *env) runs() []agentorcav1alpha1.AgentRun {
	e.t.Helper()
	var l agentorcav1alpha1.AgentRunList
	if err := e.kube.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
		e.t.Fatal(err)
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items
}

// finish marks every run that is not over as over, with the given phase, output and spend.
func (e *env) finishRuns(phase agentorcav1alpha1.AgentRunPhase, output, spend, reason string) {
	e.t.Helper()
	for _, run := range e.runs() {
		if isTerminal(&run) {
			continue
		}
		run.Status.Phase, run.Status.Output, run.Status.SpendUSD, run.Status.FailureReason = phase, output, spend, reason
		start := metav1.NewTime(e.now)
		done := metav1.NewTime(e.now.Add(2 * time.Second))
		run.Status.StartTime, run.Status.CompletionTime = &start, &done
		if err := e.kube.Update(context.Background(), &run); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) names(list client.ObjectList) []string {
	e.t.Helper()
	if err := e.kube.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		e.t.Fatal(err)
	}
	var out []string
	switch l := list.(type) {
	case *agentorcav1alpha1.AgentList:
		for _, a := range l.Items {
			out = append(out, a.Name)
		}
	case *agentorcav1alpha1.ModelSelectorList:
		for _, a := range l.Items {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, want string) bool { return slices.Contains(list, want) }

func containsGrader() postgresql.EvalGrader {
	return postgresql.EvalGrader{ID: 9, DatasetID: 7, Name: "ok", Kind: "contains", Config: []byte(`{"value":"ok"}`), Weight: 1, Required: true}
}

//nolint:gocyclo // one scenario, step by step
func TestExperimentRunsToCompletion(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = []postgresql.EvalGrader{containsGrader()}
	e.store.add(baseExperiment(1))

	e.tick() // starts the experiment and creates every run
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentRunning || len(exp.Arms[0].Hash) != 64 || exp.Arms[0].Hash != exp.Arms[1].Hash {
		t.Fatalf("started = %+v", exp)
	}
	if got := e.names(&agentorcav1alpha1.AgentList{}); !contains(got, "eval-1-base") || !contains(got, "eval-1-cand") {
		t.Fatalf("arm agents = %v", got)
	}
	var agent agentorcav1alpha1.Agent
	_ = e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "eval-1-cand"}, &agent)
	if agent.Spec.ModelSelectorRef != "eval-1-cand" || agent.Labels[tenancy.LabelEvalExperiment] != "1" || agent.Labels[tenancy.LabelTenantID] != tenant ||
		agent.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI {
		t.Errorf("model arm agent = %+v", agent.ObjectMeta)
	}
	var sel agentorcav1alpha1.ModelSelector
	if err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "eval-1-cand"}, &sel); err != nil ||
		len(sel.Spec.Providers) != 1 || sel.Spec.Providers[0].Name != "cheap" {
		t.Errorf("one-provider selector = %+v, %v", sel.Spec, err)
	}
	runs := e.runs()
	if len(runs) != 4 {
		t.Fatalf("runs = %d, want 4", len(runs))
	}
	for _, run := range runs {
		l := run.Labels
		if l[tenancy.LabelEval] != "true" || l[tenancy.LabelEvalExperiment] != "1" || l[tenancy.LabelEvalArm] == "" || l[tenancy.LabelEvalCase] == "" ||
			l[tenancy.LabelTenantID] != tenant || l["agentorca.io/tenant"] != "acme" {
			t.Errorf("run labels = %v", l)
		}
		sp := run.Spec
		if sp.Callbacks != nil || sp.Egress != nil || sp.Budget == nil || sp.Budget.OnExceeded != agentorcav1alpha1.RunBudgetFail ||
			sp.Timeout == nil || sp.Timeout.Duration != 2*time.Minute || run.Annotations["agentorca.io/created-by"] != user {
			t.Errorf("run spec = %+v", sp)
		}
		if cap := parseUSD(sp.Budget.MaxUSD); cap <= 0 || cap > 1 {
			t.Errorf("run cap = %v", sp.Budget.MaxUSD)
		}
	}

	e.finishRuns(agentorcav1alpha1.AgentRunPhaseSucceeded, "all ok", "0.05", "")
	e.tick()
	exp = e.store.exp(1)
	if exp.Status != postgresql.ExperimentComplete || exp.SpentUSD < 0.199 || exp.SpentUSD > 0.201 {
		t.Fatalf("finished = %+v", exp)
	}
	var sum Summary
	if err := json.Unmarshal(exp.Summary, &sum); err != nil || sum.Results != 4 || sum.StoppedBy != "" || sum.Comparison == nil || len(sum.Comparison.Arms) != 2 {
		t.Fatalf("summary = %s, %v", exp.Summary, err)
	}
	res := e.store.results[1][slot{101, "base", 0}]
	if res.Phase != phaseSucceeded || res.Output != "all ok" || res.LatencyMs != 2000 || !res.CasePass || res.RunName == "" {
		t.Errorf("result = %+v", res)
	}
	if g := e.store.grades[1][slot{101, "base", 0}]; len(g) != 1 || !g[0].Pass {
		t.Errorf("grades = %+v", g)
	}
	if got := e.names(&agentorcav1alpha1.AgentList{}); contains(got, "eval-1-base") || !contains(got, "support") {
		t.Errorf("arm agents were not cleaned up: %v", got)
	}
	if got := e.names(&agentorcav1alpha1.ModelSelectorList{}); contains(got, "eval-1-cand") {
		t.Errorf("arm selector was not cleaned up: %v", got)
	}
	// The experiment is over: another tick does nothing.
	e.tick()
	if len(e.runs()) != 4 {
		t.Errorf("runs after the end = %d", len(e.runs()))
	}
}

func TestBudgetStopsTheExperimentWithPairedData(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.cases = append(e.store.cases, postgresql.EvalCase{ID: 103, DatasetID: 7, Input: "question 3"})
	e.store.graders = []postgresql.EvalGrader{containsGrader()}
	x := baseExperiment(1)
	x.BudgetUSD, x.RunBudgetUSD, x.Concurrency = 1, 0.4, 1
	e.store.add(x)

	var caps []float64
	for i := 0; i < 12 && e.store.exp(1).Status == postgresql.ExperimentRunning || i == 0; i++ {
		e.tick()
		for _, run := range e.runs() {
			if !isTerminal(&run) {
				caps = append(caps, runCapOf(&run))
			}
		}
		// Each run spends exactly its cap, the most it may.
		for _, run := range e.runs() {
			if !isTerminal(&run) {
				e.finishOne(&run, runCapOf(&run))
			}
		}
	}
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentComplete {
		t.Fatalf("status = %s", exp.Status)
	}
	var sum Summary
	_ = json.Unmarshal(exp.Summary, &sum)
	if sum.StoppedBy != StoppedByBudget || !sum.Partial {
		t.Errorf("summary = %+v", sum)
	}
	if exp.SpentUSD > x.BudgetUSD+1e-9 {
		t.Errorf("spent %.4f over the budget %.4f (caps %v)", exp.SpentUSD, x.BudgetUSD, caps)
	}
	if len(e.store.results[1]) >= 6 {
		t.Errorf("all %d results were recorded despite the budget", len(e.store.results[1]))
	}
	// Runs went out in case order, both arms of a case together, so the cases
	// that were run are paired as far as the budget went.
	if sum.Comparison == nil || sum.Comparison.Comparisons[0].Pairs != 1 {
		t.Errorf("comparison = %+v", sum.Comparison)
	}
}

func (e *env) finishOne(run *agentorcav1alpha1.AgentRun, spend float64) {
	e.t.Helper()
	run.Status.Phase, run.Status.Output = agentorcav1alpha1.AgentRunPhaseSucceeded, "ok"
	run.Status.SpendUSD = strconv.FormatFloat(spend, 'f', 6, 64)
	if err := e.kube.Update(context.Background(), run); err != nil {
		e.t.Fatal(err)
	}
}

func TestRunCap(t *testing.T) {
	cases := []struct {
		name                                   string
		perRun, budget, spent, committed, want float64
		inflight                               int
	}{
		{"per-run cap wins", 1, 10, 0, 0, 1, 0},
		{"even share among runs in flight", 1, 1, 0, 0, 0.25, 3},
		{"what the in-flight caps leave", 1, 1, 0, 0.9, 0.1, 0},
		{"nothing left", 1, 1, 1, 0, 0, 0},
		{"overspent", 1, 1, 1.2, 0, 0, 0},
	}
	for _, c := range cases {
		if got := runCap(c.perRun, c.budget, c.spent, c.committed, c.inflight); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("%s: cap = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCancelDeletesRunsKeepsResultsAndSummarisesPartial(t *testing.T) {
	e := newEnv(t, opts{})
	x := baseExperiment(1)
	x.Concurrency = 2
	e.store.add(x)
	e.tick() // two runs: both arms of case 101
	if len(e.runs()) != 2 {
		t.Fatalf("runs = %d", len(e.runs()))
	}
	// One finishes, the other is still running when the cancel arrives.
	runs := e.runs()
	e.finishOne(&runs[0], 0.02)
	e.store.mu.Lock()
	e.store.exps[1].CancelRequested = true
	e.store.mu.Unlock()
	e.tick()

	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentCancelled {
		t.Fatalf("status = %s", exp.Status)
	}
	var sum Summary
	_ = json.Unmarshal(exp.Summary, &sum)
	if sum.StoppedBy != StoppedByCancel || !sum.Partial || sum.Results != 2 {
		t.Errorf("summary = %+v", sum)
	}
	phases := map[string]int{}
	for _, r := range e.store.results[1] {
		phases[r.Phase]++
	}
	if phases["Succeeded"] != 1 || phases["Cancelled"] != 1 {
		t.Errorf("phases = %v", phases)
	}
	if left := e.runs(); len(left) != 1 || left[0].Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded {
		t.Errorf("the in-flight run was not deleted, the finished one kept: %d runs", len(left))
	}
	if got := e.names(&agentorcav1alpha1.AgentList{}); contains(got, "eval-1-base") {
		t.Errorf("arm agents survived the cancel: %v", got)
	}
	e.tick()
	if len(e.runs()) != 1 {
		t.Error("a cancelled experiment created runs")
	}
}

func TestResumeAfterRestartAdoptsRunsAndCreatesNoDuplicates(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = []postgresql.EvalGrader{containsGrader()}
	x := baseExperiment(1)
	x.Concurrency = 2
	e.store.add(x)
	e.tick() // case 101, both arms
	before := e.runs()
	if len(before) != 2 {
		t.Fatalf("runs = %d", len(before))
	}
	e.finishOne(&before[0], 0.01) // finished while the operator was down

	// A new process: a new runner, the same database and cluster.
	e.r = New(e.store, e.kube, Config{Owner: "test", Now: func() time.Time { return e.now }})
	e.tick()
	if got := e.store.results[1][slot{101, "base", 0}]; got.Phase != "Succeeded" || got.Output != "ok" {
		t.Errorf("the run that finished during the restart was not recorded: %+v", got)
	}
	// Two slots are in flight again (the unfinished run, one new): no extra runs.
	if n := len(e.runs()); n != 3 {
		t.Fatalf("runs after resume = %d, want 3", n)
	}
	// Drive it to the end; no slot ever has two runs, and every slot has one result.
	for i := 0; i < 8 && e.store.exp(1).Status == postgresql.ExperimentRunning; i++ {
		e.finishRuns(agentorcav1alpha1.AgentRunPhaseSucceeded, "ok", "0.01", "")
		e.tick()
	}
	if e.store.exp(1).Status != postgresql.ExperimentComplete || len(e.store.results[1]) != 4 {
		t.Fatalf("after resume: %s, %d results", e.store.exp(1).Status, len(e.store.results[1]))
	}
	seen := map[string]bool{}
	for _, run := range e.runs() {
		if seen[run.Name] {
			t.Errorf("duplicate run %s", run.Name)
		}
		seen[run.Name] = true
	}
	if len(e.runs()) != 4 {
		t.Errorf("runs = %d, want 4", len(e.runs()))
	}
}

func TestSweepDeletesWhatFinishedOrVanishedExperimentsLeft(t *testing.T) {
	mk := func(id string) map[string]string {
		l := labelled(tenant)
		l[tenancy.LabelEvalExperiment] = id
		return l
	}
	extras := []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "eval-1-base", Namespace: ns, Labels: mk("1")}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "eval-2-base", Namespace: ns, Labels: mk("2")}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "eval-9-base", Namespace: ns, Labels: mk("9")}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "eval-2-cand", Namespace: ns, Labels: mk("2")}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "eval-1-cand", Namespace: ns, Labels: mk("1")}},
		&agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "eval-2-running", Namespace: ns, Labels: mk("2")},
			Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning}},
		&agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "eval-2-done", Namespace: ns, Labels: mk("2")},
			Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded}},
		&agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "eval-1-running", Namespace: ns, Labels: mk("1")},
			Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning}},
		&agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "production-run", Namespace: ns, Labels: labelled(tenant)}},
	}
	e := newEnv(t, opts{extras: extras})
	e.store.add(postgresql.EvalExperiment{ID: 1, TenantID: tenant, Namespace: ns, Status: postgresql.ExperimentRunning})
	e.store.add(postgresql.EvalExperiment{ID: 2, TenantID: tenant, Namespace: ns, Status: postgresql.ExperimentComplete})
	// Experiment 9 does not exist at all.

	if err := e.r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	agents := e.names(&agentorcav1alpha1.AgentList{})
	if !contains(agents, "eval-1-base") || contains(agents, "eval-2-base") || contains(agents, "eval-9-base") || !contains(agents, "support") {
		t.Errorf("agents = %v", agents)
	}
	if sels := e.names(&agentorcav1alpha1.ModelSelectorList{}); !contains(sels, "eval-1-cand") || contains(sels, "eval-2-cand") || !contains(sels, "default") {
		t.Errorf("selectors = %v", sels)
	}
	runs := e.runs()
	names := make([]string, 0, len(runs))
	for _, run := range runs {
		names = append(names, run.Name)
	}
	want := []string{"eval-1-running", "eval-2-done", "production-run"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("runs = %v, want %v", names, want)
	}
}

func TestPlatformFailureRetriesOnceThenSkipsThePair(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = []postgresql.EvalGrader{containsGrader()}
	x := baseExperiment(1)
	x.Concurrency = 2
	e.store.add(x)
	e.tick() // case 101: base and cand
	for _, run := range e.runs() {
		if run.Labels[tenancy.LabelEvalArm] == "base" {
			run.Status.Phase, run.Status.FailureReason = agentorcav1alpha1.AgentRunPhaseFailed, "pod not found"
			_ = e.kube.Update(context.Background(), &run)
		} else {
			e.finishOne(&run, 0.01)
		}
	}
	e.tick() // base is retried, cand recorded
	if _, ok := e.store.results[1][slot{101, "base", 0}]; ok {
		t.Fatal("a platform failure was recorded before its retry")
	}
	var retry *agentorcav1alpha1.AgentRun
	for _, run := range e.runs() {
		if run.Labels[labelAttempt] == "1" {
			retry = &run
		}
	}
	if retry == nil || retry.Labels[tenancy.LabelEvalArm] != "base" || retry.Name != "eval-1-0-101-0-r1" {
		t.Fatalf("no retry of the failed slot (runs %d)", len(e.runs()))
	}
	// The retry fails the same way: the slot is skipped, not scored as a failure.
	retry.Status.Phase, retry.Status.FailureReason = agentorcav1alpha1.AgentRunPhaseFailed, "sandbox not found"
	if err := e.kube.Update(context.Background(), retry); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if got := e.store.results[1][slot{101, "base", 0}]; got.Phase != "Skipped" || got.Error == "" {
		t.Errorf("result = %+v", got)
	}
	// Finish the experiment: case 102's arms run, and the comparison drops case 101.
	for i := 0; i < 6 && e.store.exp(1).Status == postgresql.ExperimentRunning; i++ {
		e.finishRuns(agentorcav1alpha1.AgentRunPhaseSucceeded, "ok", "0.01", "")
		e.tick()
	}
	var sum Summary
	_ = json.Unmarshal(e.store.exp(1).Summary, &sum)
	if sum.Skipped != 1 || sum.Comparison == nil || sum.Comparison.Comparisons[0].Pairs != 1 || len(sum.Comparison.Comparisons[0].SkippedCases) != 1 {
		t.Errorf("summary = %s", e.store.exp(1).Summary)
	}
}

func TestAnAgentFailureIsDataNotARetry(t *testing.T) {
	e := newEnv(t, opts{})
	x := baseExperiment(1)
	e.store.add(x)
	e.tick()
	e.finishRuns(agentorcav1alpha1.AgentRunPhaseFailed, "", "0.03", "exit code 1: boom")
	e.tick()
	if got := e.store.results[1][slot{101, "base", 0}]; got.Phase != "Failed" || got.CostUSD != 0.03 {
		t.Errorf("result = %+v", got)
	}
	if n := len(e.runs()); n != 4 {
		t.Errorf("runs = %d: a failing agent was retried", n)
	}
}

func TestConcurrencyIsBoundedByTheExperimentAndTheTenant(t *testing.T) {
	e := newEnv(t, opts{})
	x := baseExperiment(1)
	x.Concurrency = 3
	e.store.add(x)
	e.tick()
	if n := len(e.runs()); n != 3 {
		t.Errorf("experiment concurrency 3: %d runs", n)
	}

	tc := agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "capped"}, Spec: agentorcav1alpha1.TenantConfigSpec{
		TenantID: otherTen, RateLimit: &agentorcav1alpha1.TenantRateLimit{ConcurrentRuns: 1}}}
	e2 := newEnv(t, opts{tcs: []agentorcav1alpha1.TenantConfig{tc}, extras: []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-b", Labels: labelled(otherTen)}, Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-b", Labels: labelled(otherTen)}},
	}})
	y := baseExperiment(5)
	y.TenantID, y.Namespace, y.Concurrency = otherTen, "team-b", 16
	y.Arms = []postgresql.EvalArm{{Label: "a", Agent: "support"}, {Label: "b", Agent: "support"}}
	// Two arms of one agent are refused by the store, but the runner only needs distinct objects.
	y.Arms[1].Revision = 0
	e2.store.add(y)
	e2.tick()
	var l agentorcav1alpha1.AgentRunList
	_ = e2.kube.List(context.Background(), &l, client.InNamespace("team-b"))
	if len(l.Items) != 1 {
		t.Errorf("tenant ConcurrentRuns 1: %d runs", len(l.Items))
	}
	// 16 is the ceiling whatever the experiment asks for.
	if per, ten := runLimit(100, tenantLimits{}); per != 16 || ten != 16 {
		t.Errorf("limits = %d, %d", per, ten)
	}
}

type denyAll struct{ calls int }

func (d *denyAll) Allow(string, int) (time.Duration, bool) { d.calls++; return time.Second, false }

type allowN struct{ n, calls int }

func (a *allowN) Allow(string, int) (time.Duration, bool) {
	a.calls++
	if a.calls > a.n {
		return time.Second, false
	}
	return 0, true
}

func TestTenantRateLimitIsConsultedOncePerRun(t *testing.T) {
	tc := agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "limited"}, Spec: agentorcav1alpha1.TenantConfigSpec{
		TenantID: otherTen, RateLimit: &agentorcav1alpha1.TenantRateLimit{RequestsPerMinute: 60}}}
	lim := &allowN{n: 2}
	extras := []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-b", Labels: labelled(otherTen)}, Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-b", Labels: labelled(otherTen)}},
		&agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "cheap", Namespace: "team-b", Labels: labelled(otherTen)}},
	}
	e := newEnv(t, opts{lim: lim, tcs: []agentorcav1alpha1.TenantConfig{tc}, extras: extras})
	x := baseExperiment(1)
	x.TenantID, x.Namespace = otherTen, "team-b"
	e.store.add(x)
	e.tick()
	var l agentorcav1alpha1.AgentRunList
	_ = e.kube.List(context.Background(), &l, client.InNamespace("team-b"))
	if len(l.Items) != 2 {
		t.Errorf("rate limit allowed 2: %d runs", len(l.Items))
	}
	// A tenant with no TenantConfig limit is never asked.
	deny := &denyAll{}
	e2 := newEnv(t, opts{lim: deny})
	e2.store.add(baseExperiment(1))
	e2.tick()
	if deny.calls != 0 || len(e2.runs()) != 4 {
		t.Errorf("limiter consulted %d times for an unlimited tenant, %d runs", deny.calls, len(e2.runs()))
	}
}

func TestDailyBudgetOfTheTenantStopsAndRefuses(t *testing.T) {
	tc := agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: agentorcav1alpha1.TenantConfigSpec{TenantID: tenant, BudgetPerDayUSD: "5"}}
	// The default TenantConfig of newEnv has the same name; replace it.
	e := newEnv(t, opts{})
	_ = e.kube.Delete(context.Background(), &agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "acme"}})
	if err := e.kube.Create(context.Background(), &tc); err != nil {
		t.Fatal(err)
	}
	// An experiment that wants $10 when $5 a day is allowed does not start.
	e.store.add(baseExperiment(1))
	e.tick()
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentFailed || exp.Error == "" || len(e.runs()) != 0 {
		t.Errorf("experiment = %+v, runs %d", exp, len(e.runs()))
	}
	// One that fits starts, and stops creating runs when today's other spend takes the room.
	x := baseExperiment(2)
	x.BudgetUSD, x.Concurrency = 3, 1
	e.store.add(x)
	e.tick()
	if len(e.runs()) != 1 {
		t.Fatalf("runs = %d", len(e.runs()))
	}
	prod := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns, Labels: labelled(tenant), CreationTimestamp: metav1.NewTime(e.now)},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "4"}}
	if err := e.kube.Create(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	e.finishRuns(agentorcav1alpha1.AgentRunPhaseSucceeded, "ok", "0.1", "")
	e.tick()
	exp := e.store.exp(2)
	var sum Summary
	_ = json.Unmarshal(exp.Summary, &sum)
	if exp.Status != postgresql.ExperimentComplete || sum.StoppedBy != StoppedByDailyBudget {
		t.Errorf("experiment = %s, summary %+v", exp.Status, sum)
	}
}

func TestMissingReferencesFailTheExperimentBeforeAnythingIsCreated(t *testing.T) {
	e := newEnv(t, opts{extras: []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "needy", Namespace: ns, Labels: labelled(tenant)},
			Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", Tools: []string{"search"}}},
	}})
	x := baseExperiment(1)
	x.Arms = []postgresql.EvalArm{{Label: "base", Agent: "support"}, {Label: "cand", Agent: "needy"}}
	e.store.add(x)
	e.tick()
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentFailed || len(e.runs()) != 0 {
		t.Fatalf("experiment = %+v", exp)
	}
	if want := `arm "cand": Tool "search" does not exist in namespace team-a`; exp.Error != want {
		t.Errorf("error = %q, want %q", exp.Error, want)
	}
	if got := e.names(&agentorcav1alpha1.AgentList{}); contains(got, "eval-1-base") {
		t.Errorf("objects were created for a failed experiment: %v", got)
	}

	// A model arm whose provider is not in the workspace is the same failure.
	e = newEnv(t, opts{})
	y := baseExperiment(2)
	y.Arms[1].ModelProvider = "nope"
	e.store.add(y)
	e.tick()
	if exp := e.store.exp(2); exp.Status != postgresql.ExperimentFailed || exp.Error != `arm "cand": ModelProvider "nope" does not exist in namespace team-a (it has big, cheap)` {
		t.Errorf("experiment = %s %q", exp.Status, exp.Error)
	}
	// And a reference to another tenant's object counts as missing.
	foreign := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: ns, Labels: labelled(otherTen)}}
	e = newEnv(t, opts{extras: []client.Object{foreign,
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "needy", Namespace: ns, Labels: labelled(tenant)},
			Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", Tools: []string{"search"}}}}})
	z := baseExperiment(3)
	z.Arms[1] = postgresql.EvalArm{Label: "cand", Agent: "needy"}
	e.store.add(z)
	e.tick()
	if exp := e.store.exp(3); exp.Status != postgresql.ExperimentFailed {
		t.Errorf("another tenant's tool was accepted: %s %q", exp.Status, exp.Error)
	}
}

func TestRevisionArmsRunTheStoredSpecAndAreVerified(t *testing.T) {
	e := newEnv(t, opts{})
	old := agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", SystemPrompt: "old prompt"}}
	body, _ := json.Marshal(old)
	e.store.revisions["team-a/support/3"] = postgresql.Revision{Revision: 3, Action: "update", Object: body}
	wantHash, _ := SpecHash(old.Spec)
	live := agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", SystemPrompt: "live prompt"}}
	liveHash, _ := SpecHash(live.Spec)

	x := baseExperiment(1)
	x.Arms = []postgresql.EvalArm{{Label: "live", Agent: "support"}, {Label: "old", Agent: "support", Revision: 3}}
	e.store.add(x)
	e.tick()
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentRunning || exp.Arms[0].Hash != liveHash || exp.Arms[1].Hash != wantHash || wantHash == liveHash {
		t.Fatalf("arms = %+v (status %s, %q)", exp.Arms, exp.Status, exp.Error)
	}
	var a agentorcav1alpha1.Agent
	_ = e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "eval-1-old"}, &a)
	if a.Spec.SystemPrompt != "old prompt" {
		t.Errorf("the revision arm runs %q", a.Spec.SystemPrompt)
	}

	// A hash that no longer matches its revision is refused; so is a revision that does not exist.
	for name, arm := range map[string]postgresql.EvalArm{
		"stale hash":  {Label: "old", Agent: "support", Revision: 3, Hash: liveHash},
		"no revision": {Label: "old", Agent: "support", Revision: 9},
		"no agent":    {Label: "old", Agent: "ghost"},
	} {
		_, err := ResolveArms(context.Background(), e.kube, e.store, tenant, ns, []postgresql.EvalArm{arm})
		if _, ok := err.(*ArmError); !ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDraftArmsRunTheInlineSpecWithoutTheAgentExisting(t *testing.T) {
	e := newEnv(t, opts{})
	draft := agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", SystemPrompt: "unsaved draft prompt"}
	raw, _ := json.Marshal(draft)
	wantHash, _ := SpecHash(draft)
	x := baseExperiment(1)
	x.Purpose = postgresql.PurposeCheck
	// The draft's agent was never saved; the baseline is the live one.
	x.Arms = []postgresql.EvalArm{{Label: "published", Agent: "support"}, {Label: "draft", Agent: "never-saved", Spec: raw}}
	e.store.add(x)
	e.tick()
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentRunning || exp.Arms[1].Hash != wantHash {
		t.Fatalf("arms = %+v (status %s, %q)", exp.Arms, exp.Status, exp.Error)
	}
	var a agentorcav1alpha1.Agent
	if err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "eval-1-draft"}, &a); err != nil || a.Spec.SystemPrompt != "unsaved draft prompt" {
		t.Errorf("the draft arm runs %q (%v)", a.Spec.SystemPrompt, err)
	}
	var saved agentorcav1alpha1.Agent
	if err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "never-saved"}, &saved); err == nil {
		t.Error("the draft was stored as an agent")
	}
	// A spec that cannot be read is the requester's problem.
	_, err := ResolveArms(context.Background(), e.kube, e.store, tenant, ns, []postgresql.EvalArm{{Label: "draft", Agent: "x", Spec: []byte(`{"tools":"no"}`)}})
	if _, ok := err.(*ArmError); !ok {
		t.Errorf("unreadable spec: err = %v", err)
	}
}

func TestArmNamesCannotTakeOverAnExistingObject(t *testing.T) {
	e := newEnv(t, opts{extras: []client.Object{
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "eval-1-base", Namespace: ns, Labels: labelled(tenant)}},
	}})
	e.store.add(baseExperiment(1))
	e.tick()
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentFailed {
		t.Errorf("status = %s", exp.Status)
	}
	var a agentorcav1alpha1.Agent
	_ = e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "eval-1-base"}, &a)
	if a.Spec.ModelSelectorRef != "" {
		t.Error("a user's agent was overwritten")
	}
}

func TestPhaseAndPlatformFailureReading(t *testing.T) {
	run := func(p agentorcav1alpha1.AgentRunPhase, reason string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{Status: agentorcav1alpha1.AgentRunStatus{Phase: p, FailureReason: reason}}
	}
	for _, c := range []struct {
		run   *agentorcav1alpha1.AgentRun
		phase string
	}{
		{run(agentorcav1alpha1.AgentRunPhaseSucceeded, ""), "Succeeded"},
		{run(agentorcav1alpha1.AgentRunPhaseHandedOff, ""), "Succeeded"},
		{run(agentorcav1alpha1.AgentRunPhaseFailed, "exit code 1"), "Failed"},
		{run(agentorcav1alpha1.AgentRunPhaseFailed, "pod DeadlineExceeded"), "TimedOut"},
		{run(agentorcav1alpha1.AgentRunPhaseFailed, "safeguard: loop"), "Failed"},
	} {
		if got, _ := phaseOf(c.run); got != c.phase {
			t.Errorf("%v: phase = %s, want %s", c.run.Status, got, c.phase)
		}
	}
	for reason, want := range map[string]bool{
		"pod not found": true, "creating sandbox: boom": true, "the pod was evicted": true, "0/3 nodes are unschedulable": true,
		"building router config: x": true, "exit code 1: boom": false, "safeguard: loop detected": false, "run budget exceeded": false,
	} {
		if got := platformFailure(reason); got != want {
			t.Errorf("platformFailure(%q) = %v", reason, got)
		}
	}
}

func TestSpecHashIsStable(t *testing.T) {
	a := agentorcav1alpha1.AgentSpec{ModelSelectorRef: "m", Tools: []string{"a", "b"}}
	b := agentorcav1alpha1.AgentSpec{ModelSelectorRef: "m", Tools: []string{"a", "b"}}
	h1, _ := SpecHash(a)
	h2, _ := SpecHash(b)
	b.Tools = []string{"b", "a"}
	h3, _ := SpecHash(b)
	if h1 != h2 || h1 == h3 || len(h1) != 64 {
		t.Errorf("hashes %s %s %s", h1, h2, h3)
	}
}

func TestSweepRetention(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fs := newFakeStore(t)
	r := New(fs, nil, Config{ResultRetentionDays: 90, Now: func() time.Time { return now }})
	r.SweepRetention(context.Background())
	if len(fs.purges) != 1 || !fs.purges[0].Equal(now.AddDate(0, 0, -90)) {
		t.Fatalf("purges = %v, want one at %v", fs.purges, now.AddDate(0, 0, -90))
	}
	// Zero keeps everything.
	fs = newFakeStore(t)
	New(fs, nil, Config{Now: func() time.Time { return now }}).SweepRetention(context.Background())
	if len(fs.purges) != 0 {
		t.Fatalf("retention 0 purged: %v", fs.purges)
	}
}
