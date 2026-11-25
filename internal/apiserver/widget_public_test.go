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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/checkpoint"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	chatSource = "chat"
	siteA      = "https://www.example.com"
	siteB      = "https://shop.example.org"
	evil       = "https://evil.example.net"
)

// fakeWidgetStore keeps rows per tenant like the real store's scope does.
type fakeWidgetStore struct {
	mu   sync.Mutex
	rows map[string]*postgresql.WidgetKey
}

func newFakeWidgetStore() *fakeWidgetStore {
	return &fakeWidgetStore{rows: map[string]*postgresql.WidgetKey{}}
}

func (f *fakeWidgetStore) visible(ctx context.Context, tenant string) bool {
	sc := scopeOf(ctx)
	return sc == postgresql.PlatformScope || sc == tenant
}

func (f *fakeWidgetStore) CreateWidgetKey(ctx context.Context, k postgresql.WidgetKey) (postgresql.WidgetKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	origins, err := postgresql.NormalizeOrigins(k.AllowedOrigins)
	if err != nil {
		return k, err
	}
	k.AllowedOrigins = origins
	if err := k.Validate(); err != nil {
		return k, err
	}
	k.TenantID, k.Enabled, k.CreatedAt = scopeOf(ctx), true, time.Now()
	for _, o := range f.rows {
		if o.TenantID == k.TenantID && o.Namespace == k.Namespace && o.Agent == k.Agent && o.Name == k.Name {
			return k, postgresql.ErrWidgetKeyExists
		}
	}
	f.rows[k.ID] = &k
	return k, nil
}

func (f *fakeWidgetStore) ListWidgetKeys(ctx context.Context, namespaces []string, agent string) ([]postgresql.WidgetKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []postgresql.WidgetKey{}
	for _, k := range f.rows {
		if !f.visible(ctx, k.TenantID) || (agent != "" && k.Agent != agent) {
			continue
		}
		if len(namespaces) > 0 {
			in := false
			for _, ns := range namespaces {
				in = in || ns == k.Namespace
			}
			if !in {
				continue
			}
		}
		out = append(out, *k)
	}
	return out, nil
}

func (f *fakeWidgetStore) GetWidgetKey(ctx context.Context, id string) (postgresql.WidgetKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.rows[id]
	if !ok || !f.visible(ctx, k.TenantID) {
		return postgresql.WidgetKey{}, postgresql.ErrWidgetKeyNotFound
	}
	return *k, nil
}

func (f *fakeWidgetStore) UpdateWidgetKey(ctx context.Context, id string, p postgresql.WidgetKeyPatch) (postgresql.WidgetKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.rows[id]
	if !ok || !f.visible(ctx, k.TenantID) {
		return postgresql.WidgetKey{}, postgresql.ErrWidgetKeyNotFound
	}
	next := *k
	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	if p.AllowedOrigins != nil {
		o, err := postgresql.NormalizeOrigins(p.AllowedOrigins)
		if err != nil {
			return next, err
		}
		next.AllowedOrigins = o
	}
	if p.RateLimitPerMin != nil {
		next.RateLimitPerMin = *p.RateLimitPerMin
	}
	if p.BudgetUSDPerDay != nil {
		next.BudgetUSDPerDay = *p.BudgetUSDPerDay
	}
	if err := next.Validate(); err != nil {
		return next, err
	}
	*k = next
	return next, nil
}

func (f *fakeWidgetStore) DeleteWidgetKey(ctx context.Context, id string) (postgresql.WidgetKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.rows[id]
	if !ok || !f.visible(ctx, k.TenantID) {
		return postgresql.WidgetKey{}, postgresql.ErrWidgetKeyNotFound
	}
	delete(f.rows, id)
	return *k, nil
}

type widgetFixture struct {
	*hookFixture
	keys *fakeWidgetStore
	cp   *checkpoint.InMemoryStore
}

