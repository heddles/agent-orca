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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/heddles/agent-orca/internal/evals/calibrate"
	"github.com/heddles/agent-orca/internal/evals/graders"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Human review of experiment results and the judge's calibration
// (docs/evaluations.md).
//
//	GET  /api/evals/experiments/{id}/review         viewer   the review queue (blind) and its progress
//	POST /api/evals/experiments/{id}/review/close   builder, audited (leave the unlabelled items out)
//	GET  /api/evals/review/{id}                     viewer   one item
//	POST /api/evals/review/{id}                     runner, audited (label an item)
//	GET  /api/evals/datasets/{id}/calibration       viewer   an llm grader against a human grader
//
// The queue is blind. An item holds the case, the output and what the human
// grader asks, and nothing that names the arm, the repeat, the run, its cost or
// its tools; its ID is the assignment's, not the result's, and the queue is in a
// random order. Audit rows hold the grader's name and the experiment's ID, never
// the output or the comment.

const (
	kindEvalReview = "EvalReview"
	segReview      = "review"
	segClose       = "close"
	segCalibration = "calibration"

	maxLabelBody = 8 << 10
)

// evalReviewStore is the slice of postgresql.Store the review endpoints use.
type evalReviewStore interface {
	GetEvalExperiment(ctx context.Context, id int64) (postgresql.EvalExperiment, error)
	ListEvalReviews(ctx context.Context, experimentID int64, q postgresql.EvalReviewQuery) ([]postgresql.EvalReviewItem, error)
	GetEvalReview(ctx context.Context, id int64) (postgresql.EvalReviewItem, error)
	SubmitEvalReview(ctx context.Context, id int64, label postgresql.EvalReviewLabel, reviewer string) (postgresql.EvalReviewItem, error)
	EvalReviewProgressOf(ctx context.Context, experimentID int64) (postgresql.EvalReviewProgress, error)
	CloseEvalReview(ctx context.Context, experimentID int64) (int, error)
	GetEvalDataset(ctx context.Context, id int64) (postgresql.EvalDataset, error)
	EvalCalibrationPairs(ctx context.Context, datasetID int64, llmGrader, humanGrader string, experimentID int64) ([]postgresql.EvalCalibrationPair, error)
}

