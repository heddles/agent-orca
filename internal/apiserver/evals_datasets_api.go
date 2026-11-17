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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/redact"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Evaluation datasets and cases (docs/evaluations.md).
//
//	GET    /api/evals/datasets                      viewer
//	POST   /api/evals/datasets                      builder, audited
//	GET    /api/evals/datasets/{id}                 viewer
//	PATCH  /api/evals/datasets/{id}                 builder, audited
//	DELETE /api/evals/datasets/{id}                 builder, audited
//	GET    /api/evals/datasets/{id}/cases           viewer
//	POST   /api/evals/datasets/{id}/cases           builder, audited (one case, or JSON lines)
//	GET    /api/evals/cases/{id}                    viewer
//	PATCH  /api/evals/cases/{id}                    builder, audited
//	DELETE /api/evals/cases/{id}                    builder, audited
//	POST   /api/runs/{ns}/{name}/dataset            builder, audited (copy a run into a dataset)
//
// All of it is tenant-scoped through the store (row-level security) and, inside
// the tenant, to the namespaces the caller may use. Without PostgreSQL every
// endpoint answers 503. Audit rows carry dataset names and counts, never case text.

// maxCasesPerCall is the most cases one POST may add.
const maxCasesPerCall = 200

const evalsNeedPostgres = "evaluations need the PostgreSQL store"

// EvalsConfig holds the operator-level limits on evaluation data (the chart's
// operator.evals values). The zero value means the defaults.
type EvalsConfig struct {
	// MaxCasesPerDataset is the most live cases a dataset may hold. Default 2000.
	MaxCasesPerDataset int
	// MaxCaseBytes is the size limit of a case's input and of its reference, each.
	// Default and ceiling 65536.
	MaxCaseBytes int
	// DisallowUnredactedCases refuses a capture with redact:false.
	DisallowUnredactedCases bool
}

func (c EvalsConfig) limits() postgresql.EvalLimits {
	return postgresql.EvalLimits{MaxCasesPerDataset: c.MaxCasesPerDataset, MaxCaseBytes: c.MaxCaseBytes}
}

// SetEvals sets the limits on evaluation data.
func (s *UIServer) SetEvals(c EvalsConfig) { s.evalCfg = c }

// evalStore is the slice of postgresql.Store the dataset endpoints use.
type evalStore interface {
	evalGraderStore
	CreateEvalDataset(ctx context.Context, d postgresql.EvalDataset) (postgresql.EvalDataset, error)
	GetEvalDataset(ctx context.Context, id int64) (postgresql.EvalDataset, error)
	ListEvalDatasets(ctx context.Context, q postgresql.EvalDatasetQuery) ([]postgresql.EvalDataset, error)
	UpdateEvalDataset(ctx context.Context, id int64, p postgresql.EvalDatasetPatch) (postgresql.EvalDataset, error)
	DeleteEvalDataset(ctx context.Context, id int64) (postgresql.EvalDataset, error)
	AddEvalCases(ctx context.Context, datasetID int64, cases []postgresql.EvalCase, l postgresql.EvalLimits) ([]postgresql.EvalCase, int, error)
	ListEvalCases(ctx context.Context, datasetID int64, q postgresql.EvalCaseQuery) ([]postgresql.EvalCase, error)
	GetEvalCase(ctx context.Context, id int64) (postgresql.EvalCase, error)
	UpdateEvalCase(ctx context.Context, id int64, p postgresql.EvalCasePatch, l postgresql.EvalLimits) (postgresql.EvalCase, int, bool, error)
	DeleteEvalCase(ctx context.Context, id int64) (int, error)
	// GetRun reads an archived run, for "add to dataset".
	GetRun(ctx context.Context, namespace, name string) (*postgresql.RunArchive, error)
}

// evalSource returns the evaluation store, or nil without PostgreSQL.
func (s *UIServer) evalSource() evalStore {
	if s.evals != nil {
		return s.evals
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) registerEvalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/evals/datasets", s.handleEvalDatasets)
	mux.HandleFunc("/api/evals/datasets/", s.handleEvalDataset)
	mux.HandleFunc("/api/evals/cases/", s.handleEvalCase)
	mux.HandleFunc("/api/evals/results/", s.handleEvalResult)
}

