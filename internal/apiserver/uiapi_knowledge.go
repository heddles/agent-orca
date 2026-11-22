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
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Documents for a knowledge base, from the builder:
//
//	GET    /api/knowledge/{ns}/{name}/documents         viewer   names and sizes, never content
//	PUT    /api/knowledge/{ns}/{name}/documents/{doc}   builder   add or replace a text document, audited
//	DELETE /api/knowledge/{ns}/{name}/documents/{doc}   builder   remove a document, audited
//
// How knowledge is ingested today (docs/rag.md): a KnowledgeBase lists sources in
// spec.ingestion (ConfigMaps, URLs, S3, MCP) and an mcp-ingester Job reads them,
// extracts text, chunks, embeds and upserts into the KB's Qdrant. The platform
// has no object store of its own, and the ingester has no upload endpoint. The one
// source that needs no new infrastructure is a ConfigMap: the Job reads the string
// data keys of the ConfigMaps in spec.ingestion.configMapRefs, and a change to such
// a ConfigMap re-ingests the KB (the controller watches them).
//
// So a document added here is a key of one ConfigMap per KnowledgeBase,
// <kb>-docs, which the API creates (UI-owned, stamped with the tenant) and adds to
// the KB's configMapRefs. Only a UI-owned KnowledgeBase in the caller's tenant is
// touched: a Git-owned one is a 409, a platform-shared one a 403, another
// tenant's a 404. The limits are those of a ConfigMap, which is an etcd object
// of at most 1 MiB, so the total is capped well below it. The ingester reads
// string data only, so the types are text, markdown and csv; a PDF cannot be
// carried and is refused with a pointer to the S3 source an administrator can add.
//
// The content is untrusted and opaque: it is stored as given (valid UTF-8, no NUL
// bytes), never interpreted, never returned by any endpoint, never logged and
// never in an audit row. The audit summary holds the file name and size.

const (
	kindKnowledgeBase = "KnowledgeBase"
	// labelKnowledgeDocs marks the ConfigMap that holds a KnowledgeBase's documents; its value is the KB's name.
	labelKnowledgeDocs = "agentorca.io/knowledge-docs"
	// maxConfigMapBytes is Kubernetes' limit on a ConfigMap's size.
	maxConfigMapBytes = 1 << 20

	defaultKnowledgeDocBytes   = 256 << 10
	defaultKnowledgeDocs       = 50
	defaultKnowledgeTotalBytes = 900 << 10
)

// knowledgeExtensions are the types a document may have. The ingester reads a
// key's extension to pick the extractor; all of these are plain text.
var knowledgeExtensions = []string{".txt", ".md", ".markdown", ".csv"}

func (c BuilderConfig) knowledgeDocBytes() int {
	return orInt(c.KnowledgeMaxDocBytes, defaultKnowledgeDocBytes)
}
func (c BuilderConfig) knowledgeDocs() int { return orInt(c.KnowledgeMaxDocs, defaultKnowledgeDocs) }

// knowledgeTotal is the most all the documents of one KB may add up to: the
// configured value, never above what a ConfigMap can hold with room for keys.
func (c BuilderConfig) knowledgeTotal() int {
	return min(orInt(c.KnowledgeMaxTotalBytes, defaultKnowledgeTotalBytes), maxConfigMapBytes-(64<<10))
}

type knowledgeDoc struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

type knowledgeLimits struct {
	MaxDocBytes   int      `json:"maxDocBytes"`
	MaxDocs       int      `json:"maxDocs"`
	MaxTotalBytes int      `json:"maxTotalBytes"`
	Types         []string `json:"types"`
}

// knowledgeDocsView is the body of the list and of every change.
type knowledgeDocsView struct {
	Documents  []knowledgeDoc  `json:"documents"`
	TotalBytes int             `json:"totalBytes"`
	Limits     knowledgeLimits `json:"limits"`
	// Editable is true when the caller may add and remove documents here: the
	// builder role on a UI-owned KnowledgeBase of the tenant.
	Editable bool `json:"editable"`
	// Reason says why not, in plain words, when Editable is false.
	Reason string `json:"reason,omitempty"`
	// Warning is a caution about what a change does not do.
	Warning string `json:"warning,omitempty"`
}

