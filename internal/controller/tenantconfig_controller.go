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
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
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
	// Registry is the tenant registry, the source of truth for tenant IDs. Nil
	// when Postgres is not configured: IDs are then still assigned and mirrored
	// to status, but nothing records their lifecycle.
	Registry tenancy.Registry
	// OperatorNamespace is the namespace the operator runs in, which tenant
	// workload ingress policies admit. Empty omits that peer.
	OperatorNamespace string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (r *TenantConfigReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
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
		if apierrors.IsNotFound(err) {
			// A deleted tenant's policies go with it (idempotent).
			return ctrl.Result{}, r.syncTenantWorkloadPolicies(ctx)
		}
		return ctrl.Result{}, err
	}

	if !tc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &tc)
	}

	// Register before anything else so a Ready tenant always has an ID. Existing
	// TenantConfigs get theirs here on the first reconcile after an upgrade.
	tenantID, idErr := r.syncTenantID(ctx, &tc)

	ready, message := r.validate(&tc)
	if idErr != nil {
		ready, message = false, fmt.Sprintf("tenant ID: %v", idErr)
	}

	patch := client.MergeFrom(tc.DeepCopy())
	if idErr == nil {
		tc.Status.TenantID = tenantID
	}
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
	if idErr != nil && !errors.Is(idErr, tenancy.ErrIDInUse) {
		return ctrl.Result{}, idErr // a registry outage is retried; a conflict needs a human
	}

	// Refresh the auth middleware's tenant cache.
	if r.Refresher != nil {
		if err := r.Refresher.RefreshTenants(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("refreshing tenant cache: %w", err)
		}
	}

	// Keep the tenant's workload ingress policies in step with the namespaces
	// it claims.
	if err := r.syncTenantWorkloadPolicies(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing tenant workload network policies: %w", err)
	}

	return ctrl.Result{}, nil
}

// syncTenantID registers the TenantConfig in the registry and returns its ID.
func (r *TenantConfigReconciler) syncTenantID(ctx context.Context, tc *agentorcav1alpha1.TenantConfig) (string, error) {
	if r.Registry == nil {
		switch {
		case tc.Spec.TenantID != "":
			return tc.Spec.TenantID, nil
		case tc.Status.TenantID != "":
			return tc.Status.TenantID, nil
		}
		return tenancy.NewID()
	}
	if controllerutil.AddFinalizer(tc, tenancy.RegistryFinalizer) {
		if err := r.Update(ctx, tc); err != nil {
			return "", fmt.Errorf("adding finalizer: %w", err)
		}
	}
	return tenancy.Sync(ctx, r.Registry, tenancy.Claim{
		Name: tc.Name, Namespace: tc.Namespace, UID: string(tc.UID),
		Pinned: tc.Spec.TenantID, Current: tc.Status.TenantID,
	}, r.now())
}

// finalize records the tenant's deletion so its sessions stop working, then
// lets the TenantConfig go.
func (r *TenantConfigReconciler) finalize(ctx context.Context, tc *agentorcav1alpha1.TenantConfig) error {
	// The tenant is deleting, so the sync drops its workload policies.
	if err := r.syncTenantWorkloadPolicies(ctx); err != nil {
		return fmt.Errorf("removing tenant workload network policies: %w", err)
	}
	if !controllerutil.ContainsFinalizer(tc, tenancy.RegistryFinalizer) {
		return nil
	}
	if r.Registry != nil {
		id := tc.Status.TenantID
		if id == "" {
			id = tc.Spec.TenantID
		}
		if id != "" {
			if err := tenancy.MarkDeleted(ctx, r.Registry, id, r.now()); err != nil {
				return fmt.Errorf("marking tenant deleted: %w", err)
			}
		}
	}
	controllerutil.RemoveFinalizer(tc, tenancy.RegistryFinalizer)
	if err := r.Update(ctx, tc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if r.Refresher != nil {
		_ = r.Refresher.RefreshTenants(ctx)
	}
	return nil
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
