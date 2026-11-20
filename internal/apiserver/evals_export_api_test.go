package apiserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func TestEvals_ExportIsBuilderOnlyAuditedAndTenantScoped(t *testing.T) {
	f := newEvalFixture(t)
	d := f.dataset("exportable")
	body := `{"input":"where is my refund","reference":"in 5 days","referenceKind":"example","tags":["refund"]}`
	if rec := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID), body); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	if rec := f.do(f.builder(), http.MethodPost, fmt.Sprintf("/api/evals/datasets/%d/cases", d.ID), `{"input":"second"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	path := fmt.Sprintf("/api/evals/datasets/%d/export", d.ID)
	before := f.auditCount()

	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		if rec := f.do(idFor(role, isoA), http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s export = %d, want 403", role, rec.Code)
		}
	}
	if rec := f.do(f.builder(), http.MethodPost, path, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST export = %d", rec.Code)
	}
	if f.auditCount() != before {
		t.Fatalf("a refused export was audited: %+v", f.audit.events[before:])
	}
	// Another tenant's dataset is a 404.
	if rec := f.do(idFor(tenancy.RoleAdmin, isoB), http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign export = %d, want 404", rec.Code)
	}
	if f.auditCount() != before {
		t.Fatalf("a foreign export was audited")
	}

	rec := f.do(f.builder(), http.MethodGet, path, "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/x-ndjson") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "exportable-v") {
		t.Fatalf("export = %d %v", rec.Code, rec.Header())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"input":"where is my refund"`) || !strings.Contains(lines[0], `"reference":"in 5 days"`) ||
		strings.Contains(rec.Body.String(), "tenantId") {
		t.Errorf("body = %q", rec.Body)
	}
	// The export imports back as JSON lines.
	if _, err := parseCaseLines(rec.Body.Bytes()); err != nil {
		t.Errorf("an export does not import: %v", err)
	}
	if n := f.auditCount() - before; n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	e := f.audit.events[len(f.audit.events)-1]
	if e.Action != postgresql.AuditExport || e.Kind != kindEvalDataset || strings.Contains(e.Summary, "refund") || !strings.Contains(e.Summary, "2 cases") {
		t.Errorf("event = %+v", e)
	}
}
