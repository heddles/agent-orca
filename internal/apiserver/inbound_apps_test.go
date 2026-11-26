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
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// The fake keeps routes the way the database does (its defaults are tested
// against PostgreSQL): the first agent in a place is its default.
func (f *fakeInboundStore) ListInboundRoutes(_ context.Context, connectionID string) ([]postgresql.InboundRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]postgresql.InboundRoute, 0, len(f.routes[connectionID]))
	for _, r := range f.routes[connectionID] {
		c := r
		c.Places = slices.Clone(r.Places)
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeInboundStore) SaveInboundRoute(_ context.Context, connectionID string, r postgresql.InboundRoute, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.routes[connectionID]
	i := slices.IndexFunc(list, func(x postgresql.InboundRoute) bool { return x.Namespace == r.Namespace && x.Agent == r.Agent })
	for n := range r.Places {
		r.Places[n].Default = !slices.ContainsFunc(list, func(x postgresql.InboundRoute) bool {
			return (x.Agent != r.Agent) && slices.ContainsFunc(x.Places, func(p postgresql.InboundPlace) bool { return p.ID == r.Places[n].ID && p.Default })
		})
	}
	if i < 0 {
		f.routes[connectionID] = append(list, r)
	} else {
		list[i] = r
	}
	return nil
}

func (f *fakeInboundStore) DeleteInboundRoute(_ context.Context, connectionID, ns, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.routes[connectionID]
	i := slices.IndexFunc(list, func(x postgresql.InboundRoute) bool { return x.Namespace == ns && x.Agent == agent })
	if i < 0 {
		return postgresql.ErrRouteNotFound
	}
	f.routes[connectionID] = slices.Delete(list, i, i+1)
	return nil
}

func (f *fakeInboundStore) SetInboundDefault(_ context.Context, connectionID, place, ns, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, r := range f.routes[connectionID] {
		for n := range r.Places {
			if r.Places[n].ID == place {
				r.Places[n].Default = r.Namespace == ns && r.Agent == agent
				found = found || r.Places[n].Default
			}
		}
	}
	if !found {
		return postgresql.ErrRouteNotFound
	}
	return nil
}

type appFixture struct {
	*guidedFixture
	app chatAppView
}