// evalPreflight checks the role and the store. It reports whether to go on.
func (s *UIServer) evalPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (evalStore, bool) {
	if !requireRole(w, r, role) {
		return nil, false
	}
	store := s.evalSource()
	if store == nil {
		jsonError(w, evalsNeedPostgres, http.StatusServiceUnavailable)
		return nil, false
	}
	return store, true
}

// evalFail answers a store error with the status that fits it.
func evalFail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, postgresql.ErrEvalNotFound):
		jsonError(w, "not found", http.StatusNotFound)
	case errors.Is(err, postgresql.ErrEvalExists):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, postgresql.ErrDatasetFull):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, postgresql.ErrEvalTooLarge):
		jsonError(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, postgresql.ErrInvalidEval):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, postgresql.ErrNoTenantScope):
		jsonError(w, "evaluations need a tenant identity", http.StatusForbidden)
	default:
		slog.Error(what, "err", err)
		jsonError(w, what+" failed", http.StatusInternalServerError)
	}
}

// evalAudit records a write on a dataset. Summaries hold counts and names only.
func (s *UIServer) evalAudit(w http.ResponseWriter, r *http.Request, action, namespace, name string, version int, summary string) {
	s.recordAudit(w, r, auditEventFor(action, kindEvalDataset, namespace, name, strconv.Itoa(version), summary))
}

const kindEvalDataset = "EvalDataset"

// parseEvalID reads a numeric ID from a path segment.
func parseEvalID(seg string) (int64, bool) {
	id, err := strconv.ParseInt(seg, 10, 64)
	return id, err == nil && id > 0
}

// loadDataset reads a dataset the caller may use: another tenant's is not
// found (the store's scope), and neither is one in a namespace the caller may
// not use.
func (s *UIServer) loadDataset(w http.ResponseWriter, r *http.Request, store evalStore, id int64) (postgresql.EvalDataset, bool) {
	d, err := store.GetEvalDataset(pgScope(r.Context()), id)
	if err == nil && !namespaceAllowed(r.Context(), d.Namespace) {
		err = postgresql.ErrEvalNotFound
	}
	if err != nil {
		evalFail(w, "loading the dataset", err)
		return postgresql.EvalDataset{}, false
	}
	return d, true
}

func readJSONBody(w http.ResponseWriter, r *http.Request, max int64, into any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		jsonError(w, fmt.Sprintf("the body is unreadable or over %d bytes", max), http.StatusRequestEntityTooLarge)
		return false
	}
	if err := decodeStrict(body, into); err != nil {
		jsonError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// decodeStrict decodes one JSON value and rejects unknown fields and trailing text.
func decodeStrict(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected text after the JSON value")
	}
	return nil
}

// ── datasets ────────────────────────────────────────────────────────────────

// handleEvalDatasets serves GET and POST /api/evals/datasets.
func (s *UIServer) handleEvalDatasets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEvalDatasets(w, r)
	case http.MethodPost:
		s.createEvalDataset(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) listEvalDatasets(w http.ResponseWriter, r *http.Request) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	q := postgresql.EvalDatasetQuery{Namespace: r.URL.Query().Get("namespace"), Agent: r.URL.Query().Get("agent")}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		if q.Namespace != "" && !namespaceAllowed(r.Context(), q.Namespace) {
			jsonError(w, "namespace not allowed", http.StatusForbidden)
			return
		}
		q.Namespaces = tenantNamespaces(t)
		if q.Namespaces == nil {
			q.Namespaces = []string{}
		}
	}
	list, err := store.ListEvalDatasets(pgScope(r.Context()), q)
	if err != nil {
		evalFail(w, "listing datasets", err)
		return
	}
	jsonResponse(w, map[string]any{"datasets": list})
}

type createDatasetRequest struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent,omitempty"`
}

