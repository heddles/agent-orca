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

package inbound

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// runTree answers RunState per run name, so a test can have delegated runs.
type runTree map[string]RunState

func (r runTree) RunState(_ context.Context, _, name string) (RunState, error) { return r[name], nil }

type progressCase struct {
	*fixture
	ad    *chatAdapter
	runs  runTree
	store *fakeApprovals
}

// started is a channel whose one message has started "run-b".
func started(t *testing.T) *progressCase {
	t.Helper()
	f, ca := chatFixture(t, `{"allowSenders":["U1"]}`)
	f.st.conns[f.c.ID] = postgresql.InboundConnection{ID: f.c.ID, TenantID: "t-1", Kind: "fake", Namespace: "ns", Agent: "support", Enabled: true, Config: []byte(`{"allowSenders":["U1"]}`)}
	p := &progressCase{fixture: f, ad: ca, runs: runTree{"run-b": {Found: true}}, store: &fakeApprovals{}}
	f.p.Runs, f.p.Approvals = p.runs, p.store
	if o := f.handle(chatMsg("1", "U1", "T1", "C1", ModeMention), nil); o != OutcomeStarted {
		t.Fatalf("outcome = %v", o)
	}
	return p
}

func (p *progressCase) tick() { p.p.DeliverPending(context.Background()) }

func (p *progressCase) want(t *testing.T, shown ...string) {
	t.Helper()
	if got := p.ad.shown(); !slices.Equal(got, shown) {
		t.Fatalf("shown = %q, want %q", got, shown)
	}
}

func pendingOn(run, kind string) postgresql.Approval {
	return postgresql.Approval{ID: "apr-" + run, Namespace: "ns", RunName: run, Kind: kind, Status: postgresql.ApprovalPending, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestProgress_WorkingThenDone(t *testing.T) {
	p := started(t)
	p.want(t, ">working")
	p.tick()
	p.tick()
	p.want(t, ">working") // unchanged state is not sent again
	p.runs["run-b"] = RunState{Found: true, Done: true, Succeeded: true, Output: "here you go"}
	p.tick()
	p.want(t, ">working", "working>done")
}

func TestProgress_ApprovalShowsAndClears(t *testing.T) {
	for _, kind := range []string{postgresql.ApprovalKindTool, postgresql.ApprovalKindBudget, postgresql.ApprovalKindEgress} {
		t.Run(kind, func(t *testing.T) {
			p := started(t)
			p.store.pending = []postgresql.Approval{pendingOn("run-b", kind)}
			p.tick()
			p.tick()
			p.want(t, ">working", "working>approval")
			p.store.pending[0].Status = postgresql.ApprovalApproved // decided here or in the console
			p.tick()
			p.want(t, ">working", "working>approval", "approval>working")
		})
	}
}

func TestProgress_ApprovalOfADelegatedRun(t *testing.T) {
	p := started(t)
	p.runs["child"] = RunState{Found: true, Parent: "run-b"}
	p.runs["grandchild"] = RunState{Found: true, Parent: "child"}
	p.runs["other"] = RunState{Found: true, Parent: "someone-else"}
	p.store.pending = []postgresql.Approval{pendingOn("other", postgresql.ApprovalKindTool)}
	p.tick()
	p.want(t, ">working") // another run's approval is not this message's
	p.store.pending = append(p.store.pending, pendingOn("grandchild", postgresql.ApprovalKindTool))
	p.tick()
	p.want(t, ">working", "working>approval")
}

func TestProgress_HowARunEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		state RunState
		want  string
	}{
		"failed":    {RunState{Found: true, Done: true}, "working>failed"},
		"no answer": {RunState{Found: true, Done: true, Succeeded: true, Output: "  "}, "working>failed"},
		"question":  {RunState{Found: true, Waiting: true, Question: "which node?"}, "working>question"},
		"gone":      {RunState{}, "working>failed"},
		"answered":  {RunState{Found: true, Done: true, Succeeded: true, Continued: true, Output: "x"}, "working>"},
	} {
		t.Run(name, func(t *testing.T) {
			p := started(t)
			p.runs["run-b"] = tc.state
			p.tick()
			p.want(t, ">working", tc.want)
		})
	}
}

func TestProgress_TimeoutAndUndeliverableReplyFail(t *testing.T) {
	p := started(t)
	p.p.Now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	p.tick()
	p.want(t, ">working", "working>failed")

	p = started(t)
	p.p.ReplyAttempts = 1
	p.ad.failFor = 1
	p.runs["run-b"] = RunState{Found: true, Done: true, Succeeded: true, Output: "ok"}
	p.tick()
	p.want(t, ">working", "working>failed")
}

func TestProgress_ARunThatCannotStartFails(t *testing.T) {
	for name, err := range map[string]error{"busy": &LaunchError{Busy: true, Message: "busy"}, "gone": &LaunchError{Gone: true, Message: "gone"}} {
		t.Run(name, func(t *testing.T) {
			f, ca := chatFixture(t, `{"allowSenders":["U1"]}`)
			f.l.err = err
			f.handle(chatMsg("1", "U1", "T1", "C1", ModeMention), nil)
			if got := ca.shown(); !slices.Equal(got, []string{">failed"}) {
				t.Fatalf("shown = %q", got)
			}
		})
	}
	// Someone who may not write is told nothing, by reaction either.
	f, ca := chatFixture(t, `{"allowSenders":["U1"]}`)
	f.handle(chatMsg("1", "stranger", "T1", "C1", ModeMention), nil)
	if got := ca.shown(); len(got) != 0 {
		t.Fatalf("shown to a stranger = %q", got)
	}
}
