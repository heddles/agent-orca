package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const draftSecretText = "DRAFT-PROMPT-TEXT-XYZZY"

func draftBody(datasetID int64, name string, extra string) string {
	return fmt.Sprintf(`{"datasetId":%d%s,"agent":{"apiVersion":"agentorca.agentorca.io/v1alpha1","kind":"Agent",
	"metadata":{"name":%q,"namespace":%q},"spec":{"systemPrompt":%q,"modelSelectorRef":"default","tools":["search"],"runtime":{"ociRef":"registry.example/a:1"}}}}`,
		datasetID, extra, name, sharedNS, draftSecretText)
}

func TestBuilderCheck_RunsADraftWithoutStoringIt(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("my-cases")
	before := f.auditCount()
	rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "new-bot", ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	got := evalDecode[checkResponse](t, rec)
	e := got.Experiment
	if !got.Valid || e.Purpose != postgresql.PurposeCheck || len(e.Arms) != 1 || e.Arms[0].Label != checkDraftArm || len(e.Arms[0].Spec) == 0 || got.ComparedWithPublished {
		t.Fatalf("check = %+v", got)
	}
	// The mandatory caps: a small default budget, the case and concurrency caps.
	if e.BudgetUSD != 0.25 || e.MaxCases != 25 || e.Concurrency != 2 || !got.Capped {
		t.Errorf("caps = budget %v maxCases %d concurrency %d", e.BudgetUSD, e.MaxCases, e.Concurrency)
	}
	// Nothing was saved: not the agent, and no revision or deployment.
	var a agentorcav1alpha1.Agent
	if err := f.s.crdClient.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "new-bot"}, &a); err == nil {
		t.Error("the draft was stored as an Agent")
	}
	// One audit row, with names and counts and no draft or case text.
	if f.auditCount() != before+1 {
		t.Fatalf("audit rows = %d, want %d", f.auditCount(), before+1)
	}
	ev := f.audit.events[len(f.audit.events)-1]
	if ev.Kind != kindEvalExperiment || ev.Action != postgresql.AuditCreate || !strings.Contains(ev.Summary, "check of a draft") {
		t.Errorf("audit = %+v", ev)
	}
	for _, text := range []string{draftSecretText, "secret question", "secret answer", "new-bot"} {
		if strings.Contains(ev.Summary, text) {
			t.Errorf("audit summary %q holds %q", ev.Summary, text)
		}
	}
	// A check is not in the experiments list a person browses.
	list := evalDecode[struct{ Experiments []experimentView }](t, f.do(f.builder(), http.MethodGet, "/api/evals/experiments", ""))
	if len(list.Experiments) != 0 {
		t.Errorf("a check was listed: %+v", list)
	}
	if rec := f.do(f.builder(), http.MethodGet, fmt.Sprintf("/api/evals/experiments/%d", e.ID), ""); rec.Code != http.StatusOK {
		t.Errorf("get = %d", rec.Code)
	}
}

func TestBuilderCheck_ComparesWithThePublishedAgent(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("my-cases")
	mine := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA},
			Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}},
		Spec: agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", Tools: []string{"search"}},
	}
	if err := f.s.crdClient.Create(context.Background(), mine); err != nil {
		t.Fatal(err)
	}
	// Saved but not published: only the draft runs.
	rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "mine", ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	if got := evalDecode[checkResponse](t, rec); got.ComparedWithPublished || len(got.Experiment.Arms) != 1 {
		t.Errorf("unpublished: %+v", got)
	}
	dep := &agentorcav1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "mine-live", Namespace: sharedNS}, Spec: agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "mine"}}
	if err := f.s.crdClient.Create(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	rec = f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "mine", ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	got := evalDecode[checkResponse](t, rec)
	arms := got.Experiment.Arms
	if !got.ComparedWithPublished || len(arms) != 2 || arms[0].Label != checkLiveArm || len(arms[0].Spec) != 0 || arms[1].Label != checkDraftArm || len(arms[1].Spec) == 0 {
		t.Errorf("published: %+v", got)
	}
	// The editor can turn the comparison off.
	f.exps.exps = map[int64]*postgresql.EvalExperiment{}
	rec = f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "mine", `,"compareWithPublished":false`))
	if got := evalDecode[checkResponse](t, rec); got.ComparedWithPublished || len(got.Experiment.Arms) != 1 {
		t.Errorf("opted out: %+v", got)
	}
	// The saved agent is untouched.
	var after agentorcav1alpha1.Agent
	if err := f.s.crdClient.Get(context.Background(), client.ObjectKeyFromObject(mine), &after); err != nil || after.Spec.SystemPrompt != "" {
		t.Errorf("the saved agent changed: %v %+v", err, after.Spec)
	}
}

