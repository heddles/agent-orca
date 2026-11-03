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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOperator stands in for the operator's per-run budget API: it records
// what the router reports and answers GET .../budget with a controllable state.
type fakeOperator struct {
	mu     sync.Mutex
	posts  []string // "<endpoint> <body>"
	state  operatorBudget
	srv    *httptest.Server
	getHit int
}

func newFakeOperator(t *testing.T, initial operatorBudget) *fakeOperator {
	t.Helper()
	f := &fakeOperator{state: initial}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/agentrun/default/test-run/") || r.Header.Get("Authorization") != "Bearer test-key-not-real" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusForbidden)
			return
		}
		endpoint := strings.TrimPrefix(r.URL.Path, "/agentrun/default/test-run/")
		if r.Method == http.MethodGet && endpoint == "budget" {
			f.getHit++
			_ = json.NewEncoder(w).Encode(f.state)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.posts = append(f.posts, endpoint+" "+string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOperator) set(s operatorBudget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *fakeOperator) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

func (f *fakeOperator) sawPost(prefix string) bool {
	for _, p := range f.reported() {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func budgetRouter(t *testing.T, llmURL string, op *fakeOperator, b BudgetConfig) (*Router, *testStore) {
	t.Helper()
	store := newTestStore(t)
	keyFile := writeKeyFile(t)
	r := newStreamingTestRouter(t, llmURL, keyFile, store)
	r.cfg.Budget = &b
	r.cfg.OperatorAPIURL = op.srv.URL
	r.budgetPoll = 20 * time.Millisecond
	return r, store
}

// chatOnce sends one chat request and fails the test, instead of hanging it, if
// the router holds the request for more than 5 s.
func chatOnce(t *testing.T, r *Router) *writeHeaderCountingRecorder {
	t.Helper()
	rec := &writeHeaderCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	body := `{"messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		r.HandleChatCompletions(rec, req)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the router is still holding the request after 5 s")
	}
	return rec
}

func budgetEvents(store *testStore) []map[string]string {
	store.mu.Lock()
	defer store.mu.Unlock()
	var out []map[string]string
	for _, e := range store.traceEvents {
		var m map[string]string
		if json.Unmarshal([]byte(e), &m) == nil && m["type"] == traceEventTypeBudget {
			out = append(out, m)
		}
	}
	return out
}

func TestBudgetWarnsOnceAtThreshold(t *testing.T) {
	op := newFakeOperator(t, operatorBudget{})
	r, store := budgetRouter(t, "http://unused", op, BudgetConfig{MaxUSD: 1, WarnAtPercent: 80, OnExceeded: "fail"})

	record := func(spend float64) {
		r.mu.Lock()
		r.spendUSD = spend
		r.noteBudgetSpendLocked()
		r.mu.Unlock()
	}
	record(0.5) // below the threshold
	record(0.85)
	record(0.9) // already warned
	eventually(t, "the warning", func() bool { return op.sawPost("budget ") })
	time.Sleep(50 * time.Millisecond)

	var warnings int
	for _, p := range op.reported() {
		if strings.HasPrefix(p, "budget ") && strings.Contains(p, `"state":"warning"`) {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("operator got %d warnings, want exactly one", warnings)
	}
	evs := budgetEvents(store)
	if len(evs) != 1 || evs[0]["state"] != "warning" || evs[0]["capUSD"] != "1.0000" {
		t.Errorf("budget trace events = %v", evs)
	}
}

func TestBudgetFailHaltsTheRunAtTheCap(t *testing.T) {
	llm, calls := fakeProvider(t, textTurnSSE(finishReasonStop))
	op := newFakeOperator(t, operatorBudget{})
	r, store := budgetRouter(t, llm.URL, op, BudgetConfig{MaxUSD: 1, OnExceeded: "fail"})
	r.mu.Lock()
	r.spendUSD = 1.2
	r.mu.Unlock()

	rec := chatOnce(t, r)
	if rec.Code != http.StatusGone {
		t.Fatalf("status %d, want 410", rec.Code)
	}
	if calls.Load() != 0 {
		t.Error("the LLM was called although the run was over its budget")
	}
	eventually(t, "the operator to hear about the trip", func() bool { return op.sawPost("loop-detected ") })
	for _, p := range op.reported() {
		if strings.HasPrefix(p, "loop-detected ") && !strings.Contains(p, "budget of $1.0000 reached ($1.2000 spent)") {
			t.Errorf("trip reason = %s", p)
		}
	}
	if evs := budgetEvents(store); len(evs) != 1 || evs[0]["state"] != "exceeded" {
		t.Errorf("budget events = %v", evs)
	}
	if again := chatOnce(t, r); again.Code != http.StatusGone {
		t.Errorf("a later request got %d, want 410", again.Code)
	}
}

func TestBudgetPauseHoldsUntilTheCapIsRaised(t *testing.T) {
	llm, calls := fakeProvider(t, textTurnSSE(finishReasonStop))
	op := newFakeOperator(t, operatorBudget{State: "exceeded", CapUSD: "1.0000"})
	r, store := budgetRouter(t, llm.URL, op, BudgetConfig{MaxUSD: 1, OnExceeded: "pause"})
	r.mu.Lock()
	r.spendUSD = 1.2
	r.mu.Unlock()

	done := make(chan *writeHeaderCountingRecorder, 1)
	go func() { done <- chatOnce(t, r) }()

	eventually(t, "the exceeded report", func() bool { return op.sawPost("budget ") && strings.Contains(op.reported()[0], `"state":"exceeded"`) })
	select {
	case <-done:
		t.Fatal("the request returned while the budget was exceeded")
	case <-time.After(150 * time.Millisecond):
	}
	if calls.Load() != 0 {
		t.Fatal("the LLM was called during the pause")
	}

	// An approver raises the cap; the operator reports it and the router resumes.
	op.set(operatorBudget{State: "ok", CapUSD: "5.0000"})
	var rec *writeHeaderCountingRecorder
	select {
	case rec = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the request did not resume after the cap was raised")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Stored.") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status %d, body %q: want the model's reply", rec.Code, rec.Body.String())
	}
	if rec.writeHeaderCalls != 1 {
		t.Errorf("WriteHeader called %d times, want once (the pause began the response)", rec.writeHeaderCalls)
	}
	if calls.Load() != 1 {
		t.Errorf("LLM called %d times, want once after the raise", calls.Load())
	}
	evs := budgetEvents(store)
	states := make([]string, 0, len(evs))
	for _, e := range evs {
		states = append(states, e["state"])
	}
	if strings.Join(states, ",") != "exceeded,resumed" {
		t.Errorf("budget events = %v, want exceeded then resumed", states)
	}
}

func TestBudgetPauseRejectedEndsTheRun(t *testing.T) {
	llm, calls := fakeProvider(t, textTurnSSE(finishReasonStop))
	op := newFakeOperator(t, operatorBudget{State: "exceeded", CapUSD: "1.0000"})
	r, store := budgetRouter(t, llm.URL, op, BudgetConfig{MaxUSD: 1, OnExceeded: "pause"})
	r.mu.Lock()
	r.spendUSD = 2
	r.mu.Unlock()

	done := make(chan *writeHeaderCountingRecorder, 1)
	go func() { done <- chatOnce(t, r) }()
	eventually(t, "the exceeded report", func() bool { return op.sawPost("budget ") })

	// The raise is refused (or timed out): the operator fails the run.
	op.set(operatorBudget{State: "exceeded", CapUSD: "1.0000", Terminal: true})
	var rec *writeHeaderCountingRecorder
	select {
	case rec = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the request stayed held after the run was rejected")
	}
	if calls.Load() != 0 {
		t.Error("the LLM was called for a rejected run")
	}
	if !strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "data: [DONE]") {
		t.Errorf("held stream not closed cleanly: %q", rec.Body.String())
	}
	evs := budgetEvents(store)
	states := make([]string, 0, len(evs))
	for _, e := range evs {
		states = append(states, e["state"])
	}
	if strings.Join(states, ",") != "exceeded,rejected" {
		t.Errorf("budget events = %v", states)
	}
	if again := chatOnce(t, r); again.Code != http.StatusGone {
		t.Errorf("a later request got %d, want 410", again.Code)
	}
}

func TestBudgetRestartedRouterAdoptsTheRaisedCap(t *testing.T) {
	llm, calls := fakeProvider(t, textTurnSSE(finishReasonStop))
	// Spend (restored from the checkpoint) is above the initial cap, but the
	// operator says earlier approvals raised it.
	op := newFakeOperator(t, operatorBudget{State: "ok", CapUSD: "6.0000"})
	r, _ := budgetRouter(t, llm.URL, op, BudgetConfig{MaxUSD: 2, OnExceeded: "pause"})
	r.mu.Lock()
	r.spendUSD = 3
	r.mu.Unlock()

	rec := chatOnce(t, r)
	if rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status %d, %d LLM calls: want the request served under the raised cap", rec.Code, calls.Load())
	}
	if op.sawPost("budget ") {
		t.Error("the router reported an exceeded budget although the cap was raised")
	}
}

func TestBudgetOffByDefault(t *testing.T) {
	llm, calls := fakeProvider(t, textTurnSSE(finishReasonStop))
	store := newTestStore(t)
	r := newStreamingTestRouter(t, llm.URL, writeKeyFile(t), store)
	r.mu.Lock()
	r.spendUSD = 1000
	r.mu.Unlock()
	if rec := chatOnce(t, r); rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Errorf("status %d, %d LLM calls: a run without a budget must not be capped", rec.Code, calls.Load())
	}
}
