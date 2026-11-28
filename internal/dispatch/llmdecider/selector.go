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

package llmdecider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/judge"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// ErrSelector is wrapped by every error about naming or using a tenant's
// decision ModelSelector.
var ErrSelector = errors.New("the decision ModelSelector cannot be used")

// ProviderResolver finds a ModelProvider a tenant may use; judge.KubeResolver
// is the cluster one.
type ProviderResolver interface {
	Resolve(ctx context.Context, caller tenancy.Caller, namespace, ref string) (judge.Provider, error)
}

// SelectorModels are the models of the ModelSelector name in namespace, in the
// order a Chooser tries them: its providers by weight, then the rest of its
// fallback chain. A decision is one short call, so the selector's strategy,
// meta-router and Switchyard settings do not apply. Providers the tenant cannot
// use are left out; it is an error only when none is left.
func SelectorModels(ctx context.Context, reader client.Reader, providers ProviderResolver, caller tenancy.Caller, namespace, name string) ([]Model, error) {
	var sel agentorcav1alpha1.ModelSelector
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sel); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: ModelSelector %q does not exist in namespace %s", ErrSelector, name, namespace)
		}
		return nil, err
	}
	if tenancy.CheckAccess(caller, &sel, "ModelSelector") == tenancy.AccessNone {
		// The same words as for a missing one: it is not this tenant's to see.
		return nil, fmt.Errorf("%w: ModelSelector %q does not exist in namespace %s", ErrSelector, name, namespace)
	}
	weighted := slices.Clone(sel.Spec.Providers)
	slices.SortStableFunc(weighted, func(a, b agentorcav1alpha1.ProviderWeight) int { return cmp.Compare(b.Weight, a.Weight) })
	refs := make([]string, 0, len(weighted)+len(sel.Spec.FallbackChain))
	for _, p := range weighted {
		refs = append(refs, p.Name)
	}
	refs = append(refs, sel.Spec.FallbackChain...)

	var models []Model
	var failures []error
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		p, err := providers.Resolve(ctx, caller, namespace, ref)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		models = append(models, Model{Name: ref, Provider: p})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: none of the providers of %q can be used: %w", ErrSelector, name, errors.Join(failures...))
	}
	return models, nil
}