func TestBuilderCheck_InvalidDraftReturnsErrorsAndQueuesNothing(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("my-cases")
	before := f.auditCount()
	for name, body := range map[string]string{
		"wrong shape": fmt.Sprintf(`{"datasetId":%d,"agent":{"metadata":{"name":"x","namespace":%q},"spec":{"tools":"no"}}}`, d.ID, sharedNS),
	} {
		rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", body)
		var res struct {
			Valid  *bool    `json:"valid"`
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if rec.Code != http.StatusBadRequest || res.Valid == nil || *res.Valid || len(res.Errors) == 0 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(f.exps.exps) != 0 || f.auditCount() != before {
		t.Errorf("an invalid draft queued %d experiments, %d audit rows", len(f.exps.exps), f.auditCount()-before)
	}
}

func TestBuilderCheck_RefusesGitOwnedAndMissingReferences(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("my-cases")
	owned := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "from-git", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA},
			Annotations: map[string]string{tenancy.AnnotationOwner: "git"}},
	}
	if err := f.s.crdClient.Create(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "from-git", "")); rec.Code != http.StatusConflict {
		t.Errorf("Git-owned = %d %s", rec.Code, rec.Body)
	}
	// A draft that names a tool the workspace does not have is a 422 with the missing list.
	body := strings.Replace(draftBody(d.ID, "needs-tool", ""), `"search"`, `"nope"`, 1)
	if rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", body); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing reference = %d %s", rec.Code, rec.Body)
	}
	if len(f.exps.exps) != 0 {
		t.Errorf("queued %d", len(f.exps.exps))
	}
}

func TestBuilderCheck_RolesTenantsAndCaps(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("my-cases")
	body := draftBody(d.ID, "new-bot", "")
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		if rec := f.do(idFor(role, isoA), http.MethodPost, "/api/builder/check", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d", role, rec.Code)
		}
	}
	if rec := f.do(f.builder(), http.MethodGet, "/api/builder/check", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	// Another tenant cannot use this tenant's dataset.
	if rec := f.do(idFor(tenancy.RoleBuilder, isoB), http.MethodPost, "/api/builder/check", strings.Replace(body, sharedNS, "b-only", 1)); rec.Code != http.StatusNotFound {
		t.Errorf("foreign dataset = %d %s", rec.Code, rec.Body)
	}
	// The budget is capped; a draft for another workspace than the dataset's is refused.
	if rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "new-bot", `,"budgetUsd":50`)); rec.Code != http.StatusBadRequest {
		t.Errorf("over budget = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", draftBody(d.ID, "new-bot", `,"budgetUsd":1.5`)); rec.Code != http.StatusCreated {
		t.Errorf("budget 1.5 = %d %s", rec.Code, rec.Body)
	} else if got := evalDecode[checkResponse](t, rec); got.Experiment.BudgetUSD != 1.5 {
		t.Errorf("budget = %v", got.Experiment.BudgetUSD)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", `{"agent":{"metadata":{"name":"x","namespace":"shared"}}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no dataset = %d", rec.Code)
	}
	// The configured caps replace the defaults.
	f.s.SetBuilder(BuilderConfig{CheckDefaultBudgetUSD: 0.1, CheckMaxCases: 3, CheckMaxConcurrency: 1})
	f.exps.exps = map[int64]*postgresql.EvalExperiment{}
	rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", body)
	if got := evalDecode[checkResponse](t, rec); got.Experiment.BudgetUSD != 0.1 || got.Experiment.MaxCases != 3 || got.Experiment.Concurrency != 1 {
		t.Errorf("configured caps = %+v", got.Experiment)
	}
}

func TestBuilderCheck_PerUserRateLimit(t *testing.T) {
	f := newExpFixture(t)
	d := f.graded("my-cases")
	f.s.SetBuilder(BuilderConfig{CheckPerUserPerHour: 2})
	body := draftBody(d.ID, "new-bot", "")
	for i := range 2 {
		f.exps.exps = map[int64]*postgresql.EvalExperiment{}
		if rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", body); rec.Code != http.StatusCreated {
			t.Fatalf("check %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.do(f.builder(), http.MethodPost, "/api/builder/check", body)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("third = %d %s", rec.Code, rec.Body)
	}
	// Another user has their own allowance.
	other := f.builder()
	other.UserID = "someone-else"
	f.exps.exps = map[int64]*postgresql.EvalExperiment{}
	if rec := f.do(other, http.MethodPost, "/api/builder/check", body); rec.Code != http.StatusCreated {
		t.Errorf("other user = %d %s", rec.Code, rec.Body)
	}
}

func TestWindowLimiter(t *testing.T) {
	var l windowLimiter
	now := time.Now()
	for range 3 {
		if _, ok := l.allow("k", 3, now); !ok {
			t.Fatal("refused within the limit")
		}
	}
	if wait, ok := l.allow("k", 3, now.Add(time.Minute)); ok || wait != 59*time.Minute {
		t.Errorf("over the limit: ok=%v wait=%v", ok, wait)
	}
	if _, ok := l.allow("k", 3, now.Add(61*time.Minute)); !ok {
		t.Error("the window did not slide")
	}
}
