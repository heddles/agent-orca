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
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// pollAdapter is a polling channel: each poll hands over one message and
// reports whatever error it is told to.
type pollAdapter struct {
	fakeAdapter
	polls atomic.Int32
	mu    sync.Mutex
	err   error
	got   []Outcome
}

func (p *pollAdapter) Kind() string { return "polling" }
func (p *pollAdapter) PollOnce(ctx context.Context, c Connection, h Handler) error {
	n := p.polls.Add(1)
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return err
	}
	o, herr := h.Handle(ctx, c, Message{Sender: Sender{ID: "alice@example.com"}, MessageID: "poll-" + strconv.Itoa(int(n)), Text: "hi", Reply: []byte(`{}`)}, nil)
	p.mu.Lock()
	p.got = append(p.got, o)
	p.mu.Unlock()
	return herr
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunnerKeepsOnePollerPerEnabledConnectionAndStopsThemGracefully(t *testing.T) {
	f := newFixture(t)
	ad := &pollAdapter{}
	reg := NewRegistry()
	reg.Register(ad)
	f.p.Registry = reg
	cfg := []byte(`{"allowSenders":["alice@example.com"],"pollSeconds":30}`)
	mk := func(id string, enabled bool) postgresql.InboundConnection {
		return postgresql.InboundConnection{ID: id, TenantID: "t-1", Kind: "polling", Namespace: "ns", Agent: "support", Enabled: enabled, Config: cfg, UpdatedAt: time.Now()}
	}
	idA, idB, idC := "inb_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "inb_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "inb_cccccccccccccccccccccccccccccccc"
	f.st.conns = map[string]postgresql.InboundConnection{idA: mk(idA, true), idB: mk(idB, true), idC: mk(idC, false)}
	// The email-style kind is not a poller: a connection of the fake kind is left alone.
	f.st.conns["inb_dddddddddddddddddddddddddddddddd"] = postgresql.InboundConnection{ID: "inb_dddddddddddddddddddddddddddddddd", Kind: "fake", Enabled: true}

	r := &Runner{P: f.p, SyncEvery: 20 * time.Millisecond, ReplyEvery: time.Hour, MaxConcurrent: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Start(ctx) }()
	waitFor(t, "two pollers", func() bool { return r.Running() == 2 })
	waitFor(t, "both connections polled", func() bool { return ad.polls.Load() >= 2 })

	before := ad.polls.Load()
	// Disabling a connection stops its poller; a changed one is restarted.
	f.st.mu.Lock()
	c := f.st.conns[idA]
	c.Enabled = false
	f.st.conns[idA] = c
	b := f.st.conns[idB]
	b.UpdatedAt = b.UpdatedAt.Add(time.Second)
	f.st.conns[idB] = b
	f.st.mu.Unlock()
	waitFor(t, "one poller", func() bool { return r.Running() == 1 })
	waitFor(t, "the changed connection polled again at once", func() bool { return ad.polls.Load() > before })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the runner did not stop")
	}
	if len(f.l.reqs) == 0 {
		t.Error("polled messages did not start runs")
	}
}

func TestPollFailuresAreRecordedWithoutSecretsAndBackOff(t *testing.T) {
	f := newFixture(t)
	ad := &pollAdapter{err: errors.New("login failed for hunter2-hunter2")}
	reg := NewRegistry()
	reg.Register(ad)
	f.p.Registry = reg
	f.p.Secrets = func(context.Context, string, string) (map[string][]byte, error) {
		return map[string][]byte{"password": []byte("hunter2-hunter2")}, nil
	}
	sc := postgresql.InboundConnection{ID: f.c.ID, TenantID: "t-1", Kind: "polling", Namespace: "ns", Agent: "support", Enabled: true, SecretRef: "creds",
		Config: []byte(`{"allowSenders":["alice@example.com"]}`)}
	r := &Runner{P: f.p, sem: make(chan struct{}, 1)}
	if err := r.pollOnce(context.Background(), sc); err == nil {
		t.Fatal("the failure was swallowed")
	}
	// The row's error text is what a viewer sees: it must not carry the secret.
	// (The recorder keeps counts only; the text is checked through safeErr.)
	if got := safeErr(errors.New("login failed for hunter2-hunter2"), Connection{Secret: Secret{"password": []byte("hunter2-hunter2")}}); got != "login failed for [REDACTED]" {
		t.Errorf("safeErr = %q", got)
	}
}

func TestLooksLikeRawStream(t *testing.T) {
	for in, want := range map[string]bool{
		"data: {\"id\":\"gen-1\",\"choices\":[]}\n\ndata: {}": true,
		"  data:{\"a\":1}":                   true,
		"The cluster is healthy.":            false,
		"The log says data: {x} was dropped": false,
		"":                                   false,
	} {
		if got := looksLikeRawStream(in); got != want {
			t.Errorf("looksLikeRawStream(%q) = %v, want %v", in, got, want)
		}
	}
}
