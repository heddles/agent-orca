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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeRevisions is an in-memory revision store with the real one's rules: rows
// are visible only inside the call's tenant scope, numbers count up per object,
// and a save equal to the latest revision is not stored. Pruning is the real
// store's job (and tested there); the fake records the keep it was given.
type fakeRevisions struct {
	mu       sync.Mutex
	rows     []postgresql.Revision
	fail     bool
	attempts int
	lastKeep int
}

func (f *fakeRevisions) scopeOf(ctx context.Context) string {
	scope, _ := postgresql.ScopeFrom(ctx)
	return scope
}

func (f *fakeRevisions) visible(ctx context.Context, tenant string) bool {
	scope := f.scopeOf(ctx)
	return scope == postgresql.PlatformScope || scope == tenant
}

func (f *fakeRevisions) AppendRevision(ctx context.Context, rev postgresql.Revision, keep int) (postgresql.Revision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	f.lastKeep = keep
	if f.fail {
		return rev, false, errors.New("postgres is down")
	}
	if scope := f.scopeOf(ctx); scope != postgresql.PlatformScope {
		rev.TenantID = scope
	}
	var latest *postgresql.Revision
	for i := range f.rows {
		r := &f.rows[i]
		if r.TenantID == rev.TenantID && r.Kind == rev.Kind && r.Namespace == rev.Namespace && r.Name == rev.Name && (latest == nil || r.Revision > latest.Revision) {
			latest = r
		}
	}
	if latest != nil && latest.Action != postgresql.AuditDelete && rev.Action != postgresql.AuditDelete && sameJSON(latest.Object, rev.Object) {
		return *latest, false, nil
	}
	rev.Revision = 1
	if latest != nil {
		rev.Revision = latest.Revision + 1
	}
	f.rows = append(f.rows, rev)
	return rev, true, nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && jsonString(x) == jsonString(y)
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *fakeRevisions) ListRevisions(ctx context.Context, q postgresql.RevisionQuery) ([]postgresql.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []postgresql.Revision
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.Kind != q.Kind || r.Namespace != q.Namespace || r.Name != q.Name || !f.visible(ctx, r.TenantID) || (q.Before > 0 && r.Revision >= q.Before) {
			continue
		}
		r.Object = nil
		out = append(out, r)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRevisions) GetRevision(ctx context.Context, q postgresql.RevisionQuery, n int) (postgresql.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Kind == q.Kind && r.Namespace == q.Namespace && r.Name == q.Name && r.Revision == n && f.visible(ctx, r.TenantID) {
			return r, nil
		}
	}
	return postgresql.Revision{}, postgresql.ErrRevisionNotFound
}

func (f *fakeRevisions) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r.Action)
	}
	return out
}

type revisionFixture struct {
	*writeFixture
	revs *fakeRevisions
}

func newRevisionFixture(t *testing.T) *revisionFixture {
	t.Helper()
	f := newWriteFixture(t)
	revs := &fakeRevisions{}
	f.s.revisions = revs
	return &revisionFixture{f, revs}
}

func (f *revisionFixture) tenantB(role tenancy.Role) *TenantIdentity {
	id := f.as(role)
	id.TenantID, id.TenantName, id.UserID = isoB, "globex", "bob"
	return id
}

const (
	verOne = "one"
	verTwo = "two"
)

const toolBodyJSON = `{"metadata":{"name":"mine","annotations":{"note":"hi"}},"spec":{"ociRef":"%s"}}`

func toolBodyAt(version, rv string) string {
	body := strings.Replace(toolBodyJSON, "%s", version, 1)
	if rv != "" {
		body = strings.Replace(body, `"name":"mine"`, `"name":"mine","resourceVersion":"`+rv+`"`, 1)
	}
	return body
}

