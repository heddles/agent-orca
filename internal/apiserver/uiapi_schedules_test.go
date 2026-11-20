package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const schedPath = "/api/schedules/shared/digest"

func uiSchedule(mod func(*agentorcav1alpha1.AgentSchedule)) *agentorcav1alpha1.AgentSchedule {
	s := &agentorcav1alpha1.AgentSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "digest", Namespace: sharedNS,
			Labels:      map[string]string{tenancy.LabelTenantID: isoA},
			Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI},
		},
		Spec: agentorcav1alpha1.AgentScheduleSpec{
			AgentRef: helperAgent, Schedule: "0 8 * * 1-5", TimeZone: "Europe/Berlin", Input: "digest for {{schedule.date}}",
			Budget: &agentorcav1alpha1.RunBudget{MaxUSD: "1"},
		},
	}
	if mod != nil {
		mod(s)
	}
	return s
}

func schedAgentObj() *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: helperAgent, Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}}
}

// act sends a request to the schedule routes.
func (f *writeFixture) act(id *TenantIdentity, method, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *writeFixture) sched(name string) *agentorcav1alpha1.AgentSchedule {
	f.t.Helper()
	var s agentorcav1alpha1.AgentSchedule
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: name}, &s); err != nil {
		f.t.Fatal(err)
	}
	return &s
}

func TestScheduleActionRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		min          tenancy.Role
	}{
		{http.MethodGet, "/api/schedules", tenancy.RoleViewer},
		{http.MethodGet, schedPath + "/runs", tenancy.RoleViewer},
		{http.MethodGet, "/api/schedules/preview?cron=0+8+*+*+*&tz=UTC", tenancy.RoleViewer},
		{http.MethodPost, schedPath + "/suspend", tenancy.RoleRunner},
		{http.MethodPost, schedPath + "/resume", tenancy.RoleRunner},
		{http.MethodPost, schedPath + "/run", tenancy.RoleRunner},
	} {
		for _, role := range allRoles {
			f := newWriteFixture(t, uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Suspend = true }), schedAgentObj())
			rec := f.act(f.as(role), tc.method, tc.path)
			if denied, want := isRoleDenial(rec), !role.Includes(tc.min); denied != want {
				t.Errorf("%s %s as %s: %d %s; role denial = %v, want %v", tc.method, tc.path, role, rec.Code, rec.Body, denied, want)
			}
		}
	}
	// CRUD needs the builder.
	f := newWriteFixture(t)
	for _, role := range allRoles {
		rec := f.do(f.as(role), http.MethodPost, "/api/resources/agentschedules", `{}`)
		if denied, want := isRoleDenial(rec), !role.Includes(tenancy.RoleBuilder); denied != want {
			t.Errorf("create as %s: %d %s", role, rec.Code, rec.Body)
		}
	}
}

func TestSuspendAndResumeAreAuditedUIEdits(t *testing.T) {
	f := newWriteFixture(t, uiSchedule(nil), schedAgentObj())
	if rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/suspend"); rec.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", rec.Code, rec.Body)
	}
	s := f.sched("digest")
	if !s.Spec.Suspend || s.Spec.Schedule != "0 8 * * 1-5" || s.Spec.Budget == nil {
		t.Fatalf("suspend lost or changed other fields: %+v", s.Spec)
	}
	if s.Annotations[tenancy.AnnotationLastModifiedBy] != builderUser {
		t.Errorf("annotations = %v", s.Annotations)
	}
	if rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/suspend"); !strings.Contains(rec.Body.String(), "unchanged") {
		t.Errorf("a second suspend = %s", rec.Body)
	}
	if rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/resume"); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body)
	}
	if f.sched("digest").Spec.Suspend {
		t.Fatal("still suspended")
	}
	actions := make([]string, 0, len(f.audit.events))
	for _, e := range f.audit.events {
		if e.Kind != "AgentSchedule" || e.UserID != builderUser {
			t.Errorf("audit = %+v", e)
		}
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != postgresql.AuditSuspend+","+postgresql.AuditResume {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestResumeOfADegradedScheduleStampsTheResumeAnnotation(t *testing.T) {
	f := newWriteFixture(t, uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) {
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: agentorcav1alpha1.ScheduleConditionDegraded, Status: metav1.ConditionTrue, Reason: "x"})
	}), schedAgentObj())
	if rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/resume"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "unchanged") {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body)
	}
	if _, err := time.Parse(time.RFC3339, f.sched("digest").Annotations[schedule.AnnotationResumedAt]); err != nil {
		t.Fatalf("no resume annotation: %v", f.sched("digest").Annotations)
	}
}

