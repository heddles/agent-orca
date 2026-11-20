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
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeTriggerStore keeps rows per tenant like the real store's scope does.
type fakeTriggerStore struct {
	mu   sync.Mutex
	rows map[string]*postgresql.Trigger
}

func newFakeTriggerStore() *fakeTriggerStore {
	return &fakeTriggerStore{rows: map[string]*postgresql.Trigger{}}
}

func (f *fakeTriggerStore) visible(ctx context.Context, tenant string) bool {
	sc := scopeOf(ctx)
	return sc == postgresql.PlatformScope || sc == tenant
}

func (f *fakeTriggerStore) CreateTrigger(ctx context.Context, t postgresql.Trigger) (postgresql.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := t.Validate(); err != nil {
		return t, err
	}
	t.TenantID, t.Enabled, t.CreatedAt = scopeOf(ctx), true, time.Now()
	for _, o := range f.rows {
		if o.TenantID == t.TenantID && o.Namespace == t.Namespace && o.Agent == t.Agent && o.Name == t.Name {
			return t, postgresql.ErrTriggerExists
		}
	}
	f.rows[t.ID] = &t
	return t, nil
}

func (f *fakeTriggerStore) ListTriggers(ctx context.Context, namespaces []string, agent string) ([]postgresql.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []postgresql.Trigger{}
	for _, t := range f.rows {
		if !f.visible(ctx, t.TenantID) || (agent != "" && t.Agent != agent) {
			continue
		}
		if len(namespaces) > 0 {
			in := false
			for _, ns := range namespaces {
				in = in || ns == t.Namespace
			}
			if !in {
				continue
			}
		}
		out = append(out, *t)
	}
	return out, nil
}

func (f *fakeTriggerStore) GetTrigger(ctx context.Context, id string) (postgresql.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || !f.visible(ctx, t.TenantID) {
		return postgresql.Trigger{}, postgresql.ErrTriggerNotFound
	}
	return *t, nil
}

func (f *fakeTriggerStore) SetTriggerEnabled(ctx context.Context, id string, enabled bool) (postgresql.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || !f.visible(ctx, t.TenantID) {
		return postgresql.Trigger{}, postgresql.ErrTriggerNotFound
	}
	t.Enabled = enabled
	return *t, nil
}

func (f *fakeTriggerStore) DeleteTrigger(ctx context.Context, id string) (postgresql.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || !f.visible(ctx, t.TenantID) {
		return postgresql.Trigger{}, postgresql.ErrTriggerNotFound
	}
	delete(f.rows, id)
	return *t, nil
}

func (f *fakeTriggerStore) RecordTriggerFire(ctx context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.rows[id]; ok && f.visible(ctx, t.TenantID) {
		t.LastFiredAt = &at
		t.FireCount++
	}
	return nil
}

const hookTestAgent = "support"

type hookFixture struct {
	t     *testing.T
	s     *UIServer
	c     client.Client
	store *fakeTriggerStore
	audit *fakeAudit
	h     http.Handler
	now   time.Time
}

func hookAgent(ns, name string, labels map[string]string) *agentorcav1alpha1.Agent {
	l := map[string]string{tenancy.LabelTenantID: isoA}
	maps.Copy(l, labels)
	return &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: l}}
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	isoMode(t, tenancy.ModeEnforce)
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		hookAgent("shared", "support", nil),
		hookAgent("shared", "draft", nil),
		hookAgent("shared", "tmpl", map[string]string{LabelTemplate: templateMarker}),
		&agentorcav1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "support-dep", Namespace: "shared"}, Spec: agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "support"}},
		&agentorcav1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-dep", Namespace: "shared"}, Spec: agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "tmpl"}},
	).Build()
	f := &hookFixture{t: t, c: cl, store: newFakeTriggerStore(), audit: &fakeAudit{}, now: time.Unix(1_800_000_000, 0)}
	f.s = &UIServer{crdClient: cl, triggers: f.store, audit: f.audit}
	f.s.hooks.now = func() time.Time { return f.now }
	f.s.hooks.tenant = func(id string) (*TenantIdentity, bool) {
		if id != isoA {
			return nil, false
		}
		a := tenantA()
		a.Roles = tenancy.RoleRunner.Expand()
		return a, true
	}
	f.h = f.s.Handler()
	return f
}

func (f *hookFixture) api(id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// create makes a trigger as tenant A's builder and returns it with its secret.
func (f *hookFixture) create(name, extra string) triggerCreated {
	f.t.Helper()
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/triggers",
		`{"namespace":"shared","agent":"`+hookTestAgent+`","name":"`+name+`"`+extra+`}`)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create trigger = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[triggerCreated](f.t, rec)
}

