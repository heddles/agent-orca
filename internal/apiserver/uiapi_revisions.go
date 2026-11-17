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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Revision history of the objects the UI API writes.
//
//	GET  /api/resources/{kind}/{ns}/{name}/revisions?limit=&before=   the revisions, newest first
//	GET  /api/resources/{kind}/{ns}/{name}/revisions/{n}              one revision with its object
//	POST /api/resources/{kind}/{ns}/{name}/revisions/{n}/restore      apply it again
//
// Every successful UI create, update, restore and delete stores the object as
// it was after the write (before it, for a delete), without status and server
// metadata, and without the UI's own ownership and actor annotations.
//
// Failure policy: the same as the audit trail (audit.go). The Kubernetes write
// is already done, so the request is not failed; the revision write is retried,
// and if it still fails the answer carries X-Revision-Status: failed, one log
// line carries the snapshot, and agentorca_revision_write_failures_total counts
// it. Without PostgreSQL there is no history: the endpoints answer 501 and
// writes work as before.

// DefaultRevisionsKeep is how many revisions an object keeps unless REVISIONS_KEEP says otherwise.
const DefaultRevisionsKeep = 50

// EnvRevisionsKeep names the environment variable that sets how many revisions an object keeps.
const EnvRevisionsKeep = "REVISIONS_KEEP"

// RevisionsKeepFromEnv reads REVISIONS_KEEP; unset or invalid is DefaultRevisionsKeep.
func RevisionsKeepFromEnv() int {
	if n, err := strconv.Atoi(os.Getenv(EnvRevisionsKeep)); err == nil && n > 0 {
		return n
	}
	return DefaultRevisionsKeep
}

const (
	revisionAttempts   = 3
	revisionRetryDelay = 100 * time.Millisecond

	revisionsNeedPostgres = "revision history needs PostgreSQL"
	defaultRevisionsPage  = 50
	maxRevisionsPage      = 200
)

// revisionStore is the slice of postgresql.Store the revision history uses.
type revisionStore interface {
	AppendRevision(ctx context.Context, rev postgresql.Revision, keep int) (postgresql.Revision, bool, error)
	ListRevisions(ctx context.Context, q postgresql.RevisionQuery) ([]postgresql.Revision, error)
	GetRevision(ctx context.Context, q postgresql.RevisionQuery, n int) (postgresql.Revision, error)
}

var revisionWriteFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "agentorca_revision_write_failures_total",
	Help: "Revisions that could not be written to PostgreSQL after retries. Each one is in the operator log.",
})

func init() {
	ctrlmetrics.Registry.MustRegister(revisionWriteFailures)
}

// SetRevisionsKeep sets how many revisions an object keeps (zero or less: the default).
func (s *UIServer) SetRevisionsKeep(n int) { s.revisionsKeep = n }

func (s *UIServer) keepRevisions() int {
	if s.revisionsKeep > 0 {
		return s.revisionsKeep
	}
	return DefaultRevisionsKeep
}

