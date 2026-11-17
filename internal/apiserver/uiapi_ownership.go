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
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Ownership in the UI API: who owns each object (UI or Git), whether the caller
// may edit it here, and the three ways to move work between the two:
//
//	POST   /api/resources/{kind}/{ns}/{name}/export     a clean manifest to commit, and a handoff
//	DELETE /api/resources/{kind}/{ns}/{name}/handoff    cancel a pending handoff
//	POST   /api/resources/{kind}/{ns}/{name}/duplicate  a UI-owned copy under a new name
//
// There is no Git write-back: the UI never opens a pull request or edits a
// repository. A human commits the exported manifest.

// SetOwnership sets how Git tracking is detected and how long a handoff lasts.
func (s *UIServer) SetOwnership(detect tenancy.DetectOptions, handoffTTL time.Duration) {
	s.ownDetect = detect
	s.handoffTTL = handoffTTL
}

func (s *UIServer) handoffLifetime() time.Duration {
	if s.handoffTTL > 0 {
		return s.handoffTTL
	}
	return tenancy.DefaultHandoffTTL
}

// handoffView is the handoff part of ownershipView.
type handoffView struct {
	Pending bool   `json:"pending"`
	Expires string `json:"expires,omitempty"`
}

// ownershipView is the ownership field every object the UI API returns carries.
type ownershipView struct {
	Owner string `json:"owner"`
	Tool  string `json:"tool,omitempty"`
	App   string `json:"app,omitempty"`
	// Editable is true only for a UI-owned object of the caller's own tenant, when
	// the caller holds the role that writes this kind.
	Editable bool        `json:"editable"`
	Handoff  handoffView `json:"handoff"`
}

// ownershipOf describes obj for the caller in ctx.
func (s *UIServer) ownershipOf(ctx context.Context, obj client.Object, kind string) ownershipView {
	o := tenancy.Detect(obj, s.ownDetect)
	v := ownershipView{Owner: o.Owner, Tool: o.Tool, App: o.App}
	if h := tenancy.HandoffOf(obj); h.Active(time.Now()) {
		v.Handoff = handoffView{Pending: true, Expires: h.Expires.UTC().Format(time.RFC3339)}
	}
	v.Editable = o.Owner == tenancy.OwnerUI && canWrite(ctx, obj, kind) && hasRole(ctx, writeRoleFor(kind))
	return v
}

// objectWithOwnership is obj as JSON with an ownership field.
func (s *UIServer) objectWithOwnership(ctx context.Context, obj client.Object, kind string) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	out["ownership"] = s.ownershipOf(ctx, obj, kind)
	return out, nil
}

// listWithOwnership is list as JSON, each item carrying an ownership field.
func (s *UIServer) listWithOwnership(ctx context.Context, list client.ObjectList, kind string) (map[string]any, error) {
	b, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	raw, _ := out["items"].([]any)
	for i := range raw {
		m, _ := raw[i].(map[string]any)
		obj, ok := items[i].(client.Object)
		if m == nil || !ok {
			continue
		}
		m["ownership"] = s.ownershipOf(ctx, obj, kind)
	}
	return out, nil
}

// notEditable answers 409 for an object the UI does not own. Git-owned objects
// point at Duplicate; objects with no owner are not editable either.
func notEditable(w http.ResponseWriter, kind, ns, name string, o tenancy.Ownership, resourceVersion string) {
	switch o.Owner {
	case tenancy.OwnerGit:
		by := "Git"
		if o.Tool != "" && o.App != "" {
			by = fmt.Sprintf("Git (%s %s)", o.Tool, o.App)
		}
		writeConflict(w, fmt.Sprintf("%s %s/%s is managed in %s and cannot be edited here: change it in Git, or use Duplicate to make a UI-owned copy",
			kind, ns, name, by), resourceVersion)
	default:
		writeConflict(w, fmt.Sprintf("%s %s/%s is not owned by the UI (%s is %q): change it where it was created, or use Duplicate to make a UI-owned copy",
			kind, ns, name, tenancy.AnnotationOwner, o.Owner), resourceVersion)
	}
}

// exportedAnnotations are dropped from an exported manifest: the UI's record of
// its own writes, the handoff and kubectl's copy of an earlier apply.
var exportedAnnotations = []string{
	tenancy.AnnotationOwner, tenancy.AnnotationLastModifiedBy, tenancy.AnnotationLastModifiedAt,
	tenancy.AnnotationHandoff, tenancy.AnnotationHandoffExpires, "kubectl.kubernetes.io/last-applied-configuration",
}

// exportManifest builds the manifest a human commits to Git: no status and no
// server-owned metadata, owner Git, the tenant ID kept.
func exportManifest(obj client.Object, kind string) ([]byte, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	u.SetKind(kind)
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		for _, f := range append([]string{"finalizers"}, uiMetadataDropped...) {
			delete(md, f)
		}
		delete(md, "annotations")
	}
	a := obj.GetAnnotations()
	clean := map[string]string{}
	maps.Copy(clean, a)
	for _, k := range exportedAnnotations {
		delete(clean, k)
	}
	clean[tenancy.AnnotationOwner] = tenancy.OwnerGit
	u.SetAnnotations(clean)
	return yaml.Marshal(u.Object)
}

