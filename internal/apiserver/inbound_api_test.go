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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeInboundStore keeps rows per tenant like the real store's scope does. It
// serves both the API and the pipeline.
type fakeInboundStore struct {
	mu    sync.Mutex
	conns map[string]*postgresql.InboundConnection
	msgs  map[string]*postgresql.InboundMessage
}

func newFakeInboundStore() *fakeInboundStore {
	return &fakeInboundStore{conns: map[string]*postgresql.InboundConnection{}, msgs: map[string]*postgresql.InboundMessage{}}
}

func visibleTo(ctx context.Context, tenant string) bool {
	sc := scopeOf(ctx)
	return sc == postgresql.PlatformScope || sc == tenant
}

func (f *fakeInboundStore) CreateInboundConnection(ctx context.Context, c postgresql.InboundConnection) (postgresql.InboundConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.Validate(); err != nil {
		return c, err
	}
	c.TenantID, c.Enabled, c.CreatedAt = scopeOf(ctx), true, time.Now()
	for _, o := range f.conns {
		if o.TenantID == c.TenantID && o.Namespace == c.Namespace && o.Agent == c.Agent && o.Name == c.Name {
			return c, postgresql.ErrInboundExists
		}
	}
	f.conns[c.ID] = &c
	return c, nil
}

func (f *fakeInboundStore) ListInboundConnections(ctx context.Context, namespaces []string, agent string) ([]postgresql.InboundConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []postgresql.InboundConnection{}
	for _, c := range f.conns {
		if !visibleTo(ctx, c.TenantID) || (agent != "" && c.Agent != agent) {
			continue
		}
		if len(namespaces) > 0 {
			in := false
			for _, ns := range namespaces {
				in = in || ns == c.Namespace
			}
			if !in {
				continue
			}
		}
		out = append(out, *c)
	}
	return out, nil
}

func (f *fakeInboundStore) GetInboundConnection(ctx context.Context, id string) (postgresql.InboundConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.conns[id]
	if !ok || !visibleTo(ctx, c.TenantID) {
		return postgresql.InboundConnection{}, postgresql.ErrInboundNotFound
	}
	return *c, nil
}

func (f *fakeInboundStore) UpdateInboundConnection(ctx context.Context, id string, p postgresql.InboundPatch) (postgresql.InboundConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.conns[id]
	if !ok || !visibleTo(ctx, c.TenantID) {
		return postgresql.InboundConnection{}, postgresql.ErrInboundNotFound
	}
	if p.Enabled != nil {
		c.Enabled = *p.Enabled
	}
	if p.Config != nil {
		c.Config = *p.Config
	}
	if p.SecretRef != nil {
		c.SecretRef = *p.SecretRef
	}
	return *c, nil
}

func (f *fakeInboundStore) DeleteInboundConnection(ctx context.Context, id string) (postgresql.InboundConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.conns[id]
	if !ok || !visibleTo(ctx, c.TenantID) {
		return postgresql.InboundConnection{}, postgresql.ErrInboundNotFound
	}
	delete(f.conns, id)
	return *c, nil
}

func (f *fakeInboundStore) ClaimInboundMessage(_ context.Context, m postgresql.InboundMessage) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := m.ConnectionID + "|" + m.MessageKey
	if _, dup := f.msgs[k]; dup {
		return false, nil
	}
	m.Status = postgresql.InboundReceived
	f.msgs[k] = &m
	return true, nil
}

func (f *fakeInboundStore) UpdateInboundMessage(_ context.Context, conn, key string, u postgresql.InboundUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.msgs[conn+"|"+key]; m != nil {
		m.Status, m.Reason, m.RunName = u.Status, u.Reason, u.RunName
	}
	return nil
}

func (f *fakeInboundStore) ListInboundMessagesByStatus(context.Context, string, int) ([]postgresql.InboundMessage, error) {
	return nil, nil
}

