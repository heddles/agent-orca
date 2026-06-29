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

// Package controller implements the Kubernetes controllers for agent-orc CRDs.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/security"
)

const agentFinalizer = "agentorc.io/agent-cleanup"

// AgentReconciler reconciles a Agent object.
type AgentReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	CloudProvider security.CloudProvider
}

// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete

func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var agent agentorcv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion via finalizer.
	if !agent.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &agent)
	}

	// Ensure finalizer is present so we can clean up the SA on deletion.
	if !controllerutil.ContainsFinalizer(&agent, agentFinalizer) {
		controllerutil.AddFinalizer(&agent, agentFinalizer)
		if err := r.Update(ctx, &agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// If the user supplied their own SA, just reflect it in status.
	if agent.Spec.ServiceAccountRef != nil {
		return r.setStatus(ctx, &agent, agent.Spec.ServiceAccountRef.Name)
	}

	// Reconcile the operator-managed SA.
	saName := security.AgentSAName(agent.Name)
	if err := r.reconcileManagedSA(ctx, &agent, saName); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("agent reconciled", "sa", saName, "cloud", r.CloudProvider)
	return r.setStatus(ctx, &agent, saName)
}

// reconcileManagedSA creates or updates the stable Agent-scoped ServiceAccount.
func (r *AgentReconciler) reconcileManagedSA(ctx context.Context, agent *agentorcv1alpha1.Agent, saName string) error {
	desired := security.BuildManagedServiceAccount(agent.Name, agent.Namespace)
	security.ApplyCloudAuthAnnotations(desired, agent.Spec.CloudAuth, r.CloudProvider)

	var existing corev1.ServiceAccount
	err := r.Get(ctx, client.ObjectKey{Name: saName, Namespace: agent.Namespace}, &existing)
	if errors.IsNotFound(err) {
		if err2 := ctrl.SetControllerReference(agent, desired, r.Scheme); err2 != nil {
			return fmt.Errorf("setting owner reference on SA: %w", err2)
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting SA %s: %w", saName, err)
	}

	// Patch cloud auth annotations in case they changed.
	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	security.ApplyCloudAuthAnnotations(&existing, agent.Spec.CloudAuth, r.CloudProvider)
	return r.Patch(ctx, &existing, patch)
}

// setStatus patches AgentStatus with the SA name and detected cloud provider.
func (r *AgentReconciler) setStatus(ctx context.Context, agent *agentorcv1alpha1.Agent, saName string) (ctrl.Result, error) {
	if agent.Status.ServiceAccountName == saName &&
		agent.Status.CloudProviderDetected == string(r.CloudProvider) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.ServiceAccountName = saName
	agent.Status.CloudProviderDetected = string(r.CloudProvider)
	if err := r.Status().Patch(ctx, agent, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching agent status: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleDeletion removes the operator-managed SA and clears the finalizer.
func (r *AgentReconciler) handleDeletion(ctx context.Context, agent *agentorcv1alpha1.Agent) (ctrl.Result, error) {
	if agent.Spec.ServiceAccountRef == nil {
		saName := security.AgentSAName(agent.Name)
		var sa corev1.ServiceAccount
		if err := r.Get(ctx, client.ObjectKey{Name: saName, Namespace: agent.Namespace}, &sa); err == nil {
			if err := r.Delete(ctx, &sa); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("deleting managed SA: %w", err)
			}
		}
	}
	controllerutil.RemoveFinalizer(agent, agentFinalizer)
	return ctrl.Result{}, r.Update(ctx, agent)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcv1alpha1.Agent{}).
		Owns(&corev1.ServiceAccount{}).
		Named("agent").
		Complete(r)
}
