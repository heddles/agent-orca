package apiserver

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const docSecret = "CONFIDENTIAL-PRICING-TEXT-QWERTY"

func newKnowledgeFixture(t *testing.T) *builderFixture {
	t.Helper()
	mine := func(owner string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}, Annotations: map[string]string{tenancy.AnnotationOwner: owner}}
	}
	named := func(m metav1.ObjectMeta, name string) metav1.ObjectMeta { m.Name = name; return m }
	return newBuilderFixture(t,
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: named(mine(tenancy.OwnerUI), "handbook")},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: named(mine("git"), "from-git")},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: named(mine(tenancy.OwnerUI), "pending-handoff")},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "theirs", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoB}, Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}}},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "platform-kb", Namespace: platformNS, Labels: platformLabels(), Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}}},
		// A ConfigMap that is not ours sits where the documents would go.
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: named(mine(tenancy.OwnerUI), "taken")},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "taken-docs", Namespace: sharedNS}, Data: map[string]string{"x": "y"}},
	)
}

func (f *builderFixture) putDoc(id *TenantIdentity, kb, doc, body string) *httptest.ResponseRecorder {
	return f.serve(id, http.MethodPut, "/api/knowledge/"+sharedNS+"/"+kb+"/documents/"+doc, body)
}

func (f *builderFixture) docsOf(kb string) *corev1.ConfigMap {
	f.t.Helper()
	var cm corev1.ConfigMap
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: kb + "-docs"}, &cm); err != nil {
		return nil
	}
	return &cm
}

func TestKnowledgeDocuments_AddListReplaceRemove(t *testing.T) {
	f := newKnowledgeFixture(t)
	b := f.as(tenancy.RoleBuilder)
	rec := f.putDoc(b, "handbook", "returns.md", "# Returns\n"+docSecret)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	view := decode[knowledgeDocsView](t, rec)
	if len(view.Documents) != 1 || view.Documents[0].Name != "returns.md" || view.TotalBytes != len("# Returns\n"+docSecret) || !view.Editable {
		t.Errorf("view = %+v", view)
	}
	// The ConfigMap is ours: labelled, tenant-stamped, UI-owned; and the KB lists it as a source.
	cm := f.docsOf("handbook")
	if cm == nil || cm.Labels[labelKnowledgeDocs] != "handbook" || cm.Labels[tenancy.LabelTenantID] != isoA || cm.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI || cm.Data["returns.md"] == "" {
		t.Fatalf("configmap = %+v", cm)
	}
	var kb agentorcav1alpha1.KnowledgeBase
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "handbook"}, &kb); err != nil ||
		kb.Spec.Ingestion == nil || len(kb.Spec.Ingestion.ConfigMapRefs) != 1 || kb.Spec.Ingestion.ConfigMapRefs[0].Name != "handbook-docs" {
		t.Errorf("kb ingestion = %+v (%v)", kb.Spec.Ingestion, err)
	}
	// A second document, and a replacement of the first; the source is listed once.
	if rec := f.putDoc(b, "handbook", "faq.csv", "q,a\n1,2"); rec.Code != http.StatusCreated {
		t.Fatalf("second = %d %s", rec.Code, rec.Body)
	}
	rec = f.putDoc(b, "handbook", "returns.md", "# Returns v2")
	if rec.Code != http.StatusOK || !strings.Contains(decode[knowledgeDocsView](t, rec).Warning, "stays embedded") {
		t.Errorf("replace = %d %s", rec.Code, rec.Body)
	}
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "handbook"}, &kb)
	if len(kb.Spec.Ingestion.ConfigMapRefs) != 1 {
		t.Errorf("refs = %+v", kb.Spec.Ingestion.ConfigMapRefs)
	}

	f.checkListAndRemove(t)
}