// writeTool creates the tool (or updates it to version) as an admin and returns the stored one.
func (f *revisionFixture) writeTool(version string, create bool) {
	f.t.Helper()
	admin := f.as(tenancy.RoleAdmin)
	var rec = f.do(admin, http.MethodPost, "/api/resources/tools", strings.Replace(toolBodyAt(version, ""), `"name":"mine"`, `"name":"mine","namespace":"shared"`, 1))
	want := http.StatusCreated
	if !create {
		rec = f.do(admin, http.MethodPut, "/api/resources/tools/shared/mine", toolBodyAt(version, f.tool("mine").ResourceVersion))
		want = http.StatusOK
	}
	if rec.Code != want {
		f.t.Fatalf("write tool %s = %d %s", version, rec.Code, rec.Body.String())
	}
}

func TestWritesRecordRevisions(t *testing.T) {
	f := newRevisionFixture(t)
	f.s.SetRevisionsKeep(7)

	f.writeTool(verOne, true)
	f.writeTool(verTwo, false)
	f.writeTool(verTwo, false) // a no-op save
	if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodDelete, "/api/resources/tools/shared/mine", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if got := strings.Join(f.revs.actions(), ","); got != "create,update,delete" {
		t.Fatalf("revisions = %s, want create,update,delete (a no-op save adds none)", got)
	}
	if f.revs.lastKeep != 7 {
		t.Errorf("keep = %d, want 7", f.revs.lastKeep)
	}

	rows := f.revs.rows
	obj := string(rows[1].Object)
	for _, gone := range []string{"status", "managedFields", "resourceVersion", "uid", "generation", "creationTimestamp", "agentorca.io/owner", "last-modified", "ownership"} {
		if strings.Contains(obj, gone) {
			t.Errorf("revision object keeps %q: %s", gone, obj)
		}
	}
	for _, want := range []string{`"ociRef":"two"`, `"note":"hi"`, `"kind":"Tool"`, isoA} {
		if !strings.Contains(obj, want) {
			t.Errorf("revision object lacks %q: %s", want, obj)
		}
	}
	if rows[1].UserID != builderUser || rows[1].TenantID != isoA || rows[1].ResourceVersion == "" || rows[1].Summary == "" {
		t.Errorf("revision = %+v", rows[1])
	}
	if !strings.Contains(string(rows[2].Object), `"ociRef":"two"`) {
		t.Errorf("the delete snapshot is the object as it was: %s", rows[2].Object)
	}
}

func TestDryRunAndFailedWritesRecordNoRevision(t *testing.T) {
	f := newRevisionFixture(t)
	admin := f.as(tenancy.RoleAdmin)
	body := `{"metadata":{"name":"mine","namespace":"shared"},"spec":{"ociRef":"x"}}`
	if rec := f.do(admin, http.MethodPost, "/api/resources/tools?dryRun=true", body); rec.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(f.as(tenancy.RoleViewer), http.MethodPost, "/api/resources/tools", body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d", rec.Code)
	}
	if len(f.revs.rows) != 0 || f.revs.attempts != 0 {
		t.Errorf("revisions = %+v after a dry run and a denied write", f.revs.rows)
	}
}

