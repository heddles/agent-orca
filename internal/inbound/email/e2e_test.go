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

package email

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// memStore is just enough of the ledger for one end-to-end run.
type memStore struct {
	mu   sync.Mutex
	rows []*postgresql.InboundMessage
	conn postgresql.InboundConnection
}

func (s *memStore) ClaimInboundMessage(_ context.Context, m postgresql.InboundMessage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.MessageKey == m.MessageKey {
			return false, nil
		}
	}
	m.Status, m.ReceivedAt = postgresql.InboundReceived, time.Now()
	s.rows = append(s.rows, &m)
	return true, nil
}

func (s *memStore) UpdateInboundMessage(_ context.Context, _, key string, u postgresql.InboundUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.MessageKey == key {
			r.Status, r.Reason = u.Status, u.Reason
			if u.RunName != "" {
				r.RunNamespace, r.RunName = u.RunNamespace, u.RunName
			}
			if u.ClearTarget {
				r.ReplyTarget = nil
			} else if len(u.ReplyTarget) > 0 {
				r.ReplyTarget = u.ReplyTarget
			}
		}
	}
	return nil
}

func (s *memStore) ListInboundMessagesByStatus(_ context.Context, status string, _ int) ([]postgresql.InboundMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []postgresql.InboundMessage
	for _, r := range s.rows {
		if r.Status == status {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *memStore) LatestInboundRunInThread(_ context.Context, _, thread string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.rows) - 1; i >= 0; i-- {
		if r := s.rows[i]; r.ThreadKey == thread && r.RunName != "" {
			return r.RunNamespace, r.RunName, nil
		}
	}
	return "", "", nil
}

func (s *memStore) CountInboundBySender(context.Context, string, string, time.Time) (int, error) {
	return 0, nil
}

func (s *memStore) RecordInboundStatus(context.Context, string, postgresql.InboundStatus) error {
	return nil
}

func (s *memStore) ListInboundConnections(context.Context, []string, string) ([]postgresql.InboundConnection, error) {
	return []postgresql.InboundConnection{s.conn}, nil
}

func (s *memStore) GetInboundConnection(context.Context, string) (postgresql.InboundConnection, error) {
	return s.conn, nil
}

func (s *memStore) DeleteInboundMessagesBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

type launcher struct {
	mu   sync.Mutex
	reqs []inbound.LaunchRequest
}

func (l *launcher) Launch(_ context.Context, r inbound.LaunchRequest) (inbound.RunRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, r)
	return inbound.RunRef{Namespace: r.Namespace, Name: "run-" + string(rune('0'+len(l.reqs)))}, nil
}

type answers struct{ out string }

func (a answers) RunState(context.Context, string, string) (inbound.RunState, error) {
	return inbound.RunState{Found: true, Done: true, Succeeded: true, Output: a.out}, nil
}

// TestEndToEndThroughTheCore: a mail arrives by IMAP, the core starts a run, the run
// answers, the answer is mailed back in the thread; a reply to that answer
// continues the same conversation; a stranger starts nothing.
func TestEndToEndThroughTheCore(t *testing.T) {
	imapSrv, adapter := startIMAP(t, "tls")
	smtpSrv, sender := startSMTP(t, true)
	adapter.SMTP = sender
	cfg := `{"allowSenders":["alice@example.com"],"adapter":` + imapCfg + `}`
	store := &memStore{conn: postgresql.InboundConnection{
		ID: "inb_" + strings.Repeat("e", 32), TenantID: "t-1", Kind: Kind, Name: "mail", Namespace: "ns", Agent: "support", Enabled: true,
		Config: []byte(cfg), SecretRef: "creds", UpdatedAt: time.Now(),
	}}
	reg := inbound.NewRegistry()
	reg.Register(adapter)
	l := &launcher{}
	p := &inbound.Pipeline{
		Store: store, Launcher: l, Runs: answers{out: "Turn it off and on. Password is app-password-1234."}, Registry: reg,
		Secrets: func(context.Context, string, string) (map[string][]byte, error) {
			return secretFor(imapSrv, smtpSrv, nil), nil
		},
	}
	ctx := context.Background()
	sc, _ := store.GetInboundConnection(ctx, "")
	c, err := p.Resolve(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}

	imapSrv.put(t, goodMail("q1"))
	imapSrv.put(t, raw([]string{"From: Mallory <mallory@evil.example>", "Subject: let me in", "Message-ID: <evil@evil.example>", "Authentication-Results: mx.agents.example; dmarc=pass header.from=evil.example"}, "hi"))
	if err := adapter.PollOnce(ctx, c, p); err != nil {
		t.Fatal(err)
	}
	if len(l.reqs) != 1 || !strings.Contains(l.reqs[0].Input, "Body of q1") || !strings.Contains(l.reqs[0].Input, "Sender: alice@example.com") || l.reqs[0].PriorRun != "" {
		t.Fatalf("launch requests = %+v (the stranger must not start a run)", l.reqs)
	}

	p.DeliverPending(ctx)
	sent := smtpSrv.sent()
	if len(sent) != 1 {
		t.Fatalf("replies sent = %d", len(sent))
	}
	m, plain, _ := parts(t, sent[0])
	if m.Header.Get("To") != aliceAddr || m.Header.Get("Subject") != "Re: Question q1" || m.Header.Get("In-Reply-To") != "<q1@example.com>" {
		t.Errorf("reply headers: %v", m.Header)
	}
	if strings.Contains(plain, "app-password-1234") || !strings.Contains(plain, "[REDACTED]") {
		t.Errorf("the connection's password reached the reply: %q", plain)
	}
	if row := store.rows[0]; row.Status != postgresql.InboundReplied || row.ReplyTarget != nil {
		t.Errorf("ledger = %+v", row)
	}

	// The sender answers our mail; the client sets only In-Reply-To (our message id).
	replyID := strings.Trim(m.Header.Get("Message-ID"), "<>")
	imapSrv.put(t, raw([]string{"From: " + goodFrom, "Subject: Re: Question q1", "Message-ID: <q2@example.com>", "In-Reply-To: <" + replyID + ">", "Authentication-Results: " + authPass},
		"And one more thing.\r\n\r\nOn Mon, Alice wrote:\r\n> old"))
	if err := adapter.PollOnce(ctx, c, p); err != nil {
		t.Fatal(err)
	}
	if len(l.reqs) != 2 || l.reqs[1].PriorRun != "run-1" || !strings.Contains(l.reqs[1].Input, "And one more thing.") || strings.Contains(l.reqs[1].Input, "> old") {
		t.Errorf("the follow-up did not continue the conversation: %+v", l.reqs)
	}
	// Redelivery of the same mail (the mailbox flag was lost) starts nothing.
	imapSrv.put(t, goodMail("q1"))
	if err := adapter.PollOnce(ctx, c, p); err != nil || len(l.reqs) != 2 {
		t.Errorf("a redelivered mail started a run: %v, %d", err, len(l.reqs))
	}
}