func (f *builderFixture) checkListAndRemove(t *testing.T) {
	t.Helper()
	b := f.as(tenancy.RoleBuilder)
	// The list shows names and sizes, never content; a viewer may read it but is not editable.
	lst := f.serve(f.as(tenancy.RoleViewer), http.MethodGet, "/api/knowledge/"+sharedNS+"/handbook/documents", "")
	if lst.Code != http.StatusOK || strings.Contains(lst.Body.String(), "Returns") || strings.Contains(lst.Body.String(), "q,a") {
		t.Fatalf("list = %d %s", lst.Code, lst.Body)
	}
	if v := decode[knowledgeDocsView](t, lst); len(v.Documents) != 2 || v.Editable || v.Reason == "" || v.Limits.MaxDocs != 50 {
		t.Errorf("list view = %+v", v)
	}
	if v := decode[knowledgeDocsView](t, f.serve(b, http.MethodGet, "/api/knowledge/"+sharedNS+"/handbook/documents", "")); !v.Editable {
		t.Errorf("builder view = %+v", v)
	}

	// Remove: the key goes, with a warning that embedded text stays.
	del := f.serve(b, http.MethodDelete, "/api/knowledge/"+sharedNS+"/handbook/documents/faq.csv", "")
	if v := decode[knowledgeDocsView](t, del); del.Code != http.StatusOK || len(v.Documents) != 1 || !strings.Contains(v.Warning, "already embedded") {
		t.Errorf("delete = %d %+v", del.Code, v)
	}
	if rec := f.serve(b, http.MethodDelete, "/api/knowledge/"+sharedNS+"/handbook/documents/nope.md", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete missing = %d", rec.Code)
	}
}

func TestKnowledgeDocuments_OwnershipRolesAndTenants(t *testing.T) {
	f := newKnowledgeFixture(t)
	b := f.as(tenancy.RoleBuilder)
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		if rec := f.putDoc(f.as(role), "handbook", "a.md", "text"); rec.Code != http.StatusForbidden {
			t.Errorf("%s put = %d", role, rec.Code)
		}
		if rec := f.serve(f.as(role), http.MethodDelete, "/api/knowledge/"+sharedNS+"/handbook/documents/a.md", ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s delete = %d", role, rec.Code)
		}
	}
	// Git-owned: refused (409), and nothing is created or changed.
	if rec := f.putDoc(b, "from-git", "a.md", "text"); rec.Code != http.StatusConflict {
		t.Errorf("Git-owned = %d %s", rec.Code, rec.Body)
	}
	if f.docsOf("from-git") != nil {
		t.Error("a ConfigMap was made for a Git-owned knowledge base")
	}
	var git agentorcav1alpha1.KnowledgeBase
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "from-git"}, &git)
	if git.Spec.Ingestion != nil {
		t.Error("the Git-owned knowledge base was edited")
	}
	view := decode[knowledgeDocsView](t, f.serve(b, http.MethodGet, "/api/knowledge/"+sharedNS+"/from-git/documents", ""))
	if view.Editable || !strings.Contains(view.Reason, "Git") {
		t.Errorf("Git-owned view = %+v", view)
	}
	// Another tenant's is a 404, a platform-shared one a 403, a missing one a 404.
	if rec := f.putDoc(b, "theirs", "a.md", "text"); rec.Code != http.StatusNotFound {
		t.Errorf("foreign = %d", rec.Code)
	}
	if rec := f.serve(b, http.MethodGet, "/api/knowledge/"+sharedNS+"/theirs/documents", ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign list = %d", rec.Code)
	}
	if rec := f.serve(b, http.MethodPut, "/api/knowledge/"+platformNS+"/platform-kb/documents/a.md", "text"); rec.Code != http.StatusForbidden {
		t.Errorf("platform-shared = %d %s", rec.Code, rec.Body)
	}
	if rec := f.putDoc(b, "ghost", "a.md", "text"); rec.Code != http.StatusNotFound {
		t.Errorf("missing = %d", rec.Code)
	}
	// A pending handoff to Git blocks it too.
	var pending agentorcav1alpha1.KnowledgeBase
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "pending-handoff"}, &pending)
	pending.Annotations[tenancy.AnnotationHandoff] = tenancy.HandoffPending
	pending.Annotations[tenancy.AnnotationHandoffExpires] = "2099-01-01T00:00:00Z"
	if err := f.c.Update(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	if rec := f.putDoc(b, "pending-handoff", "a.md", "text"); rec.Code != http.StatusConflict {
		t.Errorf("pending handoff = %d", rec.Code)
	}
	// A ConfigMap of the name that is not ours is never overwritten.
	if rec := f.putDoc(b, "taken", "a.md", "text"); rec.Code != http.StatusConflict {
		t.Errorf("taken name = %d %s", rec.Code, rec.Body)
	}
	var cm corev1.ConfigMap
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "taken-docs"}, &cm)
	if len(cm.Data) != 1 || cm.Data["x"] != "y" {
		t.Errorf("a foreign ConfigMap changed: %+v", cm.Data)
	}
	if len(f.audit.events) != 0 {
		t.Errorf("refused requests wrote audit rows: %+v", f.audit.events)
	}
}

