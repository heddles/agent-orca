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

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// memApprovals is an in-memory ApprovalStore with the same semantics as
// postgresql.Store: one row per (kind, source key), decisions only on pending.
type memApprovals struct {
	rows []postgresql.Approval
}

func (m *memApprovals) RequestApproval(_ context.Context, a postgresql.Approval) (postgresql.Approval, error) {
	for i := range m.rows {
		if m.rows[i].Kind == a.Kind && m.rows[i].SourceKey == a.SourceKey {
			m.rows[i].Details = a.Details
			return m.rows[i], nil
		}
	}
	a.ID = "a" + string(rune('0'+len(m.rows)))
	if a.Status == "" {
		a.Status = postgresql.ApprovalPending
	}
	m.rows = append(m.rows, a)
	a.Created = true
	return a, nil
}

func (m *memApprovals) ListApprovals(_ context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error) {
	var out []postgresql.Approval
	for _, a := range m.rows {
		if (q.Namespace == "" || a.Namespace == q.Namespace) && (q.RunName == "" || a.RunName == q.RunName) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *memApprovals) DecideApproval(_ context.Context, id, status, by, reason string) (postgresql.Approval, error) {
	for i := range m.rows {
		if m.rows[i].ID == id {
			if m.rows[i].Status != postgresql.ApprovalPending {
				return m.rows[i], postgresql.ErrApprovalNotPending
			}
			now := time.Now()
			m.rows[i].Status, m.rows[i].DecidedBy, m.rows[i].Reason, m.rows[i].DecidedAt = status, by, reason, &now
			return m.rows[i], nil
		}
	}
	return postgresql.Approval{}, postgresql.ErrApprovalNotPending
}

func (m *memApprovals) MarkApprovalApplied(_ context.Context, id string) error {
	for i := range m.rows {
		if m.rows[i].ID == id {
			now := time.Now()
			m.rows[i].AppliedAt = &now
		}
	}
	return nil
}

func (m *memApprovals) WithdrawRunApprovals(context.Context, string, string) error { return nil }

func (m *memApprovals) WithdrawBySourcePrefix(_ context.Context, kind, prefix, reason string) error {
	for i := range m.rows {
		if m.rows[i].Kind == kind && strings.HasPrefix(m.rows[i].SourceKey, prefix) && m.rows[i].Status == postgresql.ApprovalPending {
			m.rows[i].Status, m.rows[i].Reason = postgresql.ApprovalWithdrawn, reason
		}
	}
	return nil
}

func chunk(id, host, binary, notes string) osv1.PolicyChunk {
	return osv1.PolicyChunk{
		ID: id, Status: "pending", RuleName: "allow_" + host, Binary: binary, SecurityNotes: notes,
		ReviewToken: "tok-" + id, HitCount: 3,
		ProposedRule: &types.NetworkPolicyRule{
			Endpoints: []types.PolicyNetworkEndpoint{{Host: host, Ports: []uint32{443}}},
			Binaries:  []types.PolicyNetworkBinary{{Path: binary}},
		},
	}
}

func approvalClass() *agentorcav1alpha1.SandboxClass {
	c := coderClass(nil)
	c.Spec.Approval = &agentorcav1alpha1.SandboxApproval{
		ApproverGroups: []string{"platform"},
		Timeout:        &metav1.Duration{Duration: time.Hour},
		AutoApprove: []agentorcav1alpha1.SandboxAutoApproveRule{
			{Host: "*.githubusercontent.com", Binaries: []string{"/usr/bin/curl"}},
		},
	}
	return c
}

func TestReconcileEgressApprovals(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	r, cl := sandboxReconciler(t, gw, run, token, ns, approvalClass())
	store := &memApprovals{}
	r.Approvals = store
	agent := classedAgent()
	sb := &osv1.Sandbox{Name: "sb-1"}

	gw.drafts = map[string][]osv1.PolicyChunk{"sb-1": {
		chunk("c-auto", "objects.githubusercontent.com", "/usr/bin/curl", ""),
		chunk("c-flagged", "raw.githubusercontent.com", "/usr/bin/curl", "requests a write method"),
		chunk("c-manual", "example.com", "/usr/bin/curl", ""),
	}}

	r.reconcileEgressApprovals(context.Background(), run, agent, sb)

	// The matching chunk is approved at once, with its review token, and audited.
	if !slices.Equal(gw.approved, []string{"c-auto@tok-c-auto"}) {
		t.Fatalf("approved at the gateway = %v, want only the auto-approvable chunk", gw.approved)
	}
	byKey := map[string]postgresql.Approval{}
	for _, a := range store.rows {
		byKey[a.SourceKey] = a
	}
	if a := byKey["sb-1/c-auto"]; a.Status != postgresql.ApprovalApproved || !strings.Contains(a.DecidedBy, "auto-approve") || a.AppliedAt == nil {
		t.Errorf("auto approval = %+v", a)
	}
	// A flagged proposal never auto-approves, even when its host matches.
	for _, key := range []string{"sb-1/c-flagged", "sb-1/c-manual"} {
		a := byKey[key]
		if a.Status != postgresql.ApprovalPending || a.ApproverGroups[0] != "platform" || !strings.Contains(a.Summary, "/usr/bin/curl wants to reach") {
			t.Errorf("%s = %+v, want pending for the platform group", key, a)
		}
	}
	got := fetchSandboxRun(t, cl, run)
	if c := meta.FindStatusCondition(got.Status.Conditions, ConditionWaitingForApproval); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("WaitingForApproval = %+v, want True", c)
	}

	// An approver approves one and rejects the other; the next poll applies both.
	if _, err := store.DecideApproval(context.Background(), byKey["sb-1/c-manual"].ID, postgresql.ApprovalApproved, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideApproval(context.Background(), byKey["sb-1/c-flagged"].ID, postgresql.ApprovalRejected, "alice", "no writes"); err != nil {
		t.Fatal(err)
	}
	r.reconcileEgressApprovals(context.Background(), got, agent, sb)
	if !slices.Contains(gw.approved, "c-manual@tok-c-manual") || !slices.Equal(gw.rejected, []string{"c-flagged"}) {
		t.Errorf("gateway approved %v, rejected %v", gw.approved, gw.rejected)
	}
	got = fetchSandboxRun(t, cl, run)
	if c := meta.FindStatusCondition(got.Status.Conditions, ConditionWaitingForApproval); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("WaitingForApproval after the decisions = %+v, want False", c)
	}
}