func (s *UIServer) createEvalDataset(w http.ResponseWriter, r *http.Request) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req createDatasetRequest
	if !readJSONBody(w, r, 64<<10, &req) {
		return
	}
	if req.Namespace == "" {
		if t, ok := TenantFromContext(r.Context()); ok && t != nil {
			req.Namespace = firstNamespace(tenantNamespaces(t))
		}
	}
	if req.Namespace == "" {
		jsonError(w, "namespace is required", http.StatusBadRequest)
		return
	}
	if !namespaceAllowed(r.Context(), req.Namespace) {
		jsonError(w, "namespace not allowed", http.StatusForbidden)
		return
	}
	d := postgresql.EvalDataset{
		Namespace: req.Namespace, Name: req.Name, Description: strings.TrimSpace(req.Description),
		Agent: req.Agent, CreatedBy: actorOf(r.Context()),
	}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		d.TenantID = t.TenantID
	}
	created, err := store.CreateEvalDataset(pgScope(r.Context()), d)
	if err != nil {
		evalFail(w, "creating the dataset", err)
		return
	}
	s.evalAudit(w, r, postgresql.AuditCreate, created.Namespace, created.Name, created.Version, "created dataset")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// handleEvalDataset serves /api/evals/datasets/{id} and /api/evals/datasets/{id}/cases.
func (s *UIServer) handleEvalDataset(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/evals/datasets/"), "/"), "/")
	id, ok := parseEvalID(parts[0])
	if !ok || len(parts) > 3 {
		http.NotFound(w, r)
		return
	}
	if len(parts) >= 2 && parts[1] != "cases" {
		s.handleEvalDatasetSub(w, r, id, parts[1:])
		return
	}
	if len(parts) == 3 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			s.listEvalCases(w, r, id)
		case http.MethodPost:
			s.addEvalCases(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getEvalDataset(w, r, id)
	case http.MethodPatch:
		s.patchEvalDataset(w, r, id)
	case http.MethodDelete:
		s.deleteEvalDataset(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) getEvalDataset(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	if d, ok := s.loadDataset(w, r, store, id); ok {
		jsonResponse(w, d)
	}
}

type patchDatasetRequest struct {
	Description *string `json:"description"`
	Agent       *string `json:"agent"`
}

func (s *UIServer) patchEvalDataset(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req patchDatasetRequest
	if !readJSONBody(w, r, 64<<10, &req) {
		return
	}
	if _, ok := s.loadDataset(w, r, store, id); !ok {
		return
	}
	if req.Description != nil {
		d := strings.TrimSpace(*req.Description)
		req.Description = &d
	}
	d, err := store.UpdateEvalDataset(pgScope(r.Context()), id, postgresql.EvalDatasetPatch{Description: req.Description, Agent: req.Agent})
	if err != nil {
		evalFail(w, "updating the dataset", err)
		return
	}
	s.evalAudit(w, r, postgresql.AuditUpdate, d.Namespace, d.Name, d.Version, "changed the dataset's description or agent")
	jsonResponse(w, d)
}

func (s *UIServer) deleteEvalDataset(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	if _, ok := s.loadDataset(w, r, store, id); !ok {
		return
	}
	d, err := store.DeleteEvalDataset(pgScope(r.Context()), id)
	if err != nil {
		evalFail(w, "deleting the dataset", err)
		return
	}
	s.evalAudit(w, r, postgresql.AuditDelete, d.Namespace, d.Name, d.Version,
		fmt.Sprintf("deleted the dataset with %d cases", d.CaseCount))
	w.WriteHeader(http.StatusNoContent)
}

// ── cases ───────────────────────────────────────────────────────────────────

// casePage is the body of GET .../cases.
type casePage struct {
	Cases []postgresql.EvalCase `json:"cases"`
	// NextCursor is the value to pass as ?cursor= for the next page. Empty on the last.
	NextCursor string `json:"nextCursor,omitempty"`
	// DatasetVersion is the version the page was read at.
	DatasetVersion int `json:"datasetVersion"`
}

func (s *UIServer) listEvalCases(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	q := postgresql.EvalCaseQuery{Tag: r.URL.Query().Get("tag"), Limit: 50}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > postgresql.MaxEvalCasesPerPage {
			jsonError(w, fmt.Sprintf("limit must be between 1 and %d", postgresql.MaxEvalCasesPerPage), http.StatusBadRequest)
			return
		}
		q.Limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			jsonError(w, "cursor is not one this API gave", http.StatusBadRequest)
			return
		}
		q.After = n
	}
	d, ok := s.loadDataset(w, r, store, datasetID)
	if !ok {
		return
	}
	// One more than the page, to know whether there is a next one.
	want := q.Limit
	q.Limit = want + 1
	list, err := store.ListEvalCases(pgScope(r.Context()), datasetID, q)
	if err != nil {
		evalFail(w, "listing cases", err)
		return
	}
	page := casePage{Cases: list, DatasetVersion: d.Version}
	if len(list) > want {
		page.Cases = list[:want]
		page.NextCursor = strconv.FormatInt(list[want-1].ID, 10)
	}
	jsonResponse(w, page)
}