func (s *UIServer) knowledgeLimits() knowledgeLimits {
	c := s.builderCfg
	return knowledgeLimits{MaxDocBytes: c.knowledgeDocBytes(), MaxDocs: c.knowledgeDocs(), MaxTotalBytes: c.knowledgeTotal(), Types: knowledgeExtensions}
}

func knowledgeDocsName(kb string) string { return kb + "-docs" }

// validDocName reports whether name is a file name this API stores: a plain name
// of letters, digits, dots, dashes and underscores with a text extension.
func validDocName(name string) bool {
	if len(name) == 0 || len(name) > 100 || name != path.Base(name) || strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return slices.Contains(knowledgeExtensions, strings.ToLower(path.Ext(name)))
}

// documentTextProblem says why content is not a text document, or "" when it is.
func documentTextProblem(b []byte) string {
	switch {
	case len(b) == 0:
		return "the document is empty"
	case !utf8.Valid(b):
		return "the document is not UTF-8 text: only text, markdown and csv files can be added here (a PDF or Word file is not supported)"
	case strings.ContainsRune(string(b), 0):
		return "the document has binary content: only text, markdown and csv files can be added here"
	}
	return ""
}

func (s *UIServer) registerKnowledgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/knowledge/{ns}/{name}/documents", s.handleKnowledgeDocuments)
	mux.HandleFunc("/api/knowledge/{ns}/{name}/documents/{doc}", s.handleKnowledgeDocument)
}

func (s *UIServer) handleKnowledgeDocuments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRole(w, r, tenancy.RoleViewer) {
		return
	}
	kb, ok := s.getKnowledgeBase(w, r, r.PathValue("ns"), r.PathValue("name"), false)
	if !ok {
		return
	}
	cm, err := s.docsConfigMap(r, kb)
	if err != nil {
		jsonError(w, "reading the documents failed", http.StatusInternalServerError)
		return
	}
	view := s.docsView(cm)
	view.Editable, view.Reason = s.docsEditable(r, kb)
	jsonResponse(w, view)
}

func (s *UIServer) handleKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.putKnowledgeDocument(w, r)
	case http.MethodDelete:
		s.deleteKnowledgeDocument(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// getKnowledgeBase reads a KnowledgeBase the caller may see. With write set it
// also requires that the caller may change it: not platform-shared, UI-owned, and
// no handoff to Git under way. It has answered the request when ok is false.
func (s *UIServer) getKnowledgeBase(w http.ResponseWriter, r *http.Request, ns, name string, write bool) (*agentorcav1alpha1.KnowledgeBase, bool) {
	if s.crdClient == nil {
		jsonError(w, "the cluster is not available", http.StatusServiceUnavailable)
		return nil, false
	}
	var kb agentorcav1alpha1.KnowledgeBase
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &kb); err != nil {
		notFoundOr(w, err)
		return nil, false
	}
	if !write {
		if !canRead(r.Context(), &kb, kindKnowledgeBase) {
			jsonError(w, "not found", http.StatusNotFound)
			return nil, false
		}
		return &kb, true
	}
	if !s.authorizeWrite(w, r, &kb, kindKnowledgeBase) {
		return nil, false
	}
	if o := tenancy.Detect(&kb, s.ownDetect); o.Owner != tenancy.OwnerUI {
		notEditable(w, kindKnowledgeBase, ns, name, o, kb.ResourceVersion)
		return nil, false
	}
	if tenancy.HandoffOf(&kb).Requested {
		writeConflict(w, fmt.Sprintf("KnowledgeBase %s/%s is being handed over to Git: documents cannot be added until that ends", ns, name), kb.ResourceVersion)
		return nil, false
	}
	return &kb, true
}