// fire sends a request signed with secret; mutate may change it before it is sent.
func (f *hookFixture) fire(id, secret, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	ts := strconv.FormatInt(f.now.Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+id, strings.NewReader(body))
	req.Header.Set(HookTimestampHeader, ts)
	req.Header.Set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor(secret), ts, []byte(body)))
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *hookFixture) runs() []agentorcav1alpha1.AgentRun {
	f.t.Helper()
	var list agentorcav1alpha1.AgentRunList
	if err := f.c.List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	return list.Items
}

func TestTriggers_CreateShowsSecretOnceAndStoresOnlyTheKey(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("github", `,"inputPath":"issue.title"`)
	if !strings.HasPrefix(c.Secret, "whsec_") || c.HookPath != "/hooks/"+c.Trigger.ID || c.Trigger.TenantID != isoA || !c.Trigger.Enabled {
		t.Fatalf("created = %+v", c)
	}
	if c.Trigger.RateLimitPerMin != postgresql.DefaultTriggerRatePerMin {
		t.Errorf("default rate = %d", c.Trigger.RateLimitPerMin)
	}
	stored := f.store.rows[c.Trigger.ID]
	if stored.SigningKey == c.Secret || stored.SigningKey != postgresql.SigningKeyFor(c.Secret) {
		t.Errorf("the secret must not be stored: %q", stored.SigningKey)
	}
	// Neither the list nor the audit trail carry the secret or its key.
	rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/triggers?namespace=shared&agent=support", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), c.Secret) || strings.Contains(rec.Body.String(), stored.SigningKey) {
		t.Errorf("list leaks the secret: %d %s", rec.Code, rec.Body)
	}
	if got := evalDecode[[]postgresql.Trigger](t, rec); len(got) != 1 || got[0].ID != c.Trigger.ID || got[0].InputPath != "issue.title" {
		t.Errorf("list = %+v", got)
	}
	if len(f.audit.events) != 1 {
		t.Fatalf("audit = %+v", f.audit.events)
	}
	e := f.audit.events[0]
	if e.Action != postgresql.AuditCreate || e.Kind != kindTrigger || e.Name != hookTestAgent || e.UserID != builderUser ||
		strings.Contains(e.Summary, c.Secret) || strings.Contains(e.Summary, stored.SigningKey) {
		t.Errorf("audit event = %+v", e)
	}
}

func TestTriggers_RolesAndScope(t *testing.T) {
	f := newHookFixture(t)
	body := `{"namespace":"shared","agent":"support","name":"x"}`
	if rec := f.api(idFor(tenancy.RoleRunner, isoA), http.MethodPost, "/api/triggers", body); rec.Code != http.StatusForbidden {
		t.Errorf("runner create = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodPost, "/api/triggers", body); rec.Code != http.StatusForbidden {
		t.Errorf("viewer create = %d", rec.Code)
	}
	// Tenant B has no access to namespace a-only, and the agent in shared is A's.
	if rec := f.api(idFor(tenancy.RoleBuilder, isoB), http.MethodPost, "/api/triggers", `{"namespace":"a-only","agent":"support","name":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("foreign namespace create = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoB), http.MethodPost, "/api/triggers", body); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant's agent create = %d %s", rec.Code, rec.Body)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/triggers", `{"namespace":"shared","agent":"draft","name":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("unpublished agent create = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/triggers", `{"namespace":"shared","agent":"tmpl","name":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("template create = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/triggers", `{"namespace":"shared","agent":"support","name":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no name = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/triggers", `{"namespace":"shared","agent":"support","name":"x","rateLimitPerMin":100000}`); rec.Code != http.StatusBadRequest {
		t.Errorf("huge rate = %d", rec.Code)
	}
	if len(f.store.rows) != 0 || len(f.audit.events) != 0 {
		t.Errorf("refused requests left rows %d or audit %d", len(f.store.rows), len(f.audit.events))
	}

	c := f.create("mine", "")
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/triggers", `{"namespace":"shared","agent":"support","name":"mine"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d", rec.Code)
	}
	// Tenant B sees and changes nothing of A's.
	b := idFor(tenancy.RoleBuilder, isoB)
	if got := evalDecode[[]postgresql.Trigger](t, f.api(b, http.MethodGet, "/api/triggers", "")); len(got) != 0 {
		t.Errorf("tenant B lists %+v", got)
	}
	if rec := f.api(b, http.MethodGet, "/api/triggers?namespace=a-only", ""); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B lists A's namespace = %d", rec.Code)
	}
	if rec := f.api(b, http.MethodPatch, "/api/triggers/"+c.Trigger.ID, `{"enabled":false}`); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B disables = %d", rec.Code)
	}
	if rec := f.api(b, http.MethodDelete, "/api/triggers/"+c.Trigger.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B deletes = %d", rec.Code)
	}
	if !f.store.rows[c.Trigger.ID].Enabled {
		t.Error("tenant B changed A's trigger")
	}
	// A viewer reads but cannot change.
	v := idFor(tenancy.RoleViewer, isoA)
	if rec := f.api(v, http.MethodGet, "/api/triggers", ""); rec.Code != http.StatusOK {
		t.Errorf("viewer list = %d", rec.Code)
	}
	if rec := f.api(v, http.MethodDelete, "/api/triggers/"+c.Trigger.ID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer delete = %d", rec.Code)
	}
	if rec := f.api(nil, http.MethodGet, "/api/triggers/"+c.Trigger.ID, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET one = %d", rec.Code)
	}
}