func newWidgetFixture(t *testing.T) *widgetFixture {
	t.Helper()
	h := newHookFixture(t)
	f := &widgetFixture{hookFixture: h, keys: newFakeWidgetStore(), cp: checkpoint.NewInMemoryStore()}
	f.s.widgetKeys, f.s.checkpoint = f.keys, f.cp
	f.s.widget.secret = []byte("0123456789abcdef0123456789abcdef")
	return f
}

// newKey creates a key as tenant A's builder.
func (f *widgetFixture) newKey(name, extra string) postgresql.WidgetKey {
	f.t.Helper()
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/widget-keys",
		`{"namespace":"shared","agent":"support","name":"`+name+`","allowedOrigins":["`+siteA+`","`+siteB+`"]`+extra+`}`)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create widget key = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[postgresql.WidgetKey](f.t, rec)
}

func (f *widgetFixture) call(method, key, path, origin, token, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, "/widget/v1/"+key+path, strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set(widgetHeader, token)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *widgetFixture) session(key, origin string) string {
	f.t.Helper()
	rec := f.call(http.MethodPost, key, "/sessions", origin, "", "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("session = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[map[string]any](f.t, rec)["token"].(string)
}

func (f *widgetFixture) say(key, origin, token, text string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"input": text})
	return f.call(http.MethodPost, key, "/messages", origin, token, string(b))
}

func (f *widgetFixture) run(name string) agentorcav1alpha1.AgentRun {
	f.t.Helper()
	var r agentorcav1alpha1.AgentRun
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: name}, &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestWidget_ScriptIsPublicAndStatic(t *testing.T) {
	f := newWidgetFixture(t)
	f.s.authEnabled = true
	f.h = f.s.Handler()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/widget/v1.js", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(rec.Body.String(), "attachShadow") || strings.Contains(rec.Body.String(), "innerHTML =") {
		t.Errorf("script = %d %q", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/widget/v1.js", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST script = %d", rec.Code)
	}
}