// caseInput is one case in a request: a JSON body, or one line of JSON lines.
type caseInput struct {
	Input string `json:"input"`
	// Expected is the expected answer, a shorter name for Reference. Give one.
	Expected      *string  `json:"expected,omitempty"`
	Reference     *string  `json:"reference,omitempty"`
	ReferenceKind string   `json:"referenceKind,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Weight        *float64 `json:"weight,omitempty"`
}

func (c caseInput) toCase() (postgresql.EvalCase, error) {
	if c.Expected != nil && c.Reference != nil {
		return postgresql.EvalCase{}, errors.New("give expected or reference, not both")
	}
	ref := c.Reference
	if ref == nil {
		ref = c.Expected
	}
	out := postgresql.EvalCase{Input: c.Input, ReferenceKind: c.ReferenceKind, Tags: c.Tags}
	if ref != nil {
		out.Reference = *ref
	}
	if c.Weight != nil {
		if !(*c.Weight > 0) {
			return out, errors.New("weight must be above 0")
		}
		out.Weight = *c.Weight
	}
	return out, nil
}

// isJSONLines reports whether the request carries JSON lines.
func isJSONLines(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	switch mt {
	case "application/x-ndjson", "application/ndjson", "application/jsonl", "application/x-jsonlines", "application/jsonlines":
		return true
	}
	return false
}

// addResult is the body of a successful POST .../cases.
type addResult struct {
	Added          int                   `json:"added"`
	DatasetVersion int                   `json:"datasetVersion"`
	Cases          []postgresql.EvalCase `json:"cases"`
}

func (s *UIServer) addEvalCases(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	d, ok := s.loadDataset(w, r, store, datasetID)
	if !ok {
		return
	}
	limits := s.evalCfg.limits()
	perCase := int64(2*limitBytes(limits) + 4096)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, perCase*maxCasesPerCall))
	if err != nil {
		jsonError(w, "the body is unreadable or too large", http.StatusRequestEntityTooLarge)
		return
	}
	var inputs []caseInput
	if isJSONLines(r) {
		inputs, err = parseCaseLines(body)
	} else {
		var one caseInput
		if err = decodeStrict(body, &one); err == nil {
			inputs = []caseInput{one}
		} else {
			err = fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cases := make([]postgresql.EvalCase, len(inputs))
	for i, in := range inputs {
		c, err := in.toCase()
		if err != nil {
			jsonError(w, fmt.Sprintf("case %d: %v", i+1, err), http.StatusBadRequest)
			return
		}
		cases[i] = c
	}
	added, version, err := store.AddEvalCases(pgScope(r.Context()), datasetID, cases, limits)
	if err != nil {
		evalFail(w, "adding cases", err)
		return
	}
	s.evalAudit(w, r, postgresql.AuditUpdate, d.Namespace, d.Name, version,
		fmt.Sprintf("added %d %s; version %d", len(added), plural(len(added), "case", "cases"), version))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(addResult{Added: len(added), DatasetVersion: version, Cases: added})
}

func limitBytes(l postgresql.EvalLimits) int {
	if l.MaxCaseBytes <= 0 || l.MaxCaseBytes > postgresql.MaxEvalCaseBytes {
		return postgresql.MaxEvalCaseBytes
	}
	return l.MaxCaseBytes
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// parseCaseLines reads JSON lines. Blank lines are skipped. The error names the
// line (counting blank ones) and says nothing of its contents.
func parseCaseLines(body []byte) ([]caseInput, error) {
	var out []caseInput
	for n, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(out) == maxCasesPerCall {
			return nil, fmt.Errorf("at most %d cases per call; split the file", maxCasesPerCall)
		}
		var in caseInput
		if err := decodeStrict(line, &in); err != nil {
			return nil, fmt.Errorf("line %d: %v", n+1, err)
		}
		out = append(out, in)
	}
	if len(out) == 0 {
		return nil, errors.New("no cases in the body")
	}
	return out, nil
}

// handleEvalCase serves /api/evals/cases/{id}.
func (s *UIServer) handleEvalCase(w http.ResponseWriter, r *http.Request) {
	id, ok := parseEvalID(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/evals/cases/"), "/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getEvalCase(w, r, id)
	case http.MethodPatch:
		s.patchEvalCase(w, r, id)
	case http.MethodDelete:
		s.deleteEvalCase(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// loadCase reads a live case in a namespace the caller may use.
func (s *UIServer) loadCase(w http.ResponseWriter, r *http.Request, store evalStore, id int64) (postgresql.EvalCase, bool) {
	c, err := store.GetEvalCase(pgScope(r.Context()), id)
	if err == nil && !namespaceAllowed(r.Context(), c.Namespace) {
		err = postgresql.ErrEvalNotFound
	}
	if err != nil {
		evalFail(w, "loading the case", err)
		return postgresql.EvalCase{}, false
	}
	return c, true
}

func (s *UIServer) getEvalCase(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	if c, ok := s.loadCase(w, r, store, id); ok {
		jsonResponse(w, c)
	}
}

type patchCaseRequest struct {
	Input         *string   `json:"input"`
	Reference     *string   `json:"reference"`
	ReferenceKind *string   `json:"referenceKind"`
	Tags          *[]string `json:"tags"`
	Weight        *float64  `json:"weight"`
}

// caseChange is the body of a successful PATCH on a case.
type caseChange struct {
	Case           postgresql.EvalCase `json:"case"`
	DatasetVersion int                 `json:"datasetVersion"`
}

func (s *UIServer) patchEvalCase(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req patchCaseRequest
	if !readJSONBody(w, r, int64(2*limitBytes(s.evalCfg.limits())+4096), &req) {
		return
	}
	cur, ok := s.loadCase(w, r, store, id)
	if !ok {
		return
	}
	c, version, changed, err := store.UpdateEvalCase(pgScope(r.Context()), id, postgresql.EvalCasePatch{
		Input: req.Input, Reference: req.Reference, ReferenceKind: req.ReferenceKind, Tags: req.Tags, Weight: req.Weight,
	}, s.evalCfg.limits())
	if err != nil {
		evalFail(w, "updating the case", err)
		return
	}
	// An edit that changes nothing is not a change to audit.
	if changed {
		s.evalAudit(w, r, postgresql.AuditUpdate, cur.Namespace, cur.DatasetName, version,
			fmt.Sprintf("edited case %d; version %d", id, version))
	}
	jsonResponse(w, caseChange{Case: c, DatasetVersion: version})
}

func (s *UIServer) deleteEvalCase(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	cur, ok := s.loadCase(w, r, store, id)
	if !ok {
		return
	}
	version, err := store.DeleteEvalCase(pgScope(r.Context()), id)
	if err != nil {
		evalFail(w, "removing the case", err)
		return
	}
	s.evalAudit(w, r, postgresql.AuditUpdate, cur.Namespace, cur.DatasetName, version,
		fmt.Sprintf("removed case %d; version %d", id, version))
	jsonResponse(w, map[string]any{"datasetVersion": version})
}

// ── add to dataset, from a run ──────────────────────────────────────────────

// maxCaptureBody bounds the body of "add to dataset": a reference, tags and a few fields.
const maxCaptureBody = 512 << 10

// captureRequest is the body of POST /api/runs/{ns}/{name}/dataset.
type captureRequest struct {
	DatasetID int64 `json:"datasetId"`
	// Reference is the expected answer the person wrote. ReferenceKind is exact
	// (the default when a reference is given), example or rubric.
	Reference     string `json:"reference,omitempty"`
	ReferenceKind string `json:"referenceKind,omitempty"`
	// UseOutput takes the run's own output as the reference (kind example). It is
	// never the default: a bad run must not become ground truth by accident.
	UseOutput bool `json:"useOutput,omitempty"`
	// Redact defaults to true. false keeps the text as is, and is refused when
	// the operator forbids unredacted cases.
	Redact *bool    `json:"redact,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	// Preview returns what would be stored, with the original beside it, and
	// stores nothing.
	Preview bool `json:"preview,omitempty"`
}

