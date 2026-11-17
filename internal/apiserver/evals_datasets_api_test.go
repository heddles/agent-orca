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
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeEvalStore is an in-memory evalStore that keeps rows per tenant, as the
// store's scope does, and follows the version rules of the real one (the real
// ones are tested against PostgreSQL in internal/postgresql).
type fakeEvalStore struct {
	mu       sync.Mutex
	next     int64
	datasets map[int64]*postgresql.EvalDataset
	cases    map[int64]*postgresql.EvalCase
	runs     map[string]*postgresql.RunArchive // tenant|ns/name
}

func newFakeEvalStore() *fakeEvalStore {
	return &fakeEvalStore{datasets: map[int64]*postgresql.EvalDataset{}, cases: map[int64]*postgresql.EvalCase{}, runs: map[string]*postgresql.RunArchive{}}
}

func scopeOf(ctx context.Context) string {
	s, _ := postgresql.ScopeFrom(ctx)
	return s
}

func (f *fakeEvalStore) visible(ctx context.Context, tenant string) bool {
	sc := scopeOf(ctx)
	return sc == postgresql.PlatformScope || sc == tenant
}

func (f *fakeEvalStore) live(id int64) int {
	n := 0
	for _, c := range f.cases {
		if c.DatasetID == id && c.RemovedIn == nil {
			n++
		}
	}
	return n
}

func (f *fakeEvalStore) CreateEvalDataset(ctx context.Context, d postgresql.EvalDataset) (postgresql.EvalDataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := d.Validate(); err != nil {
		return d, err
	}
	d.TenantID = scopeOf(ctx)
	for _, o := range f.datasets {
		if o.TenantID == d.TenantID && o.Namespace == d.Namespace && o.Name == d.Name {
			return d, postgresql.ErrEvalExists
		}
	}
	f.next++
	d.ID, d.Version = f.next, 1
	f.datasets[d.ID] = &d
	return d, nil
}

func (f *fakeEvalStore) GetEvalDataset(ctx context.Context, id int64) (postgresql.EvalDataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[id]
	if !ok || !f.visible(ctx, d.TenantID) {
		return postgresql.EvalDataset{}, postgresql.ErrEvalNotFound
	}
	out := *d
	out.CaseCount = f.live(id)
	return out, nil
}

func (f *fakeEvalStore) ListEvalDatasets(ctx context.Context, q postgresql.EvalDatasetQuery) ([]postgresql.EvalDataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []postgresql.EvalDataset{}
	for _, d := range f.datasets {
		if !f.visible(ctx, d.TenantID) || (q.Namespace != "" && d.Namespace != q.Namespace) {
			continue
		}
		if q.Namespaces != nil {
			in := false
			for _, ns := range q.Namespaces {
				in = in || ns == d.Namespace
			}
			if !in {
				continue
			}
		}
		out = append(out, *d)
	}
	return out, nil
}

func (f *fakeEvalStore) UpdateEvalDataset(ctx context.Context, id int64, p postgresql.EvalDatasetPatch) (postgresql.EvalDataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[id]
	if !ok || !f.visible(ctx, d.TenantID) {
		return postgresql.EvalDataset{}, postgresql.ErrEvalNotFound
	}
	if p.Description != nil {
		d.Description = *p.Description
	}
	if p.Agent != nil {
		d.Agent = *p.Agent
	}
	return *d, d.Validate()
}

func (f *fakeEvalStore) DeleteEvalDataset(ctx context.Context, id int64) (postgresql.EvalDataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[id]
	if !ok || !f.visible(ctx, d.TenantID) {
		return postgresql.EvalDataset{}, postgresql.ErrEvalNotFound
	}
	out := *d
	out.CaseCount = f.live(id)
	delete(f.datasets, id)
	for cid, c := range f.cases {
		if c.DatasetID == id {
			delete(f.cases, cid)
		}
	}
	return out, nil
}

func (f *fakeEvalStore) AddEvalCases(ctx context.Context, id int64, cases []postgresql.EvalCase, l postgresql.EvalLimits) ([]postgresql.EvalCase, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[id]
	if !ok || !f.visible(ctx, d.TenantID) {
		return nil, 0, postgresql.ErrEvalNotFound
	}
	if len(cases) == 0 {
		return nil, 0, fmt.Errorf("%w: no cases", postgresql.ErrInvalidEval)
	}
	max := l.MaxCasesPerDataset
	if max <= 0 {
		max = postgresql.DefaultMaxCasesPerDataset
	}
	if f.live(id)+len(cases) > max {
		return nil, 0, fmt.Errorf("%w: limit %d", postgresql.ErrDatasetFull, max)
	}
	out := make([]postgresql.EvalCase, len(cases))
	for i, c := range cases {
		if c.Weight == 0 {
			c.Weight = 1
		}
		if c.ReferenceKind == "" {
			c.ReferenceKind = postgresql.ReferenceNone
			if c.Reference != "" {
				c.ReferenceKind = postgresql.ReferenceExact
			}
		}
		if err := c.Validate(l); err != nil {
			return nil, 0, fmt.Errorf("case %d: %w", i+1, err)
		}
		out[i] = c
	}
	d.Version++
	for i := range out {
		f.next++
		out[i].ID, out[i].DatasetID, out[i].TenantID, out[i].AddedIn = f.next, id, d.TenantID, d.Version
		out[i].Namespace, out[i].DatasetName = d.Namespace, d.Name
		cp := out[i]
		f.cases[cp.ID] = &cp
	}
	return out, d.Version, nil
}