func TestGitOwnedScheduleCannotBeSuspendedOrEditedHere(t *testing.T) {
	git := uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Annotations = map[string]string{tenancy.AnnotationOwner: tenancy.OwnerGit}
	})
	f := newWriteFixture(t, git, schedAgentObj())
	rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/suspend")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Git") {
		t.Fatalf("suspend of a Git-owned schedule = %d %s", rec.Code, rec.Body)
	}
	if f.sched("digest").Spec.Suspend {
		t.Fatal("suspended anyway")
	}
	body := `{"metadata":{"resourceVersion":"` + f.sched("digest").ResourceVersion + `"},"spec":{"agentRef":"helper","schedule":"0 9 * * *","input":"x"}}`
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPut, "/api/resources/agentschedules/shared/digest", body); rec.Code != http.StatusConflict {
		t.Fatalf("PUT of a Git-owned schedule = %d", rec.Code)
	}
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodDelete, "/api/resources/agentschedules/shared/digest", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE of a Git-owned schedule = %d", rec.Code)
	}
	// It is listed, read-only.
	rec = f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules")
	var list []scheduleSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].Ownership.Owner != tenancy.OwnerGit || list[0].Ownership.Editable {
		t.Fatalf("list = %s", rec.Body)
	}
}

func TestScheduleStaleResourceVersionIs409(t *testing.T) {
	f := newWriteFixture(t, uiSchedule(nil), schedAgentObj())
	body := `{"metadata":{"resourceVersion":"1"},"spec":{"agentRef":"helper","schedule":"0 9 * * *","input":"x"}}`
	f.sched("digest")
	// Bump the stored version.
	if rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/suspend"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPut, "/api/resources/agentschedules/shared/digest", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "currentResourceVersion") {
		t.Fatalf("stale PUT = %d %s", rec.Code, rec.Body)
	}
}

func TestScheduleEgressSecretNeedsAdmin(t *testing.T) {
	f := newWriteFixture(t)
	body := `{"metadata":{"name":"s","namespace":"shared"},"spec":{"agentRef":"helper","schedule":"0 8 * * *","input":"x",
	  "result":{"egress":{"type":"kafka","topic":"t","secretRef":{"name":"kafka","key":"k"}}}}}`
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agentschedules", body); rec.Code != http.StatusForbidden {
		t.Fatalf("builder created a schedule with an egress secret: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodPost, "/api/resources/agentschedules", body); rec.Code != http.StatusCreated {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	plain := strings.Replace(body, `"name":"s"`, `"name":"plain"`, 1)
	plain = strings.Replace(plain, `,"secretRef":{"name":"kafka","key":"k"}`, "", 1)
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agentschedules", plain); rec.Code != http.StatusCreated {
		t.Fatalf("builder without a secret: %d %s", rec.Code, rec.Body)
	}
	// A builder may edit a schedule whose admin-set secret stays as it is.
	rv := f.sched("s").ResourceVersion
	keep := strings.Replace(body, `"metadata":{`, `"metadata":{"resourceVersion":"`+rv+`",`, 1)
	keep = strings.Replace(keep, `"input":"x"`, `"input":"y"`, 1)
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPut, "/api/resources/agentschedules/shared/s", keep); rec.Code != http.StatusOK {
		t.Fatalf("builder edit keeping the secret: %d %s", rec.Code, rec.Body)
	}
}

func TestRunNowCreatesARunStartedByThePersonAndAudits(t *testing.T) {
	f := newWriteFixture(t, uiSchedule(nil), schedAgentObj())
	rec := f.act(f.as(tenancy.RoleRunner), http.MethodPost, schedPath+"/run")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	var runs agentorcav1alpha1.AgentRunList
	if err := f.c.List(context.Background(), &runs, client.InNamespace(sharedNS)); err != nil || len(runs.Items) != 1 {
		t.Fatalf("runs = %v %v", runs.Items, err)
	}
	r := runs.Items[0]
	if r.Labels[schedule.LabelSchedule] != "digest" || r.Labels[tenancy.LabelTenantID] != isoA || r.Annotations[schedule.AnnotationCreatedBy] != builderUser ||
		len(r.OwnerReferences) != 0 || r.Spec.AgentRef != helperAgent || strings.Contains(r.Spec.Input, "{{") || r.Spec.Budget == nil {
		t.Fatalf("run = %+v", r)
	}
	last := f.audit.events[len(f.audit.events)-1]
	if last.Action != postgresql.AuditRequest || last.Kind != "AgentRun" || last.Name != r.Name {
		t.Fatalf("audit = %+v", last)
	}
	// It is "manual" in the history, and shows in the run list.
	rec = f.act(f.as(tenancy.RoleViewer), http.MethodGet, schedPath+"/runs")
	if !strings.Contains(rec.Body.String(), `"trigger":"manual"`) {
		t.Fatalf("history = %s", rec.Body)
	}
	rec = f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/runs?schedule=digest")
	if !strings.Contains(rec.Body.String(), `"trigger":"manual"`) || !strings.Contains(rec.Body.String(), `"schedule":"digest"`) {
		t.Fatalf("runs = %s", rec.Body)
	}
	if rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/runs?schedule=other"); strings.Contains(rec.Body.String(), "digest") {
		t.Fatalf("schedule filter leaked: %s", rec.Body)
	}
}

