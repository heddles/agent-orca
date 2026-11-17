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
	"net/http"
	"strconv"
	"strings"

	"github.com/heddles/agent-orca/internal/evals/graders"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Deterministic graders of a dataset, and grading a supplied result
// (docs/evaluations.md). No model is called.
//
//	GET    /api/evals/datasets/{id}/graders          viewer
//	GET    /api/evals/datasets/{id}/graders/{name}   viewer
//	PUT    /api/evals/datasets/{id}/graders/{name}   builder, audited (create or replace)
//	DELETE /api/evals/datasets/{id}/graders/{name}   builder, audited
//	POST   /api/evals/datasets/{id}/grade            runner, audited (grade and keep one result)
//	GET    /api/evals/datasets/{id}/results          viewer
//	GET    /api/evals/results/{id}                   viewer
//
// Everything is tenant-scoped through the store and, inside the tenant, to the
// namespaces the caller may use; a dataset, grader or result of another tenant
// or namespace is a 404. Audit rows carry dataset, grader names and counts,
// never an output.

const (
	kindEvalGrader = "EvalGrader"
	kindEvalResult = "EvalResult"

	// maxGraderBody bounds a grader definition (a schema can be 64 KiB).
	maxGraderBody = 192 << 10
	// maxGradeBody bounds a grade request: an output, a reference and a trace.
	maxGradeBody = 768 << 10

	gradersSeg = "graders"
)

// evalGraderStore is the slice of postgresql.Store the grader endpoints use.
type evalGraderStore interface {
	UpsertEvalGrader(ctx context.Context, datasetID int64, g postgresql.EvalGrader) (postgresql.EvalGrader, bool, error)
	ListEvalGraders(ctx context.Context, datasetID int64) ([]postgresql.EvalGrader, error)
	GetEvalGrader(ctx context.Context, datasetID int64, name string) (postgresql.EvalGrader, error)
	DeleteEvalGrader(ctx context.Context, datasetID int64, name string) (postgresql.EvalGrader, error)
	SaveEvalResult(ctx context.Context, datasetID int64, r postgresql.EvalResult, grades []postgresql.EvalGrade) (postgresql.EvalResult, error)
	GetEvalResult(ctx context.Context, id int64) (postgresql.EvalResult, error)
	ListEvalResults(ctx context.Context, datasetID int64, q postgresql.EvalResultQuery) ([]postgresql.EvalResult, error)
}

// graderAudit records a write on a grader of a dataset.
func (s *UIServer) graderAudit(w http.ResponseWriter, r *http.Request, action string, d postgresql.EvalDataset, grader, summary string) {
	s.recordAudit(w, r, auditEventFor(action, kindEvalGrader, d.Namespace, d.Name+"/"+grader, strconv.Itoa(d.Version), summary))
}