func (f *fakeEvalStore) ListEvalCases(ctx context.Context, id int64, q postgresql.EvalCaseQuery) ([]postgresql.EvalCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[id]
	if !ok || !f.visible(ctx, d.TenantID) {
		return nil, postgresql.ErrEvalNotFound
	}
	out := []postgresql.EvalCase{}
	for cid := int64(1); cid <= f.next; cid++ {
		c, ok := f.cases[cid]
		if !ok || c.DatasetID != id || c.RemovedIn != nil || cid <= q.After {
			continue
		}
		if q.Tag != "" {
			has := false
			for _, tg := range c.Tags {
				has = has || tg == q.Tag
			}
			if !has {
				continue
			}
		}
		out = append(out, *c)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeEvalStore) GetEvalCase(ctx context.Context, id int64) (postgresql.EvalCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cases[id]
	if !ok || c.RemovedIn != nil || !f.visible(ctx, c.TenantID) {
		return postgresql.EvalCase{}, postgresql.ErrEvalNotFound
	}
	return *c, nil
}

func (f *fakeEvalStore) UpdateEvalCase(ctx context.Context, id int64, p postgresql.EvalCasePatch, l postgresql.EvalLimits) (postgresql.EvalCase, int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cases[id]
	if !ok || c.RemovedIn != nil || !f.visible(ctx, c.TenantID) {
		return postgresql.EvalCase{}, 0, false, postgresql.ErrEvalNotFound
	}
	d := f.datasets[c.DatasetID]
	next := *c
	if p.Input != nil {
		next.Input = *p.Input
	}
	if p.Reference != nil {
		next.Reference = *p.Reference
		next.ReferenceKind = postgresql.ReferenceNone
		if next.Reference != "" {
			next.ReferenceKind = postgresql.ReferenceExact
		}
	}
	if p.Tags != nil {
		next.Tags = *p.Tags
	}
	if err := next.Validate(l); err != nil {
		return postgresql.EvalCase{}, 0, false, err
	}
	if next.Input == c.Input && next.Reference == c.Reference && strings.Join(next.Tags, ",") == strings.Join(c.Tags, ",") {
		return *c, d.Version, false, nil
	}
	d.Version++
	*c = next
	return *c, d.Version, true, nil
}

func (f *fakeEvalStore) DeleteEvalCase(ctx context.Context, id int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cases[id]
	if !ok || c.RemovedIn != nil || !f.visible(ctx, c.TenantID) {
		return 0, postgresql.ErrEvalNotFound
	}
	d := f.datasets[c.DatasetID]
	d.Version++
	v := d.Version
	c.RemovedIn = &v
	return v, nil
}

func (f *fakeEvalStore) GetRun(ctx context.Context, ns, name string) (*postgresql.RunArchive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[scopeOf(ctx)+"|"+ns+"/"+name]; ok {
		return r, nil
	}
	return nil, nil
}

// ── fixture ─────────────────────────────────────────────────────────────────

type evalFixture struct {
	t     *testing.T
	s     *UIServer
	store *fakeEvalStore
	audit *fakeAudit
	h     http.Handler
}

func evalRun(ns, name, tenant, input, output string, phase agentorcav1alpha1.AgentRunPhase) *agentorcav1alpha1.AgentRun {
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{tenancy.LabelTenantID: tenant}},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "support-bot", Input: input},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: phase, Output: output},
	}
}

func newEvalFixture(t *testing.T) *evalFixture {
	t.Helper()
	isoMode(t, tenancy.ModeEnforce)
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		evalRun("shared", "run-a", isoA, "mail ada@example.com about the refund", "Done, refund issued.", agentorcav1alpha1.AgentRunPhaseSucceeded),
		evalRun("shared", "run-live", isoA, "still going", "", agentorcav1alpha1.AgentRunPhaseRunning),
		evalRun("shared", "run-b", isoB, "tenant B's private input", "B output", agentorcav1alpha1.AgentRunPhaseSucceeded),
		evalRun("b-only", "run-b2", isoB, "B in B's namespace", "x", agentorcav1alpha1.AgentRunPhaseSucceeded),
		&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "support-bot", Namespace: "shared"},
			Spec:       agentorcav1alpha1.AgentSpec{GuardrailPolicyRef: "pii"},
		},
		&agentorcav1alpha1.GuardrailPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "pii", Namespace: "shared"},
			Spec: agentorcav1alpha1.GuardrailPolicySpec{
				InputFilters: []agentorcav1alpha1.GuardrailFilter{{
					Name: "ids", Type: "regex", Action: "redact",
					Patterns: []agentorcav1alpha1.RegexPattern{{Name: "order", Pattern: `ORD-\d{5}`, Replacement: "[ORDER]"}},
				}},
				OutputFilters: []agentorcav1alpha1.GuardrailFilter{{
					Name: "words", Type: "keyword-blocklist", Action: "warn",
					Keywords: &agentorcav1alpha1.KeywordConfig{Inline: []string{"Project Falcon"}},
				}},
			},
		},
	).Build()
	store, audit := newFakeEvalStore(), &fakeAudit{}
	s := &UIServer{crdClient: cl, evals: store, audit: audit}
	return &evalFixture{t: t, s: s, store: store, audit: audit, h: s.Handler()}
}

// idFor returns tenant A's identity with the given role, or tenant B's for isoB.
func idFor(role tenancy.Role, tenantID string) *TenantIdentity {
	if tenantID == isoB {
		return &TenantIdentity{Roles: role.Expand(), TenantID: isoB, TenantName: "globex", Namespace: "shared", Namespaces: []string{"shared", "b-only"}, UserID: "bob"}
	}
	id := tenantA()
	id.Roles, id.UserID = role.Expand(), builderUser
	return id
}

func (f *evalFixture) do(id *TenantIdentity, method, path, body string, header ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *evalFixture) builder() *TenantIdentity { return idFor(tenancy.RoleBuilder, isoA) }

func evalDecode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

