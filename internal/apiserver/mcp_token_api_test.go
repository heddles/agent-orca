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
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	tokNS         = "team-a"
	tokAccess     = "gho_ACCESS_TOKEN_SENTINEL_0123456789"
	tokMaintainer = "maria"
	tokUID        = "uid-github"
	tokApp        = "github-app"
)

var tokCaller = "system:serviceaccount:" + tokNS + ":" + security.AgentSAName("reporter")

type fakeBroker struct {
	reqs []connections.TokenRequest
	err  error
	ctx  context.Context
}

func (f *fakeBroker) AccessToken(ctx context.Context, req connections.TokenRequest) (connections.AccessToken, error) {
	f.reqs = append(f.reqs, req)
	f.ctx = ctx
	if f.err != nil {
		return connections.AccessToken{}, f.err
	}
	return connections.AccessToken{Token: connections.Secret(tokAccess), ExpiresAt: time.Unix(1_900_000_000, 0), AccountLabel: tokMaintainer}, nil
}

type tokFixture struct {
	s      *Server
	broker *fakeBroker
}

func newTokFixture(t *testing.T, mutate ...func(*agentorcav1alpha1.Agent, *agentorcav1alpha1.AgentRun, *agentorcav1alpha1.MCPServer)) *tokFixture {
	t.Helper()
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reporter", Namespace: tokNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}},
		Spec:       agentorcav1alpha1.AgentSpec{Tools: []string{"github"}},
		Status:     agentorcav1alpha1.AgentStatus{PublishedBy: tokMaintainer},
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: tokNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "reporter", Input: "x"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
	server := connMCPServer(tokNS, "github", isoA, "maintainer", nil)
	server.Spec.AllowedAgents = []string{"reporter"}
	tool := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: tokNS,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "agentorca.agentorca.io/v1alpha1", Kind: "MCPServer", Name: "github", UID: tokUID}}}}
	for _, m := range mutate {
		m(agent, run, server)
	}
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, run, server, tool).Build()
	b := &fakeBroker{}
	return &tokFixture{s: (&Server{crdClient: cl, k8s: k8sfake.NewClientset()}).WithMCPTokens(b), broker: b}
}

func (f *tokFixture) call(caller, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agentrun/team-a/run-1/mcp-token", strings.NewReader(body))
	f.s.issueMCPToken(rec, req, caller, tokNS, "run-1")
	return rec
}

func tokError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct{ Error, Message string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error
}

