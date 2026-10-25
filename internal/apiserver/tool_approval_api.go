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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// Tool-call approvals (Tool.spec.sideEffect, GuardrailPolicy.spec.toolApproval).
// The model-router asks here before running a tool whose class needs an
// approver, and polls for the decision:
//
//	POST /agentrun/{ns}/{name}/tool-approval       {"callKey","tool","sideEffect","arguments"}
//	GET  /agentrun/{ns}/{name}/tool-approval/{id}  → {"id","status","decidedBy","reason"}
//
// The approval goes through the approval engine like any other (inbox,
// notification, audit row). Who may decide and how long the request waits come
// from the run's GuardrailPolicy, read here; nothing in the router's request can
// widen them. A request nobody decides in time expires when it is next read.

// defaultToolApprovalTimeout applies when the policy sets none.
const defaultToolApprovalTimeout = 30 * time.Minute

// maxToolApprovalArgs caps the arguments stored for the approver to read.
const maxToolApprovalArgs = 4000

var callKeyPattern = regexp.MustCompile(`^[a-f0-9]{8,64}$`)

// ToolApprovalStore is the part of the approval engine this API uses
// (postgresql.Store).
type ToolApprovalStore interface {
	RequestApproval(ctx context.Context, a postgresql.Approval) (postgresql.Approval, error)
	GetApproval(ctx context.Context, id string) (postgresql.Approval, error)
	DecideApproval(ctx context.Context, id, status, by, reason string) (postgresql.Approval, error)
}

// ApprovalNotifier tells approvers about a new pending approval.
type ApprovalNotifier interface {
	NotifyApproval(ctx context.Context, a postgresql.Approval) error
}

// WithApprovals gives the server the approval store (nil leaves tool approvals
// off) and an optional notifier. It returns s for chaining.
func (s *Server) WithApprovals(store ToolApprovalStore, notifier ApprovalNotifier) *Server {
	s.approvals, s.approvalNotifier = store, notifier
	return s
}

// toolApprovalRequest is the body of POST .../tool-approval.
type toolApprovalRequest struct {
	CallKey    string `json:"callKey"`
	Tool       string `json:"tool"`
	SideEffect string `json:"sideEffect"`
	Arguments  string `json:"arguments"`
}