func (s *UIServer) reviewSource() evalReviewStore {
	if s.evalReviews != nil {
		return s.evalReviews
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// reviewPreflight checks the role, the switch and the store.
func (s *UIServer) reviewPreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (evalReviewStore, bool) {
	if !requireRole(w, r, role) {
		return nil, false
	}
	if s.evalCfg.DisableExperiments {
		jsonError(w, "evaluation experiments are turned off (operator.evals.enabled)", http.StatusServiceUnavailable)
		return nil, false
	}
	store := s.reviewSource()
	if store == nil {
		jsonError(w, evalsNeedPostgres, http.StatusServiceUnavailable)
		return nil, false
	}
	return store, true
}

// loadReviewExperiment reads an experiment the caller may use.
func (s *UIServer) loadReviewExperiment(w http.ResponseWriter, r *http.Request, store evalReviewStore, id int64) (postgresql.EvalExperiment, bool) {
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

func (s *UIServer) reviewAudit(w http.ResponseWriter, r *http.Request, action string, e postgresql.EvalExperiment, summary string) {
	s.recordAudit(w, r, auditEventFor(action, kindEvalReview, e.Namespace, strconv.FormatInt(e.ID, 10), strconv.Itoa(e.DatasetVersion), summary))
}

// handleEvalReviewItem serves /api/evals/review/{id}.
func (s *UIServer) handleEvalReviewItem(w http.ResponseWriter, r *http.Request) {
	id, ok := parseEvalID(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/evals/review/"), "/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getEvalReview(w, r, id)
	case http.MethodPost:
		s.labelEvalReview(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// reviewPage is the body of GET /api/evals/experiments/{id}/review.
type reviewPage struct {
	// Status is the experiment's status: awaiting_review while labels are wanted.
	Status   string                        `json:"status"`
	Progress postgresql.EvalReviewProgress `json:"progress"`
	Items    []postgresql.EvalReviewItem   `json:"items"`
}

func (s *UIServer) listEvalReviews(w http.ResponseWriter, r *http.Request, experimentID int64) {
	store, ok := s.reviewPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	e, ok := s.loadReviewExperiment(w, r, store, experimentID)
	if !ok {
		return
	}
	q := postgresql.EvalReviewQuery{Status: r.URL.Query().Get("status"), Limit: postgresql.DefaultReviewPage}
	switch q.Status {
	case "", postgresql.ReviewPending, postgresql.ReviewDone, postgresql.ReviewClosed:
	default:
		jsonError(w, "status must be pending, done or closed", http.StatusBadRequest)
		return
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > postgresql.MaxReviewPage {
			jsonError(w, fmt.Sprintf("limit must be between 1 and %d", postgresql.MaxReviewPage), http.StatusBadRequest)
			return
		}
		q.Limit = n
	}
	ctx := pgScope(r.Context())
	items, err := store.ListEvalReviews(ctx, experimentID, q)
	if err != nil {
		evalFail(w, "listing review items", err)
		return
	}
	progress, err := store.EvalReviewProgressOf(ctx, experimentID)
	if err != nil {
		evalFail(w, "counting review items", err)
		return
	}
	jsonResponse(w, reviewPage{Status: e.Status, Progress: progress, Items: items})
}

func (s *UIServer) getEvalReview(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.reviewPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	item, ok := s.loadReviewItem(w, r, store, id)
	if !ok {
		return
	}
	jsonResponse(w, item)
}

// loadReviewItem reads an item whose experiment the caller may use.
func (s *UIServer) loadReviewItem(w http.ResponseWriter, r *http.Request, store evalReviewStore, id int64) (postgresql.EvalReviewItem, bool) {
	item, err := store.GetEvalReview(pgScope(r.Context()), id)
	if err != nil {
		evalFail(w, "loading the review item", err)
		return postgresql.EvalReviewItem{}, false
	}
	if _, ok := s.loadReviewExperiment(w, r, store, item.ExperimentID); !ok {
		return postgresql.EvalReviewItem{}, false
	}
	return item, true
}

// labelRequest is the body of POST /api/evals/review/{id}.
type labelRequest struct {
	// Pass answers a passfail grader.
	Pass *bool `json:"pass"`
	// Stars answers a 1to5 grader.
	Stars int `json:"stars"`
	// Comment is optional, at most 2048 bytes.
	Comment string `json:"comment"`
}

func (s *UIServer) labelEvalReview(w http.ResponseWriter, r *http.Request, id int64) {
	store, ok := s.reviewPreflight(w, r, tenancy.RoleRunner)
	if !ok {
		return
	}
	var req labelRequest
	if !readJSONBody(w, r, maxLabelBody, &req) {
		return
	}
	item, ok := s.loadReviewItem(w, r, store, id)
	if !ok {
		return
	}
	e, ok := s.loadReviewExperiment(w, r, store, item.ExperimentID)
	if !ok {
		return
	}
	reviewer := actorOf(r.Context())
	if s.evalCfg.RequireIndependentReview && reviewer == e.CreatedBy {
		jsonError(w, "the person who started the experiment may not review it (operator.evals.requireIndependentReview)", http.StatusForbidden)
		return
	}
	done, err := store.SubmitEvalReview(pgScope(r.Context()), id, postgresql.EvalReviewLabel{Pass: req.Pass, Stars: req.Stars, Comment: req.Comment}, reviewer)
	if err != nil {
		if errors.Is(err, postgresql.ErrReviewClosed) {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		evalFail(w, "saving the label", err)
		return
	}
	s.reviewAudit(w, r, postgresql.AuditCreate, e, fmt.Sprintf("labelled one review item of the grader %s", done.Grader))
	jsonResponse(w, done)
}

func (s *UIServer) closeEvalReview(w http.ResponseWriter, r *http.Request, experimentID int64) {
	store, ok := s.reviewPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	e, ok := s.loadReviewExperiment(w, r, store, experimentID)
	if !ok {
		return
	}
	n, err := store.CloseEvalReview(pgScope(r.Context()), experimentID)
	if err != nil {
		evalFail(w, "closing the review", err)
		return
	}
	s.reviewAudit(w, r, postgresql.AuditUpdate, e, fmt.Sprintf("closed the review with %d items unlabelled", n))
	jsonResponse(w, map[string]int{"closed": n})
}

// calibrationView is the body of GET /api/evals/datasets/{id}/calibration.
type calibrationView struct {
	LLMGrader   string           `json:"llmGrader"`
	HumanGrader string           `json:"humanGrader"`
	MinPairs    int              `json:"minPairs"`
	TooFewPairs bool             `json:"tooFewPairs"`
	Report      calibrate.Report `json:"report"`
}

func (s *UIServer) getEvalCalibration(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, ok := s.reviewPreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	ctx := pgScope(r.Context())
	d, err := store.GetEvalDataset(ctx, datasetID)
	if err == nil && !namespaceAllowed(r.Context(), d.Namespace) {
		err = postgresql.ErrEvalNotFound
	}
	if err != nil {
		evalFail(w, "loading the dataset", err)
		return
	}
	q := r.URL.Query()
	llm, human := q.Get(graders.KindLLM), q.Get(graders.KindHuman)
	if llm == "" || human == "" {
		jsonError(w, "llm and human (the names of an llm grader and a human grader) are required", http.StatusBadRequest)
		return
	}
	var experimentID int64
	if v := q.Get("experimentId"); v != "" {
		var ok bool
		if experimentID, ok = parseEvalID(v); !ok {
			jsonError(w, "experimentId must be a number", http.StatusBadRequest)
			return
		}
	}
	pairs, err := store.EvalCalibrationPairs(ctx, datasetID, llm, human, experimentID)
	if err != nil {
		evalFail(w, "reading calibration pairs", err)
		return
	}
	in := make([]calibrate.Pair, len(pairs))
	for i, p := range pairs {
		in[i] = calibrate.Pair{ResultID: p.ResultID, CaseID: p.CaseID, LLMPass: p.LLMPass, HumanPass: p.HumanPass, LLMScore: p.LLMScore, HumanScore: p.HumanScore}
	}
	report, err := calibrate.Compute(in)
	jsonResponse(w, calibrationView{LLMGrader: llm, HumanGrader: human, MinPairs: calibrate.MinPairs, TooFewPairs: errors.Is(err, calibrate.ErrTooFewPairs), Report: report})
}