func TestMCPToken_IssuesTheMaintainersShortLivedToken(t *testing.T) {
	f := newTokFixture(t)
	rec := f.call(tokCaller, `{"server":"github"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["accessToken"] != tokAccess || out["expiresAt"] == "" || out["identity"] != "maria (maintainer)" {
		t.Errorf("answer = %v", out)
	}
	if strings.Contains(rec.Body.String(), "refresh") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("answer or headers wrong: %s %v", rec.Body, rec.Header())
	}
	// Identity: the maintainer is Agent.status.publishedBy and the tenant the agent's,
	// the server is resolved from the object, and the call is scoped to the tenant.
	if len(f.broker.reqs) != 1 {
		t.Fatalf("broker calls = %d", len(f.broker.reqs))
	}
	got := f.broker.reqs[0]
	if got.UserID != tokMaintainer || got.TenantID != isoA || got.Server.UID != tokUID || got.Server.Name != tokServerName ||
		got.Server.AppRef != tokApp || got.Server.URL != "https://mcp.github.example/mcp" {
		t.Errorf("request = %+v", got)
	}
	if scope, _ := postgresql.ScopeFrom(f.broker.ctx); scope != isoA {
		t.Errorf("store scope = %q, want the tenant", scope)
	}
}

func TestMCPToken_IdentityComesFromTheObjectsNotTheBody(t *testing.T) {
	f := newTokFixture(t)
	// The router can send anything: only "server" is read.
	rec := f.call(tokCaller, `{"server":"github","user":"mallory","userId":"mallory","tenant":"`+isoB+`","tenantId":"`+isoB+`","maintainer":"mallory"}`)
	if rec.Code != http.StatusOK || f.broker.reqs[0].UserID != tokMaintainer || f.broker.reqs[0].TenantID != isoA {
		t.Fatalf("status %d, request %+v", rec.Code, f.broker.reqs)
	}
}

func TestMCPToken_Refusals(t *testing.T) {
	type tc struct {
		name   string
		caller string
		body   string
		mutate func(*agentorcav1alpha1.Agent, *agentorcav1alpha1.AgentRun, *agentorcav1alpha1.MCPServer)
		status int
		code   string
	}
	otherNS := "system:serviceaccount:other:" + security.AgentSAName("reporter")
	otherAgentSA := "system:serviceaccount:" + tokNS + ":" + security.AgentSAName("someone-else")
	for _, c := range []tc{
		{"a pod of another namespace", otherNS, `{"server":"github"}`, nil, http.StatusForbidden, errCodeForbid},
		{"a pod of another agent in the same namespace", otherAgentSA, `{"server":"github"}`, nil, http.StatusForbidden, errCodeForbid},
		{"no server in the body", tokCaller, `{}`, nil, http.StatusBadRequest, errCodeBadReq},
		{"a malformed server name", tokCaller, `{"server":"../x"}`, nil, http.StatusBadRequest, errCodeBadReq},
		{"not json", tokCaller, `nope`, nil, http.StatusBadRequest, errCodeBadReq},
		{"an unknown server", tokCaller, `{"server":"nothere"}`, nil, http.StatusNotFound, errCodeForbid},
		{"a finished run", tokCaller, `{"server":"github"}`, func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			r.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
		}, http.StatusConflict, connections.CodeUnavailable},
		{"an agent not in allowedAgents", tokCaller, `{"server":"github"}`, func(_ *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, s *agentorcav1alpha1.MCPServer) {
			s.Spec.AllowedAgents = []string{"someone-else"}
		}, http.StatusForbidden, errCodeForbid},
		{"an agent that does not use the server", tokCaller, `{"server":"github"}`, func(a *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			a.Spec.Tools = nil
		}, http.StatusForbidden, errCodeForbid},
		{"a shared-mode server", tokCaller, `{"server":"github"}`, func(_ *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, s *agentorcav1alpha1.MCPServer) {
			s.Spec.Auth.OAuth.ConnectionMode = agentorcav1alpha1.MCPConnectionShared
		}, http.StatusConflict, connections.CodeUnavailable},
		{"an agent nobody published", tokCaller, `{"server":"github"}`, func(a *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			a.Status.PublishedBy = ""
		}, http.StatusConflict, connections.CodeUnavailable},
		{"an agent with no tenant", tokCaller, `{"server":"github"}`, func(a *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			a.Labels = nil
		}, http.StatusForbidden, errCodeForbid},
		{"a run of another tenant", tokCaller, `{"server":"github"}`, func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			r.Labels[tenancy.LabelTenantID] = isoB
		}, http.StatusForbidden, errCodeForbid},
		{"a server of another tenant", tokCaller, `{"server":"github"}`, func(_ *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, s *agentorcav1alpha1.MCPServer) {
			s.Labels[tenancy.LabelTenantID] = isoB
		}, http.StatusForbidden, errCodeForbid},
	} {
		t.Run(c.name, func(t *testing.T) {
			var muts []func(*agentorcav1alpha1.Agent, *agentorcav1alpha1.AgentRun, *agentorcav1alpha1.MCPServer)
			if c.mutate != nil {
				muts = append(muts, c.mutate)
			}
			f := newTokFixture(t, muts...)
			rec := f.call(c.caller, c.body)
			if rec.Code != c.status || tokError(t, rec) != c.code {
				t.Errorf("status %d code %q: %s", rec.Code, tokError(t, rec), rec.Body)
			}
			if len(f.broker.reqs) != 0 || strings.Contains(rec.Body.String(), tokAccess) {
				t.Errorf("the broker was asked or the token leaked: %+v %s", f.broker.reqs, rec.Body)
			}
		})
	}
}

func TestMCPToken_BrokerErrorsKeepTheirCodeAndNoSecrets(t *testing.T) {
	for _, code := range []string{connections.CodeRequired, connections.CodeNeedsReauth, connections.CodeUnavailable} {
		f := newTokFixture(t)
		f.broker.err = &connections.TokenError{Code: code, Message: "the connection needs attention"}
		rec := f.call(tokCaller, `{"server":"github"}`)
		if rec.Code != http.StatusConflict || tokError(t, rec) != code {
			t.Errorf("%s: status %d %s", code, rec.Code, rec.Body)
		}
	}
	f := newTokFixture(t)
	f.broker.err = context.DeadlineExceeded
	rec := f.call(tokCaller, `{"server":"github"}`)
	if rec.Code != http.StatusBadGateway || tokError(t, rec) != connections.CodeUnavailable || strings.Contains(rec.Body.String(), "deadline") {
		t.Errorf("unexpected error answer: %d %s", rec.Code, rec.Body)
	}
	off := &Server{crdClient: f.s.crdClient}
	rec = httptest.NewRecorder()
	off.issueMCPToken(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"server":"github"}`)), tokCaller, tokNS, "run-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("feature off = %d", rec.Code)
	}
}

// The route authenticates with TokenReview like the other router-facing calls.
func TestMCPToken_RouteNeedsAValidServiceAccountToken(t *testing.T) {
	f := newTokFixture(t)
	k := k8sfake.NewClientset()
	k.PrependReactor("create", "tokenreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		tr := a.(clienttesting.CreateAction).GetObject().(*authv1.TokenReview)
		tr.Status = authv1.TokenReviewStatus{Authenticated: tr.Spec.Token == "good", User: authv1.UserInfo{Username: tokCaller, UID: "u"}}
		return true, tr, nil
	})
	f.s.k8s = k
	h := f.s.Handler()
	do := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/agentrun/team-a/run-1/mcp-token", strings.NewReader(`{"server":"github"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", rec.Code)
	}
	if rec := do("bad"); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("bad token = %d", rec.Code)
	}
	if rec := do("good"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tokAccess) {
		t.Errorf("good token = %d %s", rec.Code, rec.Body)
	}
}
