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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heddles/agent-orca/internal/evals/runner"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Evaluation experiments (docs/evaluations.md).
//
//	POST /api/evals/experiments                     builder, audited (create and queue)
//	GET  /api/evals/experiments                     viewer
//	GET  /api/evals/experiments/{id}                viewer
//	GET  /api/evals/experiments/{id}/results        viewer (paged)
//	POST /api/evals/experiments/{id}/cancel         builder, audited
//
// An experiment is tenant data in PostgreSQL, scoped by the store like datasets.
// The operator's experiment runner (internal/evals/runner) does the running; the
// API only queues, shows and cancels. A foreign tenant's experiment, and one in
// a workspace the caller may not use, is a 404. Audit rows hold dataset names,
// the experiment ID and counts, never case or output text.

const (
	kindEvalExperiment = "EvalExperiment"
	experimentsSeg     = "experiments"
	maxExperimentBody  = 64 << 10
)

// evalExperimentStore is the slice of postgresql.Store the experiment endpoints use.
type evalExperimentStore interface {
	GetEvalDataset(ctx context.Context, id int64) (postgresql.EvalDataset, error)
	ListEvalGraders(ctx context.Context, datasetID int64) ([]postgresql.EvalGrader, error)
	CreateEvalExperiment(ctx context.Context, e postgresql.EvalExperiment, l postgresql.EvalExperimentLimits) (postgresql.EvalExperiment, error)
	GetEvalExperiment(ctx context.Context, id int64) (postgresql.EvalExperiment, error)
	ListEvalExperiments(ctx context.Context, q postgresql.EvalExperimentQuery) ([]postgresql.EvalExperiment, error)
	RequestEvalCancel(ctx context.Context, id int64) (postgresql.EvalExperiment, error)
	ListEvalExperimentResults(ctx context.Context, id int64, q postgresql.EvalExperimentResultQuery) ([]postgresql.EvalResult, error)
}

