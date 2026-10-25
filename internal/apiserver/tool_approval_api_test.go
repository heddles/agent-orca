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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// memToolApprovals is an in-memory ToolApprovalStore with the semantics of
// postgresql.Store: one row per (kind, source key), decisions only on pending.
type memToolApprovals struct{ rows []postgresql.Approval }

func (m *memToolApprovals) RequestApproval(_ context.Context, a postgresql.Approval) (postgresql.Approval, error) {
	for i := range m.rows {
		if m.rows[i].Kind == a.Kind && m.rows[i].SourceKey == a.SourceKey {
			return m.rows[i], nil
		}
	}
	a.ID = "appr-" + string(rune('a'+len(m.rows)))
	if a.Status == "" {
		a.Status = postgresql.ApprovalPending
	}
	m.rows = append(m.rows, a)
	a.Created = true
	return a, nil
}

func (m *memToolApprovals) GetApproval(_ context.Context, id string) (postgresql.Approval, error) {
	for _, a := range m.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return postgresql.Approval{}, sql.ErrNoRows
}

func (m *memToolApprovals) DecideApproval(_ context.Context, id, status, by, reason string) (postgresql.Approval, error) {
	for i := range m.rows {
		if m.rows[i].ID == id {
			if m.rows[i].Status != postgresql.ApprovalPending {
				return m.rows[i], postgresql.ErrApprovalNotPending
			}
			m.rows[i].Status, m.rows[i].DecidedBy, m.rows[i].Reason = status, by, reason
			return m.rows[i], nil
		}
	}
	return postgresql.Approval{}, postgresql.ErrApprovalNotPending
}

type countingNotifier struct{ got []postgresql.Approval }

func (c *countingNotifier) NotifyApproval(_ context.Context, a postgresql.Approval) error {
	c.got = append(c.got, a)
	return nil
}

func toolFixtures(policy *agentorcav1alpha1.ToolApprovalPolicy) []client.Object {
	objs := []client.Object{
		&agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: "uid-1"},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "x"},
		},
		&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "team-a"},
			Spec: agentorcav1alpha1.AgentSpec{
				Tools: []string{"deploy"}, GuardrailPolicyRef: "factory",
			},
		},
	}
	if policy != nil {
		objs = append(objs, &agentorcav1alpha1.GuardrailPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "factory", Namespace: "team-a"},
			Spec:       agentorcav1alpha1.GuardrailPolicySpec{ToolApproval: policy},
		})
	}
	return objs
}

func toolServer(t *testing.T, store ToolApprovalStore, n ApprovalNotifier, objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentorcav1alpha1.AgentRun{}).WithObjects(objs...).Build()
	s := &Server{crdClient: cl, k8s: k8sfake.NewClientset()}
	if store != nil {
		s.WithApprovals(store, n)
	}
	return s, cl
}

// requestTool posts a tool approval request for team-a/run-1 as caller.
func requestTool(s *Server, caller, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agentrun/team-a/run-1/tool-approval", strings.NewReader(body))
	s.requestToolApproval(rec, req, caller, "team-a", "run-1")
	return rec
}

// readTool reads a tool approval of team-a/run-1 as caller.
func readTool(s *Server, caller, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agentrun/team-a/run-1/tool-approval/"+id, nil)
	s.getToolApproval(rec, req, caller, "team-a", "run-1", id)
	return rec
}

const goodToolBody = `{"callKey":"0123456789abcdef","tool":"deploy","sideEffect":"actuate","arguments":"{\"env\":\"prod\"}"}`

func decodeState(t *testing.T, rec *httptest.ResponseRecorder) toolApprovalState {
	t.Helper()
	var st toolApprovalState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return st
}

