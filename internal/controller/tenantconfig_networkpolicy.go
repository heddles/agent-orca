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
	"maps"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

type policyKey struct{ namespace, name string }

// syncTenantWorkloadPolicies makes the cluster's tenant workload ingress
// policies match the TenantConfigs: one policy per namespace each live tenant
// claims (spec.allowedNamespaces), and none for a namespace a tenant released
// or a tenant that was deleted. It works from the full set of TenantConfigs,
// not from one tenant's change, so a missed event (or a deletion with no
// finalizer to hook) is repaired by the next reconcile of any tenant.
//
// A tenant without an ID yet gets no policy: the policy selects by ID.
func (r *TenantConfigReconciler) syncTenantWorkloadPolicies(ctx context.Context) error {
	var tenants agentorcav1alpha1.TenantConfigList
	if err := r.List(ctx, &tenants); err != nil {
		return fmt.Errorf("listing TenantConfigs: %w", err)
	}
	desired := map[policyKey]*networkingv1.NetworkPolicy{}
	for i := range tenants.Items {
		tc := &tenants.Items[i]
		id := tenancy.TenantIDOf(tc)
		if !tc.DeletionTimestamp.IsZero() || id == "" {
			continue
		}
		for _, ns := range tc.Spec.AllowedNamespaces {
			np := security.BuildTenantWorkloadIngressPolicy(id, ns, r.OperatorNamespace)
			desired[policyKey{np.Namespace, np.Name}] = np
		}
	}

	var errs []error
	for key, want := range desired {
		if err := r.applyPolicy(ctx, want); err != nil {
			// A claimed namespace that does not exist yet is not an error: the
			// policy is created when a reconcile finds it.
			if k8serrors.IsNotFound(err) {
				log.FromContext(ctx).V(1).Info("namespace not found; tenant workload policy deferred", "namespace", key.namespace)
				continue
			}
			errs = append(errs, fmt.Errorf("policy %s/%s: %w", key.namespace, key.name, err))
		}
	}

	var existing networkingv1.NetworkPolicyList
	if err := r.List(ctx, &existing, client.MatchingLabels{
		security.LabelManagedBy:  security.ManagedByValue,
		security.LabelPolicyKind: security.PolicyKindTenantWorkloadIngress,
	}); err != nil {
		errs = append(errs, fmt.Errorf("listing tenant workload policies: %w", err))
		return errors.Join(errs...)
	}
	for i := range existing.Items {
		np := &existing.Items[i]
		if _, ok := desired[policyKey{np.Namespace, np.Name}]; ok {
			continue
		}
		if err := r.Delete(ctx, np); err != nil && !k8serrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting policy %s/%s: %w", np.Namespace, np.Name, err))
		}
	}
	return errors.Join(errs...)
}

// applyPolicy creates the policy or brings an existing one back to the wanted
// spec and labels.
func (r *TenantConfigReconciler) applyPolicy(ctx context.Context, want *networkingv1.NetworkPolicy) error {
	var have networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &have)
	if k8serrors.IsNotFound(err) {
		return r.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(have.Spec, want.Spec) && labelsInclude(have.Labels, want.Labels) {
		return nil
	}
	have.Spec = want.Spec
	if have.Labels == nil {
		have.Labels = map[string]string{}
	}
	maps.Copy(have.Labels, want.Labels)
	return r.Update(ctx, &have)
}

func labelsInclude(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