func TestWidgetKeys_ManagementRolesScopeAndAudit(t *testing.T) {
	f := newWidgetFixture(t)
	body := `{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["` + siteA + `"]}`
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		if rec := f.api(idFor(role, isoA), http.MethodPost, "/api/widget-keys", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s create = %d", role, rec.Code)
		}
	}
	b := idFor(tenancy.RoleBuilder, isoB)
	if rec := f.api(b, http.MethodPost, "/api/widget-keys", body); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant's agent = %d", rec.Code)
	}
	builder := idFor(tenancy.RoleBuilder, isoA)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"unpublished":   {`{"namespace":"shared","agent":"draft","name":"x","allowedOrigins":["` + siteA + `"]}`, http.StatusConflict},
		"template":      {`{"namespace":"shared","agent":"tmpl","name":"x","allowedOrigins":["` + siteA + `"]}`, http.StatusConflict},
		"no origins":    {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":[]}`, http.StatusBadRequest},
		"wildcard":      {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["https://*.example.com"]}`, http.StatusBadRequest},
		"star":          {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["*"]}`, http.StatusBadRequest},
		"plain http":    {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["http://example.com"]}`, http.StatusBadRequest},
		"path":          {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["https://example.com/app"]}`, http.StatusBadRequest},
		"no name":       {`{"namespace":"shared","agent":"support","name":"","allowedOrigins":["` + siteA + `"]}`, http.StatusBadRequest},
		"huge budget":   {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["` + siteA + `"],"budgetUsdPerDay":1000000}`, http.StatusBadRequest},
		"too fast":      {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["` + siteA + `"],"rateLimitPerMin":99999}`, http.StatusBadRequest},
		"unknown field": {`{"namespace":"shared","agent":"support","name":"x","allowedOrigins":["` + siteA + `"],"tenantId":"` + isoB + `"}`, http.StatusBadRequest},
	} {
		if rec := f.api(builder, http.MethodPost, "/api/widget-keys", tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(f.keys.rows) != 0 || len(f.audit.events) != 0 {
		t.Fatalf("refused requests left rows %d or audit %d", len(f.keys.rows), len(f.audit.events))
	}

	k := f.newKey("site", "")
	if k.TenantID != isoA || !k.Enabled || k.RateLimitPerMin != postgresql.DefaultWidgetRatePerMin || k.BudgetUSDPerDay != postgresql.DefaultWidgetBudgetPerDay || len(k.AllowedOrigins) != 2 {
		t.Errorf("created = %+v", k)
	}
	if e := f.audit.events[0]; e.Action != postgresql.AuditCreate || e.Kind != kindWidgetKey || e.Name != hookTestAgent || e.UserID != builderUser {
		t.Errorf("audit = %+v", e)
	}
	// Tenant B cannot see, change or delete it.
	if got := evalDecode[[]postgresql.WidgetKey](t, f.api(b, http.MethodGet, "/api/widget-keys", "")); len(got) != 0 {
		t.Errorf("tenant B lists %+v", got)
	}
	if rec := f.api(b, http.MethodPatch, "/api/widget-keys/"+k.ID, `{"enabled":false}`); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B disables = %d", rec.Code)
	}
	if rec := f.api(b, http.MethodDelete, "/api/widget-keys/"+k.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B deletes = %d", rec.Code)
	}
	// A viewer lists but cannot change.
	v := idFor(tenancy.RoleViewer, isoA)
	if rec := f.api(v, http.MethodGet, "/api/widget-keys?agent=support", ""); rec.Code != http.StatusOK {
		t.Errorf("viewer list = %d", rec.Code)
	}
	if rec := f.api(v, http.MethodDelete, "/api/widget-keys/"+k.ID, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer delete = %d", rec.Code)
	}
	if rec := f.api(builder, http.MethodPatch, "/api/widget-keys/"+k.ID, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty patch = %d", rec.Code)
	}
	if rec := f.api(builder, http.MethodPatch, "/api/widget-keys/"+k.ID, `{"allowedOrigins":["*"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("wildcard patch = %d", rec.Code)
	}
	if rec := f.api(builder, http.MethodPatch, "/api/widget-keys/"+k.ID, `{"allowedOrigins":["`+siteA+`"],"budgetUsdPerDay":1}`); rec.Code != http.StatusOK {
		t.Errorf("patch = %d", rec.Code)
	}
	if rec := f.api(builder, http.MethodDelete, "/api/widget-keys/"+k.ID, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete = %d", rec.Code)
	}
	actions := make([]string, 0, len(f.audit.events))
	for _, e := range f.audit.events {
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "create,update,delete" {
		t.Errorf("audit actions = %v", actions)
	}
}

func TestWidget_OriginAllowAndDeny(t *testing.T) {
	f := newWidgetFixture(t)
	k := f.newKey("site", "")

	rec := f.call(http.MethodPost, k.ID, "/sessions", siteA, "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != siteA || rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("allowed origin = %d %v", rec.Code, rec.Header())
	}
	out := evalDecode[map[string]any](t, rec)
	if out["token"] == "" || out["expiresAt"] == "" || out["agent"] != hookTestAgent {
		t.Errorf("session = %v", out)
	}
	if rec := f.call(http.MethodPost, k.ID, "/sessions", "HTTPS://WWW.EXAMPLE.COM", "", ""); rec.Code != http.StatusOK {
		t.Errorf("origin case = %d", rec.Code)
	}
	for name, origin := range map[string]string{"other site": evil, "missing": "", "null": "null", "subdomain": "https://a.www.example.com",
		"other scheme": "http://www.example.com", "other port": "https://www.example.com:8443", "suffix trick": "https://www.example.com.evil.net"} {
		rec := f.call(http.MethodPost, k.ID, "/sessions", origin, "", "")
		if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: %d %v", name, rec.Code, rec.Header())
		}
	}

	// Preflights follow the same rule.
	pre := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/widget/v1/"+k.ID+"/messages", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec
	}
	if rec := pre(siteB); rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != siteB ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), widgetHeader) {
		t.Errorf("preflight allowed = %d %v", rec.Code, rec.Header())
	}
	if rec := pre(evil); rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight denied = %d %v", rec.Code, rec.Header())
	}

	// Removing an origin takes effect at once, even for a live session.
	tok := f.session(k.ID, siteB)
	if rec := f.say(k.ID, siteB, tok, "hi"); rec.Code != http.StatusAccepted {
		t.Fatalf("before = %d %s", rec.Code, rec.Body)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPatch, "/api/widget-keys/"+k.ID, `{"allowedOrigins":["`+siteA+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d", rec.Code)
	}
	if rec := f.say(k.ID, siteB, tok, "hi again"); rec.Code != http.StatusForbidden {
		t.Errorf("removed origin = %d", rec.Code)
	}
	// Unknown, malformed and disabled keys.
	if rec := f.call(http.MethodPost, "wgt_"+strings.Repeat("0", 32), "/sessions", siteA, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key = %d", rec.Code)
	}
	if rec := f.call(http.MethodPost, "nope", "/sessions", siteA, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("malformed key = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPatch, "/api/widget-keys/"+k.ID, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d", rec.Code)
	}
	if rec := f.call(http.MethodPost, k.ID, "/sessions", siteA, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("disabled key = %d", rec.Code)
	}
}

func TestWidget_SessionTokens(t *testing.T) {
	f := newWidgetFixture(t)
	k := f.newKey("one", "")
	k2 := f.newKey("two", "")
	tok := f.session(k.ID, siteA)

	if rec := f.say(k.ID, siteA, "", "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", rec.Code)
	}
	// Change one character in the middle of the token to a different one. Replacing the
	// tail is not enough: the last base64url character of an HMAC carries unused bits, so
	// about one token in a thousand decodes to the same bytes and is rightly accepted.
	mid := len(tok) / 2
	repl := byte('A')
	if tok[mid] == 'A' {
		repl = 'B'
	}
	if rec := f.say(k.ID, siteA, tok[:mid]+string(repl)+tok[mid+1:], "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered = %d", rec.Code)
	}
	if rec := f.say(k.ID, siteA, "garbage", "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("garbage = %d", rec.Code)
	}
	// A token is for its key, its origin, and the secret of this operator.
	if rec := f.say(k2.ID, siteA, tok, "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("another key's token = %d", rec.Code)
	}
	if rec := f.say(k.ID, siteB, tok, "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("another origin's token = %d", rec.Code)
	}
	if rec := f.say(k.ID, evil, tok, "hi"); rec.Code != http.StatusForbidden {
		t.Errorf("token from a denied origin = %d", rec.Code)
	}
	forged := signWidgetToken([]byte("some other secret"), widgetToken{Key: k.ID, Session: "wgs-x", Origin: siteA, Expires: f.now.Add(time.Hour).Unix()})
	if rec := f.say(k.ID, siteA, forged, "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged = %d", rec.Code)
	}
	// It expires.
	f.now = f.now.Add(WidgetSessionTTL + time.Second)
	if rec := f.say(k.ID, siteA, tok, "hi"); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired = %d", rec.Code)
	}
	// It is no credential anywhere else: the tenant API ignores it.
	f.s.authEnabled = true
	f.s.k8s = k8sfake.NewClientset() // its TokenReview authenticates nobody
	h := f.s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/triggers", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(widgetHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("widget token on the tenant API = %d", rec.Code)
	}
	if len(f.runs()) != 0 {
		t.Error("a refused message started a run")
	}
	// Without a signing secret there are no sessions.
	f.s.widget.secret = nil
	if rec := f.call(http.MethodPost, k.ID, "/sessions", siteA, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no secret = %d", rec.Code)
	}
}