func TestRequestToolApprovalOpensAnApprovalFromThePolicy(t *testing.T) {
	store, notifier := &memToolApprovals{}, &countingNotifier{}
	policy := &agentorcav1alpha1.ToolApprovalPolicy{
		ApproverGroups: []string{"release-managers"}, Timeout: &metav1.Duration{Duration: 10 * time.Minute},
	}
	s, cl := toolServer(t, store, notifier, toolFixtures(policy)...)

	// The router can send anything; groups and timeout still come from the policy.
	rec := requestTool(s, routerSA,
		`{"callKey":"0123456789abcdef","tool":"deploy","sideEffect":"actuate","arguments":"{\"env\":\"prod\"}","approverGroups":["everyone"],"timeout":"9999h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	st := decodeState(t, rec)
	if st.Status != postgresql.ApprovalPending || st.ID == "" {
		t.Fatalf("state = %+v, want a pending approval", st)
	}
	a := store.rows[0]
	if a.Kind != postgresql.ApprovalKindTool || a.SourceKey != "uid-1/0123456789abcdef" || a.Namespace != "team-a" || a.RunName != "run-1" {
		t.Errorf("approval = %+v", a)
	}
	if len(a.ApproverGroups) != 1 || a.ApproverGroups[0] != "release-managers" {
		t.Errorf("approver groups = %v: the router's request widened them", a.ApproverGroups)
	}
	if d := time.Until(a.ExpiresAt); d > 10*time.Minute || d < 9*time.Minute {
		t.Errorf("expires in %v, want the policy's 10m", d)
	}
	if !strings.Contains(a.Summary, `run-1 wants to run the actuate tool "deploy"`) {
		t.Errorf("summary = %q", a.Summary)
	}
	var d toolApprovalDetails
	_ = json.Unmarshal(a.Details, &d)
	if d.Tool != "deploy" || d.SideEffect != "actuate" || d.Arguments != `{"env":"prod"}` || d.Agent != "coder" {
		t.Errorf("details = %+v", d)
	}
	if len(notifier.got) != 1 {
		t.Errorf("%d notifications, want 1", len(notifier.got))
	}
	var run agentorcav1alpha1.AgentRun
	_ = cl.Get(context.Background(), client.ObjectKey{Name: "run-1", Namespace: "team-a"}, &run)
	if c := meta.FindStatusCondition(run.Status.Conditions, agentorcav1alpha1.ConditionToolApprovalPending); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ToolApprovalPending = %+v, want True", c)
	}

	// The same call asked again is the same approval, and nobody is notified twice.
	again := decodeState(t, requestTool(s, routerSA, goodToolBody))
	if again.ID != st.ID || len(store.rows) != 1 || len(notifier.got) != 1 {
		t.Errorf("repeat: id %s vs %s, %d rows, %d notifications", again.ID, st.ID, len(store.rows), len(notifier.got))
	}
}

func TestRequestToolApprovalDefaultsWithoutAPolicy(t *testing.T) {
	store := &memToolApprovals{}
	s, _ := toolServer(t, store, nil, toolFixtures(nil)...)
	if rec := requestTool(s, routerSA, goodToolBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	a := store.rows[0]
	if len(a.ApproverGroups) != 0 {
		t.Errorf("groups = %v, want none (anyone with access to the namespace)", a.ApproverGroups)
	}
	if d := time.Until(a.ExpiresAt); d > 30*time.Minute || d < 29*time.Minute {
		t.Errorf("expires in %v, want the 30m default", d)
	}
}

func TestRequestToolApprovalLongArgumentsAreCut(t *testing.T) {
	store := &memToolApprovals{}
	s, _ := toolServer(t, store, nil, toolFixtures(nil)...)
	long, _ := json.Marshal(map[string]string{"callKey": "0123456789abcdef", "tool": "deploy", "sideEffect": "actuate", "arguments": strings.Repeat("x", 9000)})
	if rec := requestTool(s, routerSA, string(long)); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var d toolApprovalDetails
	_ = json.Unmarshal(store.rows[0].Details, &d)
	if len(d.Arguments) > maxToolApprovalArgs+4 || !strings.HasSuffix(d.Arguments, "…") {
		t.Errorf("stored %d bytes of arguments", len(d.Arguments))
	}
}

func TestRequestToolApprovalRefusals(t *testing.T) {
	store := &memToolApprovals{}
	s, cl := toolServer(t, store, nil, toolFixtures(nil)...)
	for name, tc := range map[string]struct {
		caller, body string
		want         int
	}{
		"another namespace's router": {"system:serviceaccount:team-b:x", goodToolBody, http.StatusForbidden},
		"a user, not a router":       {"alice@example.com", goodToolBody, http.StatusForbidden},
		"no tool":                    {routerSA, `{"callKey":"0123456789abcdef"}`, http.StatusBadRequest},
		"call key not hex":           {routerSA, `{"callKey":"../../etc","tool":"deploy"}`, http.StatusBadRequest},
		"malformed":                  {routerSA, `{`, http.StatusBadRequest},
		"a tool the agent lacks":     {routerSA, `{"callKey":"0123456789abcdef","tool":"drop_database"}`, http.StatusBadRequest},
	} {
		if rec := requestTool(s, tc.caller, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", name, rec.Code, tc.want)
		}
	}
	if len(store.rows) != 0 {
		t.Errorf("%d approvals opened by refused requests", len(store.rows))
	}

	// A run that has ended gets no new approvals.
	var run agentorcav1alpha1.AgentRun
	_ = cl.Get(context.Background(), client.ObjectKey{Name: "run-1", Namespace: "team-a"}, &run)
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	if err := cl.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	if rec := requestTool(s, routerSA, goodToolBody); rec.Code != http.StatusConflict {
		t.Errorf("ended run: status %d, want 409", rec.Code)
	}

	// Without the PostgreSQL store the router is told so (and fails closed).
	noStore, _ := toolServer(t, nil, nil, toolFixtures(nil)...)
	if rec := requestTool(noStore, routerSA, goodToolBody); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: status %d, want 503", rec.Code)
	}
	if rec := readTool(noStore, routerSA, "appr-a"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store (GET): status %d, want 503", rec.Code)
	}
}

func TestGetToolApproval(t *testing.T) {
	store := &memToolApprovals{}
	s, _ := toolServer(t, store, nil, toolFixtures(nil)...)
	id := decodeState(t, requestTool(s, routerSA, goodToolBody)).ID

	if st := decodeState(t, readTool(s, routerSA, id)); st.Status != "pending" {
		t.Errorf("state = %+v, want pending", st)
	}
	if _, err := store.DecideApproval(context.Background(), id, postgresql.ApprovalRejected, "bob", "not on a Friday"); err != nil {
		t.Fatal(err)
	}
	if st := decodeState(t, readTool(s, routerSA, id)); st.Status != "rejected" || st.DecidedBy != "bob" || st.Reason != "not on a Friday" {
		t.Errorf("state = %+v, want the rejection with who and why", st)
	}

	// Not found, someone else's namespace, a different run's approval, a different kind.
	if rec := readTool(s, routerSA, "nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d", rec.Code)
	}
	if rec := readTool(s, "system:serviceaccount:team-b:x", id); rec.Code != http.StatusForbidden {
		t.Errorf("other namespace's router: %d", rec.Code)
	}
	store.rows = append(store.rows,
		postgresql.Approval{ID: "other-run", Kind: postgresql.ApprovalKindTool, Namespace: "team-a", RunName: "run-2", Status: "pending"},
		postgresql.Approval{ID: "egress", Kind: postgresql.ApprovalKindEgress, Namespace: "team-a", RunName: "run-1", Status: "pending"})
	for _, other := range []string{"other-run", "egress"} {
		if rec := readTool(s, routerSA, other); rec.Code != http.StatusNotFound {
			t.Errorf("approval %q readable through run-1's endpoint: status %d", other, rec.Code)
		}
	}
}

func TestOverdueToolApprovalExpires(t *testing.T) {
	store := &memToolApprovals{}
	s, _ := toolServer(t, store, nil, toolFixtures(nil)...)
	id := decodeState(t, requestTool(s, routerSA, goodToolBody)).ID
	store.rows[0].ExpiresAt = time.Now().Add(-time.Second)

	if st := decodeState(t, readTool(s, routerSA, id)); st.Status != "expired" {
		t.Errorf("state = %+v, want expired", st)
	}
	if store.rows[0].Status != postgresql.ApprovalExpired {
		t.Errorf("stored status = %s", store.rows[0].Status)
	}
}

func TestToolApprovalRoutesBehindTheTokenReview(t *testing.T) {
	store := &memToolApprovals{}
	s, _ := toolServer(t, store, nil, toolFixtures(nil)...)
	s.k8s = tokenReviewAs(routerSA)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		s.handleAgentRun(rec, req)
		return rec
	}
	rec := call(http.MethodPost, "/agentrun/team-a/run-1/tool-approval", goodToolBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	id := decodeState(t, rec).ID
	if rec := call(http.MethodGet, "/agentrun/team-a/run-1/tool-approval/"+id, ""); rec.Code != http.StatusOK {
		t.Errorf("GET: %d", rec.Code)
	}
}

// tokenReviewAs returns a Kubernetes client whose token review authenticates
// every token as user.
func tokenReviewAs(user string) *k8sfake.Clientset {
	cs := k8sfake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, User: authv1.UserInfo{Username: user},
		}}, nil
	})
	return cs
}
