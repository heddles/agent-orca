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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// Tool-call approvals are requested and decided without the controller: the
// internal API opens them and sets the run's ToolApprovalPending condition. The
// controller's part is to clear that condition once no tool call waits, so it
// reads the approval store only while the condition is True.
func (r *AgentRunReconciler) reconcileToolApprovals(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	c := meta.FindStatusCondition(run.Status.Conditions, agentorcav1alpha1.ConditionToolApprovalPending)
	if c == nil || c.Status != metav1.ConditionTrue || r.Approvals == nil {
		return
	}
	pending, err := r.Approvals.ListApprovals(ctx, postgresql.ApprovalQuery{
		Namespace: run.Namespace, RunName: run.Name, Status: postgresql.ApprovalPending,
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "listing the run's pending approvals")
		return
	}
	for _, a := range pending {
		if a.Kind == postgresql.ApprovalKindTool && a.Status == postgresql.ApprovalPending {
			return // still waiting
		}
	}
	r.setRunCondition(ctx, run, metav1.Condition{
		Type: agentorcav1alpha1.ConditionToolApprovalPending, Status: metav1.ConditionFalse,
		Reason: "NoneWaiting", Message: "no tool call is waiting for an approver",
	})
}