func (s *UIServer) experimentSource() evalExperimentStore {
	if s.evalExperiments != nil {
		return s.evalExperiments
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// SetEvalExperimentLimits sets the limits on creating an experiment.
func (s *UIServer) SetEvalExperimentLimits(l postgresql.EvalExperimentLimits) { s.evalLimits = l }

// experimentPreflight checks the role, the switch and the store.
func (s *UIServer) experimentPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (evalExperimentStore, bool) {
	if !requireRole(w, r, role) {
		return nil, false
	}
	if s.evalCfg.DisableExperiments {
		jsonError(w, "evaluation experiments are turned off (operator.evals.enabled)", http.StatusServiceUnavailable)
		return nil, false
	}
	store := s.experimentSource()
	if store == nil {
		jsonError(w, evalsNeedPostgres, http.StatusServiceUnavailable)
		return nil, false
	}
	return store, true
}

// experimentView is an experiment with its progress total.
type experimentView struct {
	postgresql.EvalExperiment
	// ResultsTotal is the number of results the experiment will have when it ends
	// without stopping early: cases times arms times repeats.
	ResultsTotal int `json:"resultsTotal"`
}

func experimentViewOf(e postgresql.EvalExperiment) experimentView {
	return experimentView{EvalExperiment: e, ResultsTotal: e.Cases * len(e.Arms) * e.Repeats}
}

// loadExperiment reads an experiment the caller may use.
func (s *UIServer) loadExperiment(w http.ResponseWriter, r *http.Request, store evalExperimentStore, id int64) (postgresql.EvalExperiment, bool) {
	e, err := store.GetEvalExperiment(pgScope(r.Context()), id)
	if err == nil && !namespaceAllowed(r.Context(), e.Namespace) {
		err = postgresql.ErrEvalNotFound
	}
	if err != nil {
		evalFail(w, "loading the experiment", err)
		return postgresql.EvalExperiment{}, false
	}
	return e, true
}

func (s *UIServer) experimentAudit(w http.ResponseWriter, r *http.Request, action string, e postgresql.EvalExperiment, summary string) {
	s.recordAudit(w, r, auditEventFor(action, kindEvalExperiment, e.Namespace, strconv.FormatInt(e.ID, 10), strconv.Itoa(e.DatasetVersion), summary))
}

// handleEvalExperiments serves GET and POST /api/evals/experiments.
func (s *UIServer) handleEvalExperiments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEvalExperiments(w, r)
	case http.MethodPost:
		s.createEvalExperiment(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleEvalExperiment serves /api/evals/experiments/{id}[/results|/cancel].
func (s *UIServer) handleEvalExperiment(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/evals/experiments/"), "/"), "/")
	id, ok := parseEvalID(parts[0])
	if !ok || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.getEvalExperiment(w, r, id)
	case len(parts) == 2 && parts[1] == "results" && r.Method == http.MethodGet:
		s.listEvalExperimentResults(w, r, id)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		s.cancelEvalExperiment(w, r, id)
	case len(parts) == 2 && parts[1] != "results" && parts[1] != "cancel":
		http.NotFound(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type experimentRequest struct {
	DatasetID      int64                `json:"datasetId"`
	Name           string               `json:"name"`
	Arms           []postgresql.EvalArm `json:"arms"`
	Repeats        int                  `json:"repeats"`
	BudgetUSD      float64              `json:"budgetUsd"`
	RunBudgetUSD   float64              `json:"runBudgetUsd"`
	TimeoutSeconds int                  `json:"timeoutSeconds"`
	Concurrency    int                  `json:"concurrency"`
}

// missingArmRef is one gap in the 422 payload.
type missingArmRef struct {
	Arm          string   `json:"arm"`
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	Namespace    string   `json:"namespace"`
	Alternatives []string `json:"alternatives"`
}

func (s *UIServer) createEvalExperiment(w http.ResponseWriter, r *http.Request) {
	store, ok := s.experimentPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	var req experimentRequest
	if !readJSONBody(w, r, maxExperimentBody, &req) {
		return
	}
	if req.DatasetID <= 0 {
		jsonError(w, "datasetId is required", http.StatusBadRequest)
		return
	}
	ctx := pgScope(r.Context())
	d, err := store.GetEvalDataset(ctx, req.DatasetID)
	if err == nil && !namespaceAllowed(r.Context(), d.Namespace) {
		err = postgresql.ErrEvalNotFound
	}
	if err != nil {
		evalFail(w, "loading the dataset", err)
		return
	}
	for i := range req.Arms {
		if req.Arms[i].Label == "" {
			req.Arms[i].Label = fmt.Sprintf("arm%d", i+1)
		}
	}
	e := postgresql.EvalExperiment{
		DatasetID: d.ID, Namespace: d.Namespace, Name: req.Name, Arms: req.Arms, Repeats: req.Repeats,
		BudgetUSD: req.BudgetUSD, RunBudgetUSD: req.RunBudgetUSD, TimeoutSeconds: req.TimeoutSeconds,
		Concurrency: req.Concurrency, CreatedBy: actorOf(r.Context()),
	}
	e.Normalize()
	if err := e.Validate(s.evalLimits); err != nil {
		evalFail(w, "creating the experiment", err)
		return
	}
	graders, err := store.ListEvalGraders(ctx, d.ID)
	if err != nil {
		evalFail(w, "listing graders", err)
		return
	}
	if len(graders) == 0 {
		jsonError(w, "the dataset has no graders: add one so the results can be compared", http.StatusConflict)
		return
	}
	tenantID := ""
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		tenantID = t.TenantID
	}
	if !s.checkExperimentArms(w, r, e, tenantID) {
		return
	}
	created, err := store.CreateEvalExperiment(ctx, e, s.evalLimits)
	if err != nil {
		evalFail(w, "creating the experiment", err)
		return
	}
	s.experimentAudit(w, r, postgresql.AuditCreate, created, fmt.Sprintf("queued experiment on dataset %s v%d: %d arms, %d repeats, budget $%.2f",
		d.Name, created.DatasetVersion, len(created.Arms), created.Repeats, created.BudgetUSD))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(experimentViewOf(created))
}

// checkExperimentArms resolves every arm (its revision or the live agent) and
// runs the "use a template" reference check on it, answering 422 with the same
// missing payload when something the arm names is not in the workspace.
func (s *UIServer) checkExperimentArms(w http.ResponseWriter, r *http.Request, e postgresql.EvalExperiment, tenantID string) bool {
	var revs runner.RevisionReader
	if sink := s.revisionSink(); sink != nil {
		revs = sink
	}
	resolved, err := runner.ResolveArms(r.Context(), s.crdClient, revs, tenantID, e.Namespace, e.Arms)
	if err != nil {
		var ae *runner.ArmError
		if errors.As(err, &ae) {
			jsonError(w, ae.Msg, http.StatusUnprocessableEntity)
			return false
		}
		evalFail(w, "resolving the arms", err)
		return false
	}
	ctx := r.Context()
	gaps, err := runner.MissingArmRefs(ctx, s.crdClient, e, resolved, func(obj client.Object, kind string) bool { return canRead(ctx, obj, kind) })
	if err != nil {
		evalFail(w, "checking the arms' references", err)
		return false
	}
	if len(gaps) == 0 {
		return true
	}
	problems := make([]string, 0, len(gaps))
	missing := make([]missingArmRef, 0, len(gaps))
	for _, g := range gaps {
		problems = append(problems, g.Describe(e.Namespace))
		missing = append(missing, missingArmRef{Arm: g.Arm, Kind: g.Kind, Name: g.Name, Namespace: e.Namespace, Alternatives: g.Alternatives})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "an arm names objects that are not in the workspace: " + strings.Join(problems, "; "),
		"missing": missing,
	})
	return false
}

func (s *UIServer) listEvalExperiments(w http.ResponseWriter, r *http.Request) {
	store, ok := s.experimentPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	q := postgresql.EvalExperimentQuery{Namespace: r.URL.Query().Get("namespace"), Status: r.URL.Query().Get("status"), Limit: 50}
	if v := r.URL.Query().Get("datasetId"); v != "" {
		id, ok := parseEvalID(v)
		if !ok {
			jsonError(w, "datasetId must be a number", http.StatusBadRequest)
			return
		}
		q.DatasetID = id
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			jsonError(w, "limit must be between 1 and 200", http.StatusBadRequest)
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
		q.Before = n
	}
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
	want := q.Limit
	q.Limit = want + 1
	list, err := store.ListEvalExperiments(pgScope(r.Context()), q)
	if err != nil {
		evalFail(w, "listing experiments", err)
		return
	}
	page := struct {
		Experiments []experimentView `json:"experiments"`
		NextCursor  string           `json:"nextCursor,omitempty"`
	}{Experiments: []experimentView{}}
	if len(list) > want {
		list = list[:want]
		page.NextCursor = strconv.FormatInt(list[want-1].ID, 10)
	}
	for _, e := range list {
		page.Experiments = append(page.Experiments, experimentViewOf(e))
	}
	jsonResponse(w, page)
}

func (s *UIServer) getEvalExperiment(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.experimentPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	if e, ok := s.loadExperiment(w, r, store, id); ok {
		jsonResponse(w, experimentViewOf(e))
	}
}

func (s *UIServer) listEvalExperimentResults(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.experimentPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	if _, ok := s.loadExperiment(w, r, store, id); !ok {
		return
	}
	q := postgresql.EvalExperimentResultQuery{Arm: r.URL.Query().Get("arm"), Limit: 50}
	if v := r.URL.Query().Get("caseId"); v != "" {
		cid, ok := parseEvalID(v)
		if !ok {
			jsonError(w, "caseId must be a number", http.StatusBadRequest)
			return
		}
		q.CaseID = cid
	}
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
	want := q.Limit
	q.Limit = want + 1
	list, err := store.ListEvalExperimentResults(pgScope(r.Context()), id, q)
	if err != nil {
		evalFail(w, "listing results", err)
		return
	}
	page := struct {
		Results    []postgresql.EvalResult `json:"results"`
		NextCursor string                  `json:"nextCursor,omitempty"`
	}{Results: list}
	if len(list) > want {
		page.Results = list[:want]
		page.NextCursor = strconv.FormatInt(list[want-1].ID, 10)
	}
	jsonResponse(w, page)
}

func (s *UIServer) cancelEvalExperiment(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.experimentPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	if _, ok := s.loadExperiment(w, r, store, id); !ok {
		return
	}
	e, err := store.RequestEvalCancel(pgScope(r.Context()), id)
	if err != nil {
		evalFail(w, "cancelling the experiment", err)
		return
	}
	summary := "cancelled the experiment"
	if e.Status == postgresql.ExperimentRunning {
		summary = "asked the runner to stop the experiment"
	}
	s.experimentAudit(w, r, postgresql.AuditUpdate, e, summary)
	jsonResponse(w, experimentViewOf(e))
}