func (f *fakeInboundStore) LatestInboundRunInThread(context.Context, string, string) (string, string, error) {
	return "", "", nil
}

func (f *fakeInboundStore) CountInboundBySender(context.Context, string, string, time.Time) (int, error) {
	return 0, nil
}

func (f *fakeInboundStore) RecordInboundStatus(context.Context, string, postgresql.InboundStatus) error {
	return nil
}

func (f *fakeInboundStore) DeleteInboundMessagesBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeInboundStore) status(conn string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs {
		if m.ConnectionID == conn {
			out = append(out, m.Status+":"+m.Reason)
		}
	}
	return out
}

// chanAdapter is a made-up channel that posts signed JSON {"from","id","text"}.
type chanAdapter struct{ replay inbound.ReplayCache }

func (a *chanAdapter) Kind() string                         { return "chan" }
func (a *chanAdapter) Capabilities() inbound.Capabilities   { return inbound.Capabilities{Threads: true} }
func (a *chanAdapter) ValidateConfig(json.RawMessage) error { return nil }
func (a *chanAdapter) Reply(context.Context, inbound.Connection, inbound.ReplyTarget, inbound.Result) error {
	return nil
}

func (a *chanAdapter) Receive(_ context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	body, err := inbound.ReadSigned(r, c.Secret.Get("signingSecret"), 1<<16, time.Now(), &a.replay)
	if err != nil {
		return inbound.Message{}, err
	}
	var p struct{ From, ID, Text string }
	if err := json.Unmarshal(body, &p); err != nil {
		return inbound.Message{}, err
	}
	return inbound.Message{Sender: inbound.Sender{ID: p.From}, MessageID: p.ID, Text: p.Text, Size: len(body), Reply: inbound.ReplyTarget(`{}`)}, nil
}

func (a *chanAdapter) Test(context.Context, inbound.Connection) inbound.TestReport {
	return inbound.TestReport{OK: true, Details: map[string]string{"folders": "1"}}
}

const inboundSecretValue = "chan-signing-secret"

type inboundFixture struct {
	*hookFixture
	store *fakeInboundStore
}

func newInboundFixture(t *testing.T) *inboundFixture {
	t.Helper()
	f := &inboundFixture{hookFixture: newHookFixture(t), store: newFakeInboundStore()}
	f.s.inboundConns = f.store
	reg := inbound.NewRegistry()
	reg.Register(&chanAdapter{})
	br := f.s.InboundBridge()
	f.s.inboundPipe = &inbound.Pipeline{Store: f.store, Launcher: br, Runs: br, Resumer: br, Registry: reg, Secrets: br.ReadSecret}
	for name, labels := range map[string]map[string]string{
		"creds":    {inbound.SecretLabel: inbound.SecretLabelValue},
		"unlabled": nil,
	} {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shared", Labels: labels}, Data: map[string][]byte{"signingSecret": []byte(inboundSecretValue)}}
		if err := f.c.Create(context.Background(), sec); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

const inboundAgent = "support"

const chanConfig = `{"allowSenders":["alice@example.com"]}`

func (f *inboundFixture) create(extra string) *httptest.ResponseRecorder {
	return f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections",
		`{"namespace":"shared","agent":"support","name":"mail","kind":"chan","secretRef":"creds","config":`+chanConfig+extra+`}`)
}

func (f *inboundFixture) created() inboundView {
	f.t.Helper()
	rec := f.create("")
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	return evalDecode[inboundView](f.t, rec)
}

func (f *inboundFixture) post(v inboundView, from, id, text string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"from": from, "id": id, "text": text})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, v.Path, strings.NewReader(string(body)))
	req.Header.Set(inbound.TimestampHeader, ts)
	req.Header.Set(inbound.SignatureHeader, inbound.Sign(inbound.SigningKey(inboundSecretValue), ts, body))
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestInbound_CreateListAndAudit(t *testing.T) {
	f := newInboundFixture(t)
	v := f.created()
	if v.TenantID != isoA || !v.Enabled || v.Path != "/inbound/chan/"+v.ID || v.SecretRef != "creds" {
		t.Fatalf("created = %+v", v)
	}
	rec := f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/inbound-connections?namespace=shared&agent=support", "")
	if got := evalDecode[[]inboundView](t, rec); rec.Code != http.StatusOK || len(got) != 1 || got[0].ID != v.ID {
		t.Errorf("list = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), inboundSecretValue) {
		t.Error("the list leaks the secret")
	}
	if len(f.audit.events) != 1 {
		t.Fatalf("audit = %+v", f.audit.events)
	}
	if e := f.audit.events[0]; e.Action != postgresql.AuditCreate || e.Kind != kindInbound || e.Name != inboundAgent || e.UserID != builderUser ||
		strings.Contains(e.Summary, "alice@example.com") {
		t.Errorf("audit event = %+v", e)
	}
	if rec := f.create(""); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d", rec.Code)
	}
}

