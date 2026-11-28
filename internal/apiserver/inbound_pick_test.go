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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const secondAgent = "second"

// A shared app's automatic channel, from the signed request to the run.
func TestChatApps_AnAutomaticPlacePicksTheAgent(t *testing.T) {
	f := newAppFixture(t)
	f.addAgent("support", `{"places":[{"id":"C1"}],"allowSenders":["U1"]}`)
	f.addAgent("second", `{"places":[{"id":"C1"}],"allowSenders":["U1"]}`)
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "picking", Namespace: "agent-orca-system"},
		Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: isoA, AllowedNamespaces: []string{"shared"}},
	}
	if err := f.c.Create(context.Background(), tc); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.s.SetDispatcher(&dispatch.Dispatcher{Client: f.c, Local: wordDecider{&calls}})
	bridge := f.s.InboundBridge()
	f.s.inboundPipe.Picker = bridge

	path := "/api/chat-apps/" + f.app.ID + "/auto"
	if rec := f.api(idFor(tenancy.RoleBuilder, isoA), http.MethodPut, path, `{"place":"C1","auto":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("builder turned auto on: %d", rec.Code)
	}
	admin := idFor(tenancy.RoleAdmin, isoA)
	if rec := f.api(admin, http.MethodPut, path, `{"place":"C9","auto":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("auto where no agent answers: %d %s", rec.Code, rec.Body)
	}
	rec := f.api(admin, http.MethodPut, path, `{"place":"C1","auto":true}`)
	if got := evalDecode[chatAppView](t, rec); rec.Code != http.StatusOK || !slices.Equal(got.AutoPlaces, []string{"C1"}) {
		t.Fatalf("auto on: %d %s", rec.Code, rec.Body)
	}
	// The people the app knows are kept beside the new setting.
	if got := evalDecode[[]chatAppView](t, f.api(admin, http.MethodGet, "/api/chat-apps", "")); len(got[0].Routes) != 2 {
		t.Fatalf("app = %+v", got[0])
	}

	post := f.post(f.app.inboundView, enteredToken, map[string]string{
		"User": "U1", "Team": "T1", "Channel": "C1", "ID": "m1", "Mode": "mention", "Text": "this one is for second please",
	})
	if post.Code != http.StatusOK {
		t.Fatalf("post = %d %s", post.Code, post.Body)
	}
	runs := f.runs()
	if len(runs) != 1 || runs[0].Spec.AgentRef != secondAgent || calls != 1 {
		t.Fatalf("runs = %+v after %d decisions", runs, calls)
	}
	if got := routingOf(&runs[0]); got == nil || got.Method != decided {
		t.Fatalf("routing = %+v from %q", got, runs[0].Annotations)
	}
	state, err := bridge.RunState(context.Background(), runs[0].Namespace, runs[0].Name)
	if err != nil || !state.Picked {
		t.Fatalf("state = %+v, %v", state, err)
	}

	// An agent that is no longer published is not in the running.
	var dep agentorcav1alpha1.AgentDeployment
	dep.Name, dep.Namespace = secondAgent+"-dep", sharedNS
	if err := f.c.Delete(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}
	f.post(f.app.inboundView, enteredToken, map[string]string{
		"User": "U1", "Team": "T1", "Channel": "C1", "ID": "m2", "Mode": "mention", "Text": "for second again",
		"Thread": "t2",
	})
	for _, r := range f.runs() {
		if r.Name != runs[0].Name && r.Spec.AgentRef != inboundAgent {
			t.Errorf("the unpublished agent got %s", r.Name)
		}
	}
	if len(f.runs()) != 2 {
		t.Errorf("runs = %d", len(f.runs()))
	}

	rec = f.api(admin, http.MethodPut, path, `{"place":"C1","auto":false}`)
	if got := evalDecode[chatAppView](t, rec); rec.Code != http.StatusOK || len(got.AutoPlaces) != 0 {
		t.Fatalf("auto off: %d %s", rec.Code, rec.Body)
	}
}
