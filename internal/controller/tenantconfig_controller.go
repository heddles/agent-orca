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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// TenantRefresher is implemented by ExternalAuth to refresh the tenant cache.
type TenantRefresher interface {
	RefreshTenants(ctx context.Context) error
}

// TenantConfigReconciler reconciles TenantConfig objects and keeps the
// external auth middleware's tenant cache up to date.
type TenantConfigReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Refresher TenantRefresher
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=tenantconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=tenantconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=tenantconfigs/finalizers,verbs=update

func (r *TenantConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tc agentorcav1alpha1.TenantConfig
	if err := r.Get(ctx, req.NamespacedName, &tc); err != nil {
		// Deleted — refresh the cache to remove stale entries.
		if r.Refresher != nil {
			_ = r.Refresher.RefreshTenants(ctx)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ready, message := r.validate(&tc)

	patch := client.MergeFrom(tc.DeepCopy())
	tc.Status.Ready = ready
	tc.Status.Message = message
	now := metav1.Now()

	condStatus := metav1.ConditionTrue
	if !ready {
		condStatus = metav1.ConditionFalse
	}
	setCondition(&tc.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             "Validated",
		Message:            message,
		LastTransitionTime: now,
	})

	if err := r.Status().Patch(ctx, &tc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching TenantConfig status: %w", err)
	}

	// Refresh the auth middleware's tenant cache.
	if r.Refresher != nil {
		if err := r.Refresher.RefreshTenants(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("refreshing tenant cache: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *TenantConfigReconciler) validate(tc *agentorcav1alpha1.TenantConfig) (bool, string) {
	if len(tc.Spec.AllowedNamespaces) == 0 {
		return false, "spec.allowedNamespaces is required (at least one namespace)"
	}

	switch tc.Spec.AuthMode {
	case "issued":
		if tc.Spec.Issued == nil {
			return false, "spec.issued is required when authMode is 'issued'"
		}
		if tc.Spec.Issued.ClientID == "" {
			return false, "spec.issued.clientID is required"
		}
		if tc.Spec.Issued.ClientSecretRef.Name == "" {
			return false, "spec.issued.clientSecretRef.name is required"
		}
	case "federated":
		if tc.Spec.Federated == nil {
			return false, "spec.federated is required when authMode is 'federated'"
		}
		if tc.Spec.Federated.IssuerURL == "" {
			return false, "spec.federated.issuerURL is required"
		}
		if tc.Spec.Federated.ClientID == "" {
			return false, "spec.federated.clientID is required"
		}
		// matchClaim/matchValue are optional: when both are empty the tenant trusts
		// any token validly signed by IssuerURL with the expected ClientID audience
		// (issuer-only / default-allow). When set, both must be present together.
		claimEmpty := tc.Spec.Federated.MatchClaim == ""
		valueEmpty := tc.Spec.Federated.MatchValue == ""
		if claimEmpty != valueEmpty {
			return false, "spec.federated.matchClaim and matchValue must be set together, or both omitted for issuer-only trust"
		}
	default:
		return false, fmt.Sprintf("unknown authMode %q", tc.Spec.AuthMode)
	}

	return true, fmt.Sprintf("tenant %s validated (authMode=%s, namespaces=%v)", tc.Name, tc.Spec.AuthMode, tc.Spec.AllowedNamespaces)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TenantConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.TenantConfig{}).
		Named("tenantconfig").
		Complete(r)
}
