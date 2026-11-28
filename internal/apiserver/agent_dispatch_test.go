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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch"
	"github.com/heddles/agent-orca/internal/dispatch/decision"
)

// wordDecider picks the agent whose name the request mentions, surely; a
// request that mentions none gets an unsure pick.
type wordDecider struct{ calls *int }

func (w wordDecider) Decide(_ context.Context, req decision.Request) (decision.Response, error) {
	*w.calls++
	q := req.Questions["agent"]
	pick, confidence := "", 0.3
	for key := range q.Criteria {
		_, name, _ := strings.Cut(key, "/")
		if strings.Contains(req.State, name) {
			pick, confidence = key, 0.97
		} else if pick == "" {
			pick = key
		}
	}
	return decision.Response{Answers: map[string]decision.Answer{
		"agent": {Type: decision.TypeChoice, Choice: pick, Confidence: confidence},
	}}, nil
}

const (
	testTenantID = "tnt_0198f000-0000-7000-8000-000000000001"

	routedBilling = "billing"
	routedDeploys = "deploys"
	decided       = "decided"
)

// routingFixture is a tenant with the agents billing and deploys, a default
// agent when defaultAgent is set, and a real dispatcher over cl.
func routingFixture(defaultAgent string) []client.Object {
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "agent-orca-system"},
		Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: testTenantID, AllowedNamespaces: []string{"tenant-acme"}},
	}
	if defaultAgent != "" {
		tc.Spec.Dispatch = &agentorcav1alpha1.TenantDispatch{DefaultAgent: defaultAgent}
	}
	objs := make([]client.Object, 0, 3)
	objs = append(objs, tc)
	for _, name := range []string{routedBilling, routedDeploys} {
		objs = append(objs, &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "tenant-acme",
			Labels:      map[string]string{"agentorca.io/tenant-id": testTenantID},
			Annotations: map[string]string{AnnotationDescription: "Handles " + name},
		}})
	}
	return objs
}

func routedTenant() *TenantIdentity {
	t := testTenant()
	t.TenantID = testTenantID
	return t
}

func acpAutoRun(t *testing.T, h http.Handler, path, session, text string) (*httptest.ResponseRecorder, ACPRun) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"agent_name": "auto", "session_id": session,
		"input": []ACPMessage{{Role: "user", Parts: []ACPMessagePart{{ContentType: "text/plain", Content: text}}}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
	var run ACPRun
	_ = json.Unmarshal(rec.Body.Bytes(), &run)
	return rec, run
}

func TestACPAutoRunPicksAndKeepsTheSessionsAgent(t *testing.T) {
	for _, path := range []string{"/runs", "/agents/auto/run"} {
		t.Run(path, func(t *testing.T) {
			s := newACPServer(t, routingFixture("")...)
			calls := 0
			s.SetDispatcher(&dispatch.Dispatcher{Client: s.crdClient, Local: wordDecider{&calls}})
			h := s.withTenant(routedTenant())

			rec, first := acpAutoRun(t, h, path, "s1", "roll back the deploys")
			if rec.Code != http.StatusCreated || first.AgentName != routedDeploys {
				t.Fatalf("first run: %d %s", rec.Code, rec.Body.String())
			}
			if first.Routing == nil || first.Routing.Method != decided || first.Routing.Confidence != 0.97 {
				t.Fatalf("routing = %+v", first.Routing)
			}
			var run agentorcav1alpha1.AgentRun
			if err := s.crdClient.Get(context.Background(),
				types.NamespacedName{Name: first.RunID, Namespace: "tenant-acme"}, &run); err != nil {
				t.Fatal(err)
			}
			if got := routingOf(&run); got == nil || got.Method != decided || run.Spec.AgentRef != routedDeploys {
				t.Fatalf("run of %q records routing %+v", run.Spec.AgentRef, got)
			}

			// The session stays, whatever the next request is about.
			_, second := acpAutoRun(t, h, path, "s1", "now a billing question")
			if second.AgentName != routedDeploys || second.Routing.Method != "sticky" || calls != 1 {
				t.Fatalf("second run: %q %+v after %d decider calls", second.AgentName, second.Routing, calls)
			}
			// Another person's session of the same name is not this one.
			other := routedTenant()
			other.UserID = connOtherUser
			_, theirs := acpAutoRun(t, s.withTenant(other), path, "s1", "a billing question")
			if theirs.AgentName != routedBilling {
				t.Fatalf("another person's run went to %q", theirs.AgentName)
			}
		})
	}
}