// dataset creates a dataset as tenant A's builder and returns it.
func (f *evalFixture) dataset(name string) postgresql.EvalDataset {
	f.t.Helper()
	rec := f.do(f.builder(), http.MethodPost, "/api/evals/datasets", fmt.Sprintf(`{"namespace":"shared","name":%q}`, name))
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create dataset = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[postgresql.EvalDataset](f.t, rec)
}

func (f *evalFixture) datasetVersion(id int64) int {
	f.t.Helper()
	rec := f.do(f.builder(), http.MethodGet, fmt.Sprintf("/api/evals/datasets/%d", id), "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("get dataset = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[postgresql.EvalDataset](f.t, rec).Version
}

func (f *evalFixture) auditCount() int { return len(f.audit.events) }

// ── tests ───────────────────────────────────────────────────────────────────

func TestEvals_DatasetLifecycleIsAuditedWithoutCaseText(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("support-core")
	if d.Version != 1 || d.TenantID != isoA || d.CreatedBy != builderUser || d.Namespace != sharedNS {
		t.Fatalf("created = %+v", d)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/datasets", `{"namespace":"shared","name":"support-core"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/datasets", `{"namespace":"shared","name":"Bad Name"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name = %d", rec.Code)
	}
	// The namespace defaults to the tenant's first.
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/datasets", `{"name":"second"}`); rec.Code != http.StatusCreated {
		t.Errorf("default namespace = %d %s", rec.Code, rec.Body)
	}
	// A workspace the tenant may not use.
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/datasets", `{"namespace":"b-only","name":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("foreign namespace = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/evals/datasets", `{"name":"x","bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}

	path := fmt.Sprintf("/api/evals/datasets/%d", d.ID)
	rec := f.do(f.builder(), http.MethodPatch, path, `{"description":"  core questions "}`)
	if rec.Code != http.StatusOK || evalDecode[postgresql.EvalDataset](t, rec).Description != "core questions" {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	if rec = f.do(f.builder(), http.MethodDelete, path, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec = f.do(f.builder(), http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d", rec.Code)
	}

	// create, create, patch, delete: four audited writes; refusals and reads add none.
	if len(f.audit.events) != 4 {
		t.Fatalf("audit = %+v", f.audit.events)
	}
	want := []string{postgresql.AuditCreate, postgresql.AuditCreate, postgresql.AuditUpdate, postgresql.AuditDelete}
	for i, e := range f.audit.events {
		if e.Action != want[i] || e.Kind != kindEvalDataset || e.UserID != builderUser || e.Namespace != sharedNS {
			t.Errorf("event %d = %+v", i, e)
		}
	}
}

func TestEvals_RolesViewerReadsBuilderWrites(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("roles")
	rec := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID), `{"input":"hi"}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	caseID := evalDecode[addResult](t, rec).Cases[0].ID
	dsPath, casePath := fmt.Sprintf("/api/evals/datasets/%d", d.ID), fmt.Sprintf("/api/evals/cases/%d", caseID)
	before := f.auditCount()

	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		id := idFor(role, isoA)
		for _, p := range []string{"/api/evals/datasets", dsPath, dsPath + "/cases", casePath} {
			if rec := f.do(id, http.MethodGet, p, ""); rec.Code != http.StatusOK {
				t.Errorf("%s GET %s = %d", role, p, rec.Code)
			}
		}
		writes := []struct{ method, path, body string }{
			{http.MethodPost, "/api/evals/datasets", `{"name":"nope"}`},
			{http.MethodPatch, dsPath, `{"description":"x"}`},
			{http.MethodDelete, dsPath, ""},
			{http.MethodPost, dsPath + "/cases", `{"input":"x"}`},
			{http.MethodPatch, casePath, `{"input":"y"}`},
			{http.MethodDelete, casePath, ""},
			{http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d}`, d.ID)},
		}
		for _, w := range writes {
			if rec := f.do(id, w.method, w.path, w.body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403", role, w.method, w.path, rec.Code)
			}
		}
	}
	if f.auditCount() != before {
		t.Errorf("refused writes were audited: %+v", f.audit.events[before:])
	}
	if rec := f.do(f.builder(), http.MethodGet, casePath, ""); rec.Code != http.StatusOK || evalDecode[postgresql.EvalCase](t, rec).Input != "hi" {
		t.Errorf("the case was changed by a refused write: %s", rec.Body)
	}
}

func TestEvals_503WithoutAStore(t *testing.T) {
	isoMode(t, tenancy.ModeEnforce)
	s := &UIServer{audit: &fakeAudit{}}
	h := s.Handler()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/evals/datasets", ""},
		{http.MethodPost, "/api/evals/datasets", `{"name":"x"}`},
		{http.MethodGet, "/api/evals/datasets/1", ""},
		{http.MethodPatch, "/api/evals/datasets/1", `{}`},
		{http.MethodDelete, "/api/evals/datasets/1", ""},
		{http.MethodGet, "/api/evals/datasets/1/cases", ""},
		{http.MethodPost, "/api/evals/datasets/1/cases", `{"input":"x"}`},
		{http.MethodGet, "/api/evals/cases/1", ""},
		{http.MethodPatch, "/api/evals/cases/1", `{}`},
		{http.MethodDelete, "/api/evals/cases/1", ""},
		{http.MethodPost, "/api/runs/shared/r/dataset", `{"datasetId":1}`},
	} {
		req := asTenant(httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)), idFor(tenancy.RoleBuilder, isoA))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "PostgreSQL") {
			t.Errorf("%s %s = %d %s, want 503 naming PostgreSQL", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// The role is checked first: a viewer gets 403, not 503, on a write.
	req := asTenant(httptest.NewRequest(http.MethodPost, "/api/evals/datasets", strings.NewReader(`{"name":"x"}`)), idFor(tenancy.RoleViewer, isoA))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer write without a store = %d", rec.Code)
	}
}

func TestEvals_ForeignTenantSeesNothing(t *testing.T) {
	f := newEvalFixture(t)
	b := idFor(tenancy.RoleAdmin, isoB)
	rec := f.do(b, http.MethodPost, "/api/evals/datasets", `{"name":"b-data"}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	dB := evalDecode[postgresql.EvalDataset](t, rec)
	rec = f.do(b, http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", dB.ID), `{"input":"b secret"}`)
	cB := evalDecode[addResult](t, rec).Cases[0]
	a := idFor(tenancy.RoleAdmin, isoA)
	dsPath, casePath := fmt.Sprintf("/api/evals/datasets/%d", dB.ID), fmt.Sprintf("/api/evals/cases/%d", cB.ID)
	before := f.auditCount()

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, dsPath, ""}, {http.MethodPatch, dsPath, `{"description":"x"}`}, {http.MethodDelete, dsPath, ""},
		{http.MethodGet, dsPath + "/cases", ""}, {http.MethodPost, dsPath + "/cases", `{"input":"planted"}`},
		{http.MethodGet, casePath, ""}, {http.MethodPatch, casePath, `{"input":"x"}`}, {http.MethodDelete, casePath, ""},
		{http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d}`, dB.ID)},
	} {
		if rec := f.do(a, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("A %s %s = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if rec := f.do(a, http.MethodGet, "/api/evals/datasets", ""); strings.Contains(rec.Body.String(), "b-data") {
		t.Errorf("A lists B's dataset: %s", rec.Body)
	}
	if f.auditCount() != before {
		t.Errorf("refused writes were audited")
	}
	// B's data is untouched.
	if rec := f.do(b, http.MethodGet, casePath, ""); evalDecode[postgresql.EvalCase](t, rec).Input != "b secret" {
		t.Errorf("B's case changed: %s", rec.Body)
	}
}

func TestEvals_NamespaceInsideTheTenantIsChecked(t *testing.T) {
	f := newEvalFixture(t)
	// A dataset of tenant A in a namespace that this caller may not use.
	f.store.datasets[99] = &postgresql.EvalDataset{ID: 99, TenantID: isoA, Namespace: "a-secret", Name: "d", Version: 1}
	f.store.cases[100] = &postgresql.EvalCase{ID: 100, DatasetID: 99, TenantID: isoA, Namespace: "a-secret", Input: "x", ReferenceKind: "none", Weight: 1}
	for _, p := range []string{"/api/evals/datasets/99", "/api/evals/datasets/99/cases", "/api/evals/cases/100"} {
		if rec := f.do(f.builder(), http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := f.do(f.builder(), http.MethodGet, "/api/evals/datasets?namespace=a-secret", ""); rec.Code != http.StatusForbidden {
		t.Errorf("list of a foreign namespace = %d", rec.Code)
	}
	rec := f.do(f.builder(), http.MethodGet, "/api/evals/datasets", "")
	if strings.Contains(rec.Body.String(), "a-secret") {
		t.Errorf("list shows a namespace the caller may not use: %s", rec.Body)
	}
}

func TestEvals_VersionBumpsOnEveryCaseChange(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("versions")
	cases := fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID)
	if f.datasetVersion(d.ID) != 1 {
		t.Fatal("a new dataset starts at version 1")
	}
	// One case: +1.
	rec := f.do(f.builder(), http.MethodPost, cases, `{"input":"one","tags":["a"]}`)
	res := evalDecode[addResult](t, rec)
	if rec.Code != http.StatusCreated || res.DatasetVersion != 2 || res.Added != 1 || res.Cases[0].AddedIn != 2 {
		t.Fatalf("add one = %d %+v", rec.Code, res)
	}
	one := fmt.Sprintf("/api/evals/cases/%d", res.Cases[0].ID)
	// JSON lines: one bump for the call.
	rec = f.do(f.builder(), http.MethodPost, cases, "{\"input\":\"two\"}\n\n{\"input\":\"three\",\"expected\":\"3\",\"tags\":[\"b\"]}\n",
		"Content-Type", "application/x-ndjson")
	res = evalDecode[addResult](t, rec)
	if rec.Code != http.StatusCreated || res.DatasetVersion != 3 || res.Added != 2 {
		t.Fatalf("bulk add = %d %+v", rec.Code, res)
	}
	if res.Cases[1].Reference != "3" || res.Cases[1].ReferenceKind != postgresql.ReferenceExact {
		t.Errorf("expected maps to an exact reference: %+v", res.Cases[1])
	}
	// An edit that changes nothing: no bump, no audit row.
	audits := f.auditCount()
	rec = f.do(f.builder(), http.MethodPatch, one, `{"input":"one"}`)
	if rec.Code != http.StatusOK || evalDecode[caseChange](t, rec).DatasetVersion != 3 || f.auditCount() != audits {
		t.Errorf("no-op edit = %d %s, audits %d -> %d", rec.Code, rec.Body, audits, f.auditCount())
	}
	// A real edit: +1.
	rec = f.do(f.builder(), http.MethodPatch, one, `{"input":"uno"}`)
	if got := evalDecode[caseChange](t, rec); rec.Code != http.StatusOK || got.DatasetVersion != 4 || got.Case.Input != "uno" {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	// Removing: +1.
	rec = f.do(f.builder(), http.MethodDelete, one, "")
	if got := evalDecode[map[string]int](t, rec); rec.Code != http.StatusOK || got["datasetVersion"] != 5 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	// A failed add and a metadata change do not bump.
	if rec = f.do(f.builder(), http.MethodPost, cases, `{"input":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty input = %d", rec.Code)
	}
	f.do(f.builder(), http.MethodPatch, fmt.Sprintf("/api/evals/datasets/%d", d.ID), `{"description":"d"}`)
	if v := f.datasetVersion(d.ID); v != 5 {
		t.Errorf("version = %d, want 5", v)
	}
	checkVersionAudit(t, f)
	// The list sees the live cases and the version.
	rec = f.do(f.builder(), http.MethodGet, cases, "")
	page := evalDecode[casePage](t, rec)
	if len(page.Cases) != 2 || page.DatasetVersion != 5 {
		t.Errorf("page = %+v", page)
	}
}

// checkVersionAudit: every change is one audit row that names the dataset and
// the version, not the text.
func checkVersionAudit(t *testing.T, f *evalFixture) {
	t.Helper()
	var changes []postgresql.AuditEvent
	for _, e := range f.audit.events {
		if e.Name == "versions" && e.Action == postgresql.AuditUpdate {
			changes = append(changes, e)
		}
	}
	if len(changes) != 5 { // add, bulk add, edit, delete, description
		t.Fatalf("audit rows = %+v", changes)
	}
	for _, e := range changes {
		for _, text := range []string{"uno", "three", "two"} {
			if strings.Contains(e.Summary, text) {
				t.Errorf("audit summary holds case text: %q", e.Summary)
			}
		}
	}
	if changes[1].ResourceVersion != "3" || !strings.Contains(changes[1].Summary, "added 2 cases") {
		t.Errorf("bulk audit = %+v", changes[1])
	}
}

func TestEvals_CaseListPagesAndFilters(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("paging")
	cases := fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID)
	var lines strings.Builder
	for i := 1; i <= 5; i++ {
		tag := "odd"
		if i%2 == 0 {
			tag = "even"
		}
		fmt.Fprintf(&lines, "{\"input\":\"c%d\",\"tags\":[%q]}\n", i, tag)
	}
	if rec := f.do(f.builder(), http.MethodPost, cases, lines.String(), "Content-Type", "application/jsonl"); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	var seen []string
	cursor := ""
	for range 5 {
		rec := f.do(f.builder(), http.MethodGet, cases+"?limit=2&cursor="+cursor, "")
		page := evalDecode[casePage](t, rec)
		for _, c := range page.Cases {
			seen = append(seen, c.Input)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if strings.Join(seen, ",") != "c1,c2,c3,c4,c5" {
		t.Errorf("paged cases = %v", seen)
	}
	page := evalDecode[casePage](t, f.do(f.builder(), http.MethodGet, cases+"?tag=even", ""))
	if len(page.Cases) != 2 || page.Cases[0].Input != "c2" {
		t.Errorf("tag filter = %+v", page.Cases)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x", "?cursor=abc"} {
		if rec := f.do(f.builder(), http.MethodGet, cases+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rec.Code)
		}
	}
}

func TestEvals_SizeLimits(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("sizes")
	cases := fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID)

	big := strings.Repeat("x", postgresql.MaxEvalCaseBytes+1)
	if rec := f.do(f.builder(), http.MethodPost, cases, fmt.Sprintf(`{"input":%q}`, big)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("input over 64 KiB = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPost, cases, fmt.Sprintf(`{"input":"ok","reference":%q}`, big)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("reference over 64 KiB = %d", rec.Code)
	}
	atLimit := strings.Repeat("x", postgresql.MaxEvalCaseBytes)
	if rec := f.do(f.builder(), http.MethodPost, cases, fmt.Sprintf(`{"input":%q}`, atLimit)); rec.Code != http.StatusCreated {
		t.Errorf("input at the limit = %d %.100s", rec.Code, rec.Body)
	}

	// The limit is configurable, and cannot be raised above the table's.
	f.s.SetEvals(EvalsConfig{MaxCaseBytes: 10})
	if rec := f.do(f.builder(), http.MethodPost, cases, `{"input":"12345678901"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("input over a configured limit = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPost, cases, `{"input":"1234567890"}`); rec.Code != http.StatusCreated {
		t.Errorf("input at a configured limit = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPatch, "/api/evals/cases/2", `{"input":"12345678901"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("edit over a configured limit = %d", rec.Code)
	}
	f.s.SetEvals(EvalsConfig{MaxCaseBytes: 1 << 30})
	if rec := f.do(f.builder(), http.MethodPost, cases, fmt.Sprintf(`{"input":%q}`, big)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a configured limit above the table's must not raise it: %d", rec.Code)
	}

	// At most 200 cases per call.
	f.s.SetEvals(EvalsConfig{})
	var b strings.Builder
	for range 201 {
		b.WriteString("{\"input\":\"x\"}\n")
	}
	before := f.datasetVersion(d.ID)
	if rec := f.do(f.builder(), http.MethodPost, cases, b.String(), "Content-Type", "application/x-ndjson"); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "200") {
		t.Errorf("201 lines = %d %s", rec.Code, rec.Body)
	}
	b.Reset()
	for range 200 {
		b.WriteString("{\"input\":\"x\"}\n")
	}
	if rec := f.do(f.builder(), http.MethodPost, cases, b.String(), "Content-Type", "application/x-ndjson"); rec.Code != http.StatusCreated {
		t.Errorf("200 lines = %d %.100s", rec.Code, rec.Body)
	}
	if f.datasetVersion(d.ID) != before+1 {
		t.Errorf("200 lines must be one version bump")
	}
}

func TestEvals_BadBodiesAreRejectedWholesale(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("bad")
	cases := fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID)
	nd := "application/x-ndjson"
	for _, c := range []struct{ name, body, ct, want string }{
		{"bad json line", "{\"input\":\"ok\"}\n{nope}\n", nd, "line 2"},
		{"unknown field", "{\"input\":\"ok\",\"output\":\"x\"}\n", nd, "line 1"},
		{"line without input", "{\"input\":\"ok\"}\n{\"expected\":\"x\"}\n", nd, "case 2"},
		{"both expected and reference", "{\"input\":\"a\",\"expected\":\"x\",\"reference\":\"y\"}\n", nd, "not both"},
		{"empty body", "\n\n", nd, "no cases"},
		{"an array", `[{"input":"a"}]`, "application/json", "invalid JSON"},
		{"two values", `{"input":"a"}{"input":"b"}`, "application/json", "unexpected text"},
		{"zero weight", `{"input":"a","weight":0}`, "application/json", "weight"},
	} {
		rec := f.do(f.builder(), http.MethodPost, cases, c.body, "Content-Type", c.ct)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s = %d %s, want 400 mentioning %q", c.name, rec.Code, rec.Body, c.want)
		}
	}
	// Nothing was added by the bad bodies, not even the good first line.
	if f.datasetVersion(d.ID) != 1 {
		t.Error("a rejected body changed the dataset")
	}
}

func TestEvals_DatasetCaseCap(t *testing.T) {
	f := newEvalFixture(t)
	f.s.SetEvals(EvalsConfig{MaxCasesPerDataset: 3})
	d := f.dataset("capped")
	cases := fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID)
	if rec := f.do(f.builder(), http.MethodPost, cases, "{\"input\":\"1\"}\n{\"input\":\"2\"}\n", "Content-Type", "application/x-ndjson"); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	// Two more would make four.
	audits, v := f.auditCount(), f.datasetVersion(d.ID)
	if rec := f.do(f.builder(), http.MethodPost, cases, "{\"input\":\"3\"}\n{\"input\":\"4\"}\n", "Content-Type", "application/x-ndjson"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "full") {
		t.Errorf("over the cap = %d %s", rec.Code, rec.Body)
	}
	if f.auditCount() != audits || f.datasetVersion(d.ID) != v {
		t.Error("a refused add changed the version or was audited")
	}
	// Exactly the cap is fine; then one more is not.
	rec := f.do(f.builder(), http.MethodPost, cases, `{"input":"3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("at the cap = %d", rec.Code)
	}
	if rec2 := f.do(f.builder(), http.MethodPost, cases, `{"input":"4"}`); rec2.Code != http.StatusConflict {
		t.Errorf("past the cap = %d", rec2.Code)
	}
	// "Add to dataset" from a run is held to the same cap.
	if rec3 := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d}`, d.ID)); rec3.Code != http.StatusConflict {
		t.Errorf("capture past the cap = %d", rec3.Code)
	}
	// Removing a case frees a slot.
	id := evalDecode[addResult](t, rec).Cases[0].ID
	f.do(f.builder(), http.MethodDelete, fmt.Sprintf("/api/evals/cases/%d", id), "")
	if rec4 := f.do(f.builder(), http.MethodPost, cases, `{"input":"4"}`); rec4.Code != http.StatusCreated {
		t.Errorf("after a removal = %d", rec4.Code)
	}
}

func TestEvals_CaptureFromALiveRunRedactsByDefault(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("capture")
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset",
		fmt.Sprintf(`{"datasetId":%d,"tags":["refund"]}`, d.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("capture = %d %s", rec.Code, rec.Body)
	}
	got := evalDecode[captureResult](t, rec)
	c := got.Case
	if c.Input != "mail [REDACTED_EMAIL] about the refund" || !c.Redacted || c.Reference != "" || c.ReferenceKind != postgresql.ReferenceNone {
		t.Errorf("case = %+v", c)
	}
	if c.SourceRun != "run-a" || c.SourceRunNamespace != sharedNS || c.SourceUser != builderUser || len(c.Tags) != 1 {
		t.Errorf("source = %+v", c)
	}
	if got.DatasetVersion != 2 || got.Redaction["input"].Replacements != 1 || got.Redaction["input"].Rules[0] != "email" {
		t.Errorf("result = %+v", got)
	}
	// The stored case is the redacted one.
	stored := evalDecode[postgresql.EvalCase](t, f.do(f.builder(), http.MethodGet, fmt.Sprintf("/api/evals/cases/%d", c.ID), ""))
	if strings.Contains(stored.Input, "ada@example.com") {
		t.Errorf("the address was stored: %q", stored.Input)
	}
	// The audit row names the run and counts the replacements, and holds no text.
	last := f.audit.events[len(f.audit.events)-1]
	if last.Name != "capture" || !strings.Contains(last.Summary, "run shared/run-a") || !strings.Contains(last.Summary, "redacted (1 replacements)") ||
		strings.Contains(last.Summary, "ada@") || strings.Contains(last.Summary, "refund") {
		t.Errorf("audit = %+v", last)
	}
}

func TestEvals_CaptureAppliesTheAgentsGuardrailPatterns(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("guard")
	f.s.crdClient = fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		evalRun("shared", "g1", isoA, "order ORD-12345 for Project Falcon", "Project Falcon shipped ORD-12345", agentorcav1alpha1.AgentRunPhaseSucceeded),
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support-bot", Namespace: "shared"}, Spec: agentorcav1alpha1.AgentSpec{GuardrailPolicyRef: "pii"}},
		&agentorcav1alpha1.GuardrailPolicy{ObjectMeta: metav1.ObjectMeta{Name: "pii", Namespace: "shared"}, Spec: agentorcav1alpha1.GuardrailPolicySpec{
			InputFilters: []agentorcav1alpha1.GuardrailFilter{{Name: "ids", Type: "regex", Action: "block",
				Patterns: []agentorcav1alpha1.RegexPattern{{Name: "order", Pattern: `ORD-\d{5}`, Replacement: "[ORDER]"}}}},
			OutputFilters: []agentorcav1alpha1.GuardrailFilter{{Name: "words", Type: "keyword-blocklist", Action: "warn",
				Keywords: &agentorcav1alpha1.KeywordConfig{Inline: []string{"project falcon"}}}},
		}},
	).Build()
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/g1/dataset", fmt.Sprintf(`{"datasetId":%d,"useOutput":true}`, d.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("capture = %d %s", rec.Code, rec.Body)
	}
	c := evalDecode[captureResult](t, rec).Case
	// Input filters apply to the input (the keyword rule is an output filter),
	// output filters to the reference.
	if c.Input != "order [ORDER] for Project Falcon" {
		t.Errorf("input = %q", c.Input)
	}
	if c.Reference != "[REDACTED] shipped ORD-12345" || c.ReferenceKind != postgresql.ReferenceExample {
		t.Errorf("reference = %q kind %q", c.Reference, c.ReferenceKind)
	}
}

