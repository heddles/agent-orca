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
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// Egress approvals. When a sandbox's policy denies a connection to a host it
// does not list, the OpenShell gateway drafts a rule that would allow it. For
// Agents whose SandboxClass sets spec.approval, the operator turns each draft
// into an approval: approved at once if an auto-approve rule matches,
// otherwise left for an approver until it times out. Decisions are applied to
// the live sandbox policy, which takes effect without a restart. Approvals
// are stored in PostgreSQL and are the audit record.

// ConditionWaitingForApproval is True while a run has undecided approvals.
const ConditionWaitingForApproval = "WaitingForApproval"

// defaultApprovalTimeout applies when a SandboxClass sets no approval timeout.
const defaultApprovalTimeout = 30 * time.Minute

// ApprovalStore persists approvals (postgresql.Store).
type ApprovalStore interface {
	RequestApproval(ctx context.Context, a postgresql.Approval) (postgresql.Approval, error)
	ListApprovals(ctx context.Context, q postgresql.ApprovalQuery) ([]postgresql.Approval, error)
	DecideApproval(ctx context.Context, id, status, by, reason string) (postgresql.Approval, error)
	MarkApprovalApplied(ctx context.Context, id string) error
	WithdrawRunApprovals(ctx context.Context, namespace, runName string) error
	WithdrawBySourcePrefix(ctx context.Context, kind, prefix, reason string) error
}

// egressRequest is the approval's details: what the sandbox asked for.
type egressRequest struct {
	Sandbox       string   `json:"sandbox"`
	ChunkID       string   `json:"chunkID"`
	Rule          string   `json:"rule"`
	Hosts         []string `json:"hosts"`
	Binaries      []string `json:"binaries"`
	Rationale     string   `json:"rationale,omitempty"`
	SecurityNotes string   `json:"securityNotes,omitempty"`
	Validation    string   `json:"validation,omitempty"`
	Hits          int32    `json:"hits"`
	LastSeen      string   `json:"lastSeen,omitempty"`
}

func describeChunk(sandbox string, ch osv1.PolicyChunk) egressRequest {
	req := egressRequest{
		Sandbox: sandbox, ChunkID: ch.ID, Rule: ch.RuleName, Rationale: ch.Rationale,
		SecurityNotes: ch.SecurityNotes, Validation: ch.ValidationResult, Hits: ch.HitCount,
	}
	if !ch.LastSeen.IsZero() {
		req.LastSeen = ch.LastSeen.UTC().Format(time.RFC3339)
	}
	if ch.ProposedRule != nil {
		for _, ep := range ch.ProposedRule.Endpoints {
			ports := ep.Ports
			if len(ports) == 0 && ep.Port != 0 {
				ports = []uint32{ep.Port}
			}
			for _, p := range ports {
				req.Hosts = append(req.Hosts, fmt.Sprintf("%s:%d", ep.Host, p))
			}
		}
		for _, b := range ch.ProposedRule.Binaries {
			req.Binaries = append(req.Binaries, b.Path)
		}
	}
	if len(req.Binaries) == 0 && ch.Binary != "" {
		req.Binaries = []string{ch.Binary}
	}
	return req
}

func (e egressRequest) summary() string {
	who := "the agent"
	if len(e.Binaries) > 0 {
		who = strings.Join(e.Binaries, ", ")
	}
	return fmt.Sprintf("%s wants to reach %s", who, strings.Join(e.Hosts, ", "))
}

