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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func tenantPolicies(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var list networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &list, client.MatchingLabels{security.LabelPolicyKind: security.PolicyKindTenantWorkloadIngress}); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, np := range list.Items {
		out[np.Namespace] = np.Labels[security.LabelTenantID]
	}
	return out
}

func TestTenantConfigCreatesAWorkloadPolicyPerClaimedNamespace(t *testing.T) {
	r, c := newTenantReconciler(t, tenancy.NewMemoryRegistry(), tenantConfigFor("uid-1", func(tc *agentorcav1alpha1.TenantConfig) {
		tc.Spec.AllowedNamespaces = []string{"acme-ns", "acme-two"}
	}))
	reconcileTenant(t, r)
	id := getTenant(t, c).Status.TenantID

	got := tenantPolicies(t, c)
	if len(got) != 2 || got["acme-ns"] != id || got["acme-two"] != id {
		t.Fatalf("policies = %v, want one per claimed namespace for %s", got, id)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "acme-ns", Name: "agentorca-tenant-" + id}, &np); err != nil {
		t.Fatal(err)
	}
	if np.Spec.PodSelector.MatchLabels[security.LabelTenantID] != id {
		t.Errorf("policy selects %v", np.Spec.PodSelector.MatchLabels)
	}

	reconcileTenant(t, r) // idempotent
	if got := tenantPolicies(t, c); len(got) != 2 {
		t.Errorf("second reconcile: %v", got)
	}
}

func TestTenantConfigReleasingANamespaceRemovesItsPolicy(t *testing.T) {
	ctx := context.Background()
	r, c := newTenantReconciler(t, tenancy.NewMemoryRegistry(), tenantConfigFor("uid-1", func(tc *agentorcav1alpha1.TenantConfig) {
		tc.Spec.AllowedNamespaces = []string{"acme-ns", "acme-two"}
	}))
	reconcileTenant(t, r)

	tc := getTenant(t, c)
	tc.Spec.AllowedNamespaces = []string{"acme-ns"}
	if err := c.Update(ctx, tc); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	got := tenantPolicies(t, c)
	if _, ok := got["acme-two"]; ok || len(got) != 1 {
		t.Errorf("released namespace kept its policy: %v", got)
	}
}

func TestTenantConfigDeletionRemovesItsPolicies(t *testing.T) {
	ctx := context.Background()
	r, c := newTenantReconciler(t, tenancy.NewMemoryRegistry(), tenantConfigFor("uid-1", nil))
	reconcileTenant(t, r)
	if len(tenantPolicies(t, c)) != 1 {
		t.Fatal("no policy to delete")
	}
	if err := c.Delete(ctx, getTenant(t, c)); err != nil { // the finalizer keeps it, with a deletion timestamp
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	if got := tenantPolicies(t, c); len(got) != 0 {
		t.Errorf("policies left after the tenant was deleted: %v", got)
	}
}

// Without a registry there is no finalizer; the tenant simply disappears, and
// the next reconcile (here the NotFound one) removes what it left.
func TestTenantConfigGoneWithoutFinalizerRemovesItsPolicies(t *testing.T) {
	ctx := context.Background()
	r, c := newTenantReconciler(t, nil, tenantConfigFor("uid-1", nil))
	reconcileTenant(t, r)
	if len(tenantPolicies(t, c)) != 1 {
		t.Fatal("no policy to delete")
	}
	if err := c.Delete(ctx, getTenant(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	if got := tenantPolicies(t, c); len(got) != 0 {
		t.Errorf("policies left: %v", got)
	}
}

// Two tenants sharing a namespace each get their own policy there, and
// deleting one leaves the other's.
func TestSharedNamespaceKeepsEachTenantsPolicy(t *testing.T) {
	ctx := context.Background()
	other := tenantConfigFor("uid-2", func(tc *agentorcav1alpha1.TenantConfig) {
		tc.Name = "globex"
		tc.Spec.AllowedNamespaces = []string{"acme-ns"}
	})
	r, c := newTenantReconciler(t, tenancy.NewMemoryRegistry(), tenantConfigFor("uid-1", nil), other)
	reconcileTenant(t, r)
	if _, err := r.Reconcile(ctx, reqFor(other.Name)); err != nil {
		t.Fatal(err)
	}
	var list networkingv1.NetworkPolicyList
	if err := c.List(ctx, &list, client.InNamespace("acme-ns")); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("shared namespace has %d tenant policies, want 2", len(list.Items))
	}
	ids := []string{list.Items[0].Labels[security.LabelTenantID], list.Items[1].Labels[security.LabelTenantID]}
	if ids[0] == ids[1] || slices.Contains(ids, "") {
		t.Errorf("tenant IDs = %v", ids)
	}
}

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: tenantNS}}
}