func TestRevisionWriteFailureDoesNotFailTheRequest(t *testing.T) {
	f := newRevisionFixture(t)
	f.revs.fail = true
	rec := f.do(f.as(tenancy.RoleAdmin), http.MethodPost, "/api/resources/tools", `{"metadata":{"name":"mine","namespace":"shared"},"spec":{"ociRef":"x"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Revision-Status") != "failed" {
		t.Errorf("headers = %v, want X-Revision-Status: failed", rec.Header())
	}
	if f.revs.attempts != revisionAttempts {
		t.Errorf("attempts = %d, want %d", f.revs.attempts, revisionAttempts)
	}
	if len(f.audit.events) != 1 || rec.Header().Get("X-Audit-Status") != "" {
		t.Errorf("the audit trail is independent: %+v", f.audit.events)
	}
	f.tool("mine") // the write itself stands
}

func TestWritesWithoutPostgresWorkAsBefore(t *testing.T) {
	f := newWriteFixture(t)
	admin := f.as(tenancy.RoleAdmin)
	rec := f.do(admin, http.MethodPost, "/api/resources/tools", `{"metadata":{"name":"mine","namespace":"shared"},"spec":{"ociRef":"x"}}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("X-Revision-Status") != "" {
		t.Errorf("create = %d, headers %v", rec.Code, rec.Header())
	}
	for _, path := range []string{"/revisions", "/revisions/1"} {
		rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, "/api/resources/tools/shared/mine"+path, "")
		if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "revision history needs PostgreSQL") {
			t.Errorf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec = f.do(f.as(tenancy.RoleAdmin), http.MethodPost, "/api/resources/tools/shared/mine/revisions/1/restore", `{}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("restore = %d %s", rec.Code, rec.Body.String())
	}
}

type revisionList struct {
	Revisions []struct {
		Revision     int    `json:"revision"`
		Action       string `json:"action"`
		UserID       string `json:"userId"`
		CreatedAt    string `json:"createdAt"`
		RestoredFrom *int   `json:"restoredFrom"`
	} `json:"revisions"`
	Next string `json:"next"`
}

func TestListRevisions(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	f.writeTool(verTwo, false)
	f.writeTool("three", false)
	url := "/api/resources/tools/shared/mine/revisions"

	t.Run("newest first, for a viewer", func(t *testing.T) {
		rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, url, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		var out revisionList
		decodeJSON(t, rec, &out)
		if len(out.Revisions) != 3 || out.Revisions[0].Revision != 3 || out.Revisions[2].Action != "create" || out.Next != "" {
			t.Errorf("list = %+v", out)
		}
		if out.Revisions[0].UserID != builderUser || out.Revisions[0].CreatedAt == "" {
			t.Errorf("fields = %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), `"object"`) {
			t.Errorf("the list carries objects: %s", rec.Body.String())
		}
	})
	t.Run("pages", func(t *testing.T) {
		var out revisionList
		decodeJSON(t, f.do(f.as(tenancy.RoleViewer), http.MethodGet, url+"?limit=2", ""), &out)
		if len(out.Revisions) != 2 || out.Next != "2" {
			t.Fatalf("page 1 = %+v", out)
		}
		next := out.Next
		out = revisionList{}
		decodeJSON(t, f.do(f.as(tenancy.RoleViewer), http.MethodGet, url+"?limit=2&before="+next, ""), &out)
		if len(out.Revisions) != 1 || out.Revisions[0].Revision != 1 || out.Next != "" {
			t.Errorf("page 2 = %+v", out)
		}
	})
	t.Run("bad paging", func(t *testing.T) {
		for _, q := range []string{"?limit=0", "?limit=9999", "?limit=x", "?before=0", "?before=x"} {
			if rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, url+q, ""); rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d", q, rec.Code)
			}
		}
	})
	t.Run("roles", func(t *testing.T) {
		for _, role := range allRoles {
			if rec := f.do(f.as(role), http.MethodGet, url, ""); rec.Code != http.StatusOK {
				t.Errorf("%s = %d", role, rec.Code)
			}
		}
		if rec := f.do(f.as("nobody"), http.MethodGet, url, ""); rec.Code != http.StatusForbidden {
			t.Errorf("no role = %d", rec.Code)
		}
	})
	t.Run("another tenant cannot read the live object, so not its history", func(t *testing.T) {
		if rec := f.do(f.tenantB(tenancy.RoleAdmin), http.MethodGet, url, ""); rec.Code != http.StatusNotFound {
			t.Errorf("tenant B = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a namespace outside the tenant", func(t *testing.T) {
		if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodGet, "/api/resources/tools/elsewhere/mine/revisions", ""); rec.Code != http.StatusNotFound {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("a method that is not GET", func(t *testing.T) {
		if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodDelete, url, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("an object owned by git still has readable history", func(t *testing.T) {
		g := newRevisionFixture(t)
		g.writeTool(verOne, true)
		tool := g.tool("mine")
		tool.Annotations[tenancy.AnnotationOwner] = "git"
		if err := g.c.Update(context.Background(), tool); err != nil {
			t.Fatal(err)
		}
		if rec := g.do(g.as(tenancy.RoleViewer), http.MethodGet, "/api/resources/tools/shared/mine/revisions", ""); rec.Code != http.StatusOK {
			t.Errorf("code = %d", rec.Code)
		}
	})
}

func TestRevisionsOfDeletedObject(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	f.writeTool(verTwo, false)
	if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodDelete, "/api/resources/tools/shared/mine", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	url := "/api/resources/tools/shared/mine/revisions"

	if rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, url, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer lists a deleted object's history: %d", rec.Code)
	}
	var out revisionList
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodGet, url, ""); rec.Code != http.StatusOK {
		t.Fatalf("builder = %d %s", rec.Code, rec.Body.String())
	} else {
		decodeJSON(t, rec, &out)
	}
	if len(out.Revisions) != 3 || out.Revisions[0].Action != postgresql.AuditDelete {
		t.Errorf("history = %+v", out)
	}
	// Another tenant's builder sees none of it: the rows are not visible to it.
	var other revisionList
	decodeJSON(t, f.do(f.tenantB(tenancy.RoleBuilder), http.MethodGet, url, ""), &other)
	if len(other.Revisions) != 0 {
		t.Errorf("tenant B sees %+v", other)
	}
	if rec := f.do(f.tenantB(tenancy.RoleBuilder), http.MethodGet, url+"/1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B reads revision 1: %d", rec.Code)
	}
}

func TestRevisionDetail(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	url := "/api/resources/tools/shared/mine/revisions/"

	rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, url+"1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Revision        int            `json:"revision"`
		Action          string         `json:"action"`
		UserID          string         `json:"userId"`
		Summary         string         `json:"summary"`
		ResourceVersion string         `json:"resourceVersion"`
		Object          map[string]any `json:"object"`
	}
	decodeJSON(t, rec, &out)
	spec, _ := out.Object["spec"].(map[string]any)
	if out.Revision != 1 || out.Action != postgresql.AuditCreate || out.UserID != builderUser || out.Summary == "" || out.ResourceVersion == "" || spec["ociRef"] != verOne {
		t.Errorf("detail = %+v", out)
	}
	for _, path := range []string{"9", "0", "x", "-1"} {
		if rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, url+path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("revision %s = %d", path, rec.Code)
		}
	}
	if rec := f.do(f.tenantB(tenancy.RoleAdmin), http.MethodGet, url+"1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B = %d", rec.Code)
	}
}

func TestRestoreRevision(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	f.writeTool(verTwo, false)
	url := "/api/resources/tools/shared/mine/revisions/1/restore"
	admin := f.as(tenancy.RoleAdmin)

	t.Run("roles", func(t *testing.T) {
		for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder} { // Tool is an admin kind
			if rec := f.do(f.as(role), http.MethodPost, url, ""); !isRoleDenial(rec) {
				t.Errorf("%s restored: %d %s", role, rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("another tenant", func(t *testing.T) {
		if rec := f.do(f.tenantB(tenancy.RoleAdmin), http.MethodPost, url, ""); rec.Code != http.StatusNotFound {
			t.Errorf("tenant B = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a missing revision", func(t *testing.T) {
		if rec := f.do(admin, http.MethodPost, "/api/resources/tools/shared/mine/revisions/9/restore", ""); rec.Code != http.StatusNotFound {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("a stale resourceVersion is a 409", func(t *testing.T) {
		rec := f.do(admin, http.MethodPost, url, `{"resourceVersion":"1"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), f.tool("mine").ResourceVersion) {
			t.Errorf("code = %d %s", rec.Code, rec.Body.String())
		}
		if f.tool("mine").Spec.OCIRef != verTwo {
			t.Error("a refused restore changed the object")
		}
	})
	t.Run("a dry run changes nothing", func(t *testing.T) {
		before := len(f.revs.rows)
		if rec := f.do(admin, http.MethodPost, url+"?dryRun=true", ""); rec.Code != http.StatusOK {
			t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
		}
		if f.tool("mine").Spec.OCIRef != verTwo || len(f.revs.rows) != before {
			t.Error("a dry run wrote")
		}
	})
	t.Run("restores over the live object and records a restore revision", func(t *testing.T) {
		rec := f.do(admin, http.MethodPost, url, `{"resourceVersion":"`+f.tool("mine").ResourceVersion+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
		}
		got := f.tool("mine")
		if got.Spec.OCIRef != verOne || got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI || got.Annotations[tenancy.AnnotationLastModifiedBy] != builderUser || got.Labels[tenancy.LabelTenantID] != isoA {
			t.Errorf("tool = %+v %+v", got.Spec, got.ObjectMeta)
		}
		var body map[string]any
		decodeJSON(t, rec, &body)
		if body["ownership"] == nil {
			t.Errorf("the answer is the object with its ownership: %v", body)
		}
		last := f.revs.rows[len(f.revs.rows)-1]
		if last.Action != postgresql.RevisionRestore || last.Revision != 3 || last.RestoredFrom == nil || *last.RestoredFrom != 1 {
			t.Errorf("revision = %+v", last)
		}
		if ev := f.audit.events[len(f.audit.events)-1]; ev.Action != postgresql.AuditUpdate || !strings.Contains(ev.Summary, "revision 1") {
			t.Errorf("audit = %+v", ev)
		}
		// The restore shows up in the list with restoredFrom.
		var out revisionList
		decodeJSON(t, f.do(f.as(tenancy.RoleViewer), http.MethodGet, "/api/resources/tools/shared/mine/revisions", ""), &out)
		if out.Revisions[0].Action != postgresql.RevisionRestore || out.Revisions[0].RestoredFrom == nil || *out.Revisions[0].RestoredFrom != 1 {
			t.Errorf("list = %+v", out.Revisions[0])
		}
	})
	t.Run("a pending handoff is kept", func(t *testing.T) {
		tool := f.tool("mine")
		tenancy.SetHandoff(tool, time.Now().Add(time.Hour))
		if err := f.c.Update(context.Background(), tool); err != nil {
			t.Fatal(err)
		}
		if rec := f.do(admin, http.MethodPost, "/api/resources/tools/shared/mine/revisions/2/restore", ""); rec.Code != http.StatusOK {
			t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
		}
		if got := f.tool("mine"); got.Annotations[tenancy.AnnotationHandoff] == "" || got.Spec.OCIRef != verTwo {
			t.Errorf("tool = %+v %+v", got.Spec, got.Annotations)
		}
	})
}

