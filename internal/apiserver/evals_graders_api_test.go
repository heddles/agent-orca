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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	containsRefund = `{"kind":"contains","config":{"value":"refund"}}`
	exactKind      = `{"kind":"exact","config":{"normalize":["trim"]},"required":true}`
)

// ── fake store (grader half) ────────────────────────────────────────────────

func (f *fakeEvalStore) dataset(ctx context.Context, id int64) (*postgresql.EvalDataset, error) {
	d, ok := f.datasets[id]
	if !ok || !f.visible(ctx, d.TenantID) {
		return nil, postgresql.ErrEvalNotFound
	}
	return d, nil
}

func (f *fakeEvalStore) UpsertEvalGrader(ctx context.Context, datasetID int64, g postgresql.EvalGrader) (postgresql.EvalGrader, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := g.Validate(); err != nil {
		return g, false, err
	}
	d, err := f.dataset(ctx, datasetID)
	if err != nil {
		return g, false, err
	}
	for _, o := range f.graders {
		if o.DatasetID == datasetID && o.Name == g.Name {
			o.Kind, o.Config, o.Weight, o.Required = g.Kind, g.Config, g.Weight, g.Required
			return *o, false, nil
		}
	}
	f.next++
	g.ID, g.TenantID, g.DatasetID = f.next, d.TenantID, datasetID
	f.graders[g.ID] = &g
	return g, true, nil
}