func TestTriggers_NoPostgres(t *testing.T) {
	isoMode(t, tenancy.ModeEnforce)
	s := &UIServer{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/triggers", nil), idFor(tenancy.RoleViewer, isoA)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+strings.Repeat("a", 4), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("hook with no store = %d", rec.Code)
	}
}

func TestHook_StartsARunAsTheTriggerOfItsTenant(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("github", "")
	rec := f.fire(c.Trigger.ID, c.Secret, `{"event":"opened","title":"printer on fire"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("fire = %d %s", rec.Code, rec.Body)
	}
	runs := f.runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	r := runs[0]
	if r.Namespace != "shared" || r.Spec.AgentRef != "support" || r.Spec.Input != `{"event":"opened","title":"printer on fire"}` {
		t.Errorf("run = %+v", r.Spec)
	}
	if r.Annotations[AnnotationCreatedBy] != "trigger:"+c.Trigger.ID || r.Labels[LabelTriggerID] != c.Trigger.ID ||
		r.Labels[tenancy.LabelTenantID] != isoA || r.Labels["agentorca.io/tenant"] != isoAName {
		t.Errorf("run metadata: labels %v annotations %v", r.Labels, r.Annotations)
	}
	if got := evalDecode[map[string]string](t, rec); got["runName"] != r.Name {
		t.Errorf("answer = %v", got)
	}
	if fired := f.store.rows[c.Trigger.ID]; fired.FireCount != 1 || fired.LastFiredAt == nil {
		t.Errorf("fire not recorded: %+v", fired)
	}
}

func TestHook_InputPath(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("path", `,"inputPath":"issue.labels.0"`)
	if rec := f.fire(c.Trigger.ID, c.Secret, `{"issue":{"labels":["bug","ui"]}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("fire = %d %s", rec.Code, rec.Body)
	}
	if runs := f.runs(); len(runs) != 1 || runs[0].Spec.Input != "bug" {
		t.Fatalf("runs = %+v", runs)
	}
	for name, body := range map[string]string{"not json": "plain text", "no field": `{"issue":{}}`, "empty": `{"issue":{"labels":[""]}}`, "past the end": `{"issue":{"labels":[]}}`} {
		if rec := f.fire(c.Trigger.ID, c.Secret, body, func(r *http.Request) { r.Header.Set("X-Nonce", name) }); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(f.runs()) != 1 {
		t.Error("a refused body started a run")
	}
}

func TestHook_Signature(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("sig", "")
	set := func(h, v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set(h, v) } }
	ts := strconv.FormatInt(f.now.Unix(), 10)
	cases := map[string][]func(*http.Request){
		"no signature":      {func(r *http.Request) { r.Header.Del(HookSignatureHeader) }},
		"garbage":           {set(HookSignatureHeader, "sha256=zz")},
		"no prefix":         {set(HookSignatureHeader, strings.TrimPrefix(SignHook(postgresql.SigningKeyFor(c.Secret), ts, []byte("hello")), "sha256="))},
		"wrong secret":      {set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor("whsec_other"), ts, []byte("hello")))},
		"the secret itself": {set(HookSignatureHeader, SignHook(c.Secret, ts, []byte("hello")))},
		"other body":        {set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor(c.Secret), ts, []byte("bye")))},
		"body only":         {set(HookSignatureHeader, bodyOnlySignature(postgresql.SigningKeyFor(c.Secret), "hello"))},
		"no timestamp":      {func(r *http.Request) { r.Header.Del(HookTimestampHeader) }},
		"bad timestamp":     {set(HookTimestampHeader, "yesterday")},
	}
	for name, m := range cases {
		if rec := f.fire(c.Trigger.ID, c.Secret, "hello", m...); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(f.runs()) != 0 {
		t.Fatal("a bad signature started a run")
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "hello"); rec.Code != http.StatusAccepted {
		t.Errorf("the right signature = %d", rec.Code)
	}
}

