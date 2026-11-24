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
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/redact"
)

// Runner runs the background work of the channels on the elected leader: one
// poller per connection of a polling adapter, the delivery of answers, and the
// pruning of the ledger. With no connections it does nothing.
type Runner struct {
	P *Pipeline
	// SyncEvery is how often the connections are re-read (default 30s).
	SyncEvery time.Duration
	// ReplyEvery is how often finished runs are looked for (default 5s).
	ReplyEvery time.Duration
	// MaxConcurrent bounds the polls in flight at once (default 8).
	MaxConcurrent int
	// PollTimeout bounds one poll (default 2 minutes).
	PollTimeout time.Duration
	// Retention is how long ledger rows are kept for de-duplication (default 30 days).
	Retention time.Duration

	sem     chan struct{}
	mu      sync.Mutex
	pollers map[string]*pollerHandle
	wg      sync.WaitGroup
}

type pollerHandle struct {
	cancel context.CancelFunc
	print  string
}

// NeedLeaderElection makes controller-runtime run this on the leader only.
func (r *Runner) NeedLeaderElection() bool { return true }

func orDur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Start runs until ctx ends, then waits for the pollers to stop.
func (r *Runner) Start(ctx context.Context) error {
	n := r.MaxConcurrent
	if n <= 0 {
		n = 8
	}
	r.mu.Lock()
	r.sem = make(chan struct{}, n)
	r.pollers = map[string]*pollerHandle{}
	r.mu.Unlock()
	syncT := time.NewTicker(orDur(r.SyncEvery, 30*time.Second))
	replyT := time.NewTicker(orDur(r.ReplyEvery, 5*time.Second))
	pruneT := time.NewTicker(time.Hour)
	defer syncT.Stop()
	defer replyT.Stop()
	defer pruneT.Stop()
	pctx := postgresql.WithPlatform(ctx)
	r.Sync(pctx)
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for _, h := range r.pollers {
				h.cancel()
			}
			r.mu.Unlock()
			r.wg.Wait()
			return nil
		case <-syncT.C:
			r.Sync(pctx)
		case <-replyT.C:
			r.P.DeliverPending(pctx)
		case <-pruneT.C:
			if _, err := r.P.Store.DeleteInboundMessagesBefore(pctx, r.P.now().Add(-orDur(r.Retention, 30*24*time.Hour))); err != nil {
				slog.Warn("pruning the inbound ledger", "err", err)
			}
		}
	}
}