func TestACPAutoRunFollowsANamedAgent(t *testing.T) {
	s := newACPServer(t, routingFixture("")...)
	calls := 0
	s.SetDispatcher(&dispatch.Dispatcher{Client: s.crdClient, Local: wordDecider{&calls}})
	h := s.withTenant(routedTenant())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/billing/run",
		strings.NewReader(`{"session_id":"s1","input":[{"role":"user","parts":[{"content":"hi"}]}]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("named run: %d %s", rec.Code, rec.Body.String())
	}
	if _, next := acpAutoRun(t, h, "/runs", "s1", "roll back the deploys"); next.AgentName != routedBilling {
		t.Fatalf("after naming billing, auto went to %q", next.AgentName)
	}
}

func TestACPAutoRunRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		dispatcher bool
		status     int
		code       string
	}{
		"no dispatcher":         {status: http.StatusBadRequest, code: "auto_unavailable"},
		"unsure and no default": {dispatcher: true, status: http.StatusUnprocessableEntity, code: "agent_undecided"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newACPServer(t, routingFixture("")...)
			if tc.dispatcher {
				s.SetDispatcher(&dispatch.Dispatcher{Client: s.crdClient, Local: wordDecider{new(int)}})
			}
			rec, _ := acpAutoRun(t, s.withTenant(routedTenant()), "/runs", "", "hello there")
			var body ACPErr
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.status || body.Code != tc.code {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			var runs agentorcav1alpha1.AgentRunList
			if err := s.crdClient.List(context.Background(), &runs); err != nil || len(runs.Items) != 0 {
				t.Fatalf("a refused request made %d runs (%v)", len(runs.Items), err)
			}
		})
	}
}

// An agent that is really called "auto" keeps its name.
func TestACPAgentNamedAutoKeepsTheName(t *testing.T) {
	objs := append(routingFixture(""), &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{
		Name: "auto", Namespace: "tenant-acme", Labels: map[string]string{"agentorca.io/tenant-id": testTenantID}}})
	s := newACPServer(t, objs...)
	calls := 0
	s.SetDispatcher(&dispatch.Dispatcher{Client: s.crdClient, Local: wordDecider{&calls}})
	rec, run := acpAutoRun(t, s.withTenant(routedTenant()), "/runs", "", "roll back the deploys")
	if rec.Code != http.StatusCreated || run.AgentName != "auto" || run.Routing != nil || calls != 0 {
		t.Fatalf("got %d %s after %d decider calls", rec.Code, rec.Body.String(), calls)
	}
}

func TestACPRoutingOffersOnlyWhatTheCallerMayRun(t *testing.T) {
	s := newACPServer(t, routingFixture("")...)
	s.SetDispatcher(&dispatch.Dispatcher{Client: s.crdClient, Local: wordDecider{new(int)}})
	tenant := routedTenant()
	tenant.AllowedAgents = []string{routedBilling}
	rec := httptest.NewRecorder()
	s.withTenant(tenant).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/routing", nil))
	var got ACPRouting
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !got.Auto || len(got.Agents) != 1 || got.Agents[0] != (ACPRoutingAgent{Name: routedBilling, Description: "Handles billing"}) {
		t.Fatalf("routing = %+v", got)
	}
}

func TestTaskAskingForAuto(t *testing.T) {
	newServer := func(defaultAgent string) (*ExternalAPIServer, *int) {
		cl := newACPServer(t, routingFixture(defaultAgent)...).crdClient
		calls := 0
		s := &ExternalAPIServer{auth: &ExternalAuth{}, crdClient: cl}
		s.SetDispatcher(&dispatch.Dispatcher{Client: cl, Local: wordDecider{&calls}})
		return s, &calls
	}
	submit := func(s *ExternalAPIServer, tenant *TenantIdentity, input string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body := mustJSON(TaskSubmission{Agent: "auto", Input: input, Metadata: map[string]string{"dispatch": "spoof"}})
		s.createTask(rec, asTenant(httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body)), tenant))
		return rec
	}

	t.Run("a sure pick", func(t *testing.T) {
		s, _ := newServer("")
		rec := submit(s, routedTenant(), "roll back the deploys")
		var task TaskResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if task.Agent != routedDeploys || task.Routing == nil || task.Routing.Method != decided {
			t.Fatalf("task = %+v routing %+v", task, task.Routing)
		}
		// Reading the task later says the same.
		got := httptest.NewRecorder()
		s.getTask(got, asTenant(httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID, nil), routedTenant()), task.ID)
		if !strings.Contains(got.Body.String(), `"routing":{"method":"`+decided+`"`) {
			t.Fatalf("get task: %d %s", got.Code, got.Body.String())
		}
	})

	t.Run("an unsure pick takes the default", func(t *testing.T) {
		s, _ := newServer(routedDeploys)
		rec := submit(s, routedTenant(), "hello there")
		var task TaskResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &task)
		if rec.Code != http.StatusCreated || task.Agent != routedDeploys || task.Routing.Method != "fallback" {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("allowedAgents still holds", func(t *testing.T) {
		s, _ := newServer("")
		tenant := routedTenant()
		tenant.AllowedAgents = []string{routedBilling}
		rec := submit(s, tenant, "roll back the deploys")
		var task TaskResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &task)
		if rec.Code != http.StatusCreated || task.Agent != routedBilling || task.Routing.Method != "only" {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("undecided names the choices", func(t *testing.T) {
		s, _ := newServer("")
		rec := submit(s, routedTenant(), "hello there")
		var body struct {
			Code   string
			Agents []string
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusUnprocessableEntity || body.Code != "agent_undecided" || len(body.Agents) != 2 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})

	// A tenant over its rate limit costs no decision.
	t.Run("quotas come first", func(t *testing.T) {
		s, calls := newServer("")
		s.rateLimiter = NewRateLimiter()
		tenant := routedTenant()
		tenant.RateLimitRPM = 1
		first := submit(s, tenant, "roll back the deploys")
		second := submit(s, tenant, "roll back the deploys")
		if first.Code != http.StatusCreated || second.Code != http.StatusTooManyRequests || *calls != 1 {
			t.Fatalf("codes %d, %d after %d decider calls", first.Code, second.Code, *calls)
		}
	})
}