func TestInbound_RolesScopeAndRefusals(t *testing.T) {
	f := newInboundFixture(t)
	body := `{"namespace":"shared","agent":"support","name":"x","kind":"chan","config":` + chanConfig + `}`
	for _, role := range []tenancy.Role{tenancy.RoleRunner, tenancy.RoleViewer} {
		if rec := f.api(idFor(role, isoA), http.MethodPost, "/api/inbound-connections", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s create = %d", role, rec.Code)
		}
	}
	v := f.created()
	b := idFor(tenancy.RoleBuilder, isoB)
	for method, path := range map[string]string{http.MethodPatch: "/api/inbound-connections/" + v.ID, http.MethodDelete: "/api/inbound-connections/" + v.ID} {
		if rec := f.api(b, method, path, `{"enabled":false}`); rec.Code != http.StatusNotFound {
			t.Errorf("tenant B %s = %d", method, rec.Code)
		}
	}
	if rec := f.api(b, http.MethodPost, "/api/inbound-connections/"+v.ID+"/test", ``); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B test = %d", rec.Code)
	}
	if got := evalDecode[[]inboundView](t, f.api(b, http.MethodGet, "/api/inbound-connections", "")); len(got) != 0 {
		t.Errorf("tenant B sees %+v", got)
	}
	if rec := f.api(b, http.MethodPost, "/api/inbound-connections", body); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B creates in A's namespace = %d", rec.Code)
	}
	vw := idFor(tenancy.RoleViewer, isoA)
	for method, path := range map[string]string{http.MethodPatch: "/api/inbound-connections/" + v.ID, http.MethodDelete: "/api/inbound-connections/" + v.ID, http.MethodPost: "/api/inbound-connections/" + v.ID + "/test"} {
		if rec := f.api(vw, method, path, `{"enabled":false}`); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s = %d", method, rec.Code)
		}
	}

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"unpublished agent": {`{"namespace":"shared","agent":"draft","name":"x","kind":"chan","config":` + chanConfig + `}`, http.StatusConflict},
		"template agent":    {`{"namespace":"shared","agent":"tmpl","name":"x","kind":"chan","config":` + chanConfig + `}`, http.StatusConflict},
		"unknown kind":      {`{"namespace":"shared","agent":"support","name":"x","kind":"carrier-pigeon","config":` + chanConfig + `}`, http.StatusBadRequest},
		"no allow-list":     {`{"namespace":"shared","agent":"support","name":"x","kind":"chan","config":{}}`, http.StatusBadRequest},
		"wildcard group":    {`{"namespace":"shared","agent":"support","name":"x","kind":"chan","config":{"allowGroups":["*"]}}`, http.StatusBadRequest},
		"unlabelled secret": {`{"namespace":"shared","agent":"support","name":"x","kind":"chan","secretRef":"unlabled","config":` + chanConfig + `}`, http.StatusBadRequest},
		"missing secret":    {`{"namespace":"shared","agent":"support","name":"x","kind":"chan","secretRef":"nope","config":` + chanConfig + `}`, http.StatusBadRequest},
		"unknown field":     {`{"namespace":"shared","agent":"support","name":"x","kind":"chan","config":` + chanConfig + `,"password":"p"}`, http.StatusBadRequest},
	} {
		if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections", tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	// Patch: validated like create; a bad config is refused and nothing changes.
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPatch, "/api/inbound-connections/"+v.ID, `{"config":{}}`)
	if rec.Code != http.StatusBadRequest || string(f.store.conns[v.ID].Config) == "{}" {
		t.Errorf("bad patch = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPatch, "/api/inbound-connections/"+v.ID, `{"enabled":false}`); rec.Code != http.StatusOK || f.store.conns[v.ID].Enabled {
		t.Errorf("disable = %d", rec.Code)
	}
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodDelete, "/api/inbound-connections/"+v.ID, ``); rec.Code != http.StatusNoContent {
		t.Errorf("delete = %d", rec.Code)
	}
}