func newAppFixture(t *testing.T) *appFixture {
	t.Helper()
	f := newGuidedFixture(t)
	f.s.inboundPipe.Routes = f.store
	// A second published agent.
	if err := f.c.Create(context.Background(), hookAgent("shared", "second", nil)); err != nil {
		t.Fatal(err)
	}
	dep := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "second-dep", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA}},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "second"},
	}
	if err := f.c.Create(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	rec := f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPost, "/api/chat-apps", `{"name":"Home","kind":"chatx"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app = %d %s", rec.Code, rec.Body)
	}
	app := evalDecode[chatAppView](t, rec)
	if rec := f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPut, "/api/inbound-connections/"+app.ID+"/credentials", enteredValues); rec.Code != http.StatusOK {
		t.Fatalf("credentials = %d %s", rec.Code, rec.Body)
	}
	return &appFixture{guidedFixture: f, app: app}
}

func (f *appFixture) addAgent(agent, body string) {
	f.t.Helper()
	rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPut, "/api/chat-apps/"+f.app.ID+"/agents/shared/"+agent, body)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("add %s = %d %s", agent, rec.Code, rec.Body)
	}
}

func TestChatApps_WhoMayDoWhat(t *testing.T) {
	f := newAppFixture(t)
	if f.app.Namespace != "" || f.app.Agent != "" || strings.Contains(string(f.app.Config), "channel") {
		t.Fatalf("app = %+v", f.app)
	}
	f.addAgent("support", `{"places":[{"id":"C1","name":"#ops"}],"allowSenders":["U1"]}`)
	builder, admin := idFor(tenancy.RoleBuilder, isoA), idFor(tenancy.RoleAdmin, isoA)
	app := "/api/inbound-connections/" + f.app.ID
	// A builder may not touch the app itself, or decide who answers by default.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/chat-apps", `{"name":"x","kind":"chatx"}`},
		{http.MethodPut, "/api/chat-apps/" + f.app.ID + "/default", `{"place":"C1","namespace":"shared","agent":"support"}`},
		{http.MethodPatch, app, `{"enabled":false}`},
		{http.MethodDelete, app, ""},
		{http.MethodPost, app + "/test", ""},
		{http.MethodPut, app + "/credentials", enteredValues},
		{http.MethodPost, app + "/pairings", ""},
	} {
		if rec := f.api(builder, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("builder %s %s = %d %s, want 403", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// Nobody can hand a shared app a channel list or a Secret.
	if rec := f.api(admin, http.MethodPatch, app, `{"secretRef":"chatx-creds"}`); rec.Code != http.StatusConflict {
		t.Errorf("secret on a shared app = %d %s", rec.Code, rec.Body)
	}
	// Another tenant sees and changes nothing.
	other := idFor(tenancy.RoleAdmin, isoB)
	if got := evalDecode[[]chatAppView](t, f.api(other, http.MethodGet, "/api/chat-apps", "")); len(got) != 0 {
		t.Errorf("tenant B sees %+v", got)
	}
	for _, path := range []string{app, "/api/chat-apps/" + f.app.ID + "/agents/shared/support"} {
		if rec := f.api(other, http.MethodDelete, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("tenant B delete %s = %d", path, rec.Code)
		}
	}
	for name, c := range map[string]struct {
		agent, body string
		want        int
	}{
		"unpublished agent":            {"draft", `{}`, http.StatusConflict},
		"template":                     {"tmpl", `{}`, http.StatusConflict},
		"approver who may not ask":     {"second", `{"allowSenders":["U1"],"approvers":["U2"]}`, http.StatusBadRequest},
		"wildcard sender":              {"second", `{"allowSenders":["*"]}`, http.StatusBadRequest},
		"a channel name instead of id": {"second", `{"places":[{"id":"#ops"}]}`, http.StatusBadRequest},
	} {
		rec := f.api(builder, http.MethodPut, "/api/chat-apps/"+f.app.ID+"/agents/shared/"+c.agent, c.body)
		if rec.Code != c.want {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
	}
	list := evalDecode[[]chatAppView](t, f.api(idFor(tenancy.RoleViewer, isoA), http.MethodGet, "/api/chat-apps", ""))
	if len(list) != 1 || len(list[0].Routes) != 1 || list[0].Routes[0].Agent != inboundAgent {
		t.Fatalf("list = %+v", list)
	}
	// The per-agent channel list does not include the shared app.
	own := f.api(builder, http.MethodGet, "/api/inbound-connections?namespace=shared&agent=support", "")
	if strings.Contains(own.Body.String(), f.app.ID) {
		t.Errorf("the shared app is listed as the agent's own channel: %s", own.Body)
	}
}

func TestChatApps_TwoAgentsShareOneBotAndChannel(t *testing.T) {
	f := newAppFixture(t)
	f.addAgent("support", `{"places":[{"id":"C1"}],"allowSenders":["U1"]}`)
	f.addAgent("second", `{"places":[{"id":"C1"}],"allowSenders":["U1","U2"]}`)
	n := 0
	send := func(user, channel, text string) string {
		n++
		rec := f.post(f.app.inboundView, enteredToken, map[string]string{
			"User": user, "Team": "T1", "Channel": channel, "ID": "m" + strconv.Itoa(n), "Mode": "mention", "Text": text,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("post = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	agents := func() []string {
		runs := f.runs()
		out := make([]string, 0, len(runs))
		for _, r := range runs {
			out = append(out, r.Spec.AgentRef+": "+r.Spec.Input[strings.LastIndex(r.Spec.Input, "\n")+1:])
		}
		slices.Sort(out)
		return out
	}
	send("U1", "C1", "who is the default")
	send("U1", "C1", "second take this one")
	if got, want := agents(), []string{"second: take this one", "support: who is the default"}; !slices.Equal(got, want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
	// Each agent has its own list: U2 may ask "second" but not the default.
	if body := send("U2", "C1", "hello default"); !strings.Contains(body, "rejected") {
		t.Errorf("U2 to the default = %s", body)
	}
	send("U2", "C1", "second hello")
	if len(f.runs()) != 3 {
		t.Errorf("runs = %v", agents())
	}
	if body := send("U1", "C9", "anyone here"); !strings.Contains(body, "rejected") {
		t.Errorf("a channel with no agent = %s", body)
	}
	if got := f.store.status(f.app.ID); !slices.Contains(got, "rejected:no_agent_here") {
		t.Errorf("ledger = %v", got)
	}
	// An administrator moves the default.
	rec := f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPut, "/api/chat-apps/"+f.app.ID+"/default", `{"place":"C1","namespace":"shared","agent":"second"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("default = %d %s", rec.Code, rec.Body)
	}
	send("U1", "C1", "who now")
	if !slices.Contains(agents(), "second: who now") {
		t.Errorf("runs = %v", agents())
	}
}

func TestChatApps_PairingFromAnAgentAddsThePersonAndPlaceToItsRoute(t *testing.T) {
	f := newAppFixture(t)
	f.addAgent("support", `{"places":[{"id":"C1"}],"allowSenders":["U1"]}`)
	builder := idFor(tenancy.RoleBuilder, isoA)
	base := "/api/inbound-connections/" + f.app.ID + "/pairings"
	rec := f.api(builder, http.MethodPost, base, `{"namespace":"shared","agent":"second"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("pairing = %d %s", rec.Code, rec.Body)
	}
	p := evalDecode[pairingView](t, rec)
	f.post(f.app.inboundView, enteredToken, map[string]string{"User": "U7", "Channel": "C2", "ID": "p1", "Mode": "mention", "Text": "pair " + p.Code})
	if rec := f.api(builder, http.MethodPost, base+"/"+p.ID+"/approve", ""); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	list := evalDecode[[]chatAppView](t, f.api(builder, http.MethodGet, "/api/chat-apps", ""))
	var second postgresql.InboundRoute
	for _, r := range list[0].Routes {
		if r.Agent == "second" {
			second = r
		}
	}
	if !slices.Equal(second.AllowSenders, []string{"U7"}) || len(second.Places) != 1 || second.Places[0].ID != "C2" {
		t.Errorf("second's route = %+v", second)
	}
	if len(list[0].People) != 1 || list[0].People[0].ID != "U7" {
		t.Errorf("people = %+v", list[0].People)
	}
	// An app-level code is the administrator's, even to read.
	rec = f.api(idFor(tenancy.RoleAdmin, isoA), http.MethodPost, base, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("app pairing = %d %s", rec.Code, rec.Body)
	}
	own := evalDecode[pairingView](t, rec)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if rec := f.api(builder, method, base+"/"+own.ID, ""); rec.Code != http.StatusForbidden {
			t.Errorf("builder %s of an app code = %d", method, rec.Code)
		}
	}
}
