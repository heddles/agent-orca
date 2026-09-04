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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

func listCRDTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add agentorca scheme: %v", err)
	}
	return s
}

// TestListCRDsMultiNamespaceAccumulates is a regression guard for the bug where
// listCRDsMultiNamespace reused the caller's list object across cl.List calls.
// controller-runtime's List decodes (replaces) the target list on every call, so
// the original implementation only surfaced the LAST tenant namespace's results
// — which is why the UI showed AgentRuns across all namespaces but no other
// resource types.
func TestListCRDsMultiNamespaceAccumulates(t *testing.T) {
	nsA, nsB, nsC := "tenant-acme", "red-team", "extra-ns"
	cl := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: nsA}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: nsB}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: nsC}},
	).Build()

	tenant := &TenantIdentity{
		TenantName: "acme",
		Namespace:  nsA,
		Namespaces: []string{nsA, nsB, nsC},
	}
	ctx := context.WithValue(context.Background(), tenantIdentityKey, tenant)

	var list agentorcav1alpha1.AgentList
	if err := listCRDsMultiNamespace(ctx, cl, &list, ""); err != nil {
		t.Fatalf("listCRDsMultiNamespace: %v", err)
	}
	if got, want := len(list.Items), 3; got != want {
		t.Fatalf("listed %d items across 3 tenant namespaces, want %d (accumulation bug?)", got, want)
	}
	seen := map[string]bool{}
	for _, a := range list.Items {
		seen[a.Namespace] = true
	}
	for _, ns := range []string{nsA, nsB, nsC} {
		if !seen[ns] {
			t.Errorf("missing results from namespace %q", ns)
		}
	}
}

// TestListCRDsMultiNamespaceNoTenantFallsBackToAll verifies the non-tenant (SA
// BFF / local dev) path lists every namespace when ?namespace= is absent.
func TestListCRDsMultiNamespaceNoTenantFallsBackToAll(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "tenant-acme"}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "red-team"}},
	).Build()

	var list agentorcav1alpha1.AgentList
	if err := listCRDsMultiNamespace(context.Background(), cl, &list, ""); err != nil {
		t.Fatalf("listCRDsMultiNamespace: %v", err)
	}
	if got := len(list.Items); got != 2 {
		t.Fatalf("non-tenant list returned %d items, want 2 (all namespaces)", got)
	}
}

// TestListCRDsMultiNamespaceNoTenantRespectsNamespaceParam verifies the
// ?namespace= filter is honored on the non-tenant fallback path.
func TestListCRDsMultiNamespaceNoTenantRespectsNamespaceParam(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "tenant-acme"}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "red-team"}},
	).Build()

	var list agentorcav1alpha1.AgentList
	if err := listCRDsMultiNamespace(context.Background(), cl, &list, "red-team"); err != nil {
		t.Fatalf("listCRDsMultiNamespace: %v", err)
	}
	if got := len(list.Items); got != 1 {
		t.Fatalf("namespace-filtered list returned %d items, want 1", got)
	}
	if list.Items[0].Name != "b1" || list.Items[0].Namespace != "red-team" {
		t.Fatalf("unexpected item: name=%q namespace=%q", list.Items[0].Name, list.Items[0].Namespace)
	}
}

// TestListCRDsMultiNamespaceEmptyTenantNamespaces verifies a tenant with no
// authorized namespaces yields no items and no error.
func TestListCRDsMultiNamespaceEmptyTenantNamespaces(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "tenant-acme"}},
	).Build()

	tenant := &TenantIdentity{TenantName: "acme", Namespace: "", Namespaces: nil}
	ctx := context.WithValue(context.Background(), tenantIdentityKey, tenant)

	var list agentorcav1alpha1.AgentList
	if err := listCRDsMultiNamespace(ctx, cl, &list, ""); err != nil {
		t.Fatalf("listCRDsMultiNamespace: %v", err)
	}
	if got := len(list.Items); got != 0 {
		t.Fatalf("listCRDsMultiNamespace with no authorized namespaces returned %d items, want 0", got)
	}
}
