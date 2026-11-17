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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// The write half of /api/resources.
//
//   - Ownership. An object created here carries agentorca.io/owner: ui. The UI
//     changes and deletes only objects that carry it. The body's own owner and
//     actor annotations are dropped. app.kubernetes.io/managed-by is not touched:
//     controllers use it, so it says nothing about who made an object.
//   - Server-side apply. Writes are applied with field manager agent-orca-ui and
//     forced ownership, so the UI's full intent replaces what it set before while
//     fields other writers own (a controller's defaults) are left alone. An update
//     must carry the metadata.resourceVersion the editor read; a stale one is a
//     409 that names the current version. Status is never taken from the body.
//   - Admission. Apply goes through the same webhooks as any write.
//   - Actor. Every write stamps agentorca.io/last-modified-by and -at, and adds a
//     row to the audit trail (see audit.go).
//
// A create is a plain Create with the same field manager, which the API server
// makes atomic: a name that already exists is an AlreadyExists, never a takeover.
// Create records its fields under the "Update" operation, which a later apply
// could not remove fields from, so the create is followed by an apply of the same
// content and the removal of the Update entry (adoptFields), leaving the apply
// entry as the only owner. Updates are server-side applies.

const maxResourceBody = 2 << 20 // 2 MB

// resourceBody is a request body reduced to what a write may carry.
type resourceBody struct {
	obj *unstructured.Unstructured
	// resourceVersion is the one the editor read, "" for a create.
	resourceVersion string
}

// uiMetadataDropped are the metadata fields the server owns or that would let a
// body reach beyond its own object (owner references make a garbage-collection
// parent, finalizers can block deletion).
var uiMetadataDropped = []string{
	"managedFields", "uid", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds",
	"generation", "selfLink", "ownerReferences", "finalizers", "generateName", "resourceVersion",
}

// readResourceBody parses and sanitises a write body: it must be a JSON object
// of the kind the URL names, with status and server-owned metadata removed.
func readResourceBody(w http.ResponseWriter, r *http.Request, info crdKindInfo) (resourceBody, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxResourceBody))
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return resourceBody{}, false
	}
	// A typed decode rejects bodies the kind cannot hold (a string where a list belongs).
	if err := json.Unmarshal(body, info.newObj()); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return resourceBody{}, false
	}
	var content map[string]any
	if err := json.Unmarshal(body, &content); err != nil || content == nil {
		http.Error(w, "invalid JSON: the body must be an object", http.StatusBadRequest)
		return resourceBody{}, false
	}
	obj := &unstructured.Unstructured{Object: content}
	if k := obj.GetKind(); k != "" && k != info.gvk {
		jsonError(w, fmt.Sprintf("the body is a %s, the URL is for %s", k, info.gvk), http.StatusBadRequest)
		return resourceBody{}, false
	}
	rv := obj.GetResourceVersion()
	delete(content, "status")
	if meta, ok := content["metadata"].(map[string]any); ok {
		for _, f := range uiMetadataDropped {
			delete(meta, f)
		}
	}
	obj.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	obj.SetKind(info.gvk)
	return resourceBody{obj: obj, resourceVersion: rv}, true
}

// applyResource server-side applies obj as the UI. On return obj holds the
// stored object, managedFields included; callers strip them before answering.
func (s *UIServer) applyResource(r *http.Request, obj *unstructured.Unstructured) error {
	if err := s.crdClient.Patch(r.Context(), obj, client.Apply, //nolint:staticcheck // Client.Apply takes typed apply configurations, not the sanitised unstructured body
		client.FieldOwner(tenancy.FieldManagerUI), client.ForceOwnership); err != nil {
		return err
	}
	return nil
}