func TestRunNowRespectsQuotasAndTheAllowlist(t *testing.T) {
	busy := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: sharedNS, Labels: map[string]string{schedule.LabelSchedule: "other", tenancy.LabelTenantID: isoA}},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
	f := newWriteFixture(t, uiSchedule(nil), schedAgentObj(), busy)
	id := f.as(tenancy.RoleRunner)
	id.ConcurrentRuns, id.TenantName = 1, "a"
	if rec := f.act(id, http.MethodPost, schedPath+"/run"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the concurrent-run cap = %d %s: scheduled runs count", rec.Code, rec.Body)
	}
	id.ConcurrentRuns, id.AllowedAgents = 0, []string{"someone-else"}
	if rec := f.act(id, http.MethodPost, schedPath+"/run"); rec.Code != http.StatusForbidden {
		t.Fatalf("agent outside the allowlist = %d", rec.Code)
	}
}

func TestScheduleOfAnotherTenantIsNotFound(t *testing.T) {
	other := uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) { s.Labels[tenancy.LabelTenantID] = isoB })
	f := newWriteFixture(t, other, schedAgentObj())
	for _, p := range []struct{ m, path string }{{http.MethodPost, schedPath + "/run"}, {http.MethodPost, schedPath + "/suspend"}, {http.MethodGet, schedPath + "/runs"}} {
		if rec := f.act(f.as(tenancy.RoleAdmin), p.m, p.path); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", p.m, p.path, rec.Code)
		}
	}
	rec := f.act(f.as(tenancy.RoleAdmin), http.MethodGet, "/api/schedules")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list leaked: %s", rec.Body)
	}
}

func TestScheduleListSummarisesState(t *testing.T) {
	now := metav1.NewTime(time.Now())
	next := metav1.NewTime(time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC))
	done := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "digest-1", Namespace: sharedNS, Labels: map[string]string{schedule.LabelSchedule: "digest", tenancy.LabelTenantID: isoA}},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseFailed, CompletionTime: &now},
	}
	f := newWriteFixture(t, uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Status.NextScheduleTime = &next
		s.Status.SpentTodayUSD = "0.7500"
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: agentorcav1alpha1.ScheduleConditionBudgetExhausted, Status: metav1.ConditionTrue, Reason: "x", Message: "today's cap reached"})
	}), done)
	rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules")
	var list []scheduleSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %s", rec.Body)
	}
	g := list[0]
	if g.State != "budgetExhausted" || g.StateReason != "today's cap reached" || g.LastResult.Status != outcomeFailed || g.SpentTodayUSD != "0.7500" ||
		g.NextRun != "2026-11-16T07:00:00Z" || g.NextRunLocal != "2026-11-16T08:00:00+01:00" || g.TargetKind != "agent" {
		t.Fatalf("summary = %+v", g)
	}
}

func TestSchedulePreview(t *testing.T) {
	f := newWriteFixture(t)
	rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules/preview?cron=0+8+*+*+1-5&tz=Europe/Berlin&count=3")
	var got struct {
		NextRuns []struct{ UTC, Local string } `json:"nextRuns"`
		Tooo     bool                          `json:"tooFrequent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.NextRuns) != 3 || got.Tooo || !strings.Contains(got.NextRuns[0].Local, "T08:00:00") {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	if rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules/preview?cron=*+*+*+*+*&tz=UTC"); !strings.Contains(rec.Body.String(), `"tooFrequent":true`) {
		t.Fatalf("every minute: %s", rec.Body)
	}
	for _, q := range []string{"cron=nope&tz=UTC", "cron=0+8+*+*+*&tz=Mars/X"} {
		if rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules/preview?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", q, rec.Code)
		}
	}
}

func TestScheduledRunsAreNotInMyRuns(t *testing.T) {
	mk := func(name, by string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}, Annotations: map[string]string{AnnotationCreatedBy: by}},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: helperAgent, Input: "x"},
		}
	}
	f := newWriteFixture(t, mk("mine", builderUser), mk("sched", schedule.Actor(sharedNS, "digest")))
	rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/my/runs")
	if !strings.Contains(rec.Body.String(), `"mine"`) || strings.Contains(rec.Body.String(), `"sched"`) {
		t.Fatalf("my runs = %s", rec.Body)
	}
	// Not even in the tenant-wide fallback.
	f2 := newWriteFixture(t, mk("sched", schedule.Actor(sharedNS, "digest")))
	if rec := f2.act(f2.as(tenancy.RoleViewer), http.MethodGet, "/api/my/runs"); strings.Contains(rec.Body.String(), `"sched"`) {
		t.Fatalf("fallback shows scheduled runs: %s", rec.Body)
	}
}

func TestRunDetailNamesItsSchedule(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "digest-1", Namespace: sharedNS, Labels: map[string]string{schedule.LabelSchedule: "digest", tenancy.LabelTenantID: isoA},
			Annotations: map[string]string{AnnotationCreatedBy: schedule.Actor(sharedNS, "digest"), schedule.AnnotationScheduledFor: "2026-11-16T08:00:00+01:00"},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: helperAgent, Input: "x"},
	}
	f := newWriteFixture(t, run)
	rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/runs/shared/digest-1")
	for _, want := range []string{`"schedule":"digest"`, `"trigger":"schedule"`, `"scheduledFor":"2026-11-16T08:00:00+01:00"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("run detail missing %s: %s", want, rec.Body)
		}
	}
}