// docsEditable reports whether the caller may add documents to kb, and why not.
func (s *UIServer) docsEditable(r *http.Request, kb *agentorcav1alpha1.KnowledgeBase) (bool, string) {
	switch {
	case !hasRole(r.Context(), tenancy.RoleBuilder):
		return false, "Adding documents needs the builder role."
	case !canWrite(r.Context(), kb, kindKnowledgeBase):
		return false, "This knowledge is shared by the platform and cannot be changed here."
	case tenancy.Detect(kb, s.ownDetect).Owner != tenancy.OwnerUI:
		return false, "This knowledge is managed in Git (or outside this UI), so documents are added there."
	case tenancy.HandoffOf(kb).Requested:
		return false, "This knowledge is being handed over to Git."
	}
	return true, ""
}

// docsConfigMap returns the KB's documents ConfigMap, nil when there is none. A
// ConfigMap of that name that is not this KB's (no label) is an error: it is not
// ours to read or overwrite.
func (s *UIServer) docsConfigMap(r *http.Request, kb *agentorcav1alpha1.KnowledgeBase) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: kb.Namespace, Name: knowledgeDocsName(kb.Name)}, &cm)
	if k8serrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cm.Labels[labelKnowledgeDocs] != kb.Name {
		return nil, fmt.Errorf("a ConfigMap named %s exists and is not this knowledge base's documents", cm.Name)
	}
	return &cm, nil
}

func (s *UIServer) docsView(cm *corev1.ConfigMap) knowledgeDocsView {
	v := knowledgeDocsView{Documents: []knowledgeDoc{}, Limits: s.knowledgeLimits()}
	if cm == nil {
		return v
	}
	for k, val := range cm.Data {
		v.Documents = append(v.Documents, knowledgeDoc{Name: k, Bytes: len(val)})
		v.TotalBytes += len(val)
	}
	sort.Slice(v.Documents, func(i, j int) bool { return v.Documents[i].Name < v.Documents[j].Name })
	return v
}

const docsDeleteWarning = "Removing a document stops it being read the next time the knowledge base is ingested. Text already embedded from it stays in the knowledge base until the knowledge base is rebuilt."

func (s *UIServer) putKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	if !requireRole(w, r, tenancy.RoleBuilder) {
		return
	}
	ns, name, doc := r.PathValue("ns"), r.PathValue("name"), r.PathValue("doc")
	if !validDocName(doc) {
		jsonError(w, "the file name must be letters, digits, dots, dashes and underscores, at most 100 characters, ending in "+strings.Join(knowledgeExtensions, ", "), http.StatusBadRequest)
		return
	}
	kb, ok := s.getKnowledgeBase(w, r, ns, name, true)
	if !ok {
		return
	}
	limit := s.builderCfg.knowledgeDocBytes()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		jsonError(w, fmt.Sprintf("the document is over %d bytes", limit), http.StatusRequestEntityTooLarge)
		return
	}
	if problem := documentTextProblem(body); problem != "" {
		jsonError(w, problem, http.StatusUnsupportedMediaType)
		return
	}
	cm, err := s.docsConfigMap(r, kb)
	if err != nil {
		writeConflict(w, err.Error(), "")
		return
	}
	next := map[string]string{}
	if cm != nil {
		maps.Copy(next, cm.Data)
	}
	_, replacing := next[doc]
	next[doc] = string(body)
	if !replacing && len(next) > s.builderCfg.knowledgeDocs() {
		writeConflict(w, fmt.Sprintf("a knowledge base takes at most %d documents here: remove one first", s.builderCfg.knowledgeDocs()), "")
		return
	}
	total := 0
	for _, v := range next {
		total += len(v)
	}
	if total > s.builderCfg.knowledgeTotal() {
		jsonError(w, fmt.Sprintf("the documents of one knowledge base may add up to %d bytes here, and this would make %d", s.builderCfg.knowledgeTotal(), total), http.StatusRequestEntityTooLarge)
		return
	}
	if err := s.saveDocs(r, kb, cm, next); err != nil {
		s.docsWriteFailed(w, err)
		return
	}
	verb := "added"
	if replacing {
		verb = "replaced"
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindKnowledgeBase, ns, name, kb.ResourceVersion,
		fmt.Sprintf("%s document %s (%d bytes) in knowledge base %s", verb, doc, len(body), name)))
	cm2 := &corev1.ConfigMap{Data: next}
	view := s.docsView(cm2)
	view.Editable = true
	status := http.StatusCreated
	if replacing {
		status = http.StatusOK
		view.Warning = "The document was replaced. Text from the older version that is no longer in it stays embedded until the knowledge base is rebuilt."
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(view)
}

