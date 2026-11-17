package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeReviewStore is an in-memory review queue. The rules of the real store (the
// scoped reads, one label per item, the blind query) are tested against
// PostgreSQL in internal/postgresql; here the handlers' roles, scoping, audit
// and what they show are tested.
type fakeReviewStore struct {
	*fakeExperimentStore
	mu     sync.Mutex
	items  map[int64]*postgresql.EvalReviewItem
	tenant map[int64]string // item -> tenant
	pairs  []postgresql.EvalCalibrationPair
}

func newFakeReviewStore(exps *fakeExperimentStore) *fakeReviewStore {
	return &fakeReviewStore{fakeExperimentStore: exps, items: map[int64]*postgresql.EvalReviewItem{}, tenant: map[int64]string{}}
}

func (f *fakeReviewStore) add(tenant string, it postgresql.EvalReviewItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[it.ID], f.tenant[it.ID] = &it, tenant
}

func (f *fakeReviewStore) ListEvalReviews(ctx context.Context, experimentID int64, q postgresql.EvalReviewQuery) ([]postgresql.EvalReviewItem, error) {
	if _, err := f.GetEvalExperiment(ctx, experimentID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []postgresql.EvalReviewItem{}
	for id := int64(1); id <= int64(len(f.items)); id++ {
		if it := f.items[id]; it != nil && it.ExperimentID == experimentID && (q.Status == "" || q.Status == it.Status) {
			out = append(out, *it)
		}
	}
	return out, nil
}

func (f *fakeReviewStore) GetEvalReview(ctx context.Context, id int64) (postgresql.EvalReviewItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok || !f.visible(ctx, f.tenant[id]) {
		return postgresql.EvalReviewItem{}, postgresql.ErrEvalNotFound
	}
	return *it, nil
}

func (f *fakeReviewStore) SubmitEvalReview(ctx context.Context, id int64, label postgresql.EvalReviewLabel, reviewer string) (postgresql.EvalReviewItem, error) {
	if _, err := f.GetEvalReview(ctx, id); err != nil {
		return postgresql.EvalReviewItem{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[id]
	if it.Status != postgresql.ReviewPending {
		return *it, postgresql.ErrReviewClosed
	}
	if (it.Scale == "passfail") != (label.Pass != nil) {
		return *it, fmt.Errorf("%w: the label does not fit the %s scale", postgresql.ErrInvalidEval, it.Scale)
	}
	pass := label.Pass != nil && *label.Pass
	score := 0.0
	if pass {
		score = 1
	}
	it.Status, it.Reviewer, it.Pass, it.Score, it.Comment = postgresql.ReviewDone, reviewer, &pass, &score, label.Comment
	return *it, nil
}

func (f *fakeReviewStore) EvalReviewProgressOf(ctx context.Context, experimentID int64) (postgresql.EvalReviewProgress, error) {
	var p postgresql.EvalReviewProgress
	items, err := f.ListEvalReviews(ctx, experimentID, postgresql.EvalReviewQuery{})
	for _, it := range items {
		switch it.Status {
		case postgresql.ReviewPending:
			p.Pending++
		case postgresql.ReviewDone:
			p.Done++
		default:
			p.Closed++
		}
	}
	return p, err
}

func (f *fakeReviewStore) CloseEvalReview(ctx context.Context, experimentID int64) (int, error) {
	e, err := f.GetEvalExperiment(ctx, experimentID)
	if err != nil {
		return 0, err
	}
	if e.Status != postgresql.ExperimentReview {
		return 0, postgresql.ErrExperimentFinished
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, it := range f.items {
		if it.ExperimentID == experimentID && it.Status == postgresql.ReviewPending {
			it.Status = postgresql.ReviewClosed
			n++
		}
	}
	return n, nil
}

func (f *fakeReviewStore) EvalCalibrationPairs(ctx context.Context, datasetID int64, llm, human string, _ int64) ([]postgresql.EvalCalibrationPair, error) {
	if _, err := f.GetEvalDataset(ctx, datasetID); err != nil {
		return nil, err
	}
	if llm == "nope" {
		return nil, postgresql.ErrEvalNotFound
	}
	return f.pairs, nil
}

type reviewFixture struct {
	*expFixture
	reviews *fakeReviewStore
	exp     postgresql.EvalExperiment
	dataset postgresql.EvalDataset
}

const (
	secretOutput = "the refund is issued: SECRET-OUTPUT"
	armLabelBase = "arm-base-label"
)

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	f := newExpFixture(t)
	reviews := newFakeReviewStore(f.exps)
	f.s.evalReviews = reviews
	d := f.graded("reviewed")
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, strings.NewReplacer("base", armLabelBase).Replace(twoModelArms)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	e := evalDecode[experimentView](t, rec)
	f.exps.exps[e.ID].Status = postgresql.ExperimentReview
	for i := int64(1); i <= 3; i++ {
		reviews.add(isoA, postgresql.EvalReviewItem{ID: i, ExperimentID: e.ID, CaseID: 10 + i, Grader: "quality", Scale: "passfail",
			Instructions: "is it right?", PassAt: 0.75, Input: "what about my refund?", Output: secretOutput, Status: postgresql.ReviewPending})
	}
	return &reviewFixture{expFixture: f, reviews: reviews, exp: e.EvalExperiment, dataset: d}
}

func (f *reviewFixture) queue() string {
	return fmt.Sprintf("/api/evals/experiments/%d/review", f.exp.ID)
}

func TestReview_RolesViewerReadsRunnerLabelsBuilderCloses(t *testing.T) {
	f := newReviewFixture(t)
	viewer, runnerID, builder := idFor(tenancy.RoleViewer, isoA), idFor(tenancy.RoleRunner, isoA), f.builder()
	if rec := f.do(viewer, http.MethodGet, f.queue(), ""); rec.Code != http.StatusOK {
		t.Errorf("viewer reads the queue = %d", rec.Code)
	}
	if rec := f.do(viewer, http.MethodGet, "/api/evals/review/1", ""); rec.Code != http.StatusOK {
		t.Errorf("viewer reads an item = %d", rec.Code)
	}
	if rec := f.do(viewer, http.MethodPost, "/api/evals/review/1", `{"pass":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer labels = %d", rec.Code)
	}
	for _, id := range []*TenantIdentity{viewer, runnerID} {
		if rec := f.do(id, http.MethodPost, f.queue()+"/close", ""); rec.Code != http.StatusForbidden {
			t.Errorf("%v closes the review = %d", id.Roles, rec.Code)
		}
	}
	if rec := f.do(runnerID, http.MethodPost, "/api/evals/review/1", `{"pass":true,"comment":"yes"}`); rec.Code != http.StatusOK {
		t.Fatalf("runner labels = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(builder, http.MethodPost, "/api/evals/review/2", `{"pass":false}`); rec.Code != http.StatusOK {
		t.Errorf("builder labels = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(builder, http.MethodPost, f.queue()+"/close", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"closed":1`) {
		t.Errorf("builder closes = %d %s", rec.Code, rec.Body)
	}
}

func TestReview_QueueIsBlind(t *testing.T) {
	f := newReviewFixture(t)
	rec := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodGet, f.queue(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("queue = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, leak := range []string{armLabelBase, `"cheap"`, `"arm`, "repeat", "runName", "runNamespace", "costUsd", "toolCalls", "resultId", "modelProvider", "helper"} {
		if strings.Contains(body, leak) {
			t.Errorf("the review queue shows %q:\n%s", leak, body)
		}
	}
	page := evalDecode[reviewPage](t, rec)
	if page.Status != postgresql.ExperimentReview || page.Progress.Pending != 3 || len(page.Items) != 3 {
		t.Fatalf("page = %+v", page)
	}
	if it := page.Items[0]; it.Output != secretOutput || it.Instructions != "is it right?" || it.Scale != "passfail" || it.Input == "" {
		t.Errorf("item = %+v", it)
	}
	// An item is not the experiment: it names nothing about the arms either.
	one := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodGet, "/api/evals/review/1", "")
	if strings.Contains(one.Body.String(), armLabelBase) {
		t.Errorf("an item shows an arm label: %s", one.Body)
	}
	if rec := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodGet, f.queue()+"?status=done", ""); evalDecode[reviewPage](t, rec).Items == nil {
		t.Errorf("an empty page has no items array")
	}
	for _, bad := range []string{"?status=maybe", "?limit=0", "?limit=999"} {
		if rec := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodGet, f.queue()+bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", bad, rec.Code)
		}
	}
}

func TestReview_LabelIsAuditedByNameAndCountOnly(t *testing.T) {
	f := newReviewFixture(t)
	before := f.auditCount()
	rec := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodPost, "/api/evals/review/1", `{"pass":true,"comment":"PRIVATE-COMMENT"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("label = %d %s", rec.Code, rec.Body)
	}
	if f.auditCount() != before+1 {
		t.Fatalf("audit rows = %d", f.auditCount())
	}
	e := f.audit.events[len(f.audit.events)-1]
	if e.Kind != kindEvalReview || e.Action != postgresql.AuditCreate || e.Namespace != sharedNS || e.Name != fmt.Sprint(f.exp.ID) {
		t.Errorf("audit = %+v", e)
	}
	for _, text := range []string{"PRIVATE-COMMENT", "SECRET-OUTPUT", "my refund", armLabelBase} {
		if strings.Contains(e.Summary, text) {
			t.Errorf("the audit summary %q holds %q", e.Summary, text)
		}
	}
	if !strings.Contains(e.Summary, "quality") {
		t.Errorf("the audit summary %q does not name the grader", e.Summary)
	}
	rec = f.do(f.builder(), http.MethodPost, f.queue()+"/close", "")
	closed := f.audit.events[len(f.audit.events)-1]
	if rec.Code != http.StatusOK || closed.Action != postgresql.AuditUpdate || !strings.Contains(closed.Summary, "2 items") {
		t.Errorf("close audit = %+v (%d)", closed, rec.Code)
	}
}

func TestReview_OneLabelPerItemAndTheLabelMustFitTheScale(t *testing.T) {
	f := newReviewFixture(t)
	runnerID := idFor(tenancy.RoleRunner, isoA)
	if rec := f.do(runnerID, http.MethodPost, "/api/evals/review/1", `{"stars":3}`); rec.Code != http.StatusBadRequest {
		t.Errorf("stars on a passfail item = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(runnerID, http.MethodPost, "/api/evals/review/1", `{"pass":true,"verdict":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}
	if rec := f.do(runnerID, http.MethodPost, "/api/evals/review/1", `{"pass":true}`); rec.Code != http.StatusOK {
		t.Fatalf("label = %d", rec.Code)
	}
	if rec := f.do(runnerID, http.MethodPost, "/api/evals/review/1", `{"pass":false}`); rec.Code != http.StatusConflict {
		t.Errorf("second label = %d", rec.Code)
	}
	done := evalDecode[postgresql.EvalReviewItem](t, f.do(runnerID, http.MethodGet, "/api/evals/review/1", ""))
	if done.Status != postgresql.ReviewDone || done.Reviewer != builderUser || done.Pass == nil || !*done.Pass {
		t.Errorf("item = %+v", done)
	}
	for _, p := range []string{"/api/evals/review/0", "/api/evals/review/x", "/api/evals/review/99"} {
		if rec := f.do(runnerID, http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
	if rec := f.do(runnerID, http.MethodDelete, "/api/evals/review/1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d", rec.Code)
	}
}

func TestReview_IndependentReviewCanBeRequired(t *testing.T) {
	f := newReviewFixture(t)
	f.s.evalCfg.RequireIndependentReview = true
	// The experiment was started by builderUser; every tenant A identity of the
	// fixture is builderUser until a test names another.
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/review/1", `{"pass":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("the experiment's own starter labels = %d", rec.Code)
	}
	other := idFor(tenancy.RoleRunner, isoA)
	other.UserID = "rita"
	if rec := f.do(other, http.MethodPost, "/api/evals/review/1", `{"pass":true}`); rec.Code != http.StatusOK {
		t.Errorf("another person labels = %d %s", rec.Code, rec.Body)
	}
	f.s.evalCfg.RequireIndependentReview = false
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/review/2", `{"pass":true}`); rec.Code != http.StatusOK {
		t.Errorf("not required = %d", rec.Code)
	}
}

func TestReview_ForeignTenantSeesNothing(t *testing.T) {
	f := newReviewFixture(t)
	b := idFor(tenancy.RoleAdmin, isoB)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, f.queue(), ""},
		{http.MethodPost, f.queue() + "/close", ""},
		{http.MethodGet, "/api/evals/review/1", ""},
		{http.MethodPost, "/api/evals/review/1", `{"pass":true}`},
		{http.MethodGet, fmt.Sprintf("/api/evals/datasets/%d/calibration?llm=a&human=b", f.dataset.ID), ""},
	} {
		if rec := f.do(b, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("tenant B %s %s = %d", c.method, c.path, rec.Code)
		}
	}
	if it := f.reviews.items[1]; it.Status != postgresql.ReviewPending {
		t.Errorf("tenant A's item changed: %+v", it)
	}
}

func TestReview_ANamespaceTheCallerMayNotUseIsNotFound(t *testing.T) {
	f := newReviewFixture(t)
	f.exps.exps[f.exp.ID].Namespace = "elsewhere"
	runnerID := idFor(tenancy.RoleRunner, isoA)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, f.queue()}, {http.MethodGet, "/api/evals/review/1"}, {http.MethodPost, "/api/evals/review/1"},
	} {
		if rec := f.do(runnerID, c.method, c.path, `{"pass":true}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d", c.method, c.path, rec.Code)
		}
	}
}

func TestReview_NeedsThePostgresStoreAndExperimentsOn(t *testing.T) {
	f := newReviewFixture(t)
	f.s.evalReviews = nil
	f.s.pgStore = nil
	if rec := f.do(idFor(tenancy.RoleViewer, isoA), http.MethodGet, f.queue(), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store = %d", rec.Code)
	}
	f.s.evalReviews = f.reviews
	f.s.evalCfg.DisableExperiments = true
	if rec := f.do(idFor(tenancy.RoleViewer, isoA), http.MethodGet, f.queue(), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("experiments off = %d", rec.Code)
	}
}

func TestCalibration_ReportsAgreementAndNeedsEnoughPairs(t *testing.T) {
	f := newReviewFixture(t)
	url := fmt.Sprintf("/api/evals/datasets/%d/calibration?llm=judge&human=quality", f.dataset.ID)
	viewer := idFor(tenancy.RoleViewer, isoA)
	few := evalDecode[calibrationView](t, f.do(viewer, http.MethodGet, url, ""))
	if !few.TooFewPairs || few.MinPairs != 20 || few.Report.Disagreements == nil {
		t.Errorf("no pairs = %+v", few)
	}
	for i := range 30 {
		agree := i < 24
		f.reviews.pairs = append(f.reviews.pairs, postgresql.EvalCalibrationPair{ResultID: int64(i + 1), CaseID: int64(i + 1),
			LLMPass: i%2 == 0, HumanPass: (i%2 == 0) == agree, LLMScore: 0.9, HumanScore: 1})
	}
	got := evalDecode[calibrationView](t, f.do(viewer, http.MethodGet, url, ""))
	if got.TooFewPairs || got.Report.Pairs != 30 || got.Report.Agreement != 0.8 || !got.Report.KappaDefined || len(got.Report.Disagreements) != 6 {
		t.Errorf("report = %+v", got)
	}
	for path, want := range map[string]int{
		fmt.Sprintf("/api/evals/datasets/%d/calibration", f.dataset.ID):                                  http.StatusBadRequest,
		fmt.Sprintf("/api/evals/datasets/%d/calibration?llm=judge", f.dataset.ID):                        http.StatusBadRequest,
		fmt.Sprintf("/api/evals/datasets/%d/calibration?llm=judge&human=q&experimentId=x", f.dataset.ID): http.StatusBadRequest,
		fmt.Sprintf("/api/evals/datasets/%d/calibration?llm=nope&human=quality", f.dataset.ID):           http.StatusNotFound,
		"/api/evals/datasets/999/calibration?llm=a&human=b":                                              http.StatusNotFound,
	} {
		if rec := f.do(viewer, http.MethodGet, path, ""); rec.Code != want {
			t.Errorf("%s = %d, want %d", path, rec.Code, want)
		}
	}
	if rec := f.do(viewer, http.MethodPost, url, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

// ── graders and experiments with a judge ───────────────────────────────────

const (
	llmRubricGrader = `{"kind":"llm","config":{"providerRef":"cheap","rubric":"the answer is right"},"required":false}`
	llmPairGrader   = `{"kind":"llm","config":{"providerRef":"judge-model","rubric":"better answer wins","mode":"pairwise"}}`
	humanGraderBody = `{"kind":"human","config":{"scale":"1to5","instructions":"look at the tone"}}`
)

func TestGraders_LLMAndHumanConfigsAreCheckedOnWrite(t *testing.T) {
	f := newExpFixture(t)
	d := f.dataset("judged")
	put := func(name, body string) int { return f.put(f.builder(), d.ID, name, body).Code }
	if c := put("judge", llmRubricGrader); c != http.StatusCreated {
		t.Errorf("llm grader = %d", c)
	}
	if c := put("human", humanGraderBody); c != http.StatusCreated {
		t.Errorf("human grader = %d", c)
	}
	bad := map[string]string{
		"no provider":     `{"kind":"llm","config":{"rubric":"r"}}`,
		"no rubric":       `{"kind":"llm","config":{"providerRef":"p"}}`,
		"bad mode":        `{"kind":"llm","config":{"providerRef":"p","rubric":"r","mode":"vibes"}}`,
		"long rubric":     fmt.Sprintf(`{"kind":"llm","config":{"providerRef":"p","rubric":%q}}`, strings.Repeat("x", 4001)),
		"passAt over 1":   `{"kind":"llm","config":{"providerRef":"p","rubric":"r","passAt":1.5}}`,
		"repeats over 3":  `{"kind":"llm","config":{"providerRef":"p","rubric":"r","judgeRepeats":4}}`,
		"unknown field":   `{"kind":"llm","config":{"providerRef":"p","rubric":"r","tools":["x"]}}`,
		"bad human scale": `{"kind":"human","config":{"scale":"1to10"}}`,
		"negative sample": `{"kind":"human","config":{"sample":-1}}`,
		"human unknown":   `{"kind":"human","config":{"reviewer":"x"}}`,
		"too many tokens": `{"kind":"llm","config":{"providerRef":"p","rubric":"r","maxTokens":99999}}`,
	}
	for name, body := range bad {
		if c := put("bad", body); c != http.StatusBadRequest {
			t.Errorf("%s = %d", name, c)
		}
	}
}

func TestGrade_LLMAndHumanGradersAreNotGradedByTheGradeEndpoint(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("grade-judged")
	f.put(f.builder(), d.ID, "judge", llmRubricGrader)
	f.put(f.builder(), d.ID, "human", humanGraderBody)
	rec := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/grade", d.ID), `{"output":"a refund"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("grade = %d %s", rec.Code, rec.Body)
	}
	res := evalDecode[postgresql.EvalResult](t, rec)
	for _, g := range res.Grades {
		if (g.Kind == "llm" || g.Kind == "human") && (!g.NotGraded || g.Pass) {
			t.Errorf("%s grade = %+v: a judge or human grade must not be made up here", g.Kind, g)
		}
	}
}

func TestExperiments_JudgeChecksAtCreate(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("with-judge")
	f.put(f.builder(), d.ID, "judge", llmRubricGrader)
	// The judge is also the arm "cheap": created, with a warning.
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	got := evalDecode[experimentView](t, rec)
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], `"cheap"`) || !strings.Contains(got.Warnings[0], "favour its own output") {
		t.Errorf("warnings = %v", got.Warnings)
	}
	// A judge that is not an arm has no warning.
	judgeOnly := &agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "judge-only", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}}
	if err := f.s.crdClient.Create(context.Background(), judgeOnly); err != nil {
		t.Fatal(err)
	}
	d2 := f.graded("other-judge")
	f.put(f.builder(), d2.ID, "judge", `{"kind":"llm","config":{"providerRef":"judge-only","rubric":"r"}}`)
	rec = f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d2.ID, twoModelArms))
	if rec.Code != http.StatusCreated || len(evalDecode[experimentView](t, rec).Warnings) != 0 {
		t.Errorf("no judge arm = %d %s", rec.Code, rec.Body)
	}
	// A provider the workspace does not have is a 422.
	d3 := f.graded("missing-judge")
	f.put(f.builder(), d3.ID, "judge", `{"kind":"llm","config":{"providerRef":"ghost","rubric":"r"}}`)
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d3.ID, twoModelArms)); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "ghost") {
		t.Errorf("unknown judge provider = %d %s", rec.Code, rec.Body)
	}
	// Another tenant's provider is not usable.
	other := &agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "theirs", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoB}}}
	if err := f.s.crdClient.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	d4 := f.graded("foreign-judge")
	f.put(f.builder(), d4.ID, "judge", `{"kind":"llm","config":{"providerRef":"theirs","rubric":"r"}}`)
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d4.ID, twoModelArms)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("foreign judge provider = %d %s", rec.Code, rec.Body)
	}
}

func TestExperiments_PairwiseNeedsTwoArms(t *testing.T) {
	f := newExpFixture(t)
	big := &agentorcav1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}}
	if err := f.s.crdClient.Create(context.Background(), big); err != nil {
		t.Fatal(err)
	}
	d := f.graded("pairwise")
	f.put(f.builder(), d.ID, "pair", strings.ReplaceAll(llmPairGrader, "judge-model", "cheap"))
	three := `[{"label":"a","agent":"helper"},{"label":"b","agent":"helper","modelProvider":"cheap"},{"label":"c","agent":"helper","modelProvider":"big"}]`
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, three)); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "two arms") {
		t.Errorf("pairwise with three arms = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/experiments", expBody(d.ID, twoModelArms)); rec.Code != http.StatusCreated {
		t.Errorf("pairwise with two arms = %d %s", rec.Code, rec.Body)
	}
}