func TestReconcileEgressApprovalsTimesOut(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	r, _ := sandboxReconciler(t, gw, run, token, ns, approvalClass())
	store := &memApprovals{}
	r.Approvals = store
	sb := &osv1.Sandbox{Name: "sb-1"}
	gw.drafts = map[string][]osv1.PolicyChunk{"sb-1": {chunk("c1", "example.com", "/usr/bin/curl", "")}}

	r.reconcileEgressApprovals(context.Background(), run, classedAgent(), sb)
	store.rows[0].ExpiresAt = time.Now().Add(-time.Minute)
	r.reconcileEgressApprovals(context.Background(), run, classedAgent(), sb)

	if store.rows[0].Status != postgresql.ApprovalExpired || !slices.Equal(gw.rejected, []string{"c1"}) {
		t.Errorf("after the timeout: status %s, gateway rejected %v", store.rows[0].Status, gw.rejected)
	}
}

func TestReconcileEgressApprovalsNeedsClassConfig(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	r, _ := sandboxReconciler(t, gw, run, token, ns, coderClass(nil))
	store := &memApprovals{}
	r.Approvals = store
	gw.drafts = map[string][]osv1.PolicyChunk{"sb-1": {chunk("c1", "example.com", "/usr/bin/curl", "")}}

	r.reconcileEgressApprovals(context.Background(), run, classedAgent(), &osv1.Sandbox{Name: "sb-1"})
	if len(store.rows) != 0 || len(gw.approved)+len(gw.rejected) != 0 {
		t.Error("a class without spec.approval must leave drafts alone (they stay denied)")
	}
}

func TestAutoApprovesMatching(t *testing.T) {
	rules := []agentorcav1alpha1.SandboxAutoApproveRule{
		{Host: "registry.example.com", Ports: []int32{443, 8443}},
		{Host: "*.cdn.example.com", Binaries: []string{"/usr/bin/*"}},
	}
	for name, tc := range map[string]struct {
		ch   osv1.PolicyChunk
		want bool
	}{
		"exact host":                 {chunk("1", "registry.example.com", "/opt/x", ""), true},
		"wildcard host and binary":   {chunk("2", "a.cdn.example.com", "/usr/bin/curl", ""), true},
		"wildcard needs a subdomain": {chunk("3", "cdn.example.com", "/usr/bin/curl", ""), false},
		"binary outside the glob":    {chunk("4", "a.cdn.example.com", "/sandbox/bin/curl", ""), false},
		"unlisted host":              {chunk("5", "example.org", "/usr/bin/curl", ""), false},
		"security notes":             {chunk("6", "registry.example.com", "/usr/bin/curl", "flagged"), false},
	} {
		if got := autoApproves(rules, tc.ch); got != tc.want {
			t.Errorf("%s: autoApproves = %v, want %v", name, got, tc.want)
		}
	}
}

type recordingNotifier struct{ sent []postgresql.Approval }

func (n *recordingNotifier) NotifyApproval(_ context.Context, a postgresql.Approval) error {
	n.sent = append(n.sent, a)
	return nil
}

func TestEgressApprovalsNotifyOncePerNewPendingRequest(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	r, _ := sandboxReconciler(t, gw, run, token, ns, approvalClass())
	r.Approvals = &memApprovals{}
	notifier := &recordingNotifier{}
	r.ApprovalNotifier = notifier
	sb := &osv1.Sandbox{Name: "sb-1"}
	gw.drafts = map[string][]osv1.PolicyChunk{"sb-1": {
		chunk("c-auto", "objects.githubusercontent.com", "/usr/bin/curl", ""),
		chunk("c-manual", "example.com", "/usr/bin/curl", ""),
	}}

	r.reconcileEgressApprovals(context.Background(), run, classedAgent(), sb)
	r.reconcileEgressApprovals(context.Background(), run, classedAgent(), sb) // the draft is still pending

	if len(notifier.sent) != 1 || !strings.Contains(notifier.sent[0].SourceKey, "c-manual") {
		t.Errorf("notified %+v, want exactly one message for the manual request", notifier.sent)
	}
}
