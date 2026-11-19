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
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/calibrate"
	"github.com/heddles/agent-orca/internal/evals/gate"
	"github.com/heddles/agent-orca/internal/evals/graders"
	"github.com/heddles/agent-orca/internal/evals/runner"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// The publish gate (docs/evaluations.md, "The publish gate").
//
//	GET    /api/evals/gates/{ns}/{agent}        viewer   the gate and where it stands
//	PUT    /api/evals/gates/{ns}/{agent}        admin, audited
//	DELETE /api/evals/gates/{ns}/{agent}        admin, audited
//	POST   /api/agents/{ns}/{name}/evaluate     builder, audited (queues the gate experiment)
//	POST   /api/agents/{ns}/{name}/publish      checks the gate (uiapi_publish.go)
//
// The gate is off unless an agent has an enabled row in eval_gates, so a
// workspace that never sets one publishes as before.

const (
	kindEvalGate = "EvalGate"
	maxGateBody  = 16 << 10

	// Override reasons are 10 to 500 characters.
	minOverrideReason = 10
	maxOverrideReason = 500

	gateBaselineArm  = "baseline"
	gateCandidateArm = "candidate"
)

// evalGateStore is the slice of postgresql.Store the gate endpoints use.
type evalGateStore interface {
	evalExperimentStore
	GetEvalGate(ctx context.Context, namespace, agent string) (postgresql.EvalGate, error)
	PutEvalGate(ctx context.Context, g postgresql.EvalGate) (postgresql.EvalGate, *postgresql.EvalGate, error)
	DeleteEvalGate(ctx context.Context, namespace, agent string) (postgresql.EvalGate, error)
	LatestGateExperiment(ctx context.Context, namespace, agent string) (postgresql.GateEvidence, bool, error)
	EvalCalibrationPairs(ctx context.Context, datasetID int64, llmGrader, humanGrader string, experimentID int64) ([]postgresql.EvalCalibrationPair, error)
}

func (s *UIServer) gateSource() evalGateStore {
	if s.evalGates != nil {
		return s.evalGates
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) registerGateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/evals/gates/{ns}/{agent}", s.handleEvalGate)
	mux.HandleFunc("/api/agents/{ns}/{name}/evaluate", s.handleEvaluateAgent)
}

// gateRequest is the body of PUT /api/evals/gates/{ns}/{agent}.
type gateRequest struct {
	DatasetID        int64    `json:"datasetId"`
	BaselineRevision int      `json:"baselineRevision"`
	Margin           float64  `json:"margin"`
	MinCases         int      `json:"minCases"`
	MinPassRate      *float64 `json:"minPassRate"`
	MaxRegressions   *int     `json:"maxRegressions"`
	BudgetUSD        float64  `json:"budgetUsd"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled"`
}

// gateView is the body of GET /api/evals/gates/{ns}/{agent}.
type gateView struct {
	Gate *postgresql.EvalGate `json:"gate"`
	// Decision is where the agent stands against an enabled gate now, for the
	// Publish button; absent when there is no gate or it is off.
	Decision *gate.Decision `json:"decision,omitempty"`
	// OverrideAllowed is false when the operator turned gate overrides off.
	OverrideAllowed bool `json:"overrideAllowed"`
}

func (s *UIServer) handleEvalGate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getEvalGate(w, r)
	case http.MethodPut:
		s.putEvalGate(w, r)
	case http.MethodDelete:
		s.deleteEvalGate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *UIServer) gatePreflight(w http.ResponseWriter, r *http.Request, role tenancy.Role) (evalGateStore, string, string, bool) {
	if !requireRole(w, r, role) {
		return nil, "", "", false
	}
	ns, agent := r.PathValue("ns"), r.PathValue("agent")
	if !namespaceAllowed(r.Context(), ns) {
		jsonError(w, "not found", http.StatusNotFound)
		return nil, "", "", false
	}
	store := s.gateSource()
	if store == nil {
		jsonError(w, evalsNeedPostgres, http.StatusServiceUnavailable)
		return nil, "", "", false
	}
	return store, ns, agent, true
}

// getAgentFor reads an agent the caller may see, answering 404 otherwise.
func (s *UIServer) getAgentFor(w http.ResponseWriter, r *http.Request, ns, name string) (*agentorcav1alpha1.Agent, bool) {
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		notFoundOr(w, err)
		return nil, false
	}
	if !canRead(r.Context(), &agent, kindAgent) {
		jsonError(w, "not found", http.StatusNotFound)
		return nil, false
	}
	return &agent, true
}