// adoptFields makes the apply entry the only owner of what a create wrote. The
// apply is made first, so the object then has both the create's "Update" entry
// and an "Apply" entry holding the same fields; removing the Update entry (a JSON
// patch guarded by the resourceVersion) leaves the apply entry alone, which is
// what lets a later apply remove a field. intent is refreshed to the stored object.
func (s *UIServer) adoptFields(r *http.Request, created, intent *unstructured.Unstructured) error {
	intent.SetResourceVersion(created.GetResourceVersion())
	if err := s.applyResource(r, intent); err != nil {
		return err
	}
	for i, mf := range intent.GetManagedFields() {
		if mf.Manager != tenancy.FieldManagerUI || mf.Operation != metav1.ManagedFieldsOperationUpdate {
			continue
		}
		patch := fmt.Sprintf(`[{"op":"test","path":"/metadata/resourceVersion","value":%q},{"op":"remove","path":"/metadata/managedFields/%d"}]`,
			intent.GetResourceVersion(), i)
		stored := intent.DeepCopy()
		if err := s.crdClient.Patch(r.Context(), stored, client.RawPatch(types.JSONPatchType, []byte(patch))); err != nil {
			return err
		}
		intent.Object = stored.Object
		break
	}
	unstructured.RemoveNestedField(intent.Object, "metadata", "managedFields")
	return nil
}

// writeAPIError answers a failed write with the status the API server (or a
// webhook) gave when it is a client error, else 500.
func writeAPIError(w http.ResponseWriter, what string, err error) {
	code := http.StatusInternalServerError
	var st k8serrors.APIStatus
	if errors.As(err, &st) {
		if c := int(st.Status().Code); c >= 400 && c < 500 {
			code = c
		}
	}
	http.Error(w, what+": "+err.Error(), code)
}

// notFoundOr answers an error from a Get: 404 for a missing object, else 500.
func notFoundOr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if k8serrors.IsNotFound(err) {
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

// writeConflict answers 409 with the version the editor needs to reload.
func writeConflict(w http.ResponseWriter, message, current string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "currentResourceVersion": current})
}

// stampUI marks obj as UI-owned and records the caller as its last modifier.
func stampUI(r *http.Request, obj client.Object) {
	tenancy.StripOwnerAnnotations(obj)
	tenancy.StampUIWrite(obj, actorOf(r.Context()), time.Now())
}

// createResource handles POST /api/resources/{kind}. The body is the full object
// (metadata.name and metadata.namespace included). It needs the builder role,
// or admin for ModelProvider, ModelSelector, MCPServer and Tool.
func (s *UIServer) createResource(w http.ResponseWriter, r *http.Request, info crdKindInfo) {
	if !requireRole(w, r, writeRoleFor(info.gvk)) {
		return
	}
	in, ok := readResourceBody(w, r, info)
	if !ok {
		return
	}
	obj := in.obj

	// With a tenant identity the namespace is the tenant's own, never the body's.
	// Ownership comes from the caller's tenant, never from the body: drop any
	// tenant-id or platform-scope label the body carries, then stamp our own.
	tenancy.StripOwnership(obj)
	if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil {
		if tenant.Namespace != "" {
			obj.SetNamespace(tenant.Namespace)
		}
		if tenant.TenantID == "" {
			http.Error(w, `{"error":"tenant has no ID yet"}`, http.StatusForbidden)
			return
		}
		tenancy.Stamp(obj, tenant.TenantID)
	}
	if obj.GetName() == "" || obj.GetNamespace() == "" {
		http.Error(w, `{"error":"metadata.name and metadata.namespace are required"}`, http.StatusBadRequest)
		return
	}
	stampUI(r, obj)

	intent := obj.DeepCopy()
	if err := s.crdClient.Create(r.Context(), obj, client.FieldOwner(tenancy.FieldManagerUI)); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			current := ""
			if existing := info.newObj(); s.crdClient.Get(r.Context(), client.ObjectKeyFromObject(obj), existing) == nil && canRead(r.Context(), existing, info.gvk) {
				current = existing.GetResourceVersion()
			}
			writeConflict(w, fmt.Sprintf("%s %s/%s already exists", info.gvk, obj.GetNamespace(), obj.GetName()), current)
			return
		}
		writeAPIError(w, "creating resource", err)
		return
	}
	// Hand the fields to the apply entry. The version makes it a no-op if anyone
	// wrote in between; the object is then still correct, only its field
	// ownership stays with the create, so the first edit may not remove a field.
	if err := s.adoptFields(r, obj, intent); err != nil {
		slog.Warn("taking over field ownership after create failed", "kind", info.gvk, "namespace", obj.GetNamespace(), "name", obj.GetName(), "err", err)
		unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	} else {
		obj = intent
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, info.gvk, obj.GetNamespace(), obj.GetName(), obj.GetResourceVersion(),
		fmt.Sprintf("created %s %s/%s", info.gvk, obj.GetNamespace(), obj.GetName())))
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, obj)
}

