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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// This file centralizes multi-namespace tenant authorization: a tenant is
// authorized to access the namespaces listed in spec.allowedNamespaces.

// tenantNamespaces returns the namespaces the tenant identity is authorized to
// access. It prefers the multi-namespace list; for identities minted from
// older session/issued JWTs that only carry a single "namespace" claim, it falls
// back to that primary namespace so legacy tokens keep working.
func tenantNamespaces(t *TenantIdentity) []string {
	if t == nil {
		return nil
	}
	if len(t.Namespaces) > 0 {
		return t.Namespaces
	}
	if t.Namespace != "" {
		return []string{t.Namespace}
	}
	return nil
}

// firstNamespace returns the tenant's primary (first) authorized namespace.
func firstNamespace(namespaces []string) string {
	if len(namespaces) == 0 {
		return ""
	}
	return namespaces[0]
}

// listCRDsMultiNamespace lists listType across every namespace the tenant identity
// in ctx is authorized to access, accumulating the per-namespace results into the
// provided list. controller-runtime's client.List decodes (replaces) the target
// list on every call, so each namespace is listed into a fresh deep copy and its
// items extracted and accumulated — otherwise only the last namespace's items
// would survive.
//
// When no tenant identity is present (K8s SA BFF flow / auth disabled), it falls
// back to a single cl.List honoring the optional ns (the ?namespace= query param,
// or all namespaces when empty), preserving the previous non-tenant behavior.
func listCRDsMultiNamespace(ctx context.Context, cl client.Client, listType client.ObjectList, ns string, extra ...client.ListOption) error {
	tenant, ok := TenantFromContext(ctx)
	if !ok || tenant == nil {
		return cl.List(ctx, listType, append(nsListOpts(ns), extra...)...)
	}

	namespaces := tenantNamespaces(tenant)
	if len(namespaces) == 0 {
		return nil
	}

	var all []runtime.Object
	for _, n := range namespaces {
		sub, ok := listType.DeepCopyObject().(client.ObjectList)
		if !ok {
			return fmt.Errorf("listCRDsMultiNamespace: %T does not implement client.ObjectList", listType)
		}
		opts := append([]client.ListOption{client.InNamespace(n)}, extra...)
		if err := cl.List(ctx, sub, opts...); err != nil {
			return err
		}
		items, err := meta.ExtractList(sub)
		if err != nil {
			return err
		}
		all = append(all, items...)
	}
	return meta.SetList(listType, all)
}

// listAgentsInNamespaces lists Agent CRDs across all of the tenant's authorized
// namespaces.
func listAgentsInNamespaces(ctx context.Context, cl client.Client, t *TenantIdentity) (agentorcav1alpha1.AgentList, error) {
	ns := tenantNamespaces(t)
	if len(ns) == 0 {
		return agentorcav1alpha1.AgentList{}, fmt.Errorf("tenant %q has no authorized namespaces", t.TenantName)
	}
	var all agentorcav1alpha1.AgentList
	for _, n := range ns {
		var l agentorcav1alpha1.AgentList
		if err := cl.List(ctx, &l, client.InNamespace(n)); err != nil {
			return agentorcav1alpha1.AgentList{}, err
		}
		all.Items = append(all.Items, l.Items...)
	}
	return all, nil
}

// listRunsInNamespaces lists AgentRun CRDs across all of the tenant's authorized
// namespaces.
func listRunsInNamespaces(ctx context.Context, cl client.Client, t *TenantIdentity, extra ...client.ListOption) (agentorcav1alpha1.AgentRunList, error) {
	ns := tenantNamespaces(t)
	if len(ns) == 0 {
		return agentorcav1alpha1.AgentRunList{}, fmt.Errorf("tenant %q has no authorized namespaces", t.TenantName)
	}
	var all agentorcav1alpha1.AgentRunList
	for _, n := range ns {
		var l agentorcav1alpha1.AgentRunList
		if err := cl.List(ctx, &l, append([]client.ListOption{client.InNamespace(n)}, extra...)...); err != nil {
			return agentorcav1alpha1.AgentRunList{}, err
		}
		all.Items = append(all.Items, l.Items...)
	}
	return all, nil
}

// findAgentInNamespaces returns the Agent named name in any authorized namespace.
func findAgentInNamespaces(ctx context.Context, cl client.Client, t *TenantIdentity, name string) (*agentorcav1alpha1.Agent, error) {
	list, err := listAgentsInNamespaces(ctx, cl, t)
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("agent %q not found in any authorized namespace", name)
}

// findRunInNamespaces returns the AgentRun named runID in any authorized namespace.
func findRunInNamespaces(ctx context.Context, cl client.Client, t *TenantIdentity, runID string) (*agentorcav1alpha1.AgentRun, error) {
	list, err := listRunsInNamespaces(ctx, cl, t, client.MatchingLabels{"agentorca.io/tenant": t.TenantName})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Name == runID {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("run %q not found", runID)
}