func bodyOnlySignature(key, body string) string {
	return SignHook(key, "", []byte(body))
}

func TestHook_ReplayWindow(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("replay", "")
	signedAt := func(at time.Time) func(*http.Request) {
		return func(r *http.Request) {
			ts := strconv.FormatInt(at.Unix(), 10)
			r.Header.Set(HookTimestampHeader, ts)
			r.Header.Set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor(c.Secret), ts, []byte("hi")))
		}
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "hi", signedAt(f.now.Add(-HookReplayWindow-time.Second))); rec.Code != http.StatusUnauthorized {
		t.Errorf("stale = %d", rec.Code)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "hi", signedAt(f.now.Add(HookReplayWindow+time.Second))); rec.Code != http.StatusUnauthorized {
		t.Errorf("from the future = %d", rec.Code)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "hi", signedAt(f.now.Add(-time.Minute))); rec.Code != http.StatusAccepted {
		t.Fatalf("inside the window = %d", rec.Code)
	}
	// The same request again is a replay, even though it is still fresh.
	if rec := f.fire(c.Trigger.ID, c.Secret, "hi", signedAt(f.now.Add(-time.Minute))); rec.Code != http.StatusUnauthorized {
		t.Errorf("replay = %d", rec.Code)
	}
	if len(f.runs()) != 1 {
		t.Errorf("runs = %d", len(f.runs()))
	}
	// A new timestamp is a new request.
	if rec := f.fire(c.Trigger.ID, c.Secret, "hi", signedAt(f.now.Add(-30*time.Second))); rec.Code != http.StatusAccepted {
		t.Errorf("fresh retry = %d", rec.Code)
	}
}

func TestHook_BodySize(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("big", "")
	if rec := f.fire(c.Trigger.ID, c.Secret, strings.Repeat("x", HookMaxBodyBytes+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the cap = %d", rec.Code)
	}
	// Without a Content-Length (chunked) the read itself is capped.
	ts := strconv.FormatInt(f.now.Unix(), 10)
	big := strings.Repeat("y", HookMaxBodyBytes+10)
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+c.Trigger.ID, strings.NewReader(big))
	req.ContentLength = -1
	req.Header.Set(HookTimestampHeader, ts)
	req.Header.Set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor(c.Secret), ts, []byte(big)))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked over the cap = %d", rec.Code)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, strings.Repeat("x", HookMaxBodyBytes)); rec.Code != http.StatusAccepted {
		t.Errorf("at the cap = %d %s", rec.Code, rec.Body)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "  "); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty body = %d", rec.Code)
	}
}