// updateResource handles PUT /api/resources/{kind}/{ns}/{name}: the body is the
// full object as the editor read it, with metadata.resourceVersion. Status in
// the body is ignored. Name and namespace come from the URL.
//
//	404  the object is missing or not the caller's
//	403  it is platform-shared (read-only)
//	409  it is not owned by the UI, or resourceVersion is stale (the body
//	     names the current version)
//	428  the body has no resourceVersion
func (s *UIServer) updateResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	if !requireRole(w, r, writeRoleFor(info.gvk)) {
		return
	}
	in, ok := readResourceBody(w, r, info)
	if !ok {
		return
	}
	obj := in.obj
	obj.SetName(name)
	obj.SetNamespace(ns)

	// The object must exist, belong to the caller's tenant and sit in one of the
	// caller's namespaces; anything else is not found. Platform-shared objects
	// are read-only here.
	existing := info.newObj()
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, existing); err != nil {
		notFoundOr(w, err)
		return
	}
	if !s.authorizeWrite(w, r, existing, info.gvk) {
		return
	}
	if tenancy.OwnerOf(existing) != tenancy.OwnerUI {
		writeConflict(w, fmt.Sprintf("%s %s/%s is not owned by the UI (%s is %q): change it where it was created, or duplicate it",
			info.gvk, ns, name, tenancy.AnnotationOwner, tenancy.OwnerOf(existing)), existing.GetResourceVersion())
		return
	}
	if in.resourceVersion == "" {
		http.Error(w, `{"error":"metadata.resourceVersion is required: send the version you read"}`, http.StatusPreconditionRequired)
		return
	}
	if in.resourceVersion != existing.GetResourceVersion() {
		writeConflict(w, fmt.Sprintf("%s %s/%s changed since you read it", info.gvk, ns, name), existing.GetResourceVersion())
		return
	}

	// Ownership is fixed by the stored object, not the body.
	tenancy.StripOwnership(obj)
	if tid, _ := tenancy.TenantOf(existing); tid != "" {
		tenancy.Stamp(obj, tid)
	} else if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		tenancy.Stamp(obj, t.TenantID)
	}
	stampUI(r, obj)
	// The apply names the version, so a write that lands between the check above
	// and here is refused by the API server rather than overwritten.
	obj.SetResourceVersion(in.resourceVersion)

	if err := s.applyResource(r, obj); err != nil {
		if k8serrors.IsConflict(err) {
			current := ""
			if fresh := info.newObj(); s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, fresh) == nil {
				current = fresh.GetResourceVersion()
			}
			writeConflict(w, fmt.Sprintf("%s %s/%s changed since you read it", info.gvk, ns, name), current)
			return
		}
		writeAPIError(w, "updating resource", err)
		return
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, info.gvk, ns, name, obj.GetResourceVersion(),
		fmt.Sprintf("updated %s %s/%s (resourceVersion %s to %s)", info.gvk, ns, name, in.resourceVersion, obj.GetResourceVersion())))
	jsonResponse(w, obj)
}

// deleteResource handles DELETE /api/resources/{kind}/{ns}/{name}. It deletes
// only objects the UI owns; anything else is a 403.
func (s *UIServer) deleteResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	if !requireRole(w, r, writeRoleFor(info.gvk)) {
		return
	}
	obj := info.newObj()
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, obj); err != nil {
		notFoundOr(w, err)
		return
	}
	if !s.authorizeWrite(w, r, obj, info.gvk) {
		return
	}
	if tenancy.OwnerOf(obj) != tenancy.OwnerUI {
		jsonError(w, fmt.Sprintf("%s %s/%s is not owned by the UI (%s is %q), so the UI cannot delete it",
			info.gvk, ns, name, tenancy.AnnotationOwner, tenancy.OwnerOf(obj)), http.StatusForbidden)
		return
	}
	if err := s.crdClient.Delete(r.Context(), obj); err != nil {
		writeAPIError(w, "deleting resource", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditDelete, info.gvk, ns, name, obj.GetResourceVersion(),
		fmt.Sprintf("deleted %s %s/%s", info.gvk, ns, name)))
	jsonResponse(w, map[string]string{"status": "deleted"})
}
