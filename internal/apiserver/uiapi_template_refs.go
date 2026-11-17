package apiserver

import (
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/runner"
)

// Names in a copy are same-namespace references, so a template copied into a
// workspace works only if the workspace has what the template names outside its
// group (its ModelSelector first of all). The check runs before anything is
// created.

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

// missingRef is one reference of a template that points at nothing in the target
// namespace, with what the caller could use instead.
type missingRef struct {
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	Namespace    string   `json:"namespace"`
	Alternatives []string `json:"alternatives"`
	ReplaceKey   string   `json:"replaceKey"`
}

// unresolvedRefs lists, one plain-language message each, the references the copies
// make that point at nothing: not another copy, and not an object of the target
// namespace that the caller may read. The lookup is the experiment runner's
// (runner.RefChecker), so a gap reads the same in both.
func (s *UIServer) unresolvedRefs(r *http.Request, target string, members []templateMember, copies []*unstructured.Unstructured) ([]string, []missingRef, error) {
	ctx := r.Context()
	inSet := map[string]bool{}
	for _, m := range members {
		inSet[m.kind+"/"+m.newName] = true
	}
	checker := &runner.RefChecker{
		Reader: s.crdClient, Target: target, InSet: inSet,
		Readable: func(obj client.Object, kind string) bool { return canRead(ctx, obj, kind) },
	}
	var problems []string
	var missing []missingRef
	for i, u := range copies {
		obj, err := typedOf(u, members[i].kind)
		if err != nil {
			return nil, nil, err
		}
		if obj == nil {
			continue
		}
		gaps, err := checker.Check(ctx, obj)
		if err != nil {
			return nil, nil, err
		}
		for _, g := range gaps {
			key := g.Kind + "/" + g.Name
			msg := fmt.Sprintf("%s %q does not exist in namespace %s: ask an administrator to add it", g.Kind, g.Name, target)
			if len(g.Alternatives) > 0 {
				msg += fmt.Sprintf(", or use one of its own (%s) by sending {\"replace\":{%q:%q}}", strings.Join(g.Alternatives, ", "), key, g.Alternatives[0])
			}
			problems = append(problems, msg)
			missing = append(missing, missingRef{Kind: g.Kind, Name: g.Name, Namespace: target, Alternatives: g.Alternatives, ReplaceKey: key})
		}
	}
	return problems, missing, nil
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