func TestWidget_ChatUsesTheDeploymentPathAndOnlyTheKeysAgent(t *testing.T) {
	f := newWidgetFixture(t)
	k := f.newKey("site", "")
	tok := f.session(k.ID, siteA)

	rec := f.say(k.ID, siteA, tok, "Where is my order?")
	if rec.Code != http.StatusAccepted || rec.Header().Get("Access-Control-Allow-Origin") != siteA {
		t.Fatalf("say = %d %s", rec.Code, rec.Body)
	}
	name := evalDecode[map[string]string](t, rec)["runName"]
	r := f.run(name)
	claims, _ := verifyWidgetToken(f.s.widget.secret, tok, f.now)
	if r.Spec.AgentRef != "support" || r.Spec.Input != "Where is my order?" || r.Labels["agentorca.io/deployment"] != "support-dep" ||
		r.Labels["agentorca.io/source"] != chatSource || r.Labels["agentorca.io/session"] != claims.Session ||
		r.Labels[LabelWidgetKey] != k.ID || r.Labels[tenancy.LabelTenantID] != isoA || r.Annotations[AnnotationCreatedBy] != "widget:"+k.ID {
		t.Errorf("run = labels %v annotations %v spec %+v", r.Labels, r.Annotations, r.Spec)
	}
	cp, _ := f.cp.Load(context.Background(), claims.Session)
	if cp == nil || len(cp.ConversationHistory) != 1 || cp.ConversationHistory[0].Role != "user" || cp.LastRunRef != name {
		t.Errorf("checkpoint = %+v", cp)
	}

	// The visitor cannot name another agent, deployment, namespace or tenant.
	for _, extra := range []string{`"agent":"draft"`, `"deployment":"tmpl-dep"`, `"namespace":"a-only"`, `"tenantId":"` + isoB + `"`, `"sessionId":"wgs-other"`} {
		if rec := f.call(http.MethodPost, k.ID, "/messages", siteA, tok, `{"input":"hi",`+extra+`}`); rec.Code != http.StatusBadRequest {
			t.Errorf("extra field %s = %d", extra, rec.Code)
		}
	}
	for name, body := range map[string]string{"empty": `{"input":"  "}`, "too long": `{"input":"` + strings.Repeat("x", widgetMaxInput+1) + `"}`, "not json": `hello`} {
		if rec := f.call(http.MethodPost, k.ID, "/messages", siteA, tok, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", name, rec.Code)
		}
	}
	if got := len(f.runs()); got != 1 {
		t.Errorf("runs = %d, want 1", got)
	}
	// The next turn chains to the first run.
	f.markDone(name, "It left the warehouse.", "0.01")
	if rec := f.call(http.MethodGet, k.ID, "/runs/"+name, siteA, tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("poll = %d", rec.Code)
	}
	second := evalDecode[map[string]string](t, f.say(k.ID, siteA, tok, "And the invoice?"))["runName"]
	if got := f.run(second).Spec.PriorRunRef; got != name {
		t.Errorf("PriorRunRef = %q, want %q", got, name)
	}
}