func (s *UIServer) deleteKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	if !requireRole(w, r, tenancy.RoleBuilder) {
		return
	}
	ns, name, doc := r.PathValue("ns"), r.PathValue("name"), r.PathValue("doc")
	kb, ok := s.getKnowledgeBase(w, r, ns, name, true)
	if !ok {
		return
	}
	cm, err := s.docsConfigMap(r, kb)
	if err != nil {
		writeConflict(w, err.Error(), "")
		return
	}
	if cm == nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if _, ok := cm.Data[doc]; !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	size := len(cm.Data[doc])
	next := map[string]string{}
	for k, v := range cm.Data {
		if k != doc {
			next[k] = v
		}
	}
	if err := s.saveDocs(r, kb, cm, next); err != nil {
		s.docsWriteFailed(w, err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditUpdate, kindKnowledgeBase, ns, name, kb.ResourceVersion,
		fmt.Sprintf("removed document %s (%d bytes) from knowledge base %s", doc, size, name)))
	view := s.docsView(&corev1.ConfigMap{Data: next})
	view.Editable = true
	view.Warning = docsDeleteWarning
	jsonResponse(w, view)
}

func (s *UIServer) docsWriteFailed(w http.ResponseWriter, err error) {
	if k8serrors.IsConflict(err) || k8serrors.IsAlreadyExists(err) {
		writeConflict(w, "the documents changed while you were adding yours: try again", "")
		return
	}
	writeAPIError(w, "saving the documents", err)
}

// saveDocs writes the documents ConfigMap (creating it on the first document)
// and makes sure the KnowledgeBase lists it as a source. The ConfigMap is written
// with the version that was read, so two writers cannot overwrite each other.
func (s *UIServer) saveDocs(r *http.Request, kb *agentorcav1alpha1.KnowledgeBase, cm *corev1.ConfigMap, data map[string]string) error {
	ctx := r.Context()
	if cm == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: knowledgeDocsName(kb.Name), Namespace: kb.Namespace,
			Labels:      map[string]string{labelKnowledgeDocs: kb.Name},
			Annotations: map[string]string{},
		}}
		if tid, _ := tenancy.TenantOf(kb); tid != "" {
			tenancy.Stamp(cm, tid)
		}
		stampUI(r, cm)
		cm.Data = data
		if err := s.crdClient.Create(ctx, cm, client.FieldOwner(tenancy.FieldManagerUI)); err != nil {
			return err
		}
	} else {
		cm.Data = data
		stampUI(r, cm)
		if err := s.crdClient.Update(ctx, cm, client.FieldOwner(tenancy.FieldManagerUI)); err != nil {
			return err
		}
	}
	return s.ensureDocsSource(r, kb, cm.Name)
}

// ensureDocsSource adds the documents ConfigMap to the KnowledgeBase's
// ingestion sources when it is not there yet; the KB is otherwise left alone.
func (s *UIServer) ensureDocsSource(r *http.Request, kb *agentorcav1alpha1.KnowledgeBase, cmName string) error {
	if kb.Spec.Ingestion != nil {
		for _, ref := range kb.Spec.Ingestion.ConfigMapRefs {
			if ref.Name == cmName {
				return nil
			}
		}
	}
	orig := kb.DeepCopy()
	if kb.Spec.Ingestion == nil {
		kb.Spec.Ingestion = &agentorcav1alpha1.IngestionConfig{}
	}
	kb.Spec.Ingestion.ConfigMapRefs = append(kb.Spec.Ingestion.ConfigMapRefs, agentorcav1alpha1.LocalObjectRef{Name: cmName})
	stampUI(r, kb)
	return s.crdClient.Patch(r.Context(), kb, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}), client.FieldOwner(tenancy.FieldManagerUI))
}
