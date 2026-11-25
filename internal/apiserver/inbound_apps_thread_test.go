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
	"strconv"
	"sync"
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// threadStore answers "which run is latest in this thread", which the plain fake
// does not.
type threadStore struct {
	*fakeInboundStore
	mu     sync.Mutex
	latest *agentorcav1alpha1.AgentRun
}

func (s *threadStore) LatestInboundRunInThread(context.Context, string, string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return "", "", nil
	}
	return s.latest.Namespace, s.latest.Name, nil
}

// waitingApproval holds one tool call waiting on whatever run it is asked about.
type waitingApproval struct{ decided []string }

func (w *waitingApproval) ListApprovals(_ context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error) {
	return []postgresql.Approval{{ID: "apr_12345678", Namespace: q.Namespace, RunName: q.RunName, Kind: postgresql.ApprovalKindTool, Summary: "delete a pod"}}, nil
}

func (w *waitingApproval) DecideApproval(_ context.Context, id, _, by, _ string) (postgresql.Approval, error) {
	w.decided = append(w.decided, id+" by "+by)
	return postgresql.Approval{ID: id}, nil
}

// In a thread where one agent's run waits, a message routed to another agent
// must neither decide that run's tool call (the other agent's approvers have no
// say over it) nor answer its question.
func TestChatApps_AnotherAgentCannotDecideOrAnswerARunInTheThread(t *testing.T) {
	f := newAppFixture(t)
	threads := &threadStore{fakeInboundStore: f.store}
	approvals := &waitingApproval{}
	f.s.inboundPipe.Store = threads
	f.s.inboundPipe.Approvals = approvals
	f.addAgent("support", `{"places":[{"id":"C1"}],"allowSenders":["U1"]}`)
	// U1 may approve for "second" only.
	f.addAgent("second", `{"places":[{"id":"C1"}],"allowSenders":["U1"],"approvers":["U1"]}`)
	n := 0
	send := func(text string) {
		n++
		rec := f.post(f.app.inboundView, enteredToken, map[string]string{
			"User": "U1", "Team": "T1", "Channel": "C1", "ID": "t" + strconv.Itoa(n), "Mode": "mention", "Text": text,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("post = %d %s", rec.Code, rec.Body)
		}
	}
	send("start something")
	runs := f.runs()
	if len(runs) != 1 || runs[0].Spec.AgentRef != inboundAgent {
		t.Fatalf("runs = %+v", runs)
	}
	waiting := runs[0]
	waiting.Status.Phase = agentorcav1alpha1.AgentRunPhaseWaitingForInput
	waiting.Status.ClarifyQuestion = "which one?"
	if err := f.c.Status().Update(context.Background(), &waiting); err != nil {
		t.Fatal(err)
	}
	threads.latest = &waiting

	send("second approve")
	if len(approvals.decided) != 0 {
		t.Fatalf("another agent's approver decided the call: %v", approvals.decided)
	}
	for _, r := range f.runs() {
		if r.Name == waiting.Name {
			continue
		}
		if r.Spec.AgentRef != "second" || r.Spec.PriorRunRef != "" {
			t.Errorf("a message for another agent carried on the waiting run: %s agent=%s prior=%q", r.Name, r.Spec.AgentRef, r.Spec.PriorRunRef)
		}
	}
	if len(f.runs()) != 2 {
		t.Errorf("runs = %d, want the waiting one and a new one", len(f.runs()))
	}

	// The same approver rule still works for the thread's own agent.
	send("approve")
	if len(approvals.decided) != 0 {
		t.Errorf("a person who is not the agent's approver decided: %v", approvals.decided)
	}
}