func (f *fakeEvalStore) ListEvalGraders(ctx context.Context, datasetID int64) ([]postgresql.EvalGrader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.dataset(ctx, datasetID); err != nil {
		return nil, err
	}
	out := []postgresql.EvalGrader{}
	for _, g := range f.graders {
		if g.DatasetID == datasetID {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeEvalStore) GetEvalGrader(ctx context.Context, datasetID int64, name string) (postgresql.EvalGrader, error) {
	list, err := f.ListEvalGraders(ctx, datasetID)
	for _, g := range list {
		if g.Name == name {
			return g, nil
		}
	}
	if err == nil {
		err = postgresql.ErrEvalNotFound
	}
	return postgresql.EvalGrader{}, err
}

func (f *fakeEvalStore) DeleteEvalGrader(ctx context.Context, datasetID int64, name string) (postgresql.EvalGrader, error) {
	g, err := f.GetEvalGrader(ctx, datasetID, name)
	if err != nil {
		return g, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.graders, g.ID)
	return g, nil
}

func (f *fakeEvalStore) SaveEvalResult(ctx context.Context, datasetID int64, r postgresql.EvalResult, grades []postgresql.EvalGrade) (postgresql.EvalResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := r.Validate(); err != nil {
		return r, err
	}
	d, err := f.dataset(ctx, datasetID)
	if err != nil {
		return r, err
	}
	f.next++
	r.ID, r.TenantID, r.DatasetID, r.Grades = f.next, d.TenantID, datasetID, grades
	f.results[r.ID] = &r
	return r, nil
}

func (f *fakeEvalStore) GetEvalResult(ctx context.Context, id int64) (postgresql.EvalResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[id]
	if !ok || !f.visible(ctx, r.TenantID) {
		return postgresql.EvalResult{}, postgresql.ErrEvalNotFound
	}
	return *r, nil
}

func (f *fakeEvalStore) ListEvalResults(ctx context.Context, datasetID int64, q postgresql.EvalResultQuery) ([]postgresql.EvalResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.dataset(ctx, datasetID); err != nil {
		return nil, err
	}
	out := []postgresql.EvalResult{}
	for _, r := range f.results {
		if r.DatasetID == datasetID && r.ID > q.After {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// ── tests ───────────────────────────────────────────────────────────────────

func (f *evalFixture) put(id *TenantIdentity, ds int64, name, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(id, http.MethodPut, fmt.Sprintf("/api/evals/datasets/%d/graders/%s", ds, name), body)
}

func TestEvalGraders_CRUDIsAuditedWithoutOutputs(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("graded")
	base := fmt.Sprintf("/api/evals/datasets/%d/graders", d.ID)
	if rec := f.put(f.builder(), d.ID, "has-refund", containsRefund); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec := f.put(f.builder(), d.ID, "has-refund", `{"kind":"contains","config":{"value":"Refund","caseInsensitive":true},"weight":2}`); rec.Code != http.StatusOK {
		t.Fatalf("replace = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(f.builder(), http.MethodGet, base+"/has-refund", "")
	g := evalDecode[postgresql.EvalGrader](t, rec)
	if rec.Code != http.StatusOK || g.Weight != 2 || !strings.Contains(string(g.Config), "caseInsensitive") {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	if rec = f.do(f.builder(), http.MethodGet, base, ""); !strings.Contains(rec.Body.String(), "has-refund") {
		t.Errorf("list = %s", rec.Body)
	}
	if rec = f.do(f.builder(), http.MethodDelete, base+"/has-refund", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec = f.do(f.builder(), http.MethodGet, base+"/has-refund", ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d", rec.Code)
	}
	if rec = f.do(f.builder(), http.MethodDelete, base+"/has-refund", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d", rec.Code)
	}
	// dataset create + grader create, replace, delete.
	if len(f.audit.events) != 4 {
		t.Fatalf("audit = %+v", f.audit.events)
	}
	want := []string{postgresql.AuditCreate, postgresql.AuditCreate, postgresql.AuditUpdate, postgresql.AuditDelete}
	for i, e := range f.audit.events {
		if e.Action != want[i] {
			t.Errorf("event %d action = %s", i, e.Action)
		}
		if i > 0 && (e.Kind != kindEvalGrader || e.Name != "graded/has-refund" || e.UserID != builderUser) {
			t.Errorf("event %d = %+v", i, e)
		}
	}
}

func TestEvalGraders_BadDefinitionsAre400(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("bad")
	for name, body := range map[string]string{
		"unknown kind":    `{"kind":"llm","config":{}}`,
		"bad regex":       `{"kind":"regex","config":{"pattern":"("}}`,
		"lookahead":       `{"kind":"regex","config":{"pattern":"(?=a)b"}}`,
		"unknown field":   `{"kind":"contains","config":{"value":"a"},"bogus":1}`,
		"config field":    `{"kind":"contains","config":{"value":"a","bogus":1}}`,
		"remote ref":      `{"kind":"json_schema","config":{"schema":{"$ref":"http://127.0.0.1:1/x"}}}`,
		"negative weight": `{"kind":"contains","config":{"value":"a"},"weight":-1}`,
		"no kind":         `{"config":{}}`,
		"not json":        `nope`,
	} {
		if rec := f.put(f.builder(), d.ID, "g", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.put(f.builder(), d.ID, "Bad_Name", containsRefund); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name = %d", rec.Code)
	}
	if rec := f.put(f.builder(), d.ID, "big", `{"kind":"regex","config":{"pattern":"`+strings.Repeat("a", 2000)+`"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("long pattern = %d", rec.Code)
	}
	if rec := f.put(f.builder(), d.ID, "huge", `{"kind":"contains","config":{"value":"`+strings.Repeat("a", 300<<10)+`"}}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body = %d", rec.Code)
	}
	if len(f.store.graders) != 0 || len(f.audit.events) != 1 {
		t.Errorf("refused writes left %d graders and %d audit events", len(f.store.graders), len(f.audit.events))
	}
}

func TestEvalGraders_Roles(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("roles-g")
	f.put(f.builder(), d.ID, "g", containsRefund)
	viewer, runner := idFor(tenancy.RoleViewer, isoA), idFor(tenancy.RoleRunner, isoA)
	base := fmt.Sprintf("/api/evals/datasets/%d", d.ID)
	grade := `{"output":"a refund","costUsd":0.01,"latencyMs":5}`
	before := len(f.audit.events)

	for _, c := range []struct {
		who          *TenantIdentity
		method, path string
		body         string
		want         int
	}{
		{viewer, http.MethodGet, base + "/graders", "", http.StatusOK},
		{viewer, http.MethodGet, base + "/graders/g", "", http.StatusOK},
		{viewer, http.MethodGet, base + "/results", "", http.StatusOK},
		{viewer, http.MethodPut, base + "/graders/g", containsRefund, http.StatusForbidden},
		{viewer, http.MethodDelete, base + "/graders/g", "", http.StatusForbidden},
		{viewer, http.MethodPost, base + "/grade", grade, http.StatusForbidden},
		{runner, http.MethodPut, base + "/graders/g", containsRefund, http.StatusForbidden},
		{runner, http.MethodDelete, base + "/graders/g", "", http.StatusForbidden},
		{runner, http.MethodPost, base + "/grade", grade, http.StatusCreated},
	} {
		rec := f.do(c.who, c.method, c.path, c.body)
		if rec.Code != c.want {
			t.Errorf("%s %s %s = %d, want %d", c.who.Roles, c.method, c.path, rec.Code, c.want)
		}
	}
	if len(f.audit.events) != before+1 {
		t.Errorf("only the runner's grade is audited; events = %d", len(f.audit.events)-before)
	}
}

func TestEvalGrade_GradesAndKeepsOneResult(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("grading")
	cases := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID), `{"input":"q","reference":"Refund issued"}`)
	c := evalDecode[addResult](t, cases).Cases[0]
	f.put(f.builder(), d.ID, "exact-ref", exactKind)
	f.put(f.builder(), d.ID, "no-secret", `{"kind":"not_contains","config":{"value":"secret"}}`)
	f.put(f.builder(), d.ID, "used-tool", `{"kind":"tool_called","config":{"tool":"lookup"}}`)
	f.put(f.builder(), d.ID, "cheap", `{"kind":"max_cost","config":{"usd":0.05}}`)
	url := fmt.Sprintf("/api/evals/datasets/%d/grade", d.ID)
	before := len(f.audit.events)

	body := fmt.Sprintf(`{"caseId":%d,"arm":"v2","output":" Refund issued ","toolCalls":["lookup"],"costUsd":0.01,"latencyMs":100}`, c.ID)
	rec := f.do(idFor(tenancy.RoleRunner, isoA), http.MethodPost, url, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("grade = %d %s", rec.Code, rec.Body)
	}
	res := evalDecode[postgresql.EvalResult](t, rec)
	if !res.CasePass || len(res.Grades) != 4 || res.Arm != "v2" || res.CaseID == nil || *res.CaseID != c.ID {
		t.Fatalf("result = %+v", res)
	}
	for _, g := range res.Grades {
		if !g.Pass || g.Score != 1 || g.Detail == "" {
			t.Errorf("grade = %+v", g)
		}
	}
	// A required grader that fails fails the case, whatever the mean.
	rec = f.do(f.builder(), http.MethodPost, url, fmt.Sprintf(`{"caseId":%d,"output":"nope","costUsd":0.01}`, c.ID))
	res = evalDecode[postgresql.EvalResult](t, rec)
	if res.CasePass {
		t.Errorf("a failed required grader passed the case: %+v", res.Grades)
	}
	// Without a reference the exact grader is not graded, and, being required, fails the case.
	rec = f.do(f.builder(), http.MethodPost, url, `{"output":"x"}`)
	res = evalDecode[postgresql.EvalResult](t, rec)
	var ng bool
	for _, g := range res.Grades {
		ng = ng || (g.Grader == "exact-ref" && g.NotGraded && !g.Pass)
	}
	if !ng || res.CasePass {
		t.Errorf("not-graded result = %+v", res)
	}
	// Audit rows say what and how many, never the output.
	for _, e := range f.audit.events[before:] {
		if e.Kind != kindEvalResult || strings.Contains(e.Summary, "Refund") || strings.Contains(e.Summary, "nope") {
			t.Errorf("audit = %+v", e)
		}
	}
	if len(f.audit.events) != before+3 || len(f.store.results) != 3 {
		t.Errorf("audit events %d, stored %d", len(f.audit.events)-before, len(f.store.results))
	}
	// Read back.
	got := f.do(idFor(tenancy.RoleViewer, isoA), http.MethodGet, fmt.Sprintf("/api/evals/results/%d", res.ID), "")
	if got.Code != http.StatusOK || evalDecode[postgresql.EvalResult](t, got).ID != res.ID {
		t.Errorf("get result = %d %s", got.Code, got.Body)
	}
	list := f.do(idFor(tenancy.RoleViewer, isoA), http.MethodGet, fmt.Sprintf("/api/evals/datasets/%d/results?limit=2", d.ID), "")
	page := evalDecode[resultPage](t, list)
	if len(page.Results) != 2 || page.NextCursor == "" {
		t.Errorf("page = %+v", page)
	}
}

func TestEvalGrade_Refusals(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("refusals")
	other := f.dataset("other-set")
	url := fmt.Sprintf("/api/evals/datasets/%d/grade", d.ID)
	runner := idFor(tenancy.RoleRunner, isoA)
	if rec := f.do(runner, http.MethodPost, url, `{"output":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("no graders = %d", rec.Code)
	}
	f.put(f.builder(), d.ID, "g", containsRefund)
	oc := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", other.ID), `{"input":"q"}`)
	foreign := evalDecode[addResult](t, oc).Cases[0]
	for name, body := range map[string]string{
		"case of another dataset": fmt.Sprintf(`{"caseId":%d,"output":"x"}`, foreign.ID),
		"unknown case":            `{"caseId":99999,"output":"x"}`,
	} {
		if rec := f.do(runner, http.MethodPost, url, body); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d", name, rec.Code)
		}
	}
	for name, body := range map[string]string{
		"case and reference": fmt.Sprintf(`{"caseId":%d,"reference":"r","output":"x"}`, foreign.ID),
		"unknown field":      `{"output":"x","bogus":1}`,
		"negative cost":      `{"output":"x","costUsd":-1}`,
		"negative latency":   `{"output":"x","latencyMs":-1}`,
	} {
		if rec := f.do(runner, http.MethodPost, url, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do(runner, http.MethodPost, url, `{"output":"`+strings.Repeat("a", 300<<10)+`"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("output over the limit = %d", rec.Code)
	}
	if rec := f.do(runner, http.MethodGet, url, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET grade = %d", rec.Code)
	}
	if len(f.store.results) != 0 {
		t.Errorf("refused requests stored %d results", len(f.store.results))
	}
}

func TestEvalGraders_ForeignTenantAndNamespaceAre404(t *testing.T) {
	f := newEvalFixture(t)
	b := idFor(tenancy.RoleAdmin, isoB)
	rec := f.do(b, http.MethodPost, "/api/evals/datasets", `{"name":"b-graded"}`)
	dB := evalDecode[postgresql.EvalDataset](t, rec)
	f.put(b, dB.ID, "g", containsRefund)
	res := evalDecode[postgresql.EvalResult](t, f.do(b, http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/grade", dB.ID), `{"output":"refund"}`))
	base := fmt.Sprintf("/api/evals/datasets/%d", dB.ID)
	a := idFor(tenancy.RoleAdmin, isoA)
	before := len(f.audit.events)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base + "/graders", ""}, {http.MethodGet, base + "/graders/g", ""},
		{http.MethodPut, base + "/graders/g", containsRefund}, {http.MethodPut, base + "/graders/new", containsRefund},
		{http.MethodDelete, base + "/graders/g", ""}, {http.MethodPost, base + "/grade", `{"output":"x"}`},
		{http.MethodGet, base + "/results", ""}, {http.MethodGet, fmt.Sprintf("/api/evals/results/%d", res.ID), ""},
	} {
		if rec := f.do(a, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("A %s %s = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if len(f.audit.events) != before || len(f.store.graders) != 1 || len(f.store.results) != 1 {
		t.Error("a refused write changed or audited something")
	}

	// A dataset of tenant A in a namespace the caller may not use.
	f.store.datasets[99] = &postgresql.EvalDataset{ID: 99, TenantID: isoA, Namespace: "a-secret", Name: "d", Version: 1}
	f.store.graders[100] = &postgresql.EvalGrader{ID: 100, DatasetID: 99, TenantID: isoA, Name: "g", Kind: "contains"}
	f.store.results[101] = &postgresql.EvalResult{ID: 101, DatasetID: 99, TenantID: isoA}
	for _, p := range []string{"/api/evals/datasets/99/graders", "/api/evals/datasets/99/graders/g", "/api/evals/datasets/99/results", "/api/evals/results/101"} {
		if rec := f.do(f.builder(), http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := f.put(f.builder(), 99, "g", containsRefund); rec.Code != http.StatusNotFound {
		t.Errorf("PUT in a foreign namespace = %d", rec.Code)
	}
}

func TestEvalGraders_RoutesRejectBadPaths(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("paths")
	base := fmt.Sprintf("/api/evals/datasets/%d", d.ID)
	for _, p := range []string{base + "/graders/a/b", base + "/grade/x", base + "/results/1", "/api/evals/results/", "/api/evals/results/x", "/api/evals/results/0"} {
		if rec := f.do(f.builder(), http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := f.do(f.builder(), http.MethodPost, base+"/graders", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST graders = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/results/1", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST result = %d", rec.Code)
	}
}

func TestEvalGraders_OpenAPIDescribesTheEndpoints(t *testing.T) {
	raw, err := uiOpenAPIJSONBytes()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"/api/evals/datasets/{id}/graders":        {"get"},
		"/api/evals/datasets/{id}/graders/{name}": {"get", "put", "delete"},
		"/api/evals/datasets/{id}/grade":          {"post"},
		"/api/evals/datasets/{id}/results":        {"get"},
		"/api/evals/results/{id}":                 {"get"},
	}
	for path, methods := range want {
		for _, m := range methods {
			if _, ok := doc.Paths[path][m]; !ok {
				t.Errorf("openapi-ui.yaml lacks %s %s", strings.ToUpper(m), path)
			}
		}
	}
	for _, name := range []string{"EvalGrader", "EvalGraderInput", "EvalGradeRequest", "EvalGrade", "EvalResult"} {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schema %s is not defined", name)
		}
	}
}

func TestEvalGraders_503WithoutAStore(t *testing.T) {
	f := newEvalFixture(t)
	f.s.evals = nil
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/evals/datasets/1/graders"}, {http.MethodPost, "/api/evals/datasets/1/grade"},
		{http.MethodGet, "/api/evals/results/1"},
	} {
		if rec := f.do(idFor(tenancy.RoleAdmin, isoA), c.method, c.path, `{}`); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d", c.method, c.path, rec.Code)
		}
	}
}