func TestHook_RateLimitPerTrigger(t *testing.T) {
	f := newHookFixture(t)
	slow := f.create("slow", `,"rateLimitPerMin":2`)
	other := f.create("other", "")
	nonce := func(n string) func(*http.Request) {
		return func(r *http.Request) {
			ts := strconv.FormatInt(f.now.Add(time.Duration(len(n))*time.Second).Unix(), 10)
			r.Header.Set(HookTimestampHeader, ts)
			r.Header.Set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor(slow.Secret), ts, []byte("go")))
		}
	}
	for i, n := range []string{"a", "bb"} {
		if rec := f.fire(slow.Trigger.ID, slow.Secret, "go", nonce(n)); rec.Code != http.StatusAccepted {
			t.Fatalf("request %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.fire(slow.Trigger.ID, slow.Secret, "go", nonce("ccc"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("over the limit = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// An unsigned flood cannot use up the limit of the trigger.
	for range 10 {
		f.fire(slow.Trigger.ID, "whsec_wrong", "go")
	}
	// The limit is per trigger: another one is not slowed.
	if rec := f.fire(other.Trigger.ID, other.Secret, "go"); rec.Code != http.StatusAccepted {
		t.Errorf("other trigger = %d", rec.Code)
	}
	if got := len(f.runs()); got != 3 {
		t.Errorf("runs = %d, want 3", got)
	}
	// A limited request can be retried with a new signature: it was not marked as seen.
	f.s.hooks.limiter = NewRateLimiter()
	if rec := f.fire(slow.Trigger.ID, slow.Secret, "go", nonce("ccc")); rec.Code != http.StatusAccepted {
		t.Errorf("retry after the limit = %d %s", rec.Code, rec.Body)
	}
}

func TestHook_DisabledDeletedAndUnknownAre404(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("life", "")
	b := idFor(tenancy.RoleBuilder, isoA)

	if rec := f.api(b, http.MethodPatch, "/api/triggers/"+c.Trigger.ID, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d", rec.Code)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "x"); rec.Code != http.StatusNotFound {
		t.Errorf("disabled = %d", rec.Code)
	}
	if rec := f.api(b, http.MethodPatch, "/api/triggers/"+c.Trigger.ID, `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d", rec.Code)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "x"); rec.Code != http.StatusAccepted {
		t.Errorf("enabled again = %d", rec.Code)
	}
	if rec := f.api(b, http.MethodPatch, "/api/triggers/"+c.Trigger.ID, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("patch without enabled = %d", rec.Code)
	}
	if rec := f.api(b, http.MethodDelete, "/api/triggers/"+c.Trigger.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "y"); rec.Code != http.StatusNotFound {
		t.Errorf("deleted = %d", rec.Code)
	}
	if rec := f.fire("trg_"+strings.Repeat("0", 32), c.Secret, "y"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown = %d", rec.Code)
	}
	if rec := f.fire("not-an-id", c.Secret, "y"); rec.Code != http.StatusNotFound {
		t.Errorf("malformed id = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/hooks/"+c.Trigger.ID, nil)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	actions := make([]string, 0, len(f.audit.events))
	for _, e := range f.audit.events {
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "create,update,update,delete" {
		t.Errorf("audit actions = %v", actions)
	}
}

func TestHook_TenantBoundaries(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("iso", "")

	// A trigger row that names another tenant's ID resolves to that tenant, which
	// may not use this namespace, so there is no run.
	f.store.rows[c.Trigger.ID].TenantID = isoB
	f.s.hooks.tenant = func(id string) (*TenantIdentity, bool) {
		return idFor(tenancy.RoleRunner, id), id == isoB
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "x1"); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B's trigger on A's agent = %d %s", rec.Code, rec.Body)
	}
	// An inactive tenant (suspended, deleted) stops its triggers.
	f.store.rows[c.Trigger.ID].TenantID = isoA
	f.s.hooks.tenant = func(string) (*TenantIdentity, bool) { return nil, false }
	if rec := f.fire(c.Trigger.ID, c.Secret, "x2"); rec.Code != http.StatusNotFound {
		t.Errorf("inactive tenant = %d", rec.Code)
	}
	// An agent the tenant may not invoke.
	f.s.hooks.tenant = func(id string) (*TenantIdentity, bool) {
		a := tenantA()
		a.Roles, a.AllowedAgents = tenancy.RoleRunner.Expand(), []string{"someone-else"}
		return a, true
	}
	if rec := f.fire(c.Trigger.ID, c.Secret, "x3"); rec.Code != http.StatusNotFound {
		t.Errorf("agent not allowed = %d", rec.Code)
	}
	// A template, or an agent that was deleted since.
	var agent agentorcav1alpha1.Agent
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "support"}, &agent)
	if err := f.c.Delete(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	f.s.hooks.tenant = func(string) (*TenantIdentity, bool) { a := tenantA(); return a, true }
	if rec := f.fire(c.Trigger.ID, c.Secret, "x4"); rec.Code != http.StatusNotFound {
		t.Errorf("deleted agent = %d", rec.Code)
	}
	if len(f.runs()) != 0 {
		t.Error("a refused hook started a run")
	}
}

func TestHook_IsReachableWithoutASessionButTheAPIIsNot(t *testing.T) {
	f := newHookFixture(t)
	c := f.create("public", "")
	f.s.authEnabled = true
	h := f.s.Handler()

	ts := strconv.FormatInt(f.now.Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+c.Trigger.ID, strings.NewReader("go"))
	req.Header.Set(HookTimestampHeader, ts)
	req.Header.Set(HookSignatureHeader, SignHook(postgresql.SigningKeyFor(c.Secret), ts, []byte("go")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("signed hook without a session = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/triggers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("trigger API without a session = %d", rec.Code)
	}
}

func TestHook_InputHelper(t *testing.T) {
	for _, tc := range []struct {
		body, path, want string
		status           int
	}{
		{"hello", "", "hello", 0},
		{`{"a":{"b":42}}`, "a.b", "42", 0},
		{`{"a":{"b":{"c":1}}}`, "a.b", `{"c":1}`, 0},
		{`{"a":null}`, "a", "", 422},
		{`[1,2]`, "5", "", 422},
	} {
		got, status, _ := hookInput([]byte(tc.body), tc.path)
		if got != tc.want || status != tc.status {
			t.Errorf("hookInput(%q, %q) = %q, %d", tc.body, tc.path, got, status)
		}
	}
}
