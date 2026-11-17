package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/judge"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// ── tool calls ──────────────────────────────────────────────────────────────

func traceJSON(events ...string) []byte {
	parts := make([]string, 0, len(events))
	for i, ev := range events {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"event":%s,"ts":"2026-11-01T12:00:00Z"}`, i, ev))
	}
	return []byte("[" + strings.Join(parts, ",") + "]")
}

func toolEvent(name string) string {
	return fmt.Sprintf(`{"type":"toolCall","name":%q,"arguments":"{}","backendType":"mcp","backendRef":"x"}`, name)
}

func TestToolCallNamesReadsTheArchivedTrace(t *testing.T) {
	raw := traceJSON(toolEvent("search"), `{"type":"toolResult","name":"search"}`, toolEvent("search"), toolEvent("write_file"),
		`{"type":"thought","text":"hmm"}`, `"not an object"`)
	names, known := toolCallNames(raw)
	if !known || strings.Join(names, ",") != "search,search,write_file" {
		t.Errorf("names = %v, known %v", names, known)
	}
	// A child run's calls are the child's.
	child := []byte(`[{"id":1,"event":{"type":"toolCall","name":"inner"},"childRunName":"run-child"},{"id":2,"event":{"type":"toolCall","name":"outer"}}]`)
	if names, _ := toolCallNames(child); strings.Join(names, ",") != "outer" {
		t.Errorf("child calls were counted: %v", names)
	}
	if names, known := toolCallNames([]byte(`[]`)); !known || len(names) != 0 {
		t.Errorf("an empty trace is known and empty: %v %v", names, known)
	}
	for _, bad := range []string{``, `null`, `{"x":1}`, `not json`} {
		if _, known := toolCallNames([]byte(bad)); known && bad != `null` {
			t.Errorf("%q is known", bad)
		}
	}
	long := make([]string, postgresql.MaxEvalResultToolCalls+50)
	for i := range long {
		long[i] = toolEvent("t")
	}
	if names, _ := toolCallNames(traceJSON(long...)); len(names) != postgresql.MaxEvalResultToolCalls {
		t.Errorf("tool calls kept = %d", len(names))
	}
}

func toolGraders() []postgresql.EvalGrader {
	return []postgresql.EvalGrader{
		{ID: 21, DatasetID: 7, Name: "searched", Kind: "tool_called", Config: []byte(`{"tool":"search"}`), Weight: 1, Required: true},
		{ID: 22, DatasetID: 7, Name: "never-deletes", Kind: "tool_not_called", Config: []byte(`{"tool":"delete"}`), Weight: 1, Required: true},
	}
}

func archive(run string, trace []byte) *postgresql.RunArchive {
	return &postgresql.RunArchive{ID: ns + "/" + run, Name: run, Namespace: ns, TraceEventsJSON: trace}
}

func oneArm(id int64) postgresql.EvalExperiment {
	x := baseExperiment(id)
	return x
}

func TestToolGradersPassAndFailOnTheArchivedTrace(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = toolGraders()
	e.store.add(oneArm(1))
	e.tick()
	for _, run := range e.runs() {
		trace := traceJSON(toolEvent("search"))
		if strings.Contains(run.Name, "-1-1-") { // the second arm deletes something
			trace = traceJSON(toolEvent("search"), toolEvent("delete"))
		}
		e.store.archives[ns+"/"+run.Name] = archive(run.Name, trace)
	}
	e.finishRuns(phaseSucceeded, "done", "0.01", "")
	e.tick()
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentComplete {
		t.Fatalf("status = %s", exp.Status)
	}
	base := e.store.results[1][slot{101, "base", 0}]
	cand := e.store.results[1][slot{101, "cand", 0}]
	if strings.Join(base.ToolCalls, ",") != "search" || strings.Join(cand.ToolCalls, ",") != "search,delete" {
		t.Errorf("tool calls: base %v, cand %v", base.ToolCalls, cand.ToolCalls)
	}
	if !base.CasePass || cand.CasePass {
		t.Errorf("case pass: base %v (want true), cand %v (want false)", base.CasePass, cand.CasePass)
	}
	for _, g := range e.store.grades[1][slot{101, "cand", 0}] {
		if g.GraderID == 21 && !g.Pass || g.GraderID == 22 && g.Pass {
			t.Errorf("cand grade %+v", g)
		}
	}
}

func TestTraceNotArchivedYetIsWaitedForThenGradedAsAnError(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = toolGraders()
	e.store.add(oneArm(1))
	e.tick()
	e.finishRuns(phaseSucceeded, "done", "0.01", "")

	e.tick() // nothing is archived: nothing is recorded, and no retry run is made
	if got := len(e.store.results[1]); got != 0 {
		t.Fatalf("%d results recorded before the trace was archived", got)
	}
	if got := len(e.runs()); got != 4 {
		t.Fatalf("runs = %d: a retry was started for a run that only waits for its archive", got)
	}
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentRunning {
		t.Fatalf("status = %s", exp.Status)
	}

	// The archive lands for two of the runs; the rest never get one.
	for _, run := range e.runs()[:2] {
		e.store.archives[ns+"/"+run.Name] = archive(run.Name, traceJSON(toolEvent("search")))
	}
	e.tick()
	if got := len(e.store.results[1]); got != 2 {
		t.Fatalf("%d results after two archives landed", got)
	}
	e.now = e.now.Add(traceGrace + time.Minute)
	e.tick()
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentComplete {
		t.Fatalf("status after the grace = %s", exp.Status)
	}
	var errored int
	for s, grades := range e.store.grades[1] {
		res := e.store.results[1][s]
		if len(res.ToolCalls) > 0 {
			continue
		}
		for _, g := range grades {
			if g.Status != postgresql.GradeError || g.Pass || !strings.Contains(g.Detail, "not archived") {
				t.Errorf("a result without a trace has grade %+v", g)
			}
			errored++
		}
		if res.CasePass {
			t.Errorf("a run without a trace passes: %+v", res)
		}
	}
	if errored != 4 {
		t.Errorf("error grades = %d, want 2 results x 2 graders", errored)
	}
}

func TestToolCallsAreCopiedWithoutToolGraders(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = []postgresql.EvalGrader{containsGrader()}
	e.store.add(oneArm(1))
	e.tick()
	for _, run := range e.runs() {
		e.store.archives[ns+"/"+run.Name] = archive(run.Name, traceJSON(toolEvent("lookup")))
	}
	e.finishRuns(phaseSucceeded, "ok", "0.01", "")
	e.tick()
	if got := e.store.results[1][slot{101, "base", 0}].ToolCalls; strings.Join(got, ",") != "lookup" {
		t.Errorf("tool calls = %v", got)
	}
	// With no tool grader a missing archive costs no wait.
	e2 := newEnv(t, opts{})
	e2.store.graders = []postgresql.EvalGrader{containsGrader()}
	e2.store.add(oneArm(2))
	e2.tick()
	e2.finishRuns(phaseSucceeded, "ok", "0.01", "")
	e2.tick()
	if exp := e2.store.exp(2); exp.Status != postgresql.ExperimentComplete {
		t.Errorf("status = %s", exp.Status)
	}
}

// ── the judge ───────────────────────────────────────────────────────────────

type judgeStub struct {
	mu     sync.Mutex
	calls  []judge.Request
	answer func(judge.Request) (string, error)
	// per is what a call costs; est what PriceUSD says one may cost (default per).
	per float64
	est *float64
}

func (j *judgeStub) Complete(_ context.Context, r judge.Request) (judge.Reply, error) {
	j.mu.Lock()
	j.calls = append(j.calls, r)
	j.mu.Unlock()
	text, err := j.answer(r)
	return judge.Reply{Text: text, CostUSD: j.per}, err
}

func (j *judgeStub) PriceUSD(int, int) float64 {
	if j.est != nil {
		return *j.est
	}
	return j.per
}

func (j *judgeStub) setCost(v float64) { j.per, j.est = v, &v }

func (j *judgeStub) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.calls)
}

type stubResolver struct {
	providers map[string]judge.Provider
	callers   []tenancy.Caller
	namespace []string
}

func (s *stubResolver) Resolve(_ context.Context, c tenancy.Caller, namespace, ref string) (judge.Provider, error) {
	s.callers, s.namespace = append(s.callers, c), append(s.namespace, namespace)
	if p, ok := s.providers[ref]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: no provider %q", judge.ErrProvider, ref)
}

// verdict answers like an honest judge: candidates containing "GOOD" pass, and the canary's right answer.
func honestJudge(per float64) *judgeStub {
	return &judgeStub{per: per, answer: func(r judge.Request) (string, error) {
		if strings.Contains(r.System, "compare two candidate outputs") {
			a, b := strings.Index(r.User, "GOOD"), strings.Index(r.User, "BAD")
			winner := "B"
			if a >= 0 && (b < 0 || a < b) {
				winner = "A"
			}
			return fmt.Sprintf(`{"rationale":"x","winner":%q}`, winner), nil
		}
		if strings.Contains(r.User, "GOOD") || strings.Contains(r.User, "capital of France is Paris") {
			return `{"rationale":"right","score":1,"pass":true}`, nil
		}
		return `{"rationale":"wrong","score":0,"pass":false}`, nil
	}}
}

// runToEnd finishes the runs and ticks until the experiment leaves running.
func (e *env) runToEnd(output, spend string) {
	e.t.Helper()
	for range 10 {
		e.finishRuns(phaseSucceeded, output, spend, "")
		e.tick()
		if e.store.exp(1).Status != postgresql.ExperimentRunning {
			return
		}
	}
	e.t.Fatalf("the experiment is still running: %+v", e.store.exp(1))
}

func llmGrader(mode string, required bool) postgresql.EvalGrader {
	const id = 31
	return postgresql.EvalGrader{ID: id, DatasetID: 7, Name: fmt.Sprintf("judge-%d", id), Kind: "llm", Weight: 1, Required: required,
		Config: fmt.Appendf(nil, `{"providerRef":"judge","rubric":"the answer is right","mode":%q}`, mode)}
}

func newJudgeEnv(t *testing.T, p judge.Provider) (*env, *stubResolver) {
	e := newEnv(t, opts{})
	res := &stubResolver{providers: map[string]judge.Provider{"judge": p}}
	e.r.cfg.Judges = res
	return e, res
}

func TestLLMGraderGradesRunsAndItsCostIsInTheExperimentSpend(t *testing.T) {
	p := honestJudge(0.01)
	e, res := newJudgeEnv(t, p)
	e.store.graders = []postgresql.EvalGrader{llmGrader("rubric", true)}
	e.store.add(oneArm(1))
	e.tick() // starts: the canary runs
	if p.count() != 3 {
		t.Fatalf("canary calls = %d, want 3", p.count())
	}
	if e.store.exp(1).Status != postgresql.ExperimentRunning || e.store.canary[1] < 0.0299 || e.store.canary[1] > 0.0301 {
		t.Fatalf("after the canary: %+v, canary spend %v", e.store.exp(1), e.store.canary[1])
	}
	if got := res.callers[0]; got.TenantID != tenant || got.TenantName != "acme" || len(got.Namespaces) != 1 || got.Namespaces[0] != ns || res.namespace[0] != ns {
		t.Errorf("the judge was resolved for %+v in %v", got, res.namespace)
	}

	e.runToEnd("GOOD", "0.05")
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentComplete {
		t.Fatalf("status = %s", exp.Status)
	}
	// 4 runs x 0.05 + canary 0.03 + 4 judge calls x 0.01.
	if exp.SpentUSD < 0.2699 || exp.SpentUSD > 0.2701 {
		t.Errorf("spent = %v, want 0.27", exp.SpentUSD)
	}
	for s, grades := range e.store.grades[1] {
		if len(grades) != 1 || grades[0].JudgeCostUSD != 0.01 || grades[0].Status != "ok" || !grades[0].Pass || grades[0].Detail != "right" {
			t.Errorf("%v: grades = %+v", s, grades)
		}
		if !e.store.results[1][s].CasePass {
			t.Errorf("%v: the case did not pass", s)
		}
	}
	var sum Summary
	if err := json.Unmarshal(exp.Summary, &sum); err != nil || sum.JudgeSpentUSD < 0.0399 || sum.JudgeSpentUSD > 0.0401 || len(sum.Graders) != 1 || sum.Graders[0].Graded != 4 {
		t.Errorf("summary = %s, %v", exp.Summary, err)
	}
}

func TestInvalidJudgeRepliesFailTheCaseAndCostMoney(t *testing.T) {
	var canaryDone bool
	p := &judgeStub{per: 0.01}
	p.answer = func(r judge.Request) (string, error) {
		if strings.Contains(r.User, "capital of France") {
			return honestJudge(0).answer(r)
		}
		canaryDone = true
		return "I would give it a 9 out of 10", nil
	}
	e, _ := newJudgeEnv(t, p)
	e.store.graders = []postgresql.EvalGrader{llmGrader("rubric", true), containsGrader()}
	e.store.add(oneArm(1))
	e.tick()
	e.finishRuns(phaseSucceeded, "all ok", "0.05", "")
	e.tick()
	if !canaryDone || e.store.exp(1).Status != postgresql.ExperimentComplete {
		t.Fatalf("status = %s", e.store.exp(1).Status)
	}
	for s, grades := range e.store.grades[1] {
		for _, g := range grades {
			if g.GraderID == 31 && (g.Status != postgresql.GradeInvalid || g.Pass || g.Score != 0 || g.JudgeCostUSD != 0.01) {
				t.Errorf("%v: judge grade = %+v", s, g)
			}
		}
		if e.store.results[1][s].CasePass {
			t.Errorf("%v: a case with an invalid required judge grade passes", s)
		}
	}
}

func TestBudgetStopsTheJudgeNotJustTheRuns(t *testing.T) {
	p := honestJudge(0.04)
	p.setCost(0.04)
	e, _ := newJudgeEnv(t, p)
	e.store.graders = []postgresql.EvalGrader{llmGrader("rubric", false), containsGrader()}
	x := oneArm(1)
	x.BudgetUSD = 0.5
	e.store.add(x)
	p.setCost(0.04)
	e.tick() // canary: 3 x 0.04 = 0.12
	e.runToEnd("all ok GOOD", "0.1")
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentComplete {
		t.Fatalf("status = %s", exp.Status)
	}
	if exp.SpentUSD > 0.5+1e-9 {
		t.Errorf("spent %v is over the budget of 0.5", exp.SpentUSD)
	}
	var calls, refused int
	for _, grades := range e.store.grades[1] {
		for _, g := range grades {
			if g.GraderID != 31 {
				continue
			}
			if g.Status == postgresql.GradeError && strings.Contains(g.Detail, "budget") {
				refused++
				if g.JudgeCostUSD != 0 {
					t.Errorf("a refused call cost %v", g.JudgeCostUSD)
				}
			} else {
				calls++
			}
		}
	}
	if refused == 0 || calls == 0 {
		t.Errorf("judged %d, refused %d: the budget should allow some and then stop", calls, refused)
	}
}

func TestTenantDailyBudgetBoundsTheJudge(t *testing.T) {
	p := honestJudge(0)
	e, _ := newJudgeEnv(t, p)
	tc := agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: agentorcav1alpha1.TenantConfigSpec{TenantID: tenant, BudgetPerDayUSD: "5"}}
	_ = e.kube.Delete(context.Background(), &agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "acme"}})
	if err := e.kube.Create(context.Background(), &tc); err != nil {
		t.Fatal(err)
	}
	e.store.graders = []postgresql.EvalGrader{llmGrader("rubric", false), containsGrader()}
	x := oneArm(1)
	x.BudgetUSD = 3
	e.store.add(x)
	e.tick() // starts: the canary costs nothing here
	// Today's other spend leaves little of the 5: 4 of production and the runs' 0.4.
	prod := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns, Labels: labelled(tenant), CreationTimestamp: metav1.NewTime(e.now)},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: "4"}}
	if err := e.kube.Create(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	p.setCost(0.3)
	e.runToEnd("all ok", "0.1")
	var judged, refused int
	for _, grades := range e.store.grades[1] {
		for _, g := range grades {
			switch {
			case g.GraderID != 31:
			case g.Status == postgresql.GradeError && strings.Contains(g.Detail, "daily budget"):
				refused++
			case g.Status == "ok":
				judged++
			}
		}
	}
	// The budget of the experiment lets three runs start, and what is left of the
	// day after them (0.7) fits two calls of 0.3, not the third.
	if judged != 2 || refused != 1 {
		t.Errorf("judged %d, refused %d; want 2 and 1", judged, refused)
	}
}

func TestJudgeFailsTheExperimentBeforeAnyRun(t *testing.T) {
	cases := map[string]struct {
		resolver JudgeResolver
		answer   func(judge.Request) (string, error)
		want     string
	}{
		"no judge on the operator": {nil, nil, "no judge is configured"},
		"unknown provider":         {&stubResolver{providers: map[string]judge.Provider{}}, nil, "no provider"},
		"always passes":            {nil, func(judge.Request) (string, error) { return `{"rationale":"x","score":1,"pass":true}`, nil }, "canary"},
		"unusable replies":         {nil, func(judge.Request) (string, error) { return "hmm", nil }, "canary"},
	}
	for name, c := range cases {
		e := newEnv(t, opts{})
		switch {
		case c.answer != nil:
			e.r.cfg.Judges = &stubResolver{providers: map[string]judge.Provider{"judge": &judgeStub{per: 0.01, answer: c.answer}}}
		case c.resolver != nil:
			e.r.cfg.Judges = c.resolver
		}
		e.store.graders = []postgresql.EvalGrader{llmGrader("rubric", false)}
		e.store.add(oneArm(1))
		e.tick()
		exp := e.store.exp(1)
		if exp.Status != postgresql.ExperimentFailed || !strings.Contains(exp.Error, c.want) {
			t.Errorf("%s: %+v", name, exp)
		}
		if got := len(e.runs()); got != 0 {
			t.Errorf("%s: %d runs were created", name, got)
		}
		if name == "always passes" && e.store.canary[1] <= 0 {
			t.Errorf("%s: the cost of the failed canary was not counted", name)
		}
	}
}

func TestCanaryIsPaidOncePerExperiment(t *testing.T) {
	p := honestJudge(0.01)
	e, _ := newJudgeEnv(t, p)
	e.store.graders = []postgresql.EvalGrader{llmGrader("rubric", false)}
	e.store.add(oneArm(1))
	e.tick()
	n := p.count()
	// The runs are in flight: nothing asks the judge again until they finish.
	e.tick()
	if p.count() != n || n != 3 {
		t.Errorf("judge calls = %d after two ticks, want the canary's 3", p.count())
	}
}

func TestReferenceModeOnACaseWithoutAReferenceIsNotGraded(t *testing.T) {
	p := honestJudge(0.01)
	e, _ := newJudgeEnv(t, p)
	e.store.cases[1].Reference = ""
	e.store.graders = []postgresql.EvalGrader{llmGrader("reference", false), containsGrader()}
	e.store.add(oneArm(1))
	e.tick()
	e.finishRuns(phaseSucceeded, "all ok GOOD", "0.01", "")
	e.tick()
	for s, grades := range e.store.grades[1] {
		for _, g := range grades {
			if g.GraderID == 31 && (s.caseID == 102) != g.NotGraded {
				t.Errorf("%v: grade %+v", s, g)
			}
			if g.GraderID == 31 && g.NotGraded && (g.Pass || g.JudgeCostUSD != 0) {
				t.Errorf("a not graded judge grade passes or costs: %+v", g)
			}
		}
	}
}

// ── pairwise ────────────────────────────────────────────────────────────────

func outputsByArm(e *env, base, cand string) {
	e.t.Helper()
	for _, run := range e.runs() {
		if isTerminal(&run) {
			continue
		}
		out := base
		if run.Labels[tenancy.LabelEvalArm] == "cand" {
			out = cand
		}
		run.Status.Phase, run.Status.Output, run.Status.SpendUSD = phaseSucceeded, out, "0.01"
		now := metav1.NewTime(e.now)
		run.Status.StartTime, run.Status.CompletionTime = &now, &now
		if err := e.kube.Update(context.Background(), &run); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestPairwiseGradesBothArmsAfterTheRunsAreIn(t *testing.T) {
	p := honestJudge(0.01)
	e, _ := newJudgeEnv(t, p)
	e.store.graders = []postgresql.EvalGrader{llmGrader("pairwise", false)}
	e.store.add(oneArm(1))
	e.tick()
	before := p.count()
	outputsByArm(e, "GOOD answer", "BAD answer")
	e.tick()
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentComplete {
		t.Fatalf("status = %s", exp.Status)
	}
	// 2 cases, two orderings each.
	if got := p.count() - before; got != 4 {
		t.Errorf("pairwise judge calls = %d, want 4 (two per case, positions swapped)", got)
	}
	for _, c := range []int64{101, 102} {
		a, b := e.store.grades[1][slot{c, "base", 0}], e.store.grades[1][slot{c, "cand", 0}]
		if len(a) != 1 || len(b) != 1 || a[0].Score != 1 || b[0].Score != 0 || !a[0].Pass || b[0].Pass || a[0].Inconsistent {
			t.Errorf("case %d: base %+v, cand %+v", c, a, b)
		}
		if a[0].JudgeCostUSD != 0.02 || b[0].JudgeCostUSD != 0 {
			t.Errorf("case %d: the pair's cost is on the first arm only: %v %v", c, a[0].JudgeCostUSD, b[0].JudgeCostUSD)
		}
	}
	// 4 runs, the canary and 4 pair calls.
	if exp.SpentUSD < 0.1099 || exp.SpentUSD > 0.1101 {
		t.Errorf("spent = %v, want 0.04 + 0.03 + 0.04", exp.SpentUSD)
	}
}

func TestPairwisePositionBiasIsAnInconsistentTie(t *testing.T) {
	canary := honestJudge(0)
	p := &judgeStub{per: 0.01}
	p.answer = func(r judge.Request) (string, error) {
		if strings.Contains(r.System, "compare two candidate outputs") {
			return `{"rationale":"the first one","winner":"A"}`, nil
		}
		return canary.answer(r)
	}
	e, _ := newJudgeEnv(t, p)
	e.store.graders = []postgresql.EvalGrader{llmGrader("pairwise", false)}
	e.store.add(oneArm(1))
	e.tick()
	outputsByArm(e, "one", "two")
	e.tick()
	for _, c := range []int64{101, 102} {
		for _, arm := range []string{"base", "cand"} {
			g := e.store.grades[1][slot{c, arm, 0}][0]
			if !g.Inconsistent || g.Score != 0.5 || g.Pass {
				t.Errorf("case %d arm %s: %+v", c, arm, g)
			}
		}
	}
}

func TestPairwiseNeedsExactlyTwoArms(t *testing.T) {
	e, _ := newJudgeEnv(t, honestJudge(0))
	e.store.graders = []postgresql.EvalGrader{llmGrader("pairwise", false)}
	x := oneArm(1)
	x.Arms = append(x.Arms, postgresql.EvalArm{Label: "third", Agent: "support"})
	e.store.add(x)
	e.tick()
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentFailed || !strings.Contains(exp.Error, "two arms") {
		t.Errorf("experiment = %+v", exp)
	}
}

func TestPairwiseSkipsAResultWhoseOtherArmFailed(t *testing.T) {
	e, _ := newJudgeEnv(t, honestJudge(0.01))
	e.store.graders = []postgresql.EvalGrader{llmGrader("pairwise", false), containsGrader()}
	e.store.add(oneArm(1))
	e.tick()
	for _, run := range e.runs() {
		if run.Labels[tenancy.LabelEvalArm] == "cand" {
			run.Status.Phase, run.Status.FailureReason = "Failed", "the agent gave up"
		} else {
			run.Status.Phase, run.Status.Output, run.Status.SpendUSD = phaseSucceeded, "all ok", "0.01"
		}
		if err := e.kube.Update(context.Background(), &run); err != nil {
			t.Fatal(err)
		}
	}
	e.tick()
	g := e.store.grades[1][slot{101, "base", 0}]
	var pair postgresql.EvalGrade
	for _, x := range g {
		if x.GraderID == 31 {
			pair = x
		}
	}
	if !pair.NotGraded || pair.Pass || pair.JudgeCostUSD != 0 {
		t.Errorf("pairwise grade without a partner = %+v", pair)
	}
}

// ── human review ────────────────────────────────────────────────────────────

func humanGrader(id int64, required bool) postgresql.EvalGrader {
	return postgresql.EvalGrader{ID: id, DatasetID: 7, Name: "reviewer", Kind: "human", Weight: 1, Required: required, Config: []byte(`{"scale":"passfail"}`)}
}

func TestHumanGraderPutsTheExperimentInReviewUntilLabelled(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = []postgresql.EvalGrader{containsGrader(), humanGrader(41, true)}
	e.store.add(oneArm(1))
	e.tick()
	e.finishRuns(phaseSucceeded, "all ok", "0.05", "")
	e.tick()
	exp := e.store.exp(1)
	if exp.Status != postgresql.ExperimentReview {
		t.Fatalf("status = %s, want awaiting_review", exp.Status)
	}
	if contains(e.names(&agentorcav1alpha1.AgentList{}), "eval-1-base") {
		t.Errorf("the arm objects were not cleaned up for the review stage")
	}
	var sum Summary
	if err := json.Unmarshal(exp.Summary, &sum); err != nil || sum.ReviewPending != 4 || sum.Results != 4 {
		t.Fatalf("interim summary = %s, %v", exp.Summary, err)
	}
	for s, grades := range e.store.grades[1] {
		for _, g := range grades {
			if g.GraderID == 41 && (g.Status != postgresql.GradePendingReview || g.Pass) {
				t.Errorf("%v: human grade = %+v", s, g)
			}
		}
		if e.store.results[1][s].CasePass {
			t.Errorf("%v: a case with a required human grade pending passes", s)
		}
	}
	// A pending review keeps the experiment waiting however many ticks pass.
	e.tick()
	e.tick()
	if e.store.exp(1).Status != postgresql.ExperimentReview {
		t.Fatalf("status = %s", e.store.exp(1).Status)
	}
	e.store.label(1, 3)
	e.tick()
	if e.store.exp(1).Status != postgresql.ExperimentReview {
		t.Fatalf("one item is still pending: %s", e.store.exp(1).Status)
	}
	e.store.label(1, 1)
	e.tick()
	if exp := e.store.exp(1); exp.Status != postgresql.ExperimentComplete || exp.Summary == nil {
		t.Errorf("after the last label: %+v", exp)
	}
}

func TestHumanGraderWithoutRunsToReviewCompletes(t *testing.T) {
	e := newEnv(t, opts{})
	e.store.graders = []postgresql.EvalGrader{containsGrader(), humanGrader(41, false)}
	e.store.add(oneArm(1))
	e.tick()
	// Every run fails: nothing was graded, so no review is asked for.
	e.finishRuns("Failed", "", "0", "the agent could not finish")
	e.tick()
	if st := e.store.exp(1).Status; st != postgresql.ExperimentComplete {
		t.Errorf("status = %s", st)
	}
}
