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

package runner

import (
	"context"
	"slices"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Kind names the reference check knows how to look up.
const (
	KindModelSelector = "ModelSelector"
	KindModelProvider = "ModelProvider"
	KindAgent         = "Agent"
	kindTool          = "Tool"
	kindKB            = "KnowledgeBase"
	kindGuardrail     = "GuardrailPolicy"
	kindMCPServer     = "MCPServer"
)

// checkableKinds are the referenced kinds a check can look up, with how to make
// an empty object and list of each. "Use a template" (docs/agent-builder.md) and
// an experiment's arms share this table and CheckRefs, so a gap reads the same
// in both.
var checkableKinds = map[string]struct {
	obj  func() client.Object
	list func() client.ObjectList
}{
	KindModelSelector: {func() client.Object { return &agentorcav1alpha1.ModelSelector{} }, func() client.ObjectList { return &agentorcav1alpha1.ModelSelectorList{} }},
	kindTool:          {func() client.Object { return &agentorcav1alpha1.Tool{} }, func() client.ObjectList { return &agentorcav1alpha1.ToolList{} }},
	kindKB:            {func() client.Object { return &agentorcav1alpha1.KnowledgeBase{} }, func() client.ObjectList { return &agentorcav1alpha1.KnowledgeBaseList{} }},
	kindGuardrail:     {func() client.Object { return &agentorcav1alpha1.GuardrailPolicy{} }, func() client.ObjectList { return &agentorcav1alpha1.GuardrailPolicyList{} }},
	kindMCPServer:     {func() client.Object { return &agentorcav1alpha1.MCPServer{} }, func() client.ObjectList { return &agentorcav1alpha1.MCPServerList{} }},
	KindModelProvider: {func() client.Object { return &agentorcav1alpha1.ModelProvider{} }, func() client.ObjectList { return &agentorcav1alpha1.ModelProviderList{} }},
	KindAgent:         {func() client.Object { return &agentorcav1alpha1.Agent{} }, func() client.ObjectList { return &agentorcav1alpha1.AgentList{} }},
}

// MissingRef is one reference of an object that points at nothing in the target
// namespace, with what the caller could use instead.
type MissingRef struct {
	Kind         string
	Name         string
	Alternatives []string
}

// RefChecker finds the references of objects that point at nothing: not
// another object of the set being created (InSet, keyed "Kind/name"), and not an
// object of Target that Readable accepts. The checker remembers what it already
// reported, so one gap is reported once across several objects.
type RefChecker struct {
	Reader client.Reader
	Target string
	InSet  map[string]bool
	// Readable says whether the owner may use the object. Nil accepts any.
	Readable func(obj client.Object, kind string) bool

	seen map[string]bool
}

func (c *RefChecker) readable(obj client.Object, kind string) bool {
	return c.Readable == nil || c.Readable(obj, kind)
}

// Check returns the references of obj that are missing. obj must be a typed
// object (an Agent, Tool or GuardrailPolicy, and so on); a kind that
// tenancy.RefsOf knows nothing about has no references.
func (c *RefChecker) Check(ctx context.Context, obj client.Object) ([]MissingRef, error) {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	var out []MissingRef
	for _, ref := range tenancy.RefsOf(obj) {
		key := ref.Kind + "/" + ref.Name
		info, checkable := checkableKinds[ref.Kind]
		if !checkable || c.InSet[key] || c.seen[key] {
			continue
		}
		c.seen[key] = true
		existing := info.obj()
		err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Target, Name: ref.Name}, existing)
		if err == nil && c.readable(existing, ref.Kind) {
			continue
		}
		if err != nil && !k8serrors.IsNotFound(err) {
			return nil, err
		}
		alt, err := c.alternatives(ctx, ref.Kind, info.list())
		if err != nil {
			return nil, err
		}
		out = append(out, MissingRef{Kind: ref.Kind, Name: ref.Name, Alternatives: alt})
	}
	return out, nil
}

// alternatives names the objects of kind in the target namespace the owner may use.
func (c *RefChecker) alternatives(ctx context.Context, kind string, list client.ObjectList) ([]string, error) {
	if err := c.Reader.List(ctx, list, client.InNamespace(c.Target)); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, it := range items {
		if o, ok := it.(client.Object); ok && c.readable(o, kind) {
			names = append(names, o.GetName())
		}
	}
	slices.Sort(names)
	return names, nil
}