func TestEvals_CaptureUnredactedNeedsPermission(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("raw")
	body := fmt.Sprintf(`{"datasetId":%d,"redact":false}`, d.ID)
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", body)
	got := evalDecode[captureResult](t, rec)
	if rec.Code != http.StatusCreated || got.Case.Redacted || !strings.Contains(got.Case.Input, "ada@example.com") {
		t.Fatalf("unredacted capture = %d %s", rec.Code, rec.Body)
	}
	// Keeping the text is audited as such.
	last := f.audit.events[len(f.audit.events)-1]
	if !strings.Contains(last.Summary, "kept unredacted") {
		t.Errorf("audit = %+v", last)
	}

	// An operator can forbid it.
	f.s.SetEvals(EvalsConfig{DisallowUnredactedCases: true})
	audits := f.auditCount()
	if rec = f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", body); rec.Code != http.StatusForbidden {
		t.Errorf("unredacted when forbidden = %d", rec.Code)
	}
	if f.auditCount() != audits {
		t.Error("a refused capture was audited")
	}
	// Redaction itself is still allowed.
	if rec = f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d}`, d.ID)); rec.Code != http.StatusCreated {
		t.Errorf("redacted when unredacted is forbidden = %d", rec.Code)
	}
	if rec = f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d,"redact":true}`, d.ID)); rec.Code != http.StatusCreated {
		t.Errorf("redact:true = %d", rec.Code)
	}
}