func TestKnowledgeDocuments_LimitsAndTypes(t *testing.T) {
	f := newKnowledgeFixture(t)
	b := f.as(tenancy.RoleBuilder)
	f.s.SetBuilder(BuilderConfig{KnowledgeMaxDocBytes: 100, KnowledgeMaxDocs: 2, KnowledgeMaxTotalBytes: 150})
	cases := []struct {
		name, doc, body string
		want            int
	}{
		{"pdf extension", "a.pdf", "text", http.StatusBadRequest},
		{"html extension", "a.html", "<b>x</b>", http.StatusBadRequest},
		{"no extension", "readme", "text", http.StatusBadRequest},
		{"hidden", ".env.md", "text", http.StatusBadRequest},
		{"traversal", "..%2Fx.md", "text", http.StatusBadRequest},
		{"odd characters", "a%20b.md", "text", http.StatusBadRequest},
		{"too big", "big.md", strings.Repeat("x", 101), http.StatusRequestEntityTooLarge},
		{"binary", "bin.txt", "PK\x00\x03\x04", http.StatusUnsupportedMediaType},
		{"pdf bytes under a text name", "doc.txt", "%PDF-1.7\n\xff\xfe\x00", http.StatusUnsupportedMediaType},
		{"not utf-8", "latin.txt", "caf\xe9", http.StatusUnsupportedMediaType},
		{"empty", "empty.txt", "", http.StatusUnsupportedMediaType},
		{"ok", "one.txt", strings.Repeat("a", 90), http.StatusCreated},
		{"total over", "two.txt", strings.Repeat("b", 90), http.StatusRequestEntityTooLarge},
		{"ok small", "three.md", "small", http.StatusCreated},
		{"count over", "four.md", "small", http.StatusConflict},
	}
	for _, c := range cases {
		if rec := f.putDoc(b, "handbook", c.doc, c.body); rec.Code != c.want {
			t.Errorf("%s = %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
	}
	if cm := f.docsOf("handbook"); cm == nil || len(cm.Data) != 2 {
		t.Errorf("stored = %+v", cm)
	}
	// The ConfigMap ceiling holds whatever is configured.
	if got := (BuilderConfig{KnowledgeMaxTotalBytes: 50 << 20}).knowledgeTotal(); got >= maxConfigMapBytes {
		t.Errorf("total = %d", got)
	}
	if rec := f.serve(b, http.MethodPost, "/api/knowledge/"+sharedNS+"/handbook/documents/a.md", "x"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

func TestKnowledgeDocuments_ContentIsNeverLoggedAuditedOrReturned(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newKnowledgeFixture(t)
	b := f.as(tenancy.RoleBuilder)
	rec := f.putDoc(b, "handbook", "pricing.md", "<script>alert(1)</script>\n"+docSecret)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	f.putDoc(b, "handbook", "pricing.md", docSecret+" v2")
	f.serve(b, http.MethodDelete, "/api/knowledge/"+sharedNS+"/handbook/documents/pricing.md", "")
	if len(f.audit.events) != 3 {
		t.Fatalf("audit rows = %d, want 3 (add, replace, remove): %+v", len(f.audit.events), f.audit.events)
	}
	for _, e := range f.audit.events {
		if strings.Contains(e.Summary, docSecret) || strings.Contains(e.Summary, "script") {
			t.Errorf("the audit row holds content: %q", e.Summary)
		}
		if e.Kind != kindKnowledgeBase || e.Name != "handbook" || !strings.Contains(e.Summary, "pricing.md") || !strings.Contains(e.Summary, "bytes") {
			t.Errorf("audit = %+v", e)
		}
	}
	if strings.Contains(rec.Body.String(), docSecret) {
		t.Error("the answer holds the content")
	}
	if strings.Contains(logs.String(), docSecret) || strings.Contains(logs.String(), "script") {
		t.Errorf("the log holds content: %s", logs.String())
	}
}

func TestValidDocName(t *testing.T) {
	for name, want := range map[string]bool{
		"a.md": true, "Report_2024-final.TXT": true, "data.csv": true, "x.markdown": true,
		"a.pdf": false, "a.md.exe": false, "": false, "../a.md": false, "a/b.md": false, ".hidden.md": false, "a b.md": false,
		strings.Repeat("a", 98) + ".md": false, "é.md": false,
	} {
		if got := validDocName(name); got != want {
			t.Errorf("validDocName(%q) = %v, want %v", name, got, want)
		}
	}
}
