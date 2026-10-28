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
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Conditions on an AgentWorkflow that report on its definition.
const (
	// ConditionWorkflowValid reports whether a template (or a workflow's
	// structure) is usable; False carries every problem found.
	ConditionWorkflowValid = "Valid"
	// ConditionTemplateResolved reports whether a workflow created from a
	// template got its steps.
	ConditionTemplateResolved = "TemplateResolved"
)

// templateRetry is how long a workflow waits to look for a template that does
// not exist yet.
const templateRetry = 10 * time.Second

// reconcileTemplate validates a template and records the verdict. A template is
// never run.
func (r *AgentWorkflowReconciler) reconcileTemplate(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (ctrl.Result, error) {
	cond := metav1.Condition{
		Type: ConditionWorkflowValid, Status: metav1.ConditionTrue, Reason: "Valid",
		Message: "the template is valid", ObservedGeneration: wf.Generation,
	}
	if problems := validateTemplate(&wf.Spec); len(problems) > 0 {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Invalid", strings.Join(problems, "; ")
		log.FromContext(ctx).Info("workflow template is not valid", "template", wf.Name, "problems", problems)
	}
	existing := meta.FindStatusCondition(wf.Status.Conditions, ConditionWorkflowValid)
	if wf.Status.Phase == agentorcav1alpha1.AgentWorkflowPhaseTemplate && existing != nil &&
		existing.Status == cond.Status && existing.Message == cond.Message && existing.ObservedGeneration == cond.ObservedGeneration {
		return ctrl.Result{}, nil
	}
	wf.Status.Phase = agentorcav1alpha1.AgentWorkflowPhaseTemplate
	meta.SetStatusCondition(&wf.Status.Conditions, cond)
	return ctrl.Result{}, r.Status().Update(ctx, wf)
}

// expandFromTemplate gives a workflow created from a template its steps. A
// template that does not exist yet is waited for; a template or parameters that
// are wrong fail the workflow with the reason.
func (r *AgentWorkflowReconciler) expandFromTemplate(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (ctrl.Result, error) {
	ref := wf.Spec.FromTemplate
	var tmpl agentorcav1alpha1.AgentWorkflow
	if err := r.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: ref.Name}, &tmpl); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: templateRetry}, r.setResolved(ctx, wf, metav1.ConditionFalse, "TemplateNotFound",
			fmt.Sprintf("waiting for the template %q", ref.Name))
	}

	spec, err := instantiateTemplate(&tmpl, wf)
	if err != nil {
		r.failWorkflow(ctx, wf, err.Error())
		meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
			Type: ConditionTemplateResolved, Status: metav1.ConditionFalse, Reason: "Invalid",
			Message: err.Error(), ObservedGeneration: wf.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, wf)
	}

	wf.Spec = spec
	if err := r.Update(ctx, wf); err != nil {
		return ctrl.Result{}, err
	}
	wf.Status.Template = &agentorcav1alpha1.WorkflowTemplateRevision{Name: tmpl.Name, Generation: tmpl.Generation}
	meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
		Type: ConditionTemplateResolved, Status: metav1.ConditionTrue, Reason: "Resolved",
		Message: fmt.Sprintf("built from template %q (generation %d)", tmpl.Name, tmpl.Generation), ObservedGeneration: wf.Generation,
	})
	// Requeue so the workflow's first pass runs on the expanded spec.
	return ctrl.Result{RequeueAfter: time.Second}, r.Status().Update(ctx, wf)
}

func (r *AgentWorkflowReconciler) setResolved(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow, status metav1.ConditionStatus, reason, msg string) error {
	if c := meta.FindStatusCondition(wf.Status.Conditions, ConditionTemplateResolved); c != nil &&
		c.Status == status && c.Reason == reason && c.Message == msg {
		return nil
	}
	meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
		Type: ConditionTemplateResolved, Status: status, Reason: reason, Message: msg, ObservedGeneration: wf.Generation,
	})
	return r.Status().Update(ctx, wf)
}

// checkStructure fails a workflow that has not started and cannot run: a
// duplicate step name, a dependency that does not exist, or a cycle. Without the
// check such a workflow would sit Pending forever.
func (r *AgentWorkflowReconciler) checkStructure(ctx context.Context, wf *agentorcav1alpha1.AgentWorkflow) (failed bool, err error) {
	problems := append(validateWorkflowStructure(wf.Spec.Steps), validateWorkflowSandbox(&wf.Spec)...)
	if len(problems) == 0 {
		return false, nil
	}
	msg := strings.Join(problems, "; ")
	r.failWorkflow(ctx, wf, msg)
	meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
		Type: ConditionWorkflowValid, Status: metav1.ConditionFalse, Reason: "Invalid",
		Message: msg, ObservedGeneration: wf.Generation,
	})
	return true, r.Status().Update(ctx, wf)
}