func TestRestoreRefusesGitOwnedObjects(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	f.writeTool(verTwo, false)
	tool := f.tool("mine")
	tool.Annotations[tenancy.AnnotationOwner] = "git"
	if err := f.c.Update(context.Background(), tool); err != nil {
		t.Fatal(err)
	}
	rec := f.do(f.as(tenancy.RoleAdmin), http.MethodPost, "/api/resources/tools/shared/mine/revisions/1/restore", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "use Duplicate") {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	if f.tool("mine").Spec.OCIRef != verTwo || len(f.revs.rows) != 2 {
		t.Error("a refused restore wrote")
	}
}

func TestRestoreRecreatesADeletedObject(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	f.writeTool(verTwo, false)
	admin := f.as(tenancy.RoleAdmin)
	if rec := f.do(admin, http.MethodDelete, "/api/resources/tools/shared/mine", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	url := "/api/resources/tools/shared/mine/revisions/"

	if rec := f.do(admin, http.MethodPost, url+"1/restore", `{"resourceVersion":"5"}`); rec.Code != http.StatusConflict {
		t.Errorf("a resourceVersion for a deleted object = %d, want 409", rec.Code)
	}
	if rec := f.do(f.tenantB(tenancy.RoleAdmin), http.MethodPost, url+"1/restore", ""); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B restores A's deleted object: %d", rec.Code)
	}
	rec := f.do(admin, http.MethodPost, url+"1/restore", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	got := f.tool("mine")
	if got.Spec.OCIRef != verOne || got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI || got.Labels[tenancy.LabelTenantID] != isoA {
		t.Errorf("recreated tool = %+v %+v", got.Spec, got.ObjectMeta)
	}
	last := f.revs.rows[len(f.revs.rows)-1]
	if last.Action != postgresql.RevisionRestore || last.Revision != 4 || last.RestoredFrom == nil || *last.RestoredFrom != 1 {
		t.Errorf("revision = %+v", last)
	}
	if ev := f.audit.events[len(f.audit.events)-1]; ev.Action != postgresql.AuditCreate {
		t.Errorf("audit = %+v", ev)
	}
}

func TestRestoreDeletedObjectWhoseNameIsTaken(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool(verOne, true)
	if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodDelete, "/api/resources/tools/shared/mine", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	// Someone else created a Git-owned object under the name.
	taken := isoTool(sharedNS, "mine", tenancy.LabelTenantID, isoA)
	taken.Annotations = map[string]string{tenancy.AnnotationOwner: "git"}
	if err := f.c.Create(context.Background(), taken); err != nil {
		t.Fatal(err)
	}
	rec := f.do(f.as(tenancy.RoleAdmin), http.MethodPost, "/api/resources/tools/shared/mine/revisions/1/restore", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("restore over a git-owned object = %d %s", rec.Code, rec.Body.String())
	}
}

// Concurrent saves of one object in the same tenant take distinct numbers.
func TestConcurrentWritesTakeDistinctRevisionNumbers(t *testing.T) {
	f := newRevisionFixture(t)
	f.writeTool("v0", true)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			_ = f.revs.append(postgresqlScope(), "content-"+strconv.Itoa(i))
		})
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, r := range f.revs.rows {
		if seen[r.Revision] {
			t.Fatalf("revision %d repeated: %+v", r.Revision, f.revs.rows)
		}
		seen[r.Revision] = true
	}
	if len(seen) != 11 {
		t.Errorf("got %d revisions, want 11", len(seen))
	}
}