// revisionSink returns the revision store, or nil without PostgreSQL.
func (s *UIServer) revisionSink() revisionStore {
	if s.revisions != nil {
		return s.revisions
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// revisionMeta says how a write is recorded when it is not the plain action.
type revisionMeta struct {
	// action overrides the revision's action (restore), "" takes the audit event's.
	action string
	// restoredFrom is the revision a restore applied.
	restoredFrom int
}

// recordWrite records a successful write of obj in the audit trail and in the
// revision history. Call it before the response is written so the failure
// headers can be set. A dry run records nothing.
func (s *UIServer) recordWrite(w http.ResponseWriter, r *http.Request, obj client.Object, e postgresql.AuditEvent) {
	s.recordWriteAs(w, r, obj, e, revisionMeta{})
}

func (s *UIServer) recordWriteAs(w http.ResponseWriter, r *http.Request, obj client.Object, e postgresql.AuditEvent, m revisionMeta) {
	if isDryRun(r) {
		return
	}
	s.recordAudit(w, r, e)
	s.recordRevision(w, r, obj, e, m)
}

// revisionContent is obj as a revision keeps it: no status, no server-owned
// metadata, no owner, actor or handoff annotations, the same as an export.
func revisionContent(obj client.Object, kind string) (map[string]any, error) {
	var m map[string]any
	if u, ok := obj.(*unstructured.Unstructured); ok {
		m = runtime.DeepCopyJSON(u.Object)
	} else {
		var err error
		if m, err = runtime.DefaultUnstructuredConverter.ToUnstructured(obj); err != nil {
			return nil, err
		}
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	u.SetKind(kind)
	delete(m, "status")
	delete(m, "ownership")
	if md, ok := m["metadata"].(map[string]any); ok {
		for _, f := range append([]string{"finalizers"}, uiMetadataDropped...) {
			delete(md, f)
		}
	}
	a := u.GetAnnotations()
	for _, k := range exportedAnnotations {
		delete(a, k)
	}
	if len(a) == 0 {
		unstructured.RemoveNestedField(m, "metadata", "annotations")
	} else {
		u.SetAnnotations(a)
	}
	return m, nil
}

// recordRevision stores the revision for a write, following the failure policy above.
func (s *UIServer) recordRevision(w http.ResponseWriter, r *http.Request, obj client.Object, e postgresql.AuditEvent, m revisionMeta) {
	store := s.revisionSink()
	if store == nil || isDryRun(r) {
		return
	}
	content, err := revisionContent(obj, e.Kind)
	var body []byte
	if err == nil {
		body, err = json.Marshal(content)
	}
	rev := postgresql.Revision{
		Action: e.Action, UserID: actorOf(r.Context()), Summary: e.Summary, ResourceVersion: e.ResourceVersion,
		Kind: e.Kind, Namespace: e.Namespace, Name: e.Name, Object: body,
	}
	if m.action != "" {
		rev.Action = m.action
	}
	if m.restoredFrom > 0 {
		n := m.restoredFrom
		rev.RestoredFrom = &n
	}
	if err == nil {
		// A revision is stored even if the client has gone away; the write keeps
		// the identity (and so the tenant scope).
		ctx := pgScope(context.WithoutCancel(r.Context()))
		delay := revisionRetryDelay
		for attempt := 1; attempt <= revisionAttempts; attempt++ {
			attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, _, err = store.AppendRevision(attemptCtx, rev, s.keepRevisions())
			cancel()
			if err == nil {
				return
			}
			if errors.Is(err, postgresql.ErrInvalidRevision) {
				break // retrying cannot help
			}
			if attempt < revisionAttempts {
				time.Sleep(delay)
				delay *= 2
			}
		}
	}
	revisionWriteFailures.Inc()
	slog.Error("revision could not be written", revisionFailureAttrs(r.Context(), rev, body, err)...)
	w.Header().Set("X-Revision-Status", "failed")
}

// maxLoggedSnapshotBytes is the largest snapshot a failure log line carries. A
// bigger one would break log shipping and put a whole definition (its
// instructions included) into the logs; the live object is the source of truth.
const maxLoggedSnapshotBytes = 8 << 10

// revisionFailureAttrs are the log attributes for a revision that could not be
// written: the metadata always, the snapshot itself only when it is small.
func revisionFailureAttrs(ctx context.Context, rev postgresql.Revision, snapshot []byte, err error) []any {
	tenant := ""
	if t, ok := TenantFromContext(ctx); ok && t != nil {
		tenant = t.TenantID
	}
	sum := sha256.Sum256(snapshot)
	attrs := []any{
		"err", err, "attempts", revisionAttempts, "tenant", tenant,
		"kind", rev.Kind, "namespace", rev.Namespace, "name", rev.Name, "action", rev.Action,
		"user", rev.UserID, "resourceVersion", rev.ResourceVersion,
		"snapshotBytes", len(snapshot), "snapshotSHA256", hex.EncodeToString(sum[:]),
	}
	if len(snapshot) <= maxLoggedSnapshotBytes {
		return append(attrs, "snapshot", string(snapshot))
	}
	return append(attrs, "snapshot", "omitted: over 8 KB; the live object is the source of truth")
}

// revisionPage is the body of the revision list.
type revisionPage struct {
	Revisions []postgresql.Revision `json:"revisions"`
	// Next is the value to pass as ?before= for the next (older) page; empty on the last.
	Next string `json:"next"`
}

// revisionsAction routes the paths below /revisions.
func (s *UIServer) revisionsAction(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		s.listRevisions(w, r, info, ns, name)
	case len(rest) == 1 && r.Method == http.MethodGet:
		s.getRevisionDetail(w, r, info, ns, name, rest[0])
	case len(rest) == 2 && rest[1] == actionRestore && r.Method == http.MethodPost:
		s.restoreRevision(w, r, info, ns, name, rest[0])
	case len(rest) == 0 || len(rest) == 1 || (len(rest) == 2 && rest[1] == actionRestore):
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.Error(w, `{"error":"unknown resource action"}`, http.StatusNotFound)
	}
}

const actionRestore = "restore"

func revisionsUnavailable(w http.ResponseWriter) {
	jsonError(w, revisionsNeedPostgres, http.StatusNotImplemented)
}

// revisionReadable checks that the caller may read the history of the object:
// the viewer role, a namespace of the caller, and either the live object
// readable by the caller or, if it is gone, the builder role. Revisions are
// scoped to the caller's tenant by the database, so a deleted object's history
// is its tenant's own. It answers the error itself and returns false.
func (s *UIServer) revisionReadable(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) bool {
	if !requireRole(w, r, tenancy.RoleViewer) {
		return false
	}
	if s.revisionSink() == nil {
		revisionsUnavailable(w)
		return false
	}
	if !namespaceAllowed(r.Context(), ns) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return false
	}
	live := info.newObj()
	switch err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, live); {
	case err == nil:
		if !canRead(r.Context(), live, info.gvk) {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return false
		}
	case k8serrors.IsNotFound(err):
		// Deleted: only a builder of the tenant that owned it may see what it was.
		return requireRole(w, r, tenancy.RoleBuilder)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	return true
}

func revisionNumber(w http.ResponseWriter, s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return 0, false
	}
	return n, true
}