func (f *widgetFixture) markDone(name, output, spend string) {
	f.t.Helper()
	r := f.run(name)
	r.Status.Phase, r.Status.Output, r.Status.SpendUSD = agentorcav1alpha1.AgentRunPhaseSucceeded, output, spend
	if err := f.c.Status().Update(context.Background(), &r); err != nil {
		f.t.Fatal(err)
	}
}

func TestWidget_PollingIsPerSessionAndSavesTheAnswerOnce(t *testing.T) {
	f := newWidgetFixture(t)
	k := f.newKey("site", "")
	k2 := f.newKey("other", "")
	tok, tok2, tokOtherKey := f.session(k.ID, siteA), f.session(k.ID, siteA), f.session(k2.ID, siteA)
	name := evalDecode[map[string]string](t, f.say(k.ID, siteA, tok, "hello"))["runName"]

	poll := func(key, token string) *httptest.ResponseRecorder {
		return f.call(http.MethodGet, key, "/runs/"+name, siteA, token, "")
	}
	if rec := poll(k.ID, tok); evalDecode[map[string]string](t, rec)["phase"] != "running" {
		t.Errorf("before = %s", rec.Body)
	}
	// Another visitor, or another key, cannot read it.
	if rec := poll(k.ID, tok2); rec.Code != http.StatusNotFound {
		t.Errorf("other session = %d", rec.Code)
	}
	if rec := poll(k2.ID, tokOtherKey); rec.Code != http.StatusNotFound {
		t.Errorf("other key = %d", rec.Code)
	}
	if rec := poll(k.ID, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", rec.Code)
	}

	f.markDone(name, "Hi there!", "0.002")
	for range 3 {
		got := evalDecode[map[string]string](t, poll(k.ID, tok))
		if got["phase"] != widgetPhaseDone || got["output"] != "Hi there!" {
			t.Fatalf("done = %v", got)
		}
	}
	claims, _ := verifyWidgetToken(f.s.widget.secret, tok, f.now)
	cp, _ := f.cp.Load(context.Background(), claims.Session)
	if cp == nil || len(cp.ConversationHistory) != 2 || cp.ConversationHistory[1].Role != "assistant" || cp.ConversationHistory[1].Content != "Hi there!" {
		t.Errorf("answer saved %+v", cp)
	}

	r := f.run(name)
	r.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	r.Status.Output = "secret internals"
	if err := f.c.Status().Update(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	rec := poll(k.ID, tok)
	if got := evalDecode[map[string]string](t, rec); got["phase"] != widgetPhaseFailed || strings.Contains(rec.Body.String(), "secret internals") {
		t.Errorf("failed = %s", rec.Body)
	}
}

func TestWidget_RateLimitPerKey(t *testing.T) {
	f := newWidgetFixture(t)
	slow := f.newKey("slow", `,"rateLimitPerMin":2`)
	fast := f.newKey("fast", "")
	// Two visitors share the key's budget of requests.
	t1, t2 := f.session(slow.ID, siteA), f.session(slow.ID, siteB)
	if rec := f.say(slow.ID, siteA, t1, "one"); rec.Code != http.StatusAccepted {
		t.Fatalf("1 = %d %s", rec.Code, rec.Body)
	}
	if rec := f.say(slow.ID, siteA, t1, "two"); rec.Code != http.StatusAccepted {
		t.Fatalf("2 = %d", rec.Code)
	}
	if rec := f.say(slow.ID, siteB, t2, "three"); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("over the limit = %d %v", rec.Code, rec.Header())
	}
	// Another key is not slowed.
	if rec := f.say(fast.ID, siteA, f.session(fast.ID, siteA), "hi"); rec.Code != http.StatusAccepted {
		t.Errorf("other key = %d", rec.Code)
	}
	// Session creation is limited as well.
	limited := false
	for range 5 {
		if rec := f.call(http.MethodPost, slow.ID, "/sessions", siteA, "", ""); rec.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("session creation was never limited")
	}
	// A denied origin cannot spend the key's limit.
	f.s.hooks.limiter = NewRateLimiter()
	for range 10 {
		f.call(http.MethodPost, slow.ID, "/sessions", evil, "", "")
	}
	if rec := f.call(http.MethodPost, slow.ID, "/sessions", siteA, "", ""); rec.Code != http.StatusOK {
		t.Errorf("after denied requests = %d", rec.Code)
	}
}