func postgresqlScope() context.Context {
	return postgresql.WithTenant(context.Background(), isoA)
}

func (f *fakeRevisions) append(scope context.Context, v string) error {
	obj, _ := json.Marshal(map[string]string{"v": v})
	_, _, err := f.AppendRevision(scope, postgresql.Revision{Action: postgresql.AuditUpdate, Kind: "Tool", Namespace: sharedNS, Name: "mine", Object: bytes.TrimSpace(obj)}, 0)
	return err
}

func TestRevisionFailureLogCarriesSmallSnapshotsOnly(t *testing.T) {
	attrsOf := func(snapshot []byte) map[string]any {
		attrs := revisionFailureAttrs(context.Background(), postgresql.Revision{Kind: kindAgent, Namespace: sharedNS, Name: "bot", Action: postgresql.AuditUpdate, UserID: builderUser, ResourceVersion: "9"}, snapshot, errors.New("down"))
		m := map[string]any{}
		for i := 0; i+1 < len(attrs); i += 2 {
			m[attrs[i].(string)] = attrs[i+1]
		}
		return m
	}
	small := []byte(`{"spec":{"systemPrompt":"secret"}}`)
	got := attrsOf(small)
	if got["snapshot"] != string(small) || got["snapshotBytes"] != len(small) || len(got["snapshotSHA256"].(string)) != 64 || got["kind"] != kindAgent || got["resourceVersion"] != "9" {
		t.Errorf("small = %v", got)
	}
	large := bytes.Repeat([]byte("x"), maxLoggedSnapshotBytes+1)
	got = attrsOf(large)
	if s, _ := got["snapshot"].(string); strings.Contains(s, "xxx") || !strings.Contains(s, "omitted") || got["snapshotBytes"] != len(large) || got["snapshotSHA256"] == "" {
		t.Errorf("large = %v", got)
	}
}
