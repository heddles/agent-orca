package apiserver

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Names in a copy are same-namespace references, so a template copied into a
// workspace works only if the workspace has what the template names outside its
// group (its ModelSelector first of all). The check runs before anything is
// created.

// checkableKinds are the referenced kinds the check can look up, with how to make
// an empty object and list of each.
var checkableKinds = map[string]struct {
	obj  func() client.Object
	list func() client.ObjectList
}{
	"ModelSelector":   {func() client.Object { return &agentorcav1alpha1.ModelSelector{} }, func() client.ObjectList { return &agentorcav1alpha1.ModelSelectorList{} }},
	kindTool:          {func() client.Object { return &agentorcav1alpha1.Tool{} }, func() client.ObjectList { return &agentorcav1alpha1.ToolList{} }},
	"KnowledgeBase":   {func() client.Object { return &agentorcav1alpha1.KnowledgeBase{} }, func() client.ObjectList { return &agentorcav1alpha1.KnowledgeBaseList{} }},
	"GuardrailPolicy": {func() client.Object { return &agentorcav1alpha1.GuardrailPolicy{} }, func() client.ObjectList { return &agentorcav1alpha1.GuardrailPolicyList{} }},
	"MCPServer":       {func() client.Object { return &agentorcav1alpha1.MCPServer{} }, func() client.ObjectList { return &agentorcav1alpha1.MCPServerList{} }},
	"ModelProvider":   {func() client.Object { return &agentorcav1alpha1.ModelProvider{} }, func() client.ObjectList { return &agentorcav1alpha1.ModelProviderList{} }},
	kindAgent:         {func() client.Object { return &agentorcav1alpha1.Agent{} }, func() client.ObjectList { return &agentorcav1alpha1.AgentList{} }},
}

// typedOf decodes a copy into its API type.
func typedOf(u *unstructured.Unstructured, kind string) (client.Object, error) {
	var obj client.Object
	switch kind {
	case kindAgent:
		obj = &agentorcav1alpha1.Agent{}
	case kindTool:
		obj = &agentorcav1alpha1.Tool{}
	case "GuardrailPolicy":
		obj = &agentorcav1alpha1.GuardrailPolicy{}
	case kindAgentDeployment:
		obj = &agentorcav1alpha1.AgentDeployment{}
	default:
		return nil, nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// unresolvedRefs lists, one plain-language message each, the references the copies
// make that point at nothing: not another copy, and not an object of the target
// namespace that the caller may read.
func (s *UIServer) unresolvedRefs(r *http.Request, target string, members []templateMember, copies []*unstructured.Unstructured) ([]string, error) {
	ctx := r.Context()
	inSet := map[string]bool{}
	for _, m := range members {
		inSet[m.kind+"/"+m.newName] = true
	}
	var problems []string
	seen := map[string]bool{}
	for i, u := range copies {
		obj, err := typedOf(u, members[i].kind)
		if err != nil {
			return nil, err
		}
		if obj == nil {
			continue
		}
		for _, ref := range tenancy.RefsOf(obj) {
			key := ref.Kind + "/" + ref.Name
			info, checkable := checkableKinds[ref.Kind]
			if !checkable || inSet[key] || seen[key] {
				continue
			}
			seen[key] = true
			existing := info.obj()
			if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: target, Name: ref.Name}, existing); err == nil && canRead(ctx, existing, ref.Kind) {
				continue
			} else if err != nil && !k8serrors.IsNotFound(err) {
				return nil, err
			}
			msg := fmt.Sprintf("%s %q does not exist in namespace %s: ask an administrator to add it", ref.Kind, ref.Name, target)
			if alt, err := s.alternatives(r, target, ref.Kind, info.list()); err != nil {
				return nil, err
			} else if len(alt) > 0 {
				msg += fmt.Sprintf(", or use one of its own (%s) by sending {\"replace\":{%q:%q}}", strings.Join(alt, ", "), key, alt[0])
			}
			problems = append(problems, msg)
		}
	}
	return problems, nil
}

// alternatives names the objects of kind in the target namespace that the caller
// may read.
func (s *UIServer) alternatives(r *http.Request, target, kind string, list client.ObjectList) ([]string, error) {
	if err := s.crdClient.List(r.Context(), list, client.InNamespace(target)); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, it := range items {
		if o, ok := it.(client.Object); ok && canRead(r.Context(), o, kind) {
			names = append(names, o.GetName())
		}
	}
	slices.Sort(names)
	return names, nil
}

// applyReplacements points the references of a copy at the objects the caller
// named: replace maps "Kind/name" to the name to use instead.
func applyReplacements(u *unstructured.Unstructured, kind string, replace map[string]string) {
	if len(replace) == 0 || kind != kindAgent {
		return
	}
	one := func(refKind string, path ...string) {
		if v, ok, _ := unstructured.NestedString(u.Object, path...); ok {
			if n, ok := replace[refKind+"/"+v]; ok {
				_ = unstructured.SetNestedField(u.Object, n, path...)
			}
		}
	}
	many := func(refKind string, path ...string) {
		if vs, ok, _ := unstructured.NestedStringSlice(u.Object, path...); ok {
			for i, v := range vs {
				if n, ok := replace[refKind+"/"+v]; ok {
					vs[i] = n
				}
			}
			_ = unstructured.SetNestedStringSlice(u.Object, vs, path...)
		}
	}
	one("ModelSelector", "spec", "modelSelectorRef")
	one("ModelSelector", "spec", "memory", "summaryModelSelectorRef")
	one("KnowledgeBase", "spec", "memory", "longTermMemoryRef")
	one("GuardrailPolicy", "spec", "guardrailPolicyRef")
	many(kindTool, "spec", "tools")
	many("KnowledgeBase", "spec", "knowledgeBases")
	if refs, ok, _ := unstructured.NestedSlice(u.Object, "spec", "knowledgeBaseRefs"); ok {
		for _, r := range refs {
			if m, ok := r.(map[string]any); ok {
				if v, _ := m["name"].(string); v != "" {
					if n, ok := replace["KnowledgeBase/"+v]; ok {
						m["name"] = n
					}
				}
			}
		}
		_ = unstructured.SetNestedSlice(u.Object, refs, "spec", "knowledgeBaseRefs")
	}
}
