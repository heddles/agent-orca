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

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// This file centralizes multi-namespace tenant authorization: a tenant is
// authorized to access the namespaces listed in its spec.allowedNamespaces. The
// operator resolves each agent/run to the namespace it actually lives in (which
// must be one of those), so a single tenant can span multiple namespaces. List
// operations span the whole authorized set; per-resource lookups resolve to the
// resource's own namespace.
//
// controller-runtime v0.23 only supports single-namespace List, so spanning is
// done with a per-namespace loop rather than a multi-namespace selector.

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

// firstNamespace returns the tenant's primary (first) authorized namespace, used
// for single-namespace defaults (e.g. where the client secret lives when its
// ref omits a namespace) and for the string "namespace" bearer/JWT claim that
// existing token consumers expect.
func firstNamespace(namespaces []string) string {
	if len(namespaces) == 0 {
		return ""
	}
	return namespaces[0]
}

// listAgentsInNamespaces lists Agent CRDs across all of the tenant's authorized
// namespaces (the operator knows where each agent exists).
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
// namespaces, applying the extra list options (e.g. a tenant-label selector) to
// each per-namespace query.
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

// findAgentInNamespaces returns the Agent named name living in any of the
// tenant's authorized namespaces — directing the request to "the namespace the
// agent exists in and the tenant is authorized to access." The returned agent's
// Namespace is where the operator schedules its run.
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

// findRunInNamespaces returns the AgentRun named runID in any of the tenant's
// authorized namespaces, scoped to this tenant's runs via the
// agentorca.io/tenant label so runs are never accessible cross-tenant. The
// returned run's Namespace is the canonical key for its token/answer streams
// ("tokens:<ns>:<run>"), matching the controller/executor producers.
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