// listRevisions handles GET .../revisions.
func (s *UIServer) listRevisions(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	if !s.revisionReadable(w, r, info, ns, name) {
		return
	}
	q := r.URL.Query()
	limit := defaultRevisionsPage
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxRevisionsPage {
			jsonError(w, fmt.Sprintf("limit must be between 1 and %d", maxRevisionsPage), http.StatusBadRequest)
			return
		}
		limit = n
	}
	query := postgresql.RevisionQuery{Kind: info.gvk, Namespace: ns, Name: name}
	if v := q.Get("before"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			jsonError(w, "invalid before cursor", http.StatusBadRequest)
			return
		}
		query.Before = n
	}
	// One extra row says whether another page exists.
	query.Limit = limit + 1
	revs, err := s.revisionSink().ListRevisions(pgScope(r.Context()), query)
	if err != nil {
		slog.Error("listing revisions", "err", err)
		jsonError(w, "listing revisions failed", http.StatusInternalServerError)
		return
	}
	page := revisionPage{Revisions: revs}
	if len(revs) > limit {
		page.Revisions = revs[:limit]
		page.Next = strconv.Itoa(revs[limit-1].Revision)
	}
	if page.Revisions == nil {
		page.Revisions = []postgresql.Revision{}
	}
	jsonResponse(w, page)
}

// getRevisionDetail handles GET .../revisions/{n}.
func (s *UIServer) getRevisionDetail(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name, num string) {
	if !s.revisionReadable(w, r, info, ns, name) {
		return
	}
	n, ok := revisionNumber(w, num)
	if !ok {
		return
	}
	rev, err := s.revisionSink().GetRevision(pgScope(r.Context()), postgresql.RevisionQuery{Kind: info.gvk, Namespace: ns, Name: name}, n)
	if errors.Is(err, postgresql.ErrRevisionNotFound) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("reading revision", "err", err)
		jsonError(w, "reading the revision failed", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, rev)
}

// restoreRequest is the optional body of a restore.
type restoreRequest struct {
	// ResourceVersion is the version of the live object the caller last read; a
	// different one is a 409.
	ResourceVersion string `json:"resourceVersion"`
}

// restoreRevision handles POST .../revisions/{n}/restore: it applies revision
// n's object to the live UI-owned object through the same path as an update,
// or, if the object was deleted and its name is free, creates it again.
//
//	404  no such revision, or the object is not the caller's
//	403  the object is platform-shared (read-only)
//	409  the live object is not UI-owned, or resourceVersion is stale
func (s *UIServer) restoreRevision(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name, num string) {
	if asDryRun(w, r, func(w http.ResponseWriter, r *http.Request) { s.restoreRevision(w, r, info, ns, name, num) }) {
		return
	}
	if !requireRole(w, r, writeRoleFor(info.gvk)) {
		return
	}
	if s.revisionSink() == nil {
		revisionsUnavailable(w)
		return
	}
	if !namespaceAllowed(r.Context(), ns) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	n, ok := revisionNumber(w, num)
	if !ok {
		return
	}
	var req restoreRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || (len(body) > 0 && json.Unmarshal(body, &req) != nil) {
		jsonError(w, `the body must be {"resourceVersion": "<optional>"}`, http.StatusBadRequest)
		return
	}
	rev, err := s.revisionSink().GetRevision(pgScope(r.Context()), postgresql.RevisionQuery{Kind: info.gvk, Namespace: ns, Name: name}, n)
	if errors.Is(err, postgresql.ErrRevisionNotFound) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("reading revision", "err", err)
		jsonError(w, "reading the revision failed", http.StatusInternalServerError)
		return
	}
	obj, err := objectOfRevision(rev, info.gvk, ns, name)
	if err != nil {
		jsonError(w, "the revision does not hold a usable object: "+err.Error(), http.StatusInternalServerError)
		return
	}

	existing := info.newObj()
	switch err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, existing); {
	case err == nil:
		s.restoreOverLive(w, r, info, existing, obj, n, req.ResourceVersion)
	case k8serrors.IsNotFound(err):
		s.restoreDeleted(w, r, info, obj, n, req.ResourceVersion)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// objectOfRevision turns a stored revision back into a writable object: named as
// the URL says, with nothing the server owns.
func objectOfRevision(rev postgresql.Revision, kind, ns, name string) (*unstructured.Unstructured, error) {
	var m map[string]any
	if err := json.Unmarshal(rev.Object, &m); err != nil || m == nil {
		return nil, errors.New("not a JSON object")
	}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		for _, f := range append([]string{"finalizers"}, uiMetadataDropped...) {
			delete(md, f)
		}
	}
	obj := &unstructured.Unstructured{Object: m}
	obj.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	obj.SetKind(kind)
	obj.SetName(name)
	obj.SetNamespace(ns)
	return obj, nil
}