func TestWidget_BudgetCapAndConcurrency(t *testing.T) {
	f := newWidgetFixture(t)
	k := f.newKey("capped", `,"budgetUsdPerDay":0.5`)
	other := f.newKey("neighbour", `,"budgetUsdPerDay":0.5`)
	tok := f.session(k.ID, siteA)
	mk := func(name, key, phase, spend string, created time.Time) {
		r := &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shared", Labels: map[string]string{LabelWidgetKey: key}, CreationTimestamp: metav1.NewTime(created)},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "support", Input: "x"},
			Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhase(phase), SpendUSD: spend},
		}
		if err := f.c.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	today := f.now
	// Spend of another key and of yesterday does not count.
	mk("n-1", other.ID, "Succeeded", "9", today)
	mk("old-1", k.ID, "Succeeded", "9", today.Add(-48*time.Hour))
	mk("k-1", k.ID, "Succeeded", "0.3", today)
	if rec := f.say(k.ID, siteA, tok, "under the cap"); rec.Code != http.StatusAccepted {
		t.Fatalf("under the cap = %d %s", rec.Code, rec.Body)
	}
	mk("k-2", k.ID, "Succeeded", "0.25", today)
	rec := f.say(k.ID, siteA, tok, "over the cap")
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("over the budget = %d %s", rec.Code, rec.Body)
	}
	// The neighbour is over its own cap too (its run spent $9), and the first key's tenant is not blocked.
	if rec := f.say(other.ID, siteA, f.session(other.ID, siteA), "hi"); rec.Code != http.StatusPaymentRequired {
		t.Errorf("neighbour over its own cap = %d", rec.Code)
	}

	// Many unfinished runs at once are refused before they can spend.
	g := f.newKey("busy", `,"budgetUsdPerDay":100,"rateLimitPerMin":600`)
	gt := f.session(g.ID, siteA)
	for i := range widgetMaxActive {
		mk("g-"+strings.Repeat("a", i+1), g.ID, "Running", "", today)
	}
	if rec := f.say(g.ID, siteA, gt, "one too many"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("too many active = %d %s", rec.Code, rec.Body)
	}
}

