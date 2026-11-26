package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

type fakeApprovals struct {
	mu      sync.Mutex
	pending []postgresql.Approval
	decided []string
	err     error
}

func (f *fakeApprovals) ListApprovals(_ context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []postgresql.Approval
	for _, a := range f.pending {
		if (q.RunName == "" || a.RunName == q.RunName) && a.Namespace == q.Namespace && a.Status == q.Status {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeApprovals) DecideApproval(_ context.Context, id, status, by, _ string) (postgresql.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return postgresql.Approval{}, f.err
	}
	f.decided = append(f.decided, id+" "+status+" "+by)
	for i := range f.pending {
		if f.pending[i].ID == id {
			f.pending[i].Status = status
		}
	}
	return postgresql.Approval{ID: id, Status: status}, nil
}

func toolApproval(id string, groups ...string) postgresql.Approval {
	d, _ := json.Marshal(map[string]string{"tool": "restart-node", "sideEffect": "write", "arguments": `{"node":"n1"}`})
	return postgresql.Approval{ID: id, Namespace: "ns", RunName: "run-b", Kind: postgresql.ApprovalKindTool, Status: postgresql.ApprovalPending,
		Summary: `run-b wants to run the write tool "restart-node"`, Details: d, ApproverGroups: groups, ExpiresAt: time.Now().Add(time.Hour)}
}

// approvalFixture is a thread whose run waits on tool approvals.
func approvalFixture(t *testing.T, approvers []string, as ...postgresql.Approval) (*fixture, *fakeApprovals) {
	t.Helper()
	f := newFixture(t)
	f.c.Settings.AllowSenders = append(f.c.Settings.AllowSenders, "bob@example.com")
	f.c.Settings.Approvers = approvers
	cfg, _ := json.Marshal(map[string]any{"allowSenders": []string{"alice@example.com", "bob@example.com"}, "approvers": approvers})
	f.st.conns[f.c.ID] = postgresql.InboundConnection{ID: f.c.ID, TenantID: "t-1", Kind: "fake", Namespace: "ns", Agent: "support", Enabled: true, Config: cfg}
	fa := &fakeApprovals{pending: as}
	f.p.Approvals = fa
	f.handle(msg("m1", "alice@example.com", "example.com"), nil) // starts run-b
	return f, fa
}

func reply(f *fixture, from, text string) Outcome {
	f.t.Helper()
	m := msg(fmt.Sprintf("m-%d", len(f.st.msgs)), from, "example.com")
	m.Text = text
	return f.handle(m, nil)
}

func lastReply(f *fixture) string {
	if len(f.ad.replies) == 0 {
		return ""
	}
	return f.ad.replies[len(f.ad.replies)-1].Text
}

func TestParseDecision(t *testing.T) {
	for _, c := range []struct {
		in      string
		approve bool
		id      string
		ok      bool
	}{
		{"approve", true, "", true},
		{"  Approve. ", true, "", true},
		{"yes", true, "", true},
		{"deny", false, "", true},
		{"No!", false, "", true},
		{"approve ab12cd34", true, "ab12cd34", true},
		{"deny `ab12cd34`", false, "ab12cd34", true},
		{"approve it please", false, "", false},
		{"yes please restart the staging cluster", false, "", false},
		{"approve ab", false, "", false},
		{"", false, "", false},
		{"restart it", false, "", false},
	} {
		a, id, ok := parseDecision(c.in)
		if a != c.approve || id != c.id || ok != c.ok {
			t.Errorf("parseDecision(%q) = %v %q %v", c.in, a, id, ok)
		}
	}
}

func TestAnApproverDecidesInTheThread(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"}, toolApproval("11111111-aaaa"))
	if o := reply(f, "alice@example.com", "approve"); o != OutcomeDecided {
		t.Fatalf("outcome = %v", o)
	}
	if len(fa.decided) != 1 || fa.decided[0] != "11111111-aaaa approved channel:fake:alice@example.com" {
		t.Errorf("decisions = %v", fa.decided)
	}
	if !strings.HasPrefix(lastReply(f), "Approved:") {
		t.Errorf("reply = %q", lastReply(f))
	}
	if len(f.l.reqs) != 1 {
		t.Errorf("a decision started a run: %d launches", len(f.l.reqs))
	}
	// The same words again: nothing waits any more, so it is an ordinary message.
	if o := reply(f, "alice@example.com", "approve"); o != OutcomeStarted {
		t.Errorf("second approve = %v", o)
	}
}

func TestADenialIsRecorded(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"}, toolApproval("11111111-aaaa"))
	reply(f, "alice@example.com", "deny")
	if len(fa.decided) != 1 || !strings.Contains(fa.decided[0], postgresql.ApprovalRejected) || !strings.HasPrefix(lastReply(f), "Denied:") {
		t.Errorf("decisions = %v, reply %q", fa.decided, lastReply(f))
	}
}

func TestOnlyAnApproverMayDecide(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"}, toolApproval("11111111-aaaa"))
	if o := reply(f, "bob@example.com", "approve"); o != OutcomeDecided {
		t.Fatalf("outcome = %v", o)
	}
	if len(fa.decided) != 0 {
		t.Fatalf("a sender who is not an approver decided: %v", fa.decided)
	}
	if !strings.Contains(lastReply(f), "not set up") {
		t.Errorf("reply = %q", lastReply(f))
	}
	// With no approvers at all, nobody decides from the channel.
	f2, fa2 := approvalFixture(t, nil, toolApproval("11111111-aaaa"))
	reply(f2, "alice@example.com", "approve")
	if len(fa2.decided) != 0 {
		t.Errorf("decided with no approvers: %v", fa2.decided)
	}
}

func TestAGroupLimitedRequestIsDecidedInTheConsole(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"}, toolApproval("11111111-aaaa", "sre"))
	reply(f, "alice@example.com", "approve")
	if len(fa.decided) != 0 || !strings.Contains(lastReply(f), "console") {
		t.Errorf("decisions %v, reply %q", fa.decided, lastReply(f))
	}
}

func TestSeveralWaitingNeedAnId(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"}, toolApproval("11111111-aaaa"), toolApproval("22222222-bbbb"))
	reply(f, "alice@example.com", "approve")
	if len(fa.decided) != 0 || !strings.Contains(lastReply(f), "11111111") || !strings.Contains(lastReply(f), "22222222") {
		t.Errorf("decisions %v, reply %q", fa.decided, lastReply(f))
	}
	reply(f, "alice@example.com", "deny 2222")
	if len(fa.decided) != 1 || !strings.HasPrefix(fa.decided[0], "22222222-bbbb rejected") {
		t.Errorf("decisions = %v", fa.decided)
	}
}

func TestAnApprovalDecidedElsewhereIsReportedAsGone(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"}, toolApproval("11111111-aaaa"))
	fa.err = postgresql.ErrApprovalNotPending
	reply(f, "alice@example.com", "approve")
	if !strings.Contains(lastReply(f), "no longer waiting") {
		t.Errorf("reply = %q", lastReply(f))
	}
}

func TestPlainWordsWithNothingWaitingAreAnOrdinaryMessage(t *testing.T) {
	f, fa := approvalFixture(t, []string{"alice@example.com"})
	if o := reply(f, "alice@example.com", "yes"); o != OutcomeStarted || len(fa.decided) != 0 {
		t.Errorf("outcome %v, decisions %v", o, fa.decided)
	}
}

func TestTheThreadIsToldAToolCallWaits(t *testing.T) {
	f, _ := approvalFixture(t, []string{"alice@example.com"})
	a := toolApproval("11111111-aaaa")
	if err := f.p.NotifyApproval(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	got := lastReply(f)
	for _, want := range []string{"restart-node", "write", `{"node":"n1"}`, "approve", "11111111"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q: %q", want, got)
		}
	}
	// Without approvers the prompt does not invite a reply that would be refused.
	f.c.Settings.Approvers = nil
	f.st.conns[f.c.ID] = postgresql.InboundConnection{ID: f.c.ID, TenantID: "t-1", Kind: "fake", Namespace: "ns", Agent: "support", Enabled: true, Config: []byte(`{"allowSenders":["alice@example.com"]}`)}
	if err := f.p.NotifyApproval(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := lastReply(f); !strings.Contains(got, "console") || strings.Contains(got, `reply "approve"`) {
		t.Errorf("prompt = %q", got)
	}
	// Other kinds of approval are not asked in a thread.
	n := len(f.ad.replies)
	b := toolApproval("33333333-cccc")
	b.Kind = postgresql.ApprovalKindBudget
	_ = f.p.NotifyApproval(context.Background(), b)
	if len(f.ad.replies) != n {
		t.Error("a budget approval was asked in the thread")
	}
}