// restoreOverLive applies the revision's object over the live, UI-owned one.
func (s *UIServer) restoreOverLive(w http.ResponseWriter, r *http.Request, info crdKindInfo, existing client.Object, obj *unstructured.Unstructured, n int, wantVersion string) {
	ns, name := existing.GetNamespace(), existing.GetName()
	if !s.authorizeWrite(w, r, existing, info.gvk) {
		return
	}
	if o := tenancy.Detect(existing, s.ownDetect); o.Owner != tenancy.OwnerUI {
		notEditable(w, info.gvk, ns, name, o, existing.GetResourceVersion())
		return
	}
	if wantVersion != "" && wantVersion != existing.GetResourceVersion() {
		writeConflict(w, fmt.Sprintf("%s %s/%s changed since you read it", info.gvk, ns, name), existing.GetResourceVersion())
		return
	}
	// Ownership is fixed by the stored object, not the revision.
	tenancy.StripOwnership(obj)
	if tid, _ := tenancy.TenantOf(existing); tid != "" {
		tenancy.Stamp(obj, tid)
	} else if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		tenancy.Stamp(obj, t.TenantID)
	}
	stampUI(r, obj)
	// A pending handoff is the owner's decision, not the revision's: a restore keeps it.
	if h := tenancy.HandoffOf(existing); h.Requested {
		a := obj.GetAnnotations()
		for _, k := range []string{tenancy.AnnotationHandoff, tenancy.AnnotationHandoffExpires} {
			if v, ok := existing.GetAnnotations()[k]; ok {
				a[k] = v
			}
		}
		obj.SetAnnotations(a)
	}
	// The apply names the version, so a write that lands since the read above is
	// refused by the API server rather than overwritten.
	obj.SetResourceVersion(existing.GetResourceVersion())
	if err := s.applyResource(r, obj); err != nil {
		if k8serrors.IsConflict(err) {
			current := ""
			if fresh := info.newObj(); s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, fresh) == nil {
				current = fresh.GetResourceVersion()
			}
			writeConflict(w, fmt.Sprintf("%s %s/%s changed since you read it", info.gvk, ns, name), current)
			return
		}
		writeAPIError(w, "restoring resource", err)
		return
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	s.recordWriteAs(w, r, obj, auditEventFor(postgresql.AuditUpdate, info.gvk, ns, name, obj.GetResourceVersion(),
		fmt.Sprintf("restored %s %s/%s to revision %d", info.gvk, ns, name, n)),
		revisionMeta{action: postgresql.RevisionRestore, restoredFrom: n})
	s.respondObject(w, r, obj, info.gvk)
}

// restoreDeleted creates the object again from the revision, if its name is free.
func (s *UIServer) restoreDeleted(w http.ResponseWriter, r *http.Request, info crdKindInfo, obj *unstructured.Unstructured, n int, wantVersion string) {
	ns, name := obj.GetNamespace(), obj.GetName()
	if wantVersion != "" {
		writeConflict(w, fmt.Sprintf("%s %s/%s was deleted since you read it", info.gvk, ns, name), "")
		return
	}
	if !s.prepareNew(w, r, obj) {
		return
	}
	if obj.GetNamespace() != ns || obj.GetName() != name {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	s.createUIObjectAs(w, r, info, obj, fmt.Sprintf("restored %s %s/%s to revision %d", info.gvk, ns, name, n),
		uiWrite{status: http.StatusOK, revision: revisionMeta{action: postgresql.RevisionRestore, restoredFrom: n}})
}