func TestEvals_CaptureReferenceChoices(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("refs")
	post := func(body string) *httptest.ResponseRecorder {
		return f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d,%s}`, d.ID, body))
	}
	// Taking the output is a choice: nothing is taken unless asked.
	if c := evalDecode[captureResult](t, post(`"tags":[]`)).Case; c.Reference != "" || c.ReferenceKind != postgresql.ReferenceNone {
		t.Errorf("default reference = %+v", c)
	}
	if c := evalDecode[captureResult](t, post(`"useOutput":true`)).Case; c.Reference != "Done, refund issued." || c.ReferenceKind != postgresql.ReferenceExample {
		t.Errorf("useOutput = %+v", c)
	}
	if c := evalDecode[captureResult](t, post(`"reference":"Refund of 10"`)).Case; c.Reference != "Refund of 10" || c.ReferenceKind != postgresql.ReferenceExact {
		t.Errorf("written reference = %+v", c)
	}
	if c := evalDecode[captureResult](t, post(`"reference":"polite and brief","referenceKind":"rubric"`)).Case; c.ReferenceKind != postgresql.ReferenceRubric {
		t.Errorf("rubric = %+v", c)
	}
	for _, body := range []string{`"useOutput":true,"reference":"x"`, `"referenceKind":"exact"`, `"reference":"x","referenceKind":"golden"`} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, rec.Code)
		}
	}
	// A run that is still going has no output to take.
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-live/dataset", fmt.Sprintf(`{"datasetId":%d,"useOutput":true}`, d.ID))
	if rec.Code != http.StatusConflict {
		t.Errorf("useOutput on a running run = %d", rec.Code)
	}
	// Its input can still be captured.
	if rec = f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-live/dataset", fmt.Sprintf(`{"datasetId":%d}`, d.ID)); rec.Code != http.StatusCreated {
		t.Errorf("capture of a running run's input = %d", rec.Code)
	}
}

func TestEvals_CapturePreviewStoresNothing(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("preview")
	audits := f.auditCount()
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", fmt.Sprintf(`{"datasetId":%d,"preview":true}`, d.ID))
	got := evalDecode[captureResult](t, rec)
	if rec.Code != http.StatusOK || !got.Preview || got.Case.Input != "mail [REDACTED_EMAIL] about the refund" ||
		got.Original["input"] != "mail ada@example.com about the refund" || got.Case.ID != 0 {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	if f.datasetVersion(d.ID) != 1 || f.auditCount() != audits {
		t.Error("a preview changed the dataset or was audited")
	}
}

func TestEvals_CaptureFromAnArchivedRun(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("archived")
	f.store.runs[isoA+"|shared/old-run"] = &postgresql.RunArchive{Name: "old-run", Namespace: "shared", AgentRef: "support-bot",
		Input: "call me on bob@corp.io", Output: "ok", Phase: "Succeeded"}
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/old-run/dataset", fmt.Sprintf(`{"datasetId":%d,"useOutput":true}`, d.ID))
	c := evalDecode[captureResult](t, rec).Case
	if rec.Code != http.StatusCreated || c.Input != "call me on [REDACTED_EMAIL]" || c.Reference != "ok" || c.SourceRun != "old-run" {
		t.Errorf("archived capture = %d %s", rec.Code, rec.Body)
	}
}

func TestEvals_CaptureOfAForeignOrUnknownRunIs404(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("foreign-runs")
	// An archived run of tenant B, in a namespace tenant A shares with it.
	f.store.runs[isoB+"|shared/archived-b"] = &postgresql.RunArchive{Name: "archived-b", Namespace: "shared", Input: "B's archived input", TenantID: isoB}
	audits := f.auditCount()
	for _, run := range []string{
		"shared/run-b",      // a live run labelled for tenant B, in a shared namespace
		"b-only/run-b2",     // a namespace tenant A may not use
		"shared/archived-b", // an archived run of tenant B
		"shared/does-not-exist",
	} {
		rec := f.do(f.builder(), http.MethodPost, "/api/runs/"+run+"/dataset", fmt.Sprintf(`{"datasetId":%d}`, d.ID))
		if rec.Code != http.StatusNotFound {
			t.Errorf("capture of %s = %d %s, want 404", run, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "private") {
			t.Errorf("the answer leaks the run: %s", rec.Body)
		}
	}
	if f.auditCount() != audits || f.datasetVersion(d.ID) != 1 {
		t.Error("a refused capture changed the dataset or was audited")
	}
	// An unknown dataset is a 404 too, and a missing datasetId a 400.
	if rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", `{"datasetId":4242}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown dataset = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/run-a/dataset", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no dataset = %d", rec.Code)
	}
	if rec := f.do(f.builder(), http.MethodGet, "/api/runs/shared/run-a/dataset", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
}

