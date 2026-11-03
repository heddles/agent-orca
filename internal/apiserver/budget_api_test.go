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

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const routerSA = "system:serviceaccount:team-a:agentorca-agent-coder"

func budgetRun(budget *agentorcav1alpha1.RunBudget) *agentorcav1alpha1.AgentRun {
	return &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "x", Budget: budget},
	}
}

func budgetServer(t *testing.T, runs ...*agentorcav1alpha1.AgentRun) (*Server, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentorcav1alpha1.AgentRun{})
	for _, r := range runs {
		b = b.WithObjects(r)
	}
	cl := b.Build()
	return &Server{crdClient: cl, k8s: k8sfake.NewClientset()}, cl
}

// post reports a budget for team-a/run-1 as caller.
func post(t *testing.T, s *Server, caller, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agentrun/team-a/run-1/budget", strings.NewReader(body))
	s.reportBudget(rec, req, caller, "team-a", "run-1")
	return rec
}

func fetchRun(t *testing.T, cl client.Client) *agentorcav1alpha1.AgentRun {
	t.Helper()
	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "run-1", Namespace: "team-a"}, &run); err != nil {
		t.Fatal(err)
	}
	return &run
}

func TestReportBudgetRecordsStateButNeverTheCap(t *testing.T) {
	s, cl := budgetServer(t, budgetRun(&agentorcav1alpha1.RunBudget{MaxUSD: "2.00"}))

	// The router can say anything in the body; only state and spend are read.
	rec := post(t, s, routerSA, `{"state":"exceeded","spentUSD":"2.0100","capUSD":"999"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := fetchRun(t, cl).Status.Budget
	if b == nil || b.State != agentorcav1alpha1.RunBudgetExceeded || b.SpentUSD != "2.0100" {
		t.Fatalf("status.budget = %+v", b)
	}
	if b.CapUSD != "2.00" {
		t.Errorf("cap = %q: the router reported a cap and it was accepted; only the operator may set it", b.CapUSD)
	}
	if b.UpdatedAt == nil {
		t.Error("updatedAt not set")
	}
}

func TestReportBudgetWarningDoesNotOverrideExceeded(t *testing.T) {
	s, cl := budgetServer(t, budgetRun(&agentorcav1alpha1.RunBudget{MaxUSD: "2.00"}))
	post(t, s, routerSA, `{"state":"exceeded","spentUSD":"2.1"}`)
	post(t, s, routerSA, `{"state":"warning","spentUSD":"2.1"}`)
	if got := fetchRun(t, cl).Status.Budget.State; got != agentorcav1alpha1.RunBudgetExceeded {
		t.Errorf("state = %s, want exceeded to stand until a raise is approved", got)
	}
}

func TestReportBudgetRejections(t *testing.T) {
	s, _ := budgetServer(t,
		budgetRun(&agentorcav1alpha1.RunBudget{MaxUSD: "2.00"}),
	)
	for name, tc := range map[string]struct {
		caller, body string
		want         int
	}{
		"router of another namespace":   {"system:serviceaccount:team-b:agentorca-agent-coder", `{"state":"warning"}`, http.StatusForbidden},
		"a user, not a service account": {"alice@example.com", `{"state":"warning"}`, http.StatusForbidden},
		"unknown state":                 {routerSA, `{"state":"ok"}`, http.StatusBadRequest},
		"malformed body":                {routerSA, `{`, http.StatusBadRequest},
	} {
		if rec := post(t, s, tc.caller, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", name, rec.Code, tc.want)
		}
	}

	noBudget, _ := budgetServer(t, budgetRun(nil))
	if rec := post(t, noBudget, routerSA, `{"state":"warning"}`); rec.Code != http.StatusConflict {
		t.Errorf("run without a budget: status %d, want 409", rec.Code)
	}
}

func TestGetBudget(t *testing.T) {
	run := budgetRun(&agentorcav1alpha1.RunBudget{MaxUSD: "2.00"})
	s, cl := budgetServer(t, run)
	get := func() budgetView {
		rec := httptest.NewRecorder()
		s.getBudget(rec, httptest.NewRequest(http.MethodGet, "/agentrun/team-a/run-1/budget", nil), routerSA, "team-a", "run-1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		var v budgetView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}

	// Before anything is recorded the cap is the spec's.
	if v := get(); v.CapUSD != "2.00" || v.State != "ok" || v.Terminal {
		t.Errorf("fresh run: %+v", v)
	}

	// The operator's raise shows up as the cap.
	r := fetchRun(t, cl)
	r.Status.Budget = &agentorcav1alpha1.RunBudgetStatus{State: agentorcav1alpha1.RunBudgetOK, CapUSD: "4.00", Raises: 1}
	if err := cl.Status().Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if v := get(); v.CapUSD != "4.00" {
		t.Errorf("after a raise: %+v", v)
	}

	// A finished run tells a paused router to stop.
	r = fetchRun(t, cl)
	r.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	if err := cl.Status().Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if v := get(); !v.Terminal {
		t.Errorf("failed run not reported terminal: %+v", v)
	}
}

// The endpoints are reached through handleAgentRun, behind the token review.
func TestBudgetRoutesThroughTokenReview(t *testing.T) {
	s, _ := budgetServer(t, budgetRun(&agentorcav1alpha1.RunBudget{MaxUSD: "2.00"}))
	user := routerSA
	s.k8s.(*k8sfake.Clientset).PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, User: authv1.UserInfo{Username: user},
		}}, nil
	})

	call := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		s.handleAgentRun(rec, req)
		return rec.Code
	}
	if code := call(http.MethodPost, "/agentrun/team-a/run-1/budget", `{"state":"warning","spentUSD":"1.7"}`); code != http.StatusNoContent {
		t.Errorf("POST: %d", code)
	}
	if code := call(http.MethodGet, "/agentrun/team-a/run-1/budget", ""); code != http.StatusOK {
		t.Errorf("GET: %d", code)
	}
	user = "system:serviceaccount:team-b:agentorca-agent-coder"
	if code := call(http.MethodGet, "/agentrun/team-a/run-1/budget", ""); code != http.StatusForbidden {
		t.Errorf("GET from another namespace's router: %d, want 403", code)
	}
}

// A retried "exceeded" that was sent before an approved raise reports spend
// that is already under the new cap; it must not reopen the pause.
func TestReportBudgetIgnoresAStaleExceeded(t *testing.T) {
	s, cl := budgetServer(t, budgetRun(&agentorcav1alpha1.RunBudget{MaxUSD: "2.00"}))
	r := fetchRun(t, cl)
	r.Status.Budget = &agentorcav1alpha1.RunBudgetStatus{State: agentorcav1alpha1.RunBudgetOK, CapUSD: "3.5000", Raises: 1}
	if err := cl.Status().Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	if rec := post(t, s, routerSA, `{"state":"exceeded","spentUSD":"2.0300"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	if got := fetchRun(t, cl).Status.Budget; got.State != agentorcav1alpha1.RunBudgetOK {
		t.Errorf("state = %s: a stale report (spend 2.03 under the cap of 3.50) reopened the pause", got.State)
	}

	// Spend at or over the cap is real.
	post(t, s, routerSA, `{"state":"exceeded","spentUSD":"3.5100"}`)
	if got := fetchRun(t, cl).Status.Budget; got.State != agentorcav1alpha1.RunBudgetExceeded || got.SpentUSD != "3.5100" {
		t.Errorf("status.budget = %+v, want exceeded at 3.51", got)
	}
}