func (s *UIServer) getEvalGate(w http.ResponseWriter, r *http.Request) {
	store, ns, name, ok := s.gatePreflight(w, r, tenancy.RoleViewer)
	if !ok {
		return
	}
	agent, ok := s.getAgentFor(w, r, ns, name)
	if !ok {
		return
	}
	view := gateView{OverrideAllowed: !s.evalCfg.DisallowGateOverride}
	g, err := store.GetEvalGate(pgScope(r.Context()), ns, name)
	switch {
	case errors.Is(err, postgresql.ErrEvalNotFound):
		jsonResponse(w, view)
		return
	case err != nil:
		evalFail(w, "reading the gate", err)
		return
	}
	view.Gate = &g
	if g.Enabled {
		dec, _, err := s.gateDecision(r.Context(), store, g, agent)
		if err != nil {
			evalFail(w, "checking the gate", err)
			return
		}
		view.Decision = &dec
	}
	jsonResponse(w, view)
}

func (s *UIServer) putEvalGate(w http.ResponseWriter, r *http.Request) {
	store, ns, name, ok := s.gatePreflight(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	var req gateRequest
	if !readJSONBody(w, r, maxGateBody, &req) {
		return
	}
	if req.DatasetID <= 0 {
		jsonError(w, "datasetId is required", http.StatusBadRequest)
		return
	}
	if _, ok := s.getAgentFor(w, r, ns, name); !ok {
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
	gs, err := store.ListEvalGraders(ctx, d.ID)
	if err != nil {
		evalFail(w, "listing graders", err)
		return
	}
	if msg := gateGraderProblem(gs); msg != "" {
		jsonError(w, msg, http.StatusConflict)
		return
	}
	g := postgresql.EvalGate{
		Namespace: ns, Agent: name, DatasetID: req.DatasetID, BaselineRevision: req.BaselineRevision, Margin: req.Margin,
		MinCases: req.MinCases, MinPassRate: req.MinPassRate, MaxRegressions: req.MaxRegressions, BudgetUSD: req.BudgetUSD,
		Enabled: req.Enabled == nil || *req.Enabled, UpdatedBy: actorOf(r.Context()),
	}
	saved, old, err := store.PutEvalGate(ctx, g)
	if err != nil {
		evalFail(w, "saving the gate", err)
		return
	}
	action := postgresql.AuditCreate
	if old != nil {
		action = postgresql.AuditUpdate
	}
	s.recordAudit(w, r, auditEventFor(action, kindEvalGate, ns, name, fmt.Sprint(saved.ID), gateChangeSummary(old, saved, d.Name)))
	jsonResponse(w, saved)
}

func (s *UIServer) deleteEvalGate(w http.ResponseWriter, r *http.Request) {
	store, ns, name, ok := s.gatePreflight(w, r, tenancy.RoleAdmin)
	if !ok {
		return
	}
	g, err := store.DeleteEvalGate(pgScope(r.Context()), ns, name)
	if err != nil {
		evalFail(w, "deleting the gate", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditDelete, kindEvalGate, ns, name, fmt.Sprint(g.ID),
		"removed the publish gate: "+gateNumbers(&g)))
	w.WriteHeader(http.StatusNoContent)
}

// gateGraderProblem says why a dataset's graders cannot carry a gate: a gate
// that rests only on an LLM judge can be talked past by the agent's own output
// (docs/proposals/evaluations.md, section 8), so it needs a deterministic or
// human grader too.
func gateGraderProblem(gs []postgresql.EvalGrader) string {
	if len(gs) == 0 {
		return "the dataset has no graders: a gate needs at least one"
	}
	for _, g := range gs {
		if g.Kind != graders.KindLLM {
			return ""
		}
	}
	return "the dataset has only llm graders: a judge can be swayed by the output it grades, so a gate needs a deterministic or human grader as well"
}

func gateNumbers(g *postgresql.EvalGate) string {
	if g == nil {
		return "none"
	}
	s := fmt.Sprintf("dataset %d, baseline revision %d, margin %g, min cases %d", g.DatasetID, g.BaselineRevision, g.Margin, g.MinCases)
	if g.MinPassRate != nil {
		s += fmt.Sprintf(", min pass rate %g", *g.MinPassRate)
	}
	if g.MaxRegressions != nil {
		s += fmt.Sprintf(", max regressions %d", *g.MaxRegressions)
	}
	if !g.Enabled {
		s += ", off"
	}
	return s
}

func gateChangeSummary(old *postgresql.EvalGate, g postgresql.EvalGate, dataset string) string {
	if old == nil {
		return fmt.Sprintf("set the publish gate on dataset %s: %s", dataset, gateNumbers(&g))
	}
	return fmt.Sprintf("changed the publish gate (dataset %s): was %s; now %s", dataset, gateNumbers(old), gateNumbers(&g))
}

// gateDecision checks the agent against its gate now. It returns the decision
// and the dataset the gate rests on (zero when that is gone).
func (s *UIServer) gateDecision(ctx context.Context, store evalGateStore, g postgresql.EvalGate, agent *agentorcav1alpha1.Agent) (gate.Decision, postgresql.EvalDataset, error) {
	sctx := pgScope(ctx)
	hash, err := runner.SpecHash(agent.Spec)
	if err != nil {
		return gate.Decision{}, postgresql.EvalDataset{}, err
	}
	in := gate.Input{
		Config: gate.Config{
			Enabled: g.Enabled, DatasetID: g.DatasetID, Margin: g.Margin, MinCases: g.MinCases,
			MinPassRate: g.MinPassRate, MaxRegressions: g.MaxRegressions, BaselineRevision: g.BaselineRevision,
		},
		Hash: hash,
	}
	d, err := store.GetEvalDataset(sctx, g.DatasetID)
	switch {
	case err == nil:
		in.DatasetExists, in.DatasetVersion = true, d.Version
	case !errors.Is(err, postgresql.ErrEvalNotFound):
		return gate.Decision{}, d, err
	}
	ev, found, err := store.LatestGateExperiment(sctx, g.Namespace, g.Agent)
	if err != nil {
		return gate.Decision{}, d, err
	}
	if found && len(ev.Experiment.Arms) >= 2 {
		rows, err := store.EvalSummaryRows(sctx, ev.Experiment.ID)
		if err != nil {
			return gate.Decision{}, d, err
		}
		in.Experiment = &gate.Experiment{
			ID: ev.Experiment.ID, DatasetID: ev.Experiment.DatasetID, DatasetVersion: ev.Experiment.DatasetVersion,
			CandidateHash: ev.Experiment.Arms[1].Hash, GradersChanged: ev.GradersChanged, Rows: rows,
			Baseline: ev.Experiment.Arms[0].Label, Candidate: ev.Experiment.Arms[1].Label,
		}
	}
	if in.DatasetExists {
		if in.UntrustedJudges, err = untrustedJudges(sctx, store, g.DatasetID); err != nil {
			return gate.Decision{}, d, err
		}
	}
	return gate.CheckGate(in), d, nil
}

// untrustedJudges names the dataset's llm graders that a gate may not rest on:
// those with no human grader whose labels agree with them (Cohen's kappa of at
// least calibrate.TrustworthyKappa over at least calibrate.MinPairs results).
// Docs: docs/proposals/evaluations.md, open question 5.
func untrustedJudges(ctx context.Context, store evalGateStore, datasetID int64) ([]string, error) {
	gs, err := store.ListEvalGraders(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	var humans []string
	for _, g := range gs {
		if g.Kind == graders.KindHuman {
			humans = append(humans, g.Name)
		}
	}
	var out []string
	for _, g := range gs {
		if g.Kind != graders.KindLLM {
			continue
		}
		trusted := false
		for _, h := range humans {
			pairs, err := store.EvalCalibrationPairs(ctx, datasetID, g.Name, h, 0)
			if err != nil {
				return nil, err
			}
			in := make([]calibrate.Pair, len(pairs))
			for i, p := range pairs {
				in[i] = calibrate.Pair{ResultID: p.ResultID, CaseID: p.CaseID, LLMPass: p.LLMPass, HumanPass: p.HumanPass, LLMScore: p.LLMScore, HumanScore: p.HumanScore}
			}
			if rep, err := calibrate.Compute(in); err == nil && rep.Trustworthy {
				trusted = true
				break
			}
		}
		if !trusted {
			out = append(out, g.Name)
		}
	}
	return out, nil
}

// overrideRequest is the overrideGate member of a publish request.
type overrideRequest struct {
	Reason string `json:"reason"`
}

// gatePublish applies the publish gate to a publish of agent. It reports
// whether publishing may go on; when it may not, it has answered the request.
//
// Without an enabled gate nothing changes. With one, the latest evaluation must
// pass (gate.CheckGate); otherwise the answer is 409 with the gate's state. An
// admin may publish past a failing gate with overrideGate and a reason, which
// is audited (action override); a caller who is not an admin and sends
// overrideGate gets 403 whatever the gate says. A failure to read the gate
// stops the publish: the gate fails closed.
func (s *UIServer) gatePublish(w http.ResponseWriter, r *http.Request, agent *agentorcav1alpha1.Agent, override *overrideRequest) bool {
	if override != nil {
		if !requireRole(w, r, tenancy.RoleAdmin) {
			return false
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(override.Reason)); n < minOverrideReason || n > maxOverrideReason {
			jsonError(w, fmt.Sprintf("overrideGate.reason must be %d to %d characters", minOverrideReason, maxOverrideReason), http.StatusBadRequest)
			return false
		}
	}
	store := s.gateSource()
	if store == nil {
		return true
	}
	ctx := pgScope(r.Context())
	g, err := store.GetEvalGate(ctx, agent.Namespace, agent.Name)
	switch {
	case errors.Is(err, postgresql.ErrEvalNotFound):
		return true
	case err != nil:
		slog.Error("reading the publish gate", "err", err)
		jsonError(w, "the publish gate could not be checked, so the agent was not published: try again", http.StatusServiceUnavailable)
		return false
	}
	if !g.Enabled {
		return true
	}
	dec, ds, err := s.gateDecision(r.Context(), store, g, agent)
	if err != nil {
		slog.Error("checking the publish gate", "err", err)
		jsonError(w, "the publish gate could not be checked, so the agent was not published: try again", http.StatusServiceUnavailable)
		return false
	}
	if dec.Passed() {
		return true
	}
	if override != nil {
		if s.evalCfg.DisallowGateOverride {
			jsonError(w, "gate overrides are turned off for this installation (operator.evals.allowGateOverride): change the gate or run the evaluation", http.StatusForbidden)
			return false
		}
		s.recordAudit(w, r, auditEventFor(postgresql.AuditOverride, kindAgent, agent.Namespace, agent.Name, agent.ResourceVersion,
			overrideSummary(ds, dec, strings.TrimSpace(override.Reason))))
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "the publish gate has not passed: " + dec.Reason, "gate": dec})
	return false
}

// overrideSummary is the audit summary of a gate override: what was needed,
// what the experiment had, and the reason, within MaxAuditSummaryBytes.
func overrideSummary(ds postgresql.EvalDataset, dec gate.Decision, reason string) string {
	name := ds.Name
	if name == "" {
		name = "(deleted)"
	}
	head := fmt.Sprintf("gate override (%s) on dataset %s v%d: needs a pass rate no more than %.0f points below the baseline over %d cases",
		dec.Status, name, ds.Version, dec.Required.Margin*100, dec.Required.MinCases)
	if dec.Best != nil {
		head += fmt.Sprintf("; experiment %d had %.0f%% against %.0f%% over %d cases", dec.ExperimentID, dec.Best.PassRate*100, dec.Best.BaselinePassRate*100, dec.Best.Cases)
	} else if dec.ExperimentID != 0 {
		head += fmt.Sprintf("; experiment %d did not apply", dec.ExperimentID)
	} else {
		head += "; no evaluation had been run"
	}
	const tail = "; reason: "
	room := postgresql.MaxAuditSummaryBytes - len(tail)
	if len(head) > room-minOverrideReason {
		head = strings.ToValidUTF8(head[:room-minOverrideReason], "")
	}
	reasonRoom := postgresql.MaxAuditSummaryBytes - len(head) - len(tail)
	if len(reason) > reasonRoom {
		reason = strings.ToValidUTF8(reason[:reasonRoom], "")
	}
	return head + tail + reason
}

// handleEvaluateAgent serves POST /api/agents/{ns}/{name}/evaluate: it queues
// the gate experiment, the agent as it is now (the candidate) against the
// baseline revision the gate names. The candidate arm is pinned to the hash of
// the spec read here, so a change to the agent before the runner starts it
// refuses the experiment rather than evaluating something else.
func (s *UIServer) handleEvaluateAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	exps, ok := s.experimentPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	if !namespaceAllowed(r.Context(), ns) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	store := s.gateSource()
	if store == nil {
		jsonError(w, evalsNeedPostgres, http.StatusServiceUnavailable)
		return
	}
	agent, ok := s.getAgentFor(w, r, ns, name)
	if !ok {
		return
	}
	ctx := pgScope(r.Context())
	g, err := store.GetEvalGate(ctx, ns, name)
	if errors.Is(err, postgresql.ErrEvalNotFound) {
		jsonError(w, "this agent has no publish gate: an admin sets one first", http.StatusConflict)
		return
	}
	if err != nil {
		evalFail(w, "reading the gate", err)
		return
	}
	d, err := exps.GetEvalDataset(ctx, g.DatasetID)
	if err == nil && !namespaceAllowed(r.Context(), d.Namespace) {
		err = postgresql.ErrEvalNotFound
	}
	if err != nil {
		evalFail(w, "loading the gate's dataset", err)
		return
	}
	hash, err := runner.SpecHash(agent.Spec)
	if err != nil {
		evalFail(w, "hashing the agent", err)
		return
	}
	e := postgresql.EvalExperiment{
		DatasetID: d.ID, Namespace: ns, Name: "publish gate: " + name, Purpose: postgresql.PurposeGate,
		Arms: []postgresql.EvalArm{
			{Label: gateBaselineArm, Agent: name, Revision: g.BaselineRevision},
			{Label: gateCandidateArm, Agent: name, Hash: hash},
		},
		BudgetUSD: g.BudgetUSD, CreatedBy: actorOf(r.Context()),
	}
	created, warnings, ok := s.queueExperiment(w, r, exps, d, e)
	if !ok {
		return
	}
	view := experimentViewOf(created)
	view.Warnings = warnings
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(view)
}