func TestEvals_CaptureRefusesAnOversizedRun(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("big-run")
	big := strings.Repeat("y", postgresql.MaxEvalCaseBytes+1)
	f.store.runs[isoA+"|shared/huge"] = &postgresql.RunArchive{Name: "huge", Namespace: "shared", Input: big, Output: "ok"}
	rec := f.do(f.builder(), http.MethodPost, "/api/runs/shared/huge/dataset", fmt.Sprintf(`{"datasetId":%d}`, d.ID))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "65536") {
		t.Errorf("oversized input = %d %.200s", rec.Code, rec.Body)
	}
	f.store.runs[isoA+"|shared/huge-out"] = &postgresql.RunArchive{Name: "huge-out", Namespace: "shared", Input: "small", Output: big}
	rec = f.do(f.builder(), http.MethodPost, "/api/runs/shared/huge-out/dataset", fmt.Sprintf(`{"datasetId":%d,"useOutput":true}`, d.ID))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized output as the reference = %d", rec.Code)
	}
	if f.datasetVersion(d.ID) != 1 {
		t.Error("a refused capture changed the dataset")
	}
}

func TestEvals_NoTenantIdentityOnAWrite(t *testing.T) {
	f := newEvalFixture(t)
	// A caller with a tenant name but no tenant ID has no scope: the store refuses.
	id := idFor(tenancy.RoleBuilder, isoA)
	id.TenantID = ""
	f.store.next = 0
	// The fake treats an empty scope as a tenant of its own; use a store that refuses.
	f.s.evals = refusingEvalStore{f.store}
	if rec := f.do(id, http.MethodPost, "/api/evals/datasets", `{"name":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("create without a tenant ID = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(id, http.MethodGet, "/api/evals/datasets", ""); rec.Code != http.StatusForbidden {
		t.Errorf("list without a tenant ID = %d %s", rec.Code, rec.Body)
	}
}

type refusingEvalStore struct{ *fakeEvalStore }

func (refusingEvalStore) CreateEvalDataset(context.Context, postgresql.EvalDataset) (postgresql.EvalDataset, error) {
	return postgresql.EvalDataset{}, postgresql.ErrNoTenantScope
}

func (refusingEvalStore) ListEvalDatasets(context.Context, postgresql.EvalDatasetQuery) ([]postgresql.EvalDataset, error) {
	return nil, postgresql.ErrNoTenantScope
}

func TestEvals_StoreErrorsAreNotLeaked(t *testing.T) {
	f := newEvalFixture(t)
	f.s.evals = brokenEvalStore{f.store}
	rec := f.do(f.builder(), http.MethodGet, "/api/evals/datasets", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("store failure = %d %s", rec.Code, rec.Body)
	}
}

type brokenEvalStore struct{ *fakeEvalStore }

func (brokenEvalStore) ListEvalDatasets(context.Context, postgresql.EvalDatasetQuery) ([]postgresql.EvalDataset, error) {
	return nil, fmt.Errorf("dial tcp: connection refused")
}

// Every route this file serves is in the UI API contract, and every schema it
// references is defined.
func TestEvals_OpenAPIDescribesTheEndpoints(t *testing.T) {
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
		"/api/evals/datasets":                  {"get", "post"},
		"/api/evals/datasets/{id}":             {"get", "patch", "delete"},
		"/api/evals/datasets/{id}/cases":       {"get", "post"},
		"/api/evals/cases/{id}":                {"get", "patch", "delete"},
		"/api/runs/{namespace}/{name}/dataset": {"post"},
	}
	for path, methods := range want {
		for _, m := range methods {
			if _, ok := doc.Paths[path][m]; !ok {
				t.Errorf("openapi-ui.yaml lacks %s %s", strings.ToUpper(m), path)
			}
		}
	}
	for _, name := range []string{"EvalDataset", "EvalCase", "EvalCaseInput", "EvalCasePage", "EvalCasesAdded", "EvalCapture", "EvalCaptured"} {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schema %s is not defined", name)
		}
	}
	if !strings.Contains(string(raw), "#/components/schemas/EvalCaseChanged") || doc.Components.Schemas["EvalCaseChanged"] == nil {
		t.Error("EvalCaseChanged is referenced but not defined")
	}
}

func TestEvals_RoutesRejectBadPaths(t *testing.T) {
	f := newEvalFixture(t)
	for _, p := range []string{"/api/evals/datasets/abc", "/api/evals/datasets/0", "/api/evals/datasets/1/other", "/api/evals/datasets/1/cases/2", "/api/evals/cases/", "/api/evals/cases/x"} {
		if rec := f.do(f.builder(), http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := f.do(f.builder(), http.MethodPut, "/api/evals/datasets", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d", rec.Code)
	}
}