// toolApprovalState is the router's view of an approval.
type toolApprovalState struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	DecidedBy string `json:"decidedBy,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// toolApprovalDetails is stored with the approval for the approver to read.
type toolApprovalDetails struct {
	Tool       string `json:"tool"`
	SideEffect string `json:"sideEffect"`
	Arguments  string `json:"arguments,omitempty"`
	Agent      string `json:"agent"`
}

func stateOf(a postgresql.Approval) toolApprovalState {
	return toolApprovalState{ID: a.ID, Status: a.Status, DecidedBy: a.DecidedBy, Reason: a.Reason}
}

// requestToolApproval opens (or returns) the approval for one tool call.
func (s *Server) requestToolApproval(w http.ResponseWriter, r *http.Request, caller, namespace, name string) {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.approvals == nil {
		http.Error(w, "tool approvals need the PostgreSQL store", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req toolApprovalRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Tool == "" || !callKeyPattern.MatchString(req.CallKey) {
		http.Error(w, "tool and a hex callKey are required", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}
	if postgresql.IsTerminalPhase(run.Status.Phase) {
		http.Error(w, "the run has ended", http.StatusConflict)
		return
	}
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: run.Spec.AgentRef, Namespace: namespace}, &agent); err != nil {
		http.Error(w, fmt.Sprintf("getting Agent: %v", err), http.StatusNotFound)
		return
	}
	// Only tools the agent has can be asked about.
	if !slices.Contains(agent.Spec.Tools, req.Tool) {
		http.Error(w, "the agent has no such tool", http.StatusBadRequest)
		return
	}

	timeout, groups := defaultToolApprovalTimeout, []string(nil)
	if ref := agent.Spec.GuardrailPolicyRef; ref != "" {
		var gp agentorcav1alpha1.GuardrailPolicy
		if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: ref, Namespace: namespace}, &gp); err == nil && gp.Spec.ToolApproval != nil {
			groups = gp.Spec.ToolApproval.ApproverGroups
			if t := gp.Spec.ToolApproval.Timeout; t != nil && t.Duration > 0 {
				timeout = t.Duration
			}
		}
	}

	args := req.Arguments
	if len(args) > maxToolApprovalArgs {
		args = args[:maxToolApprovalArgs] + "…"
	}
	details, _ := json.Marshal(toolApprovalDetails{Tool: req.Tool, SideEffect: req.SideEffect, Arguments: args, Agent: agent.Name})
	class := req.SideEffect
	if class == "" {
		class = "unclassified"
	}
	stored, err := s.approvals.RequestApproval(r.Context(), postgresql.Approval{
		Tenant:         run.Labels["agentorca.io/tenant"],
		Namespace:      namespace,
		RunName:        name,
		Kind:           postgresql.ApprovalKindTool,
		SourceKey:      fmt.Sprintf("%s/%s", run.UID, req.CallKey),
		Summary:        fmt.Sprintf("%s wants to run the %s tool %q", name, class, req.Tool),
		Details:        details,
		ApproverGroups: groups,
		ExpiresAt:      time.Now().Add(timeout),
	})
	if err != nil {
		slog.Error("recording a tool approval", "run", name, "tool", req.Tool, "err", err)
		http.Error(w, "recording the approval failed", http.StatusInternalServerError)
		return
	}
	if stored.Created {
		s.markToolApprovalPending(r.Context(), &run, req.Tool)
		if s.approvalNotifier != nil {
			if err := s.approvalNotifier.NotifyApproval(r.Context(), stored); err != nil {
				slog.Error("notifying approvers", "approval", stored.ID, "err", err)
			}
		}
	}
	slog.Info("tool call waiting for approval", "run", name, "namespace", namespace, "tool", req.Tool, "approval", stored.ID)
	jsonResponse(w, stateOf(stored))
}

// markToolApprovalPending sets the run's ToolApprovalPending condition; the
// controller clears it once nothing waits.
func (s *Server) markToolApprovalPending(ctx context.Context, run *agentorcav1alpha1.AgentRun, tool string) {
	patch := client.MergeFrom(run.DeepCopy())
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: agentorcav1alpha1.ConditionToolApprovalPending, Status: metav1.ConditionTrue, Reason: "ToolCallWaiting",
		Message: fmt.Sprintf("a call to %q is waiting for an approver", tool),
	})
	if err := s.crdClient.Status().Patch(ctx, run, patch); err != nil {
		slog.Error("setting the ToolApprovalPending condition", "run", run.Name, "err", err)
	}
}

// getToolApproval returns an approval's state to the router that asked for it,
// expiring it first if it is overdue.
func (s *Server) getToolApproval(w http.ResponseWriter, r *http.Request, caller, namespace, name, id string) {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.approvals == nil {
		http.Error(w, "tool approvals need the PostgreSQL store", http.StatusServiceUnavailable)
		return
	}
	a, err := s.approvals.GetApproval(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("reading a tool approval", "approval", id, "err", err)
		http.Error(w, "reading the approval failed", http.StatusInternalServerError)
		return
	}
	// A router sees only its own run's tool approvals.
	if a.Kind != postgresql.ApprovalKindTool || a.Namespace != namespace || a.RunName != name {
		http.NotFound(w, r)
		return
	}
	if a.Status == postgresql.ApprovalPending && time.Now().After(a.ExpiresAt) {
		if decided, err := s.approvals.DecideApproval(r.Context(), a.ID, postgresql.ApprovalExpired, "", "no decision within the approval timeout"); err == nil {
			a = decided
		} else if errors.Is(err, postgresql.ErrApprovalNotPending) {
			if again, gerr := s.approvals.GetApproval(r.Context(), id); gerr == nil {
				a = again
			}
		}
	}
	jsonResponse(w, stateOf(a))
}
