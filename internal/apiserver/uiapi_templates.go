package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// maxTemplateCopyName leaves room for the suffix of the objects that travel
// with the agent, and for the names the operator derives from an agent's.
const kindTool = "Tool"

const maxTemplateCopyName = 40

// useTemplateRequest is the body of POST /api/templates/{ns}/{name}/use.
type useTemplateRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Replace points references of the template at objects of the target
	// namespace, keyed "Kind/name" ("ModelSelector/default": "my-selector").
	Replace map[string]string `json:"replace,omitempty"`
}

// createdRef names an object a call created.
type createdRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// templateMember is one object of a template's group, and the name of its copy.
type templateMember struct {
	kind    string
	obj     client.Object
	newName string
}

// handleUseTemplate serves POST /api/templates/{ns}/{name}/use: copy the
// template agent and everything in its template group (the deployment, tools
// and guardrail policy) into the caller's workspace, UI-owned, under fresh
// names, with the references between them pointing at the copies.
//
//	201 {"agent": {...}, "created": [{kind, namespace, name}, ...]}
//	404 the template is missing, not a template, or not readable by the caller
//	409 a name is taken (nothing is left behind)
func (s *UIServer) handleUseTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRole(w, r, tenancy.RoleBuilder) {
		return
	}
	req, ok := readUseTemplateRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var src agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}, &src); err != nil {
		notFoundOr(w, err)
		return
	}
	if !canRead(ctx, &src, kindAgent) || !isPlatformTemplate(&src) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	target, ok := templateTarget(w, r, req.Namespace)
	if !ok {
		return
	}
	tenantID := ""
	if t, ok := TenantFromContext(ctx); ok && t != nil {
		if t.TenantID == "" {
			http.Error(w, `{"error":"tenant has no ID yet"}`, http.StatusForbidden)
			return
		}
		tenantID = t.TenantID
	}

	members, err := s.templateMembers(r, &src, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	renames := map[string]map[string]string{} // kind -> old name -> new name
	for _, m := range members {
		if renames[m.kind] == nil {
			renames[m.kind] = map[string]string{}
		}
		renames[m.kind][m.obj.GetName()] = m.newName
	}

	copies := make([]*unstructured.Unstructured, 0, len(members))
	for _, m := range members {
		u, err := templateCopy(m, renames, target, tenantID, s.ownDetect)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		applyReplacements(u, m.kind, req.Replace)
		stampUI(r, u)
		copies = append(copies, u)
	}
	// Nothing is created unless every reference the copies make will resolve.
	problems, missing, err := s.unresolvedRefs(r, target, members, copies)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(problems) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		thing := "thing that is"
		if len(missing) != 1 {
			thing = "things that are"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": fmt.Sprintf("This template needs %d %s not set up in workspace %s.", len(missing), thing, target),
			"errors":  problems,
			"missing": missing,
		})
		return
	}

	var created []*unstructured.Unstructured
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = s.crdClient.Delete(context.WithoutCancel(ctx), created[i])
		}
	}
	for i, m := range members {
		u := copies[i]
		if err := s.persistNew(r, m.kind, u); err != nil {
			rollback()
			if k8serrors.IsAlreadyExists(err) {
				writeConflict(w, fmt.Sprintf("%s %s/%s already exists", m.kind, target, m.newName), "")
				return
			}
			writeAPIError(w, "copying "+m.kind+" "+m.obj.GetName(), err)
			return
		}
		created = append(created, u)
	}

	refs := make([]createdRef, 0, len(created))
	var agent *unstructured.Unstructured
	for i, u := range created {
		refs = append(refs, createdRef{Kind: members[i].kind, Namespace: u.GetNamespace(), Name: u.GetName()})
		if members[i].kind == kindAgent {
			agent = u
		}
		s.recordWrite(w, r, u, auditEventFor(postgresql.AuditCreate, members[i].kind, u.GetNamespace(), u.GetName(), u.GetResourceVersion(),
			fmt.Sprintf("created %s %s/%s from template %s/%s", members[i].kind, u.GetNamespace(), u.GetName(), src.Namespace, src.Name)))
	}
	agent.Object["ownership"] = s.ownershipOf(ctx, agent, kindAgent)
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]any{"agent": agent.Object, "created": refs})
}

// readUseTemplateRequest parses and checks the body of a use-template call.
func readUseTemplateRequest(w http.ResponseWriter, r *http.Request) (useTemplateRequest, bool) {
	var req useTemplateRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Name == "" {
		jsonError(w, `the body must be {"name": "<name of your agent>", "namespace": "<optional>"}`, http.StatusBadRequest)
		return req, false
	}
	if errs := validation.IsDNS1123Label(req.Name); len(errs) > 0 || len(req.Name) > maxTemplateCopyName {
		jsonError(w, fmt.Sprintf("name %q must be a lowercase DNS label of at most %d characters", req.Name, maxTemplateCopyName), http.StatusBadRequest)
		return req, false
	}
	for k := range req.Replace {
		if kind, name, ok := strings.Cut(k, "/"); !ok || kind == "" || name == "" {
			jsonError(w, fmt.Sprintf(`replace key %q must be "Kind/name"`, k), http.StatusBadRequest)
			return req, false
		}
	}
	return req, true
}

// isPlatformTemplate reports whether agent is a template of the platform
// catalog: labelled as a template and platform-scoped, in a platform namespace.
func isPlatformTemplate(agent *agentorcav1alpha1.Agent) bool {
	return agent.Labels[LabelTemplate] == templateMarker &&
		agent.Labels[tenancy.LabelScope] == tenancy.ScopePlatform &&
		tenancy.IsPlatformNamespace(agent.Namespace)
}