// handleEvalDatasetSub serves what hangs off a dataset beyond its cases:
// parts is the path after /api/evals/datasets/{id}/.
func (s *UIServer) handleEvalDatasetSub(w http.ResponseWriter, r *http.Request, id int64, parts []string) {
	switch {
	case parts[0] == gradersSeg && len(parts) == 1:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.listEvalGraders(w, r, id)
	case parts[0] == gradersSeg && len(parts) == 2:
		switch r.Method {
		case http.MethodGet:
			s.getEvalGrader(w, r, id, parts[1])
		case http.MethodPut:
			s.putEvalGrader(w, r, id, parts[1])
		case http.MethodDelete:
			s.deleteEvalGrader(w, r, id, parts[1])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case parts[0] == "grade" && len(parts) == 1:
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.gradeEvalResult(w, r, id)
	case parts[0] == "results" && len(parts) == 1:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.listEvalResults(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// graderPreflight checks the role and the stores and loads the dataset.
func (s *UIServer) graderPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role, datasetID int64) (evalStore, postgresql.EvalDataset, bool) {
	store, ok := s.evalPreflight(w, r, role)
	if !ok {
		return nil, postgresql.EvalDataset{}, false
	}
	d, ok := s.loadDataset(w, r, store, datasetID)
	return store, d, ok
}

func (s *UIServer) listEvalGraders(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, _, ok := s.graderPreflight(w, r, tenancy.RoleViewer, datasetID)
	if !ok {
		return
	}
	list, err := store.ListEvalGraders(pgScope(r.Context()), datasetID)
	if err != nil {
		evalFail(w, "listing graders", err)
		return
	}
	jsonResponse(w, map[string]any{"graders": list})
}

func (s *UIServer) getEvalGrader(w http.ResponseWriter, r *http.Request, datasetID int64, name string) {
	store, _, ok := s.graderPreflight(w, r, tenancy.RoleViewer, datasetID)
	if !ok {
		return
	}
	g, err := store.GetEvalGrader(pgScope(r.Context()), datasetID, name)
	if err != nil {
		evalFail(w, "loading the grader", err)
		return
	}
	jsonResponse(w, g)
}

type putGraderRequest struct {
	Kind     string          `json:"kind"`
	Config   json.RawMessage `json:"config"`
	Weight   float64         `json:"weight"`
	Required bool            `json:"required"`
}

func (s *UIServer) putEvalGrader(w http.ResponseWriter, r *http.Request, datasetID int64, name string) {
	store, d, ok := s.graderPreflight(w, r, tenancy.RoleBuilder, datasetID)
	if !ok {
		return
	}
	var req putGraderRequest
	if !readJSONBody(w, r, maxGraderBody, &req) {
		return
	}
	g, created, err := store.UpsertEvalGrader(pgScope(r.Context()), datasetID, postgresql.EvalGrader{
		Name: name, Kind: req.Kind, Config: req.Config, Weight: req.Weight, Required: req.Required, CreatedBy: actorOf(r.Context()),
	})
	if err != nil {
		evalFail(w, "saving the grader", err)
		return
	}
	action, status, summary := postgresql.AuditUpdate, http.StatusOK, "replaced grader of kind "+g.Kind
	if created {
		action, status, summary = postgresql.AuditCreate, http.StatusCreated, "created grader of kind "+g.Kind
	}
	s.graderAudit(w, r, action, d, g.Name, summary)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(g)
}

func (s *UIServer) deleteEvalGrader(w http.ResponseWriter, r *http.Request, datasetID int64, name string) {
	store, d, ok := s.graderPreflight(w, r, tenancy.RoleBuilder, datasetID)
	if !ok {
		return
	}
	g, err := store.DeleteEvalGrader(pgScope(r.Context()), datasetID, name)
	if err != nil {
		evalFail(w, "deleting the grader", err)
		return
	}
	s.graderAudit(w, r, postgresql.AuditDelete, d, g.Name, "deleted grader of kind "+g.Kind+" and its grades")
	w.WriteHeader(http.StatusNoContent)
}

// gradeRequest is a result to grade.
type gradeRequest struct {
	// CaseID, when given, is a live case of the dataset; its reference is used.
	CaseID *int64 `json:"caseId"`
	// Reference is used when there is no case.
	Reference string   `json:"reference"`
	Arm       string   `json:"arm"`
	Output    string   `json:"output"`
	ToolCalls []string `json:"toolCalls"`
	CostUSD   float64  `json:"costUsd"`
	LatencyMs int64    `json:"latencyMs"`
}

func (s *UIServer) gradeEvalResult(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, d, ok := s.graderPreflight(w, r, tenancy.RoleRunner, datasetID)
	if !ok {
		return
	}
	var req gradeRequest
	if !readJSONBody(w, r, maxGradeBody, &req) {
		return
	}
	if len(req.Output) > postgresql.MaxEvalResultOutput {
		jsonError(w, fmt.Sprintf("output is over %d bytes", postgresql.MaxEvalResultOutput), http.StatusRequestEntityTooLarge)
		return
	}
	scope := pgScope(r.Context())
	reference := req.Reference
	if req.CaseID != nil {
		if req.Reference != "" {
			jsonError(w, "give caseId or reference, not both", http.StatusBadRequest)
			return
		}
		c, err := store.GetEvalCase(scope, *req.CaseID)
		if err == nil && c.DatasetID != datasetID {
			err = postgresql.ErrEvalNotFound
		}
		if err != nil {
			evalFail(w, "loading the case", err)
			return
		}
		reference = c.Reference
	}
	list, err := store.ListEvalGraders(scope, datasetID)
	if err != nil {
		evalFail(w, "listing graders", err)
		return
	}
	if len(list) == 0 {
		jsonError(w, "the dataset has no graders", http.StatusConflict)
		return
	}
	in := graders.Input{Output: req.Output, Reference: reference,
		Trace: graders.Trace{ToolCalls: req.ToolCalls, CostUSD: req.CostUSD, LatencyMs: req.LatencyMs}}
	grades := make([]postgresql.EvalGrade, len(list))
	graded := make([]graders.Graded, len(list))
	for i, g := range list {
		res := graders.Grade(g.Spec(), in)
		grades[i] = postgresql.EvalGrade{GraderID: g.ID, Grader: g.Name, Kind: g.Kind,
			Score: res.Score, Pass: res.Pass, NotGraded: res.NotGraded, Detail: res.Detail}
		graded[i] = graders.Graded{Result: res, Weight: g.Weight, Required: g.Required}
	}
	saved, err := store.SaveEvalResult(scope, datasetID, postgresql.EvalResult{
		CaseID: req.CaseID, Arm: req.Arm, Output: req.Output, ToolCalls: req.ToolCalls,
		CostUSD: req.CostUSD, LatencyMs: req.LatencyMs, CasePass: graders.CasePass(graded), CreatedBy: actorOf(r.Context()),
	}, grades)
	if err != nil {
		evalFail(w, "saving the result", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditCreate, kindEvalResult, d.Namespace, d.Name,
		strconv.FormatInt(saved.ID, 10), fmt.Sprintf("graded one result with %d graders", len(grades))))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(saved)
}

// resultPage is the body of GET /api/evals/datasets/{id}/results.
type resultPage struct {
	Results    []postgresql.EvalResult `json:"results"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

func (s *UIServer) listEvalResults(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, _, ok := s.graderPreflight(w, r, tenancy.RoleViewer, datasetID)
	if !ok {
		return
	}
	q := postgresql.EvalResultQuery{Limit: 50}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > postgresql.MaxEvalResultsPerPage {
			jsonError(w, fmt.Sprintf("limit must be between 1 and %d", postgresql.MaxEvalResultsPerPage), http.StatusBadRequest)
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
	if v := r.URL.Query().Get("caseId"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			jsonError(w, "caseId must be a case ID", http.StatusBadRequest)
			return
		}
		q.CaseID = n
	}
	want := q.Limit
	q.Limit = want + 1
	list, err := store.ListEvalResults(pgScope(r.Context()), datasetID, q)
	if err != nil {
		evalFail(w, "listing results", err)
		return
	}
	page := resultPage{Results: list}
	if len(list) > want {
		page.Results = list[:want]
		page.NextCursor = strconv.FormatInt(list[want-1].ID, 10)
	}
	jsonResponse(w, page)
}

// handleEvalResult serves GET /api/evals/results/{id}.
func (s *UIServer) handleEvalResult(w http.ResponseWriter, r *http.Request) {
	id, ok := parseEvalID(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/evals/results/"), "/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.evalPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	res, err := store.GetEvalResult(pgScope(r.Context()), id)
	if err == nil {
		// The result's dataset decides the namespace.
		_, ok := s.loadDataset(w, r, store, res.DatasetID)
		if !ok {
			return
		}
	}
	if err != nil {
		evalFail(w, "loading the result", err)
		return
	}
	jsonResponse(w, res)
}