// redactionReport says what redaction did to one field.
type redactionReport struct {
	Replacements int      `json:"replacements"`
	Rules        []string `json:"rules,omitempty"`
}

// captureResult is the body of a successful "add to dataset".
type captureResult struct {
	Case           postgresql.EvalCase        `json:"case"`
	DatasetVersion int                        `json:"datasetVersion,omitempty"`
	Redaction      map[string]redactionReport `json:"redaction"`
	// Preview is true when nothing was stored. Original then holds the text before redaction.
	Preview  bool              `json:"preview,omitempty"`
	Original map[string]string `json:"original,omitempty"`
}

// capturedRun is what a case takes from a run.
type capturedRun struct {
	input, output, agent string
	finished             bool
}

// loadCapturedRun reads a run in the caller's tenant, live or archived. Another
// tenant's run, and one in a namespace the caller may not use, is not found.
func (s *UIServer) loadCapturedRun(r *http.Request, store evalStore, ns, name string) (capturedRun, bool) {
	if !namespaceAllowed(r.Context(), ns) {
		return capturedRun{}, false
	}
	if s.crdClient != nil {
		var run agentorcav1alpha1.AgentRun
		err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &run)
		if err == nil {
			if !canWrite(r.Context(), &run, "AgentRun") {
				return capturedRun{}, false
			}
			phase := run.Status.Phase
			return capturedRun{
				input: run.Spec.Input, output: run.Status.Output, agent: run.Spec.AgentRef,
				finished: phase == agentorcav1alpha1.AgentRunPhaseSucceeded || phase == agentorcav1alpha1.AgentRunPhaseFailed,
			}, true
		}
	}
	archive, err := store.GetRun(pgScope(r.Context()), ns, name)
	if err != nil || archive == nil {
		if err != nil {
			slog.Error("loading archived run for a dataset", "run", name, "ns", ns, "err", err)
		}
		return capturedRun{}, false
	}
	return capturedRun{input: archive.Input, output: archive.Output, agent: archive.AgentRef, finished: true}, true
}