// templateTarget picks the namespace a template is copied into: the requested
// one when the caller may use it, else the caller's first allowed namespace.
func templateTarget(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	t, ok := TenantFromContext(r.Context())
	if !ok || t == nil {
		if requested == "" {
			jsonError(w, "namespace is required", http.StatusBadRequest)
			return "", false
		}
		return requested, true
	}
	allowed := tenantNamespaces(t)
	if requested != "" && namespaceAllowed(r.Context(), requested) {
		return requested, true
	}
	if len(allowed) == 0 {
		jsonError(w, "your tenant has no workspace namespace", http.StatusForbidden)
		return "", false
	}
	return firstNamespace(allowed), true
}

// templateMembers lists what travels with the template agent: the agent itself,
// and the deployments, tools and guardrail policies of its template group that
// the caller may read, with the names of their copies. Order is the creation
// order: policies and tools first, the agent, then its deployments.
func (s *UIServer) templateMembers(r *http.Request, src *agentorcav1alpha1.Agent, name string) ([]templateMember, error) {
	ctx := r.Context()
	group := src.Labels[LabelTemplateGroup]
	var guards []templateMember
	var tools []templateMember
	var deps []templateMember
	if group != "" {
		opts := []client.ListOption{client.InNamespace(src.Namespace), client.MatchingLabels{LabelTemplateGroup: group}}
		lists := []struct {
			kind string
			list client.ObjectList
			into *[]templateMember
			tag  string
		}{
			{"GuardrailPolicy", &agentorcav1alpha1.GuardrailPolicyList{}, &guards, "guardrail"},
			{kindTool, &agentorcav1alpha1.ToolList{}, &tools, "tool"},
			{"AgentDeployment", &agentorcav1alpha1.AgentDeploymentList{}, &deps, "deploy"},
		}
		for _, l := range lists {
			if err := s.crdClient.List(ctx, l.list, opts...); err != nil {
				return nil, err
			}
			items, err := meta.ExtractList(l.list)
			if err != nil {
				return nil, err
			}
			var objs []client.Object
			for _, it := range items {
				if o, ok := it.(client.Object); ok && canRead(ctx, o, l.kind) {
					objs = append(objs, o)
				}
			}
			sort.Slice(objs, func(i, j int) bool { return objs[i].GetName() < objs[j].GetName() })
			for i, o := range objs {
				suffix := l.tag
				if len(objs) > 1 {
					suffix = fmt.Sprintf("%s-%d", l.tag, i+1)
				}
				*l.into = append(*l.into, templateMember{kind: l.kind, obj: o, newName: name + "-" + suffix})
			}
		}
	}
	out := append(guards, tools...)
	out = append(out, templateMember{kind: kindAgent, obj: src, newName: name})
	return append(out, deps...), nil
}

// templateCopy builds the copy of one template member for the caller's workspace
// and rewrites its references to the copies.
func templateCopy(m templateMember, renames map[string]map[string]string, namespace, tenantID string, detect tenancy.DetectOptions) (*unstructured.Unstructured, error) {
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(m.obj.DeepCopyObject())
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: fields}
	u.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	u.SetKind(m.kind)
	delete(fields, "status")
	if md, ok := fields["metadata"].(map[string]any); ok {
		for _, f := range uiMetadataDropped {
			delete(md, f)
		}
	}
	u.SetName(m.newName)
	u.SetNamespace(namespace)
	tenancy.StripGitTracking(u, detect)
	tenancy.ClearHandoff(u)
	tenancy.StripOwnership(u)
	if l := u.GetLabels(); l != nil {
		delete(l, LabelTemplate)
		delete(l, LabelTemplateGroup)
		u.SetLabels(l)
	}
	if a := u.GetAnnotations(); a != nil {
		for _, k := range []string{tenancy.AnnotationOwner, tenancy.AnnotationLastModifiedBy, tenancy.AnnotationLastModifiedAt} {
			delete(a, k)
		}
		u.SetAnnotations(a)
	}
	tenancy.Stamp(u, tenantID)

	rename := func(kind, old string) string {
		if n, ok := renames[kind][old]; ok {
			return n
		}
		return old
	}
	switch m.kind {
	case kindAgent:
		if err := rewriteRefs(u, "spec", "tools", func(n string) string { return rename(kindTool, n) }); err != nil {
			return nil, err
		}
		if ref, found, _ := unstructured.NestedString(fields, "spec", "guardrailPolicyRef"); found && ref != "" {
			_ = unstructured.SetNestedField(fields, rename("GuardrailPolicy", ref), "spec", "guardrailPolicyRef")
		}
	case "AgentDeployment":
		if ref, found, _ := unstructured.NestedString(fields, "spec", "agentRef"); found {
			_ = unstructured.SetNestedField(fields, rename(kindAgent, ref), "spec", "agentRef")
		}
	case kindTool:
		if ref, found, _ := unstructured.NestedString(fields, "spec", "agentRef"); found && ref != "" {
			_ = unstructured.SetNestedField(fields, rename(kindAgent, ref), "spec", "agentRef")
		}
	}
	return u, nil
}

// rewriteRefs maps every name in the string list at path.
func rewriteRefs(u *unstructured.Unstructured, field, key string, mapName func(string) string) error {
	names, found, err := unstructured.NestedStringSlice(u.Object, field, key)
	if err != nil || !found {
		return err
	}
	for i := range names {
		names[i] = mapName(names[i])
	}
	return unstructured.SetNestedStringSlice(u.Object, names, field, key)
}