// autoApproves reports whether a rule allows every endpoint and binary of a
// proposal. Proposals with security notes never match.
func autoApproves(rules []agentorcav1alpha1.SandboxAutoApproveRule, ch osv1.PolicyChunk) bool {
	if ch.SecurityNotes != "" || ch.ProposedRule == nil || len(ch.ProposedRule.Endpoints) == 0 {
		return false
	}
	binaries := []string{}
	for _, b := range ch.ProposedRule.Binaries {
		binaries = append(binaries, b.Path)
	}
	if len(binaries) == 0 && ch.Binary != "" {
		binaries = []string{ch.Binary}
	}
	for _, ep := range ch.ProposedRule.Endpoints {
		ports := ep.Ports
		if len(ports) == 0 {
			ports = []uint32{ep.Port}
		}
		matched := false
		for _, r := range rules {
			if hostMatches(r.Host, ep.Host) && portsAllowed(r.Ports, ports) && binariesAllowed(r.Binaries, binaries) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func hostMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return pattern == host
}

func portsAllowed(allowed []int32, ports []uint32) bool {
	if len(allowed) == 0 {
		allowed = []int32{443}
	}
	for _, p := range ports {
		if !slices.Contains(allowed, int32(p)) {
			return false
		}
	}
	return true
}

func binariesAllowed(allowed, binaries []string) bool {
	if len(allowed) == 0 {
		return true
	}
	if len(binaries) == 0 {
		return false
	}
	for _, b := range binaries {
		ok := false
		for _, a := range allowed {
			if m, _ := path.Match(a, b); m || a == b {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// reconcileEgressApprovals turns the sandbox's pending policy drafts into
// approvals and applies decided ones. Failures are logged: the run goes on,
// and denied traffic stays denied until the next poll.
func (r *AgentRunReconciler) reconcileEgressApprovals(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent, sb *osv1.Sandbox) {
	if r.Approvals == nil || sb == nil || sb.Name == "" {
		return
	}
	class, err := r.sandboxClass(ctx, run, agent)
	if err != nil || class == nil || class.Spec.Approval == nil {
		return
	}
	chunks, err := r.Sandboxes.PendingDrafts(ctx, sb.Name)
	if err != nil {
		log.FromContext(ctx).Error(err, "reading sandbox policy drafts", "sandbox", sb.Name)
		return
	}
	pending := map[string]osv1.PolicyChunk{}
	for _, ch := range chunks {
		pending[ch.ID] = ch
		r.recordEgressDraft(ctx, run, class, sb.Name, ch)
	}
	waiting, err := r.applyEgressDecisions(ctx, run, sb.Name, pending)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing the run's approvals")
		return
	}
	r.setWaitingForApproval(ctx, run, waiting)
}

// recordEgressDraft stores a draft as an approval: approved at once when an
// auto-approve rule matches, else pending, notifying approvers the first time.
func (r *AgentRunReconciler) recordEgressDraft(ctx context.Context, run *agentorcav1alpha1.AgentRun, class *agentorcav1alpha1.SandboxClass, sandbox string, ch osv1.PolicyChunk) {
	cfg := class.Spec.Approval
	timeout := defaultApprovalTimeout
	if cfg.Timeout != nil && cfg.Timeout.Duration > 0 {
		timeout = cfg.Timeout.Duration
	}
	req := describeChunk(sandbox, ch)
	details, _ := json.Marshal(req)
	a := postgresql.Approval{
		Tenant:         run.Labels["agentorca.io/tenant"],
		Namespace:      run.Namespace,
		RunName:        run.Name,
		Kind:           postgresql.ApprovalKindEgress,
		SourceKey:      sandbox + "/" + ch.ID,
		Summary:        req.summary(),
		Details:        details,
		ApproverGroups: cfg.ApproverGroups,
		ExpiresAt:      time.Now().Add(timeout),
	}
	if autoApproves(cfg.AutoApprove, ch) {
		now := time.Now()
		a.Status, a.DecidedBy, a.DecidedAt = postgresql.ApprovalApproved, "auto-approve (SandboxClass "+class.Name+")", &now
		a.Reason = "matched an auto-approve rule"
	}
	logger := log.FromContext(ctx)
	stored, err := r.Approvals.RequestApproval(ctx, a)
	if err != nil {
		logger.Error(err, "recording egress approval", "sandbox", sandbox, "chunk", ch.ID)
		return
	}
	if stored.Created && stored.Status == postgresql.ApprovalPending && r.ApprovalNotifier != nil {
		if err := r.ApprovalNotifier.NotifyApproval(ctx, stored); err != nil {
			logger.Error(err, "notifying approvers", "approval", stored.ID)
		}
	}
}

// applyEgressDecisions applies the run's decided, unapplied approvals to the
// sandbox's drafts, expiring overdue ones, and returns how many still wait.
func (r *AgentRunReconciler) applyEgressDecisions(ctx context.Context, run *agentorcav1alpha1.AgentRun, sandbox string, pending map[string]osv1.PolicyChunk) (int, error) {
	approvals, err := r.Approvals.ListApprovals(ctx, postgresql.ApprovalQuery{Namespace: run.Namespace, RunName: run.Name})
	if err != nil {
		return 0, err
	}
	logger := log.FromContext(ctx)
	waiting := 0
	for _, a := range approvals {
		if a.Kind != postgresql.ApprovalKindEgress || a.AppliedAt != nil || !strings.HasPrefix(a.SourceKey, sandbox+"/") {
			continue
		}
		if a.Status == postgresql.ApprovalPending && time.Now().After(a.ExpiresAt) {
			if decided, err := r.Approvals.DecideApproval(ctx, a.ID, postgresql.ApprovalExpired, "", "no decision within the approval timeout"); err == nil {
				a = decided
			}
		}
		if a.Status == postgresql.ApprovalPending {
			waiting++
			continue
		}
		if ch, open := pending[strings.TrimPrefix(a.SourceKey, sandbox+"/")]; open {
			if err := r.applyEgressDecision(ctx, sandbox, ch, a); err != nil {
				logger.Error(err, "applying an egress decision", "approval", a.ID)
				continue
			}
		}
		if err := r.Approvals.MarkApprovalApplied(ctx, a.ID); err != nil {
			logger.Error(err, "marking an egress decision applied", "approval", a.ID)
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeNormal, "EgressApproval", "%s: %s (%s)", a.Status, a.Summary, a.DecidedBy)
		}
	}
	return waiting, nil
}

// applyEgressDecision approves or declines one draft at the gateway.
func (r *AgentRunReconciler) applyEgressDecision(ctx context.Context, sandbox string, ch osv1.PolicyChunk, a postgresql.Approval) error {
	if a.Status == postgresql.ApprovalApproved {
		return r.Sandboxes.ApproveDraft(ctx, sandbox, ch.ID, ch.ReviewToken)
	}
	reason := a.Reason
	if reason == "" {
		reason = a.Status
	}
	return r.Sandboxes.RejectDraft(ctx, sandbox, ch.ID, reason)
}

// setWaitingForApproval keeps the run's WaitingForApproval condition current.
func (r *AgentRunReconciler) setWaitingForApproval(ctx context.Context, run *agentorcav1alpha1.AgentRun, waiting int) {
	cond := metav1.Condition{
		Type:    ConditionWaitingForApproval,
		Status:  metav1.ConditionFalse,
		Reason:  "NoPendingApprovals",
		Message: "no approvals pending",
	}
	if waiting > 0 {
		cond.Status, cond.Reason = metav1.ConditionTrue, "EgressApprovalPending"
		cond.Message = fmt.Sprintf("%d egress request(s) waiting for an approver", waiting)
	}
	existing := meta.FindStatusCondition(run.Status.Conditions, ConditionWaitingForApproval)
	if existing == nil && waiting == 0 {
		return
	}
	if existing != nil && existing.Status == cond.Status && existing.Message == cond.Message {
		return
	}
	patch := client.MergeFrom(run.DeepCopy())
	meta.SetStatusCondition(&run.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		log.FromContext(ctx).Error(err, "updating the WaitingForApproval condition")
	}
}