// guardrailRules returns redaction rules for the patterns and keywords a
// GuardrailPolicy names, for the agent of a run. Anything that cannot be read is
// left out: the built-in rules still apply.
func (s *UIServer) guardrailRules(ctx context.Context, ns, agentName string) (input, output []redact.Rule) {
	if s.crdClient == nil || agentName == "" {
		return nil, nil
	}
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: agentName}, &agent); err != nil || agent.Spec.GuardrailPolicyRef == "" {
		return nil, nil
	}
	var policy agentorcav1alpha1.GuardrailPolicy
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Spec.GuardrailPolicyRef}, &policy); err != nil {
		return nil, nil
	}
	return filterRules(policy.Spec.InputFilters), filterRules(policy.Spec.OutputFilters)
}

// filterRules turns the regex patterns and inline keywords of guardrail filters
// into redaction rules, whatever the filter's action: text that a policy would
// block or flag is text that should not be kept in a dataset either.
func filterRules(filters []agentorcav1alpha1.GuardrailFilter) []redact.Rule {
	patterns := make([]redact.Pattern, 0, len(filters))
	var rules []redact.Rule
	for _, f := range filters {
		for _, p := range f.Patterns {
			patterns = append(patterns, redact.Pattern{Name: f.Name + "/" + p.Name, Pattern: p.Pattern, Replacement: p.Replacement})
		}
		if f.Keywords != nil {
			rules = append(rules, redact.Keywords(f.Keywords.Inline)...)
		}
	}
	compiled, skipped := redact.Compile(patterns)
	if len(skipped) > 0 {
		slog.Warn("guardrail patterns skipped for dataset redaction", "skipped", skipped)
	}
	return append(compiled, rules...)
}

