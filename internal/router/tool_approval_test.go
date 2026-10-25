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

package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ranMarker is what dispatchToolCall returns for a tool of backend type
// "builtin" that got past the gate: the tool "ran" and reached its backend.
const ranMarker = "is not implemented by this model-router build"

// fakeApprovalService answers the operator's tool-approval endpoints.
type fakeApprovalService struct {
	mu       sync.Mutex
	status   string // what GET reports
	decider  string
	reason   string
	failPost int // POST answers 503 this many times
	posts    []map[string]string
	gets     int
	srv      *httptest.Server
}

func newFakeApprovalService(t *testing.T, status string) *fakeApprovalService {
	t.Helper()
	f := &fakeApprovalService{status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-key-not-real" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agentrun/default/test-run/tool-approval":
			if f.failPost > 0 {
				f.failPost--
				http.Error(w, "no approval store", http.StatusServiceUnavailable)
				return
			}
			var body map[string]string
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			f.posts = append(f.posts, body)
			_ = json.NewEncoder(w).Encode(toolApprovalState{ID: "appr-1", Status: f.statusLocked(true), DecidedBy: f.decider, Reason: f.reason})
		case r.Method == http.MethodGet && r.URL.Path == "/agentrun/default/test-run/tool-approval/appr-1":
			f.gets++
			_ = json.NewEncoder(w).Encode(toolApprovalState{ID: "appr-1", Status: f.statusLocked(false), DecidedBy: f.decider, Reason: f.reason})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// statusLocked is the status to report; the first answer (the POST) of a
// pending approval is always "pending".
func (f *fakeApprovalService) statusLocked(initial bool) string {
	if initial && f.status != "approved" && f.status != "rejected" && f.status != "expired" {
		return "pending"
	}
	return f.status
}

func (f *fakeApprovalService) decide(status, by, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.decider, f.reason = status, by, reason
}

func (f *fakeApprovalService) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func toolRouter(t *testing.T, svc *fakeApprovalService, cfg *ToolApprovalConfig, class string) (*Router, *testStore) {
	t.Helper()
	store := newTestStore(t)
	r := newStreamingTestRouter(t, "http://unused", writeKeyFile(t), store)
	r.cfg.OperatorAPIURL = svc.srv.URL
	r.cfg.ToolApproval = cfg
	r.cfg.ToolDefinitions = []ToolDefinition{{Name: "deploy", BackendType: backendTypeBuiltin, SideEffect: class}}
	r.toolApprovalPoll = 20 * time.Millisecond
	return r, store
}

func callDeploy(r *Router) string {
	return r.dispatchToolCall(context.Background(), ToolCall{
		ID: "call_1", Type: "function",
		Function: FunctionCall{Name: "deploy", Arguments: `{"env":"prod"}`},
	})
}

func stringEventsOfType(store *testStore, typ string) []map[string]string {
	store.mu.Lock()
	defer store.mu.Unlock()
	var out []map[string]string
	for _, e := range store.traceEvents {
		var m map[string]string
		if json.Unmarshal([]byte(e), &m) == nil && m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func approvalStates(store *testStore) string {
	evs := stringEventsOfType(store, traceEventTypeToolApproval)
	states := make([]string, 0, len(evs))
	for _, e := range evs {
		states = append(states, e["state"])
	}
	return strings.Join(states, ",")
}

func TestToolApprovalRequirement(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg   *ToolApprovalConfig
		class string
		want  string
	}{
		"no policy: read runs":                 {nil, "read", "none"},
		"no policy: write runs":                {nil, "write", "none"},
		"no policy: unclassified runs":         {nil, "", "none"},
		"no policy: actuate needs an approver": {nil, "actuate", "approve"},
		"empty policy: actuate still approves": {&ToolApprovalConfig{}, "actuate", "approve"},
		"write set to approve":                 {&ToolApprovalConfig{Write: "approve"}, "write", "approve"},
		"actuate can be denied":                {&ToolApprovalConfig{Actuate: "deny"}, "actuate", "deny"},
		"actuate can be relaxed":               {&ToolApprovalConfig{Actuate: "none"}, "actuate", "none"},
		"unclassified can be gated":            {&ToolApprovalConfig{Unclassified: "approve"}, "", "approve"},
		"unknown value falls back to default":  {&ToolApprovalConfig{Actuate: "maybe"}, "actuate", "approve"},
		"unknown class counts as unclassified": {&ToolApprovalConfig{Unclassified: "deny"}, "teleport", "deny"},
		"read policy does not leak to actuate": {&ToolApprovalConfig{Read: "deny"}, "actuate", "approve"},
	} {
		if got := tc.cfg.requirement(tc.class); got != tc.want {
			t.Errorf("%s: requirement = %q, want %q", name, got, tc.want)
		}
	}
}

func TestToolWithoutARequirementRunsWithoutAsking(t *testing.T) {
	svc := newFakeApprovalService(t, "approved")
	r, store := toolRouter(t, svc, nil, "write")
	if got := callDeploy(r); !strings.Contains(got, ranMarker) {
		t.Fatalf("result = %q, want the tool to run", got)
	}
	if svc.postCount() != 0 {
		t.Error("the operator was asked about a tool that needs no approval")
	}
	if st := approvalStates(store); st != "" {
		t.Errorf("approval events = %q, want none", st)
	}
}

func TestActuateToolWaitsForAnApproverThenRuns(t *testing.T) {
	svc := newFakeApprovalService(t, "pending")
	r, store := toolRouter(t, svc, nil, "actuate")
	// The approval wait must not count against the per-tool timeout.
	r.toolTimeout = 50 * time.Millisecond

	done := make(chan string, 1)
	go func() { done <- callDeploy(r) }()
	eventually(t, "the approval request", func() bool { return svc.postCount() == 1 })
	select {
	case got := <-done:
		t.Fatalf("the tool ran before it was approved: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	svc.decide("approved", "alice", "ok")

	select {
	case got := <-done:
		if !strings.Contains(got, ranMarker) {
			t.Fatalf("result = %q, want the tool to run after approval (not a timeout)", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the tool did not run after approval")
	}
	svc.mu.Lock()
	post := svc.posts[0]
	svc.mu.Unlock()
	if post["tool"] != "deploy" || post["sideEffect"] != "actuate" || post["arguments"] != `{"env":"prod"}` || post["callKey"] == "" {
		t.Errorf("approval request = %v", post)
	}
	evs := stringEventsOfType(store, traceEventTypeToolApproval)
	if approvalStates(store) != "requested,approved" || evs[1]["by"] != "alice" || evs[0]["approvalId"] != "appr-1" {
		t.Errorf("approval events = %v", evs)
	}
}

func TestRejectedToolDoesNotRunAndTheModelIsTold(t *testing.T) {
	svc := newFakeApprovalService(t, "rejected")
	svc.decide("rejected", "bob", "not on a Friday")
	r, store := toolRouter(t, svc, nil, "actuate")

	got := callDeploy(r)
	if strings.Contains(got, ranMarker) {
		t.Fatal("a rejected tool ran")
	}
	for _, want := range []string{`tool \"deploy\" was not run`, "an approver rejected it (bob): not on a Friday", "Do not repeat the same call"} {
		if !strings.Contains(got, want) {
			t.Errorf("result %q lacks %q", got, want)
		}
	}
	var res map[string]string
	if err := json.Unmarshal([]byte(got), &res); err != nil || res["error"] == "" {
		t.Errorf("result is not a JSON error object: %q (%v)", got, err)
	}
	if st := approvalStates(store); st != "requested,rejected" {
		t.Errorf("approval events = %q", st)
	}
}

func TestExpiredApprovalRefusesTheTool(t *testing.T) {
	svc := newFakeApprovalService(t, "expired")
	r, store := toolRouter(t, svc, nil, "actuate")
	got := callDeploy(r)
	if strings.Contains(got, ranMarker) || !strings.Contains(got, "no approver decided in time") {
		t.Errorf("result = %q", got)
	}
	if st := approvalStates(store); st != "requested,expired" {
		t.Errorf("approval events = %q", st)
	}
}

func TestDeniedClassIsRefusedWithoutAsking(t *testing.T) {
	svc := newFakeApprovalService(t, "approved")
	r, store := toolRouter(t, svc, &ToolApprovalConfig{Actuate: "deny"}, "actuate")
	got := callDeploy(r)
	if strings.Contains(got, ranMarker) || !strings.Contains(got, "does not allow actuate tools") {
		t.Errorf("result = %q", got)
	}
	if svc.postCount() != 0 {
		t.Error("the operator was asked about a denied tool")
	}
	if st := approvalStates(store); st != "denied" {
		t.Errorf("approval events = %q", st)
	}
}

func TestUnreachableApprovalServiceFailsClosed(t *testing.T) {
	svc := newFakeApprovalService(t, "approved")
	svc.failPost = 1
	r, store := toolRouter(t, svc, nil, "actuate")
	got := callDeploy(r)
	if strings.Contains(got, ranMarker) || !strings.Contains(got, "approval service is unavailable") {
		t.Errorf("result = %q: a tool that needs an approver ran without one", got)
	}
	if st := approvalStates(store); st != "unavailable" {
		t.Errorf("approval events = %q", st)
	}

	// With no operator URL at all, the same.
	r.cfg.OperatorAPIURL = ""
	if got := callDeploy(r); strings.Contains(got, ranMarker) {
		t.Errorf("a tool that needs an approver ran with no operator configured: %q", got)
	}
}

func TestEndedRunReleasesAWaitingToolCall(t *testing.T) {
	svc := newFakeApprovalService(t, "pending")
	r, _ := toolRouter(t, svc, nil, "actuate")
	done := make(chan string, 1)
	go func() { done <- callDeploy(r) }()
	eventually(t, "the approval request", func() bool { return svc.postCount() == 1 })
	r.cancelFunc()
	select {
	case got := <-done:
		if strings.Contains(got, ranMarker) || !strings.Contains(got, "run ended") {
			t.Errorf("result = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a waiting tool call outlived its run")
	}
}

func TestOnlyOperatorDefinedToolsAreGated(t *testing.T) {
	svc := newFakeApprovalService(t, "pending")
	r, _ := toolRouter(t, svc, &ToolApprovalConfig{Unclassified: "approve"}, "")
	got := r.dispatchToolCall(context.Background(), ToolCall{ID: "c", Type: "function", Function: FunctionCall{Name: "no_such_tool", Arguments: "{}"}})
	if !strings.Contains(got, "unknown tool") || svc.postCount() != 0 {
		t.Errorf("result = %q, posts = %d: unknown tools should fail as before, ungated", got, svc.postCount())
	}
}