// Sync starts, restarts and stops pollers so they match the stored connections.
func (r *Runner) Sync(ctx context.Context) {
	conns, err := r.P.Store.ListInboundConnections(ctx, nil, "")
	if err != nil {
		slog.Warn("listing inbound connections", "err", err)
		return
	}
	want := map[string]postgresql.InboundConnection{}
	for _, c := range conns {
		if !c.Enabled {
			continue
		}
		if a, ok := r.P.Registry.Get(c.Kind); ok {
			if _, isPoller := a.(Poller); isPoller && polls(a, c.Config) {
				want[c.ID] = c
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, h := range r.pollers {
		if c, ok := want[id]; !ok || h.print != fingerprint(c) {
			h.cancel()
			delete(r.pollers, id)
		}
	}
	for id, c := range want {
		if _, running := r.pollers[id]; running {
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		r.pollers[id] = &pollerHandle{cancel: cancel, print: fingerprint(c)}
		r.wg.Go(func() { r.poll(pctx, c) })
	}
}

// Running is the number of pollers started.
func (r *Runner) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pollers)
}

// polls reports whether a connection of a polling adapter needs a poller.
func polls(a Adapter, config json.RawMessage) bool {
	sel, ok := a.(PollSelector)
	if !ok {
		return true
	}
	s, err := ParseSettings(config)
	return err == nil && sel.Polls(s.Adapter)
}

func fingerprint(c postgresql.InboundConnection) string {
	return c.UpdatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.SecretRef
}

// poll is the loop of one connection: a poll, then a wait that grows while polls
// fail (up to 15 minutes) and returns to the configured interval after a success.
func (r *Runner) poll(ctx context.Context, sc postgresql.InboundConnection) {
	settings, _ := ParseSettings(sc.Config)
	every := orDur(settings.PollEvery(), DefaultPollSeconds*time.Second)
	fails := 0
	for {
		err := r.pollOnce(ctx, sc)
		wait := every
		if err != nil {
			fails++
			wait = min(every<<min(fails, 5), 15*time.Minute)
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (r *Runner) pollOnce(ctx context.Context, sc postgresql.InboundConnection) error {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	scope := postgresql.WithTenant(ctx, sc.TenantID)
	a, _ := r.P.Registry.Get(sc.Kind)
	pl, ok := a.(Poller)
	if !ok {
		return ErrUnknownKind
	}
	c, err := r.P.Resolve(ctx, sc)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, orDur(r.PollTimeout, 2*time.Minute))
		err = pl.PollOnce(pctx, c, r.P)
		cancel()
	}
	if ctx.Err() != nil {
		return nil
	}
	now := r.P.now()
	st := postgresql.InboundStatus{PolledAt: &now, SetError: true}
	if err != nil {
		st.LastError = clip(safeErr(err, c), 300)
		slog.Warn("polling an inbound connection failed", "connection", sc.ID, "kind", sc.Kind, "err", st.LastError)
	}
	if rerr := r.P.Store.RecordInboundStatus(scope, sc.ID, st); rerr != nil {
		slog.Warn("recording an inbound poll", "connection", sc.ID, "err", rerr)
	}
	return err
}

// ── delivery of answers ──────────────────────────────────────────────────────

const failureText = "The agent could not complete this request."

// DeliverPending sends the answer of every started run that has finished.
func (p *Pipeline) DeliverPending(ctx context.Context) {
	msgs, err := p.Store.ListInboundMessagesByStatus(ctx, postgresql.InboundRunning, 50)
	if err != nil {
		slog.Warn("listing pending inbound replies", "err", err)
		return
	}
	for _, m := range msgs {
		if ctx.Err() != nil {
			return
		}
		p.deliver(ctx, m)
	}
}

func (p *Pipeline) deliver(ctx context.Context, m postgresql.InboundMessage) {
	rkey := m.ConnectionID + "|" + m.MessageKey
	p.mu.Lock()
	rt := p.retry[rkey]
	p.mu.Unlock()
	if p.now().Before(rt.next) {
		return
	}
	scope := postgresql.WithTenant(ctx, m.TenantID)
	giveUp := func(reason string, o Outcome, st postgresql.InboundStatus) {
		_ = p.Store.UpdateInboundMessage(scope, m.ConnectionID, m.MessageKey, postgresql.InboundUpdate{Status: postgresql.InboundFailed, Reason: reason, ClearTarget: true})
		if st != (postgresql.InboundStatus{}) {
			_ = p.Store.RecordInboundStatus(scope, m.ConnectionID, st)
		}
		p.mu.Lock()
		delete(p.retry, rkey)
		p.mu.Unlock()
		if conn, err := p.Store.GetInboundConnection(ctx, m.ConnectionID); err == nil {
			count(conn.Kind, o)
		}
	}
	sc, err := p.Store.GetInboundConnection(ctx, m.ConnectionID)
	if errors.Is(err, postgresql.ErrInboundNotFound) {
		giveUp("connection_gone", OutcomeReplyFailed, postgresql.InboundStatus{})
		return
	}
	if err != nil {
		return
	}
	state, err := p.Runs.RunState(ctx, m.RunNamespace, m.RunName)
	switch {
	case err != nil:
		return
	case !state.Found:
		giveUp("run_gone", OutcomeReplyFailed, postgresql.InboundStatus{})
		return
	case !state.Done:
		if p.now().Sub(m.ReceivedAt) > orDur(p.ReplyTimeout, 2*time.Hour) {
			giveUp("run_timeout", OutcomeReplyFailed, postgresql.InboundStatus{})
		}
		return
	}
	c, err := p.Resolve(ctx, sc)
	a, ok := p.Registry.Get(sc.Kind)
	if err != nil || !ok || !sc.Enabled {
		p.backoff(rkey, rt, func() { giveUp("reply_unavailable", OutcomeReplyFailed, postgresql.InboundStatus{}) })
		return
	}
	res := Result{Text: state.Output, Subject: ""}
	switch {
	case !state.Succeeded:
		res = Result{Text: failureText, Failed: true, Reason: state.Reason, Notice: true}
	case strings.TrimSpace(state.Output) == "":
		res = Result{Text: "The agent finished without an answer.", Notice: true}
	}
	res = p.Prepare(c, res)
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = a.Reply(rctx, c, m.ReplyTarget, res)
	cancel()
	if err != nil {
		slog.Warn("delivering an inbound reply failed", "connection", sc.ID, "kind", sc.Kind, "err", safeErr(err, c))
		p.backoff(rkey, rt, func() { giveUp("reply_failed", OutcomeReplyFailed, postgresql.InboundStatus{}) })
		return
	}
	status := postgresql.InboundReplied
	if err := p.Store.UpdateInboundMessage(scope, m.ConnectionID, m.MessageKey, postgresql.InboundUpdate{Status: status, ClearTarget: true}); err != nil {
		slog.Warn("recording an inbound reply", "connection", sc.ID, "err", err)
	}
	_ = p.Store.RecordInboundStatus(scope, m.ConnectionID, postgresql.InboundStatus{Replied: 1})
	p.mu.Lock()
	delete(p.retry, rkey)
	p.mu.Unlock()
	count(sc.Kind, OutcomeReplied)
}

// backoff schedules the next delivery attempt, or gives up after the last.
func (p *Pipeline) backoff(key string, rt replyRetry, giveUp func()) {
	rt.attempts++
	if rt.attempts >= orInt(p.ReplyAttempts, 5) {
		giveUp()
		return
	}
	rt.next = p.now().Add(min(15*time.Second<<rt.attempts, 10*time.Minute))
	p.mu.Lock()
	if p.retry == nil {
		p.retry = map[string]replyRetry{}
	}
	p.retry[key] = rt
	p.mu.Unlock()
}

func orInt(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// Prepare makes a result safe to send: secrets masked (the connection's own and
// anything registered with internal/redact), then cut to the channel's limit.
// Masking comes first so a secret cut in half cannot slip past it.
func (p *Pipeline) Prepare(c Connection, res Result) Result {
	res.Text = redact.String(secretsOf(c).String(res.Text))
	res.Text = truncateReply(res.Text, p.capabilities(c.Kind).MaxReplyChars)
	return res
}