// handleRunToDataset serves POST /api/runs/{ns}/{name}/dataset: copy the run's
// input (and optionally a reference) into a dataset. It is a copy: the case
// keeps a pointer to the run and nothing else, never the trace.
func (s *UIServer) handleRunToDataset(w http.ResponseWriter, r *http.Request, ns, runName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req captureRequest
	if !readJSONBody(w, r, maxCaptureBody, &req) {
		return
	}
	redactOn := req.Redact == nil || *req.Redact
	if !redactOn && s.evalCfg.DisallowUnredactedCases {
		jsonError(w, "this operator does not allow cases that keep the original text; leave redact on", http.StatusForbidden)
		return
	}
	if req.UseOutput && (req.Reference != "" || (req.ReferenceKind != "" && req.ReferenceKind != postgresql.ReferenceExample)) {
		jsonError(w, "useOutput takes the run's output as an example reference; do not also give a reference or another referenceKind", http.StatusBadRequest)
		return
	}
	if req.DatasetID <= 0 {
		jsonError(w, "datasetId is required", http.StatusBadRequest)
		return
	}
	// The run first: a run of another tenant is a 404 whatever else is wrong.
	run, ok := s.loadCapturedRun(r, store, ns, runName)
	if !ok {
		jsonError(w, "run not found", http.StatusNotFound)
		return
	}
	dataset, ok := s.loadDataset(w, r, store, req.DatasetID)
	if !ok {
		return
	}

	c := postgresql.EvalCase{
		Input: run.input, Tags: req.Tags, SourceRunNamespace: ns, SourceRun: runName,
		SourceUser: actorOf(r.Context()), Redacted: redactOn,
	}
	switch {
	case req.UseOutput:
		if !run.finished || strings.TrimSpace(run.output) == "" {
			jsonError(w, "the run has no output to use as the reference", http.StatusConflict)
			return
		}
		c.Reference, c.ReferenceKind = run.output, postgresql.ReferenceExample
	case req.Reference != "":
		c.Reference, c.ReferenceKind = req.Reference, req.ReferenceKind
		if c.ReferenceKind == "" {
			c.ReferenceKind = postgresql.ReferenceExact
		}
	case req.ReferenceKind != "" && req.ReferenceKind != postgresql.ReferenceNone:
		jsonError(w, "referenceKind "+req.ReferenceKind+" needs a reference", http.StatusBadRequest)
		return
	}

	limits := s.evalCfg.limits()
	original := map[string]string{"input": c.Input, "reference": c.Reference}
	// A field over the limit is refused as it is, which also keeps redaction cheap.
	if len(c.Input) > limitBytes(limits) || len(c.Reference) > limitBytes(limits) {
		jsonError(w, fmt.Sprintf("the run's input or reference is over %d bytes; a case holds at most that much", limitBytes(limits)), http.StatusRequestEntityTooLarge)
		return
	}
	report := map[string]redactionReport{"input": {}, "reference": {}}
	if redactOn {
		inRules, outRules := s.guardrailRules(r.Context(), ns, run.agent)
		in := redact.Text(c.Input, inRules)
		c.Input = in.Text
		report["input"] = redactionReport{Replacements: in.Count, Rules: in.Rules}
		if c.Reference != "" {
			out := redact.Text(c.Reference, outRules)
			c.Reference = out.Text
			report["reference"] = redactionReport{Replacements: out.Count, Rules: out.Rules}
		}
	}

	if req.Preview {
		jsonResponse(w, captureResult{Case: c, Redaction: report, Preview: true, Original: original})
		return
	}
	added, version, err := store.AddEvalCases(pgScope(r.Context()), dataset.ID, []postgresql.EvalCase{c}, limits)
	if err != nil {
		evalFail(w, "adding the case", err)
		return
	}
	summary := fmt.Sprintf("added case %d from run %s/%s; version %d", added[0].ID, ns, runName, version)
	if redactOn {
		summary += fmt.Sprintf("; redacted (%d replacements)", report["input"].Replacements+report["reference"].Replacements)
	} else {
		summary += "; kept unredacted"
	}
	s.evalAudit(w, r, postgresql.AuditUpdate, dataset.Namespace, dataset.Name, version, summary)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(captureResult{Case: added[0], DatasetVersion: version, Redaction: report})
}
