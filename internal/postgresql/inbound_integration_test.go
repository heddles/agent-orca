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

package postgresql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newInbound(t *testing.T, ns, name string) InboundConnection {
	t.Helper()
	id, err := NewInboundID()
	if err != nil {
		t.Fatal(err)
	}
	return InboundConnection{ID: id, Kind: "email", Name: name, Namespace: ns, Agent: "support", SecretRef: "mail-creds",
		Config: []byte(`{"allowSenders":["a@example.com"]}`), CreatedBy: evalUser}
}

//nolint:gocyclo // one scenario, many checks
func TestInboundConnectionsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)

	c := newInbound(t, evalNSA, "mail")
	got, err := s.CreateInboundConnection(a, c)
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != tenantA || !got.Enabled || got.SecretRef != "mail-creds" || !strings.Contains(string(got.Config), "allowSenders") {
		t.Fatalf("created = %+v", got)
	}
	if _, err := s.CreateInboundConnection(a, newInbound(t, evalNSA, "mail")); !errors.Is(err, ErrInboundExists) {
		t.Errorf("duplicate = %v", err)
	}

	// Another tenant sees none of it, by any path.
	if _, err := s.GetInboundConnection(b, c.ID); !errors.Is(err, ErrInboundNotFound) {
		t.Errorf("other tenant get = %v", err)
	}
	if list, err := s.ListInboundConnections(b, nil, ""); err != nil || len(list) != 0 {
		t.Errorf("other tenant list = %v, %v", list, err)
	}
	off := false
	if _, err := s.UpdateInboundConnection(b, c.ID, InboundPatch{Enabled: &off}); !errors.Is(err, ErrInboundNotFound) {
		t.Errorf("other tenant update = %v", err)
	}
	if _, err := s.DeleteInboundConnection(b, c.ID); !errors.Is(err, ErrInboundNotFound) {
		t.Errorf("other tenant delete = %v", err)
	}
	other := newInbound(t, evalNSB, "x")
	other.TenantID = tenantA
	if _, err := s.CreateInboundConnection(b, other); err == nil {
		t.Error("a row of another tenant was accepted")
	}
	if _, err := s.GetInboundConnection(context.Background(), c.ID); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped get = %v", err)
	}
	if p, err := s.GetInboundConnection(WithPlatform(context.Background()), c.ID); err != nil || p.TenantID != tenantA {
		t.Errorf("platform get = %+v, %v", p, err)
	}
	if all, err := s.ListInboundConnections(WithPlatform(context.Background()), nil, ""); err != nil || len(all) != 1 {
		t.Errorf("platform list = %v, %v", all, err)
	}

	// Update keeps what it is not asked to change, and validates the result.
	cfg := []byte(`{"allowGroups":["corp.example"]}`)
	raw := json.RawMessage(cfg)
	u, err := s.UpdateInboundConnection(a, c.ID, InboundPatch{Enabled: &off, Config: &raw})
	if err != nil || u.Enabled || u.SecretRef != "mail-creds" || !strings.Contains(string(u.Config), "corp.example") {
		t.Fatalf("update = %+v, %v", u, err)
	}
	badRef := "Not A Secret!"
	if _, err := s.UpdateInboundConnection(a, c.ID, InboundPatch{SecretRef: &badRef}); !errors.Is(err, ErrInvalidInbound) {
		t.Errorf("bad secret ref = %v", err)
	}

	now := time.Now()
	if err := s.RecordInboundStatus(a, c.ID, InboundStatus{PolledAt: &now, SetError: true, LastError: "boom", Received: 2, Replied: 1, Rejected: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordInboundStatus(a, c.ID, InboundStatus{Received: 1}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.GetInboundConnection(a, c.ID)
	if st.LastError != "boom" || st.LastPollAt == nil || st.ReceivedCount != 3 || st.RepliedCount != 1 || st.RejectedCount != 1 {
		t.Errorf("status = %+v", st)
	}
	if err := s.RecordInboundStatus(a, c.ID, InboundStatus{SetError: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.GetInboundConnection(a, c.ID); st.LastError != "" {
		t.Errorf("error not cleared: %q", st.LastError)
	}
	if _, err := s.DeleteInboundConnection(a, c.ID); err != nil {
		t.Fatal(err)
	}
}

//nolint:gocyclo // one scenario, many checks
func TestInboundMessagesAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	// raw is the superuser, for setting up and inspecting rows the app role may not see.
	raw, err := sql.Open("pgx", os.Getenv("AGENTORCA_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)
	plat := WithPlatform(context.Background())
	const conn = "inb_00000000000000000000000000000001"

	m := InboundMessage{TenantID: tenantA, ConnectionID: conn, MessageKey: HashKey("m1"), ThreadKey: HashKey("t1"), SenderHash: HashKey("alice"), SizeBytes: 42}
	if ok, err := s.ClaimInboundMessage(a, m); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	// A redelivery is a duplicate; the same key under another tenant is another message.
	if ok, err := s.ClaimInboundMessage(a, m); err != nil || ok {
		t.Errorf("duplicate claim = %v, %v", ok, err)
	}
	if ok, err := s.ClaimInboundMessage(b, InboundMessage{ConnectionID: conn, MessageKey: m.MessageKey}); err != nil || !ok {
		t.Errorf("same key, other tenant = %v, %v", ok, err)
	}
	if _, err := s.ClaimInboundMessage(a, InboundMessage{TenantID: tenantB, ConnectionID: conn, MessageKey: "x"}); err == nil {
		t.Error("a message of another tenant was accepted")
	}
	if _, err := s.ClaimInboundMessage(context.Background(), m); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped claim = %v", err)
	}
	// The unique constraint is the backstop under the ON CONFLICT.
	if _, err := raw.Exec(`INSERT INTO inbound_messages (tenant_id, connection_id, message_key, status) VALUES ($1, $2, $3, 'received')`, tenantA, conn, m.MessageKey); err == nil {
		t.Error("a duplicate row was inserted")
	}

	// A claim that never started a run can be taken again once it is stale, not before.
	if _, err := raw.Exec(`UPDATE inbound_messages SET updated_at = now() - interval '10 minutes' WHERE tenant_id = $1`, tenantA); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimInboundMessage(a, m); err != nil || !ok {
		t.Errorf("stale claim = %v, %v", ok, err)
	}
	if ok, _ := s.ClaimInboundMessage(a, m); ok {
		t.Error("a fresh claim was taken again")
	}

	// Running: the target is kept while pending, and cleared when done.
	if err := s.UpdateInboundMessage(a, conn, m.MessageKey, InboundUpdate{Status: InboundRunning, RunNamespace: "ns", RunName: itRun, ReplyTarget: []byte(`{"to":"a"}`)}); err != nil {
		t.Fatal(err)
	}
	pend, err := s.ListInboundMessagesByStatus(plat, InboundRunning, 10)
	if err != nil || len(pend) != 1 || pend[0].RunName != itRun || string(pend[0].ReplyTarget) != `{"to": "a"}` {
		t.Fatalf("pending = %+v, %v", pend, err)
	}
	if other, _ := s.ListInboundMessagesByStatus(b, InboundRunning, 10); len(other) != 0 {
		t.Errorf("tenant B lists %+v", other)
	}
	ns, name, err := s.LatestInboundRunInThread(a, conn, m.ThreadKey)
	if err != nil || ns != "ns" || name != itRun {
		t.Errorf("thread = %q %q %v", ns, name, err)
	}
	if _, name, _ := s.LatestInboundRunInThread(b, conn, m.ThreadKey); name != "" {
		t.Errorf("tenant B found the thread: %q", name)
	}
	if _, name, _ := s.LatestInboundRunInThread(a, conn, ""); name != "" {
		t.Error("an empty thread key matched")
	}
	if got, ok, err := s.FindRunningInboundMessage(a, "ns", itRun); err != nil || !ok || got.MessageKey != m.MessageKey || string(got.ReplyTarget) != `{"to": "a"}` {
		t.Errorf("running message = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := s.FindRunningInboundMessage(b, "ns", itRun); ok {
		t.Error("tenant B found the running message")
	}
	if _, ok, _ := s.FindRunningInboundMessage(a, "ns", "no-such-run"); ok {
		t.Error("found a message of no run")
	}
	n, err := s.CountInboundBySender(a, conn, m.SenderHash, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Errorf("sender count = %d, %v", n, err)
	}
	if n, _ := s.CountInboundBySender(b, conn, m.SenderHash, time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("tenant B counts %d", n)
	}
	if err := s.UpdateInboundMessage(a, conn, m.MessageKey, InboundUpdate{Status: InboundReplied, ClearTarget: true}); err != nil {
		t.Fatal(err)
	}
	var target *string
	if err := raw.QueryRow(`SELECT reply_target::text FROM inbound_messages WHERE tenant_id = $1`, tenantA).Scan(&target); err != nil || target != nil {
		t.Errorf("reply target after reply = %v, %v", target, err)
	}
	// The ledger has nowhere to put content: its columns are digests and sizes.
	var cols string
	_ = raw.QueryRow(`SELECT string_agg(column_name, ',') FROM information_schema.columns WHERE table_name = 'inbound_messages'`).Scan(&cols)
	for _, bad := range []string{"body", "text", "subject", "sender,", "address"} {
		if strings.Contains(cols, bad) {
			t.Errorf("inbound_messages has a %q column: %s", bad, cols)
		}
	}

	// Pruning keeps pending rows and removes old finished ones.
	if _, err := raw.Exec(`UPDATE inbound_messages SET received_at = now() - interval '40 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInboundMessage(a, InboundMessage{ConnectionID: conn, MessageKey: "pending-old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE inbound_messages SET received_at = now() - interval '40 days' WHERE message_key = 'pending-old'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DeleteInboundMessagesBefore(plat, time.Now().Add(-30*24*time.Hour)); err != nil || got != 1 {
		t.Errorf("pruned %d, %v, want only the finished row", got, err)
	}
	var left int
	_ = raw.QueryRow(`SELECT count(*) FROM inbound_messages`).Scan(&left)
	if left != 2 {
		t.Errorf("rows left = %d, want the two pending ones", left)
	}

	// Deleting a connection takes its ledger with it.
	c := newInbound(t, evalNSA, "mail")
	c.ID = conn
	if _, err := s.CreateInboundConnection(a, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteInboundConnection(a, conn); err != nil {
		t.Fatal(err)
	}
	_ = raw.QueryRow(`SELECT count(*) FROM inbound_messages WHERE tenant_id = $1`, tenantA).Scan(&left)
	if left != 0 {
		t.Errorf("ledger rows left after delete = %d", left)
	}
}

// The refusal counts say why a channel is turning messages away, per tenant.
func TestInboundRefusalsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)
	const conn, other = "inb_00000000000000000000000000000002", "inb_00000000000000000000000000000003"
	for i, m := range []struct{ conn, status, reason string }{
		{conn, InboundRejected, "sender_not_allowed"},
		{conn, InboundRejected, "sender_not_allowed"},
		{conn, InboundRejected, "channel_not_allowed"},
		{conn, InboundIgnored, "workspace_mismatch"},
		{conn, InboundIgnored, "bot_message"},
		{conn, InboundReplied, ""},
		{other, InboundRejected, "sender_rate"},
	} {
		key := HashKey(fmt.Sprint(i))
		if ok, err := s.ClaimInboundMessage(a, InboundMessage{ConnectionID: m.conn, MessageKey: key}); err != nil || !ok {
			t.Fatalf("claim %d = %v, %v", i, ok, err)
		}
		if err := s.UpdateInboundMessage(a, m.conn, key, InboundUpdate{Status: m.status, Reason: m.reason}); err != nil {
			t.Fatal(err)
		}
	}
	hourAgo := time.Now().Add(-time.Hour)
	got, err := s.InboundRefusals(a, []string{conn}, hourAgo)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{conn: {"sender_not_allowed": 2, "channel_not_allowed": 1, "workspace_mismatch": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refusals = %v, want %v", got, want)
	}
	if got, err := s.InboundRefusals(b, []string{conn, other}, hourAgo); err != nil || len(got) != 0 {
		t.Errorf("another tenant sees %v, %v", got, err)
	}
	if got, err := s.InboundRefusals(a, []string{conn}, time.Now().Add(time.Hour)); err != nil || len(got) != 0 {
		t.Errorf("outside the window: %v, %v", got, err)
	}
}