// patchAnnotations applies change to obj's annotations as the UI.
func (s *UIServer) patchAnnotations(ctx context.Context, obj client.Object, change func()) error {
	orig, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a client.Object", obj)
	}
	change()
	return s.crdClient.Patch(ctx, obj, client.MergeFrom(orig), client.FieldOwner(tenancy.FieldManagerUI))
}

// loadOwned reads the object a sub-resource action works on, checking the role
// and the caller's tenant: 404 for what the caller may not see, 403 for a
// platform-shared object.
func (s *UIServer) loadOwned(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) (client.Object, bool) {
	if !requireRole(w, r, writeRoleFor(info.gvk)) {
		return nil, false
	}
	obj := info.newObj()
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, obj); err != nil {
		notFoundOr(w, err)
		return nil, false
	}
	if !s.authorizeWrite(w, r, obj, info.gvk) {
		return nil, false
	}
	return obj, true
}

// exportResource handles POST /api/resources/{kind}/{ns}/{name}/export. The
// answer is the manifest as YAML. Unless ?handoff=false it also marks the live
// object agentorca.io/handoff: pending until the handoff lifetime has passed, so
// the GitOps tool the manifest is committed to may adopt it.
func (s *UIServer) exportResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	obj, ok := s.loadOwned(w, r, info, ns, name)
	if !ok {
		return
	}
	if o := tenancy.Detect(obj, s.ownDetect); o.Owner != tenancy.OwnerUI {
		notEditable(w, info.gvk, ns, name, o, obj.GetResourceVersion())
		return
	}
	withHandoff := r.URL.Query().Get("handoff") != "false"
	var expires time.Time
	if withHandoff {
		expires = time.Now().Add(s.handoffLifetime())
		if err := s.patchAnnotations(r.Context(), obj, func() { tenancy.SetHandoff(obj, expires) }); err != nil {
			writeAPIError(w, "marking the handoff", err)
			return
		}
	}
	manifest, err := exportManifest(obj, info.gvk)
	if err != nil {
		http.Error(w, "building the manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	summary := fmt.Sprintf("exported %s %s/%s", info.gvk, ns, name)
	if withHandoff {
		summary += " for handoff until " + expires.UTC().Format(time.RFC3339)
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, info.gvk, ns, name, obj.GetResourceVersion(), summary))
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".yaml"))
	_, _ = w.Write(manifest)
}

// cancelHandoff handles DELETE /api/resources/{kind}/{ns}/{name}/handoff.
func (s *UIServer) cancelHandoff(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	obj, ok := s.loadOwned(w, r, info, ns, name)
	if !ok {
		return
	}
	if !tenancy.HandoffOf(obj).Requested {
		writeConflict(w, fmt.Sprintf("%s %s/%s has no handoff to cancel", info.gvk, ns, name), obj.GetResourceVersion())
		return
	}
	if err := s.patchAnnotations(r.Context(), obj, func() { tenancy.ClearHandoff(obj) }); err != nil {
		writeAPIError(w, "cancelling the handoff", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, info.gvk, ns, name, obj.GetResourceVersion(),
		fmt.Sprintf("cancelled the handoff of %s %s/%s", info.gvk, ns, name)))
	jsonResponse(w, map[string]string{"status": "cancelled"})
}

// duplicateResource handles POST /api/resources/{kind}/{ns}/{name}/duplicate
// with {"name": "<new name>"}: a UI-owned copy in the caller's workspace
// namespace. The source may be Git-owned, UI-owned or have no owner, and may be
// platform-shared (a template); it must be readable by the caller. The copy
// loses the source's Git tracking, owner and handoff annotations.
func (s *UIServer) duplicateResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	if !requireRole(w, r, writeRoleFor(info.gvk)) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Name == "" {
		jsonError(w, `the body must be {"name": "<name of the copy>"}`, http.StatusBadRequest)
		return
	}
	src := info.newObj()
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, src); err != nil {
		notFoundOr(w, err)
		return
	}
	if !canRead(r.Context(), src, info.gvk) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	obj := &unstructured.Unstructured{Object: m}
	obj.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	obj.SetKind(info.gvk)
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		for _, f := range uiMetadataDropped {
			delete(md, f)
		}
	}
	obj.SetName(req.Name)
	obj.SetNamespace(ns)
	tenancy.StripGitTracking(obj, s.ownDetect)
	tenancy.ClearHandoff(obj)
	if !s.prepareNew(w, r, obj) {
		return
	}
	s.createUIObject(w, r, info, obj, fmt.Sprintf("duplicated %s %s/%s as %s/%s", info.gvk, ns, name, obj.GetNamespace(), obj.GetName()))
}

// The actions below an object.
const (
	actionExport    = "export"
	actionHandoff   = "handoff"
	actionDuplicate = "duplicate"
	kindAgent       = "Agent"
)

// resourceAction routes the paths below an object: /export, /handoff and /duplicate.
func (s *UIServer) resourceAction(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name, action string) {
	switch {
	case action == actionExport && r.Method == http.MethodPost:
		s.exportResource(w, r, info, ns, name)
	case action == actionHandoff && r.Method == http.MethodDelete:
		s.cancelHandoff(w, r, info, ns, name)
	case action == actionDuplicate && r.Method == http.MethodPost:
		s.duplicateResource(w, r, info, ns, name)
	case action == actionExport || action == actionHandoff || action == actionDuplicate:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.Error(w, `{"error":"unknown resource action"}`, http.StatusNotFound)
	}
}
