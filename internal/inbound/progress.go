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
	"log/slog"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// Showing, on the message itself, where its request stands (a reaction, a
// typing hint). The core decides the state; an adapter only says how its
// platform shows it. The state shown is kept with the message, so the next
// change knows what to take away, whichever replica or restart makes it.
//
// All of it is best effort: a missing permission or a deleted message never
// affects the run or its answer.

// Progress is where a request stands.
type Progress string

const (
	// ProgressNone shows nothing.
	ProgressNone Progress = ""
	// ProgressWorking: the agent is handling the request.
	ProgressWorking Progress = "working"
	// ProgressApproval: something waits for an approver, here or in a run the
	// agent delegated to.
	ProgressApproval Progress = "approval"
	// ProgressQuestion: the agent asked the sender something.
	ProgressQuestion Progress = "question"
	// ProgressDone: the answer was delivered.
	ProgressDone Progress = "done"
	// ProgressFailed: the sender got no answer.
	ProgressFailed Progress = "failed"
)

// Indicator is an adapter that can show a request's progress. Show replaces
// from with now on the message the target points at; either may be
// ProgressNone. An adapter that cannot show a state does nothing for it.
type Indicator interface {
	Show(ctx context.Context, c Connection, to ReplyTarget, from, now Progress) error
}

// maxDelegationDepth bounds the walk from a delegated run to the run it serves.
const maxDelegationDepth = 8

func (p *Pipeline) indicator(kind string) (Indicator, bool) {
	a, ok := p.Registry.Get(kind)
	if !ok || !a.Capabilities().WorkingIndicator {
		return nil, false
	}
	ind, ok := a.(Indicator)
	return ind, ok
}

// show changes what the message shows: bounded, never an error for the message.
func (p *Pipeline) show(ctx context.Context, c Connection, to ReplyTarget, from, now Progress) {
	ind, ok := p.indicator(c.Kind)
	if !ok || len(to) == 0 || from == now {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ind.Show(ictx, c, to, from, now); err != nil {
		slog.Info("showing a request's progress", "connection", c.ID, "kind", c.Kind, "state", now, "err", safeErr(err, c))
	}
}

// showStored is show for a message of the ledger, whose connection is stored.
func (p *Pipeline) showStored(ctx context.Context, sc postgresql.InboundConnection, m postgresql.InboundMessage, now Progress) {
	if _, ok := p.indicator(sc.Kind); !ok || !sc.Enabled || len(m.ReplyTarget) == 0 {
		return
	}
	c, err := p.Resolve(ctx, sc)
	if err != nil {
		return
	}
	p.show(ctx, c, m.ReplyTarget, Progress(m.Progress), now)
}

// begin shows that work has started on a message whose run was just recorded.
// The reply worker may deliver the answer while the platform is still being
// told, which would leave "working" on an answered message, so it is taken
// back if the message is no longer running.
func (p *Pipeline) begin(ctx context.Context, c Connection, m Message, run RunRef) {
	p.show(ctx, c, m.Reply, ProgressNone, ProgressWorking)
	finder, ok := p.Store.(RunMessages)
	if !ok {
		return
	}
	if _, running, err := finder.FindRunningInboundMessage(postgresql.WithTenant(ctx, c.TenantID), run.Namespace, run.Name); err == nil && !running {
		p.show(ctx, c, m.Reply, ProgressWorking, ProgressNone)
	}
}

// trackApproval switches an unfinished message between working and waiting for
// approval. An approval may be decided in the console, so this is read, not told.
func (p *Pipeline) trackApproval(ctx context.Context, scope context.Context, sc postgresql.InboundConnection, m postgresql.InboundMessage) {
	if _, ok := p.indicator(sc.Kind); !ok || p.Approvals == nil {
		return
	}
	waiting, err := p.awaitsApproval(ctx, m)
	if err != nil {
		slog.Warn("looking for the approvals of a message", "connection", sc.ID, "err", err)
		return
	}
	want := ProgressWorking
	if waiting {
		want = ProgressApproval
	}
	if Progress(m.Progress) == want {
		return
	}
	p.showStored(ctx, sc, m, want)
	if err := p.Store.UpdateInboundMessage(scope, m.ConnectionID, m.MessageKey, postgresql.InboundUpdate{Status: postgresql.InboundRunning, Progress: string(want)}); err != nil {
		slog.Warn("recording a message's progress", "connection", sc.ID, "err", err)
	}
}

// awaitsApproval reports whether any approval is pending on the message's run,
// or on a run delegated from it, at any depth.
func (p *Pipeline) awaitsApproval(ctx context.Context, m postgresql.InboundMessage) (bool, error) {
	pending, err := p.Approvals.ListApprovals(ctx, postgresql.ApprovalQuery{
		TenantID: m.TenantID, Namespace: m.RunNamespace, Status: postgresql.ApprovalPending, Limit: 500,
	})
	if err != nil {
		return false, err
	}
	parents := map[string]string{}
	for _, a := range pending {
		run := a.RunName
		for depth := 0; run != "" && depth < maxDelegationDepth; depth++ {
			if run == m.RunName {
				return true, nil
			}
			parent, seen := parents[run]
			if !seen {
				st, err := p.Runs.RunState(ctx, m.RunNamespace, run)
				if err != nil {
					return false, err
				}
				parent, parents[run] = st.Parent, st.Parent
			}
			run = parent
		}
	}
	return false, nil
}