func TestWidget_TenantBoundaries(t *testing.T) {
	f := newWidgetFixture(t)
	k := f.newKey("site", "")
	tok := f.session(k.ID, siteA)

	// The key's tenant is the one on its row. A tenant that may not use the
	// namespace, or that is inactive, gets nothing and no run is made.
	f.s.hooks.tenant = func(id string) (*TenantIdentity, bool) { return idFor(tenancy.RoleRunner, isoB), id == isoA }
	if rec := f.say(k.ID, siteA, tok, "as tenant B"); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B on A's agent = %d %s", rec.Code, rec.Body)
	}
	f.s.hooks.tenant = func(string) (*TenantIdentity, bool) { return nil, false }
	if rec := f.say(k.ID, siteA, tok, "inactive"); rec.Code != http.StatusNotFound {
		t.Errorf("inactive tenant = %d", rec.Code)
	}
	f.s.hooks.tenant = func(string) (*TenantIdentity, bool) {
		a := tenantA()
		a.Roles, a.AllowedAgents = tenancy.RoleRunner.Expand(), []string{"someone-else"}
		return a, true
	}
	if rec := f.say(k.ID, siteA, tok, "agent not allowed"); rec.Code != http.StatusNotFound {
		t.Errorf("agent not allowed = %d", rec.Code)
	}
	// Unpublished since: no deployment, no chat.
	f.s.hooks.tenant = func(string) (*TenantIdentity, bool) {
		a := tenantA()
		a.Roles = tenancy.RoleRunner.Expand()
		return a, true
	}
	var dep agentorcav1alpha1.AgentDeployment
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "support-dep"}, &dep); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}
	if rec := f.say(k.ID, siteA, tok, "unpublished"); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished = %d", rec.Code)
	}
	if len(f.runs()) != 0 {
		t.Error("a refused message started a run")
	}
}

func TestWidget_NoStoreMeansNoWidget(t *testing.T) {
	isoMode(t, tenancy.ModeEnforce)
	s := &UIServer{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/wgt_"+strings.Repeat("a", 32)+"/sessions", nil)
	req.Header.Set("Origin", siteA)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no store = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/widget-keys", nil), idFor(tenancy.RoleViewer, isoA)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("management without a store = %d", rec.Code)
	}
}