func TestInbound_TestEndpoint(t *testing.T) {
	f := newInboundFixture(t)
	v := f.created()
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPost, "/api/inbound-connections/"+v.ID+"/test", ``)
	if got := evalDecode[inbound.TestReport](t, rec); rec.Code != http.StatusOK || !got.OK || got.Details["folders"] != "1" {
		t.Errorf("test = %d %s", rec.Code, rec.Body)
	}
}

func TestInbound_NoPostgres(t *testing.T) {
	isoMode(t, tenancy.ModeEnforce)
	s := &UIServer{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/inbound-connections", nil), idFor(tenancy.RoleViewer, isoA)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inbound/chan/x", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("receive with no store = %d", rec.Code)
	}
}

func TestInbound_ReceiveStartsATriggerActorRunWithNoInvoker(t *testing.T) {
	f := newInboundFixture(t)
	v := f.created()
	rec := f.post(v, "Alice@Example.com", "m-1", "my printer is on fire")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"started"`) {
		t.Fatalf("receive = %d %s", rec.Code, rec.Body)
	}
	runs := f.runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	r := runs[0]
	if r.Annotations[AnnotationCreatedBy] != TriggerActorPrefix+v.ID || r.Labels[LabelTriggerID] != v.ID || r.Labels[LabelInboundKind] != "chan" ||
		r.Labels[tenancy.LabelTenantID] != isoA || r.Spec.AgentRef != inboundAgent || !strings.Contains(r.Spec.Input, "my printer is on fire") {
		t.Errorf("run = %v %v %+v", r.Labels, r.Annotations, r.Spec)
	}
	// Invoker-mode MCP servers fail closed: the run has no person behind it.
	if user, why := invokerOf(&r); user != "" || why == "" {
		t.Errorf("invokerOf = %q, %q: an inbound run must have no invoker", user, why)
	}
	// The same message again is a duplicate, not a second run.
	if rec := f.post(v, "alice@example.com", "m-1", "my printer is on fire"); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Errorf("redelivery = %d %s", rec.Code, rec.Body)
	}
	if n := len(f.runs()); n != 1 {
		t.Errorf("runs = %d after a redelivery", n)
	}
}

func TestInbound_ReceiveRefusals(t *testing.T) {
	f := newInboundFixture(t)
	v := f.created()
	if rec := f.post(v, "mallory@evil.example", "m-1", "hi"); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "rejected") {
		t.Errorf("stranger = %d %s", rec.Code, rec.Body)
	}
	if len(f.runs()) != 0 {
		t.Error("a stranger started a run")
	}
	if got := f.store.status(v.ID); len(got) != 1 || got[0] != "rejected:sender_not_allowed" {
		t.Errorf("ledger = %v", got)
	}
	bad := func(r *http.Request) { r.Header.Set(inbound.SignatureHeader, "sha256=00") }
	if rec := f.post(v, "alice@example.com", "m-2", "hi", bad); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature = %d", rec.Code)
	}
	stale := func(r *http.Request) { r.Header.Set(inbound.TimestampHeader, "1") }
	if rec := f.post(v, "alice@example.com", "m-2", "hi", stale); rec.Code != http.StatusUnauthorized {
		t.Errorf("stale timestamp = %d", rec.Code)
	}
	if len(f.store.status(v.ID)) != 1 {
		t.Error("an unauthenticated request reached the ledger")
	}
	// Replaying a captured request is refused.
	var captured *http.Request
	ok := f.post(v, "alice@example.com", "m-3", "hi", func(r *http.Request) { captured = r.Clone(context.Background()) })
	if ok.Code != http.StatusAccepted {
		t.Fatal(ok.Code)
	}
	body, _ := json.Marshal(map[string]string{"from": "alice@example.com", "id": "m-3", "text": "hi"})
	captured.Body = http.NoBody
	replay := httptest.NewRequest(http.MethodPost, v.Path, strings.NewReader(string(body)))
	replay.Header = captured.Header.Clone()
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, replay)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("replay = %d", rr.Code)
	}
	// Unknown id, wrong kind and a disabled connection are all the same 404.
	for name, path := range map[string]string{"unknown": "/inbound/chan/inb_" + strings.Repeat("0", 32), "wrong kind": "/inbound/email/" + v.ID, "malformed": "/inbound/chan/x"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		rr := httptest.NewRecorder()
		f.h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d", name, rr.Code)
		}
	}
	f.store.conns[v.ID].Enabled = false
	if rec := f.post(v, "alice@example.com", "m-4", "hi"); rec.Code != http.StatusNotFound {
		t.Errorf("disabled = %d", rec.Code)
	}
}

func TestInbound_LaunchChecksTheAgentAndTheTenant(t *testing.T) {
	f := newInboundFixture(t)
	br := f.s.InboundBridge()
	req := func(tenant, agent string) inbound.LaunchRequest {
		return inbound.LaunchRequest{TenantID: tenant, Namespace: "shared", Agent: agent, ConnectionID: "inb_" + strings.Repeat("a", 32), Kind: "chan", Input: "x"}
	}
	for name, r := range map[string]inbound.LaunchRequest{
		"unknown tenant":    req("t-other", inboundAgent),
		"unpublished agent": req(isoA, "draft"),
		"template":          req(isoA, "tmpl"),
		"missing agent":     req(isoA, "ghost"),
	} {
		_, err := br.Launch(context.Background(), r)
		if le, ok := err.(*inbound.LaunchError); !ok || !le.Gone {
			t.Errorf("%s: err = %v, want a Gone launch error", name, err)
		}
	}
	if len(f.runs()) != 0 {
		t.Error("a refused launch created a run")
	}
	// A prior run is continued only when it is this connection's finished run of the same agent.
	first, err := br.Launch(context.Background(), req(isoA, inboundAgent))
	if err != nil {
		t.Fatal(err)
	}
	next := req(isoA, inboundAgent)
	next.PriorRun = first.Name
	second, _ := br.Launch(context.Background(), next)
	var run agentorcav1alpha1.AgentRun
	_ = f.c.Get(context.Background(), clientKey(second.Name), &run)
	if run.Spec.PriorRunRef != "" {
		t.Errorf("continued an unfinished run: %q", run.Spec.PriorRunRef)
	}
	var done agentorcav1alpha1.AgentRun
	_ = f.c.Get(context.Background(), clientKey(first.Name), &done)
	done.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	done.Status.Output = "the answer"
	if err := f.c.Status().Update(context.Background(), &done); err != nil {
		t.Fatal(err)
	}
	third, _ := br.Launch(context.Background(), next)
	_ = f.c.Get(context.Background(), clientKey(third.Name), &run)
	if run.Spec.PriorRunRef != first.Name {
		t.Errorf("did not continue a finished run of the connection: %q", run.Spec.PriorRunRef)
	}
	other := next
	other.ConnectionID = "inb_" + strings.Repeat("b", 32)
	fourth, _ := br.Launch(context.Background(), other)
	_ = f.c.Get(context.Background(), clientKey(fourth.Name), &run)
	if run.Spec.PriorRunRef != "" {
		t.Errorf("continued another connection's run: %q", run.Spec.PriorRunRef)
	}
	st, err := br.RunState(context.Background(), "shared", first.Name)
	if err != nil || !st.Done || !st.Succeeded || st.Output != "the answer" {
		t.Errorf("RunState = %+v, %v", st, err)
	}
	if st, _ := br.RunState(context.Background(), "shared", "ghost"); st.Found {
		t.Error("a missing run was found")
	}
}

func TestInbound_SecretMustBeLabelled(t *testing.T) {
	f := newInboundFixture(t)
	br := f.s.InboundBridge()
	if _, err := br.ReadSecret(context.Background(), "shared", "unlabled"); err == nil || strings.Contains(err.Error(), inboundSecretValue) {
		t.Errorf("unlabelled secret read: %v", err)
	}
	if got, err := br.ReadSecret(context.Background(), "shared", "creds"); err != nil || string(got["signingSecret"]) != inboundSecretValue {
		t.Errorf("labelled secret: %v %v", got, err)
	}
}

func clientKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "shared", Name: name}
}

func TestInbound_AnAnswerResumesAWaitingRunOfTheConnection(t *testing.T) {
	f := newInboundFixture(t)
	br := f.s.InboundBridge()
	conn := "inb_" + strings.Repeat("a", 32)
	first, err := br.Launch(context.Background(), inbound.LaunchRequest{TenantID: isoA, Namespace: "shared", Agent: inboundAgent, ConnectionID: conn, Kind: "chan", Input: "restart it"})
	if err != nil {
		t.Fatal(err)
	}
	resume := func(connID, answer string) (inbound.RunRef, bool) {
		ref, ok, err := br.Resume(context.Background(), inbound.ResumeRequest{TenantID: isoA, ConnectionID: connID, Run: first, Answer: answer})
		if err != nil {
			t.Fatal(err)
		}
		return ref, ok
	}
	if _, ok := resume(conn, "staging"); ok {
		t.Fatal("resumed a run that was not waiting")
	}

	var run agentorcav1alpha1.AgentRun
	_ = f.c.Get(context.Background(), clientKey(first.Name), &run)
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseWaitingForInput
	run.Status.ClarifyQuestion = "Which cluster?"
	if err := f.c.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	st, _ := br.RunState(context.Background(), "shared", first.Name)
	if !st.Waiting || st.Question != "Which cluster?" || st.Done {
		t.Fatalf("a waiting run is %+v", st)
	}
	if _, ok := resume("inb_"+strings.Repeat("b", 32), "staging"); ok {
		t.Fatal("another connection answered the run")
	}
	if _, ok := resume(conn, "  "); ok {
		t.Fatal("an empty answer resumed the run")
	}

	ref, ok := resume(conn, "staging")
	if !ok {
		t.Fatal("the connection's own waiting run was not resumed")
	}
	var cont agentorcav1alpha1.AgentRun
	if err := f.c.Get(context.Background(), clientKey(ref.Name), &cont); err != nil {
		t.Fatal(err)
	}
	if cont.Spec.PriorRunRef != first.Name || cont.Labels[LabelTriggerID] != conn {
		t.Errorf("continuation = %+v", cont.Spec)
	}
	if cont.Annotations[AnnotationCreatedBy] != TriggerActorPrefix+conn {
		t.Errorf("the continuation lost the trigger actor: %v", cont.Annotations)
	}
	st, _ = br.RunState(context.Background(), "shared", first.Name)
	if !st.Continued {
		t.Errorf("the original run is %+v, want Continued", st)
	}
	if _, ok := resume(conn, "again"); ok {
		t.Error("answered the same question twice")
	}
}
