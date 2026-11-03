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
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// Budget endpoints of the internal (router-facing) API:
//
//	POST /agentrun/{ns}/{name}/budget  {"state": "warning"|"exceeded", "spentUSD": "1.6200"}
//	GET  /agentrun/{ns}/{name}/budget  → {"state", "capUSD", "spentUSD", "terminal"}
//
// The router reports where the run stands; the operator owns the cap. POST can
// never change the cap, so a run cannot raise its own budget: only an approved
// raise (applied by the AgentRun controller) does.

// budgetReport is the body of POST .../budget.
type budgetReport struct {
	State    string `json:"state"`
	SpentUSD string `json:"spentUSD"`
}

// budgetView is the body of GET .../budget.
type budgetView struct {
	State    string `json:"state"`
	CapUSD   string `json:"capUSD"`
	SpentUSD string `json:"spentUSD,omitempty"`
	// Terminal is true once the run is over (failed because a raise was
	// refused or timed out, cancelled, or finished): a paused router stops.
	Terminal bool `json:"terminal"`
}

// callerInNamespace reports whether a TokenReview username is a service
// account of namespace, so a router can only touch runs in its own namespace.
func callerInNamespace(username, namespace string) bool {
	return strings.HasPrefix(username, "system:serviceaccount:"+namespace+":")
}

func (s *Server) loadBudgetRun(w http.ResponseWriter, r *http.Request, caller, namespace, name string) (*agentorcav1alpha1.AgentRun, bool) {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return nil, false
	}
	if run.Spec.Budget == nil {
		http.Error(w, "the run has no budget", http.StatusConflict)
		return nil, false
	}
	return &run, true
}

// reportBudget records the router's view of the run's budget.
func (s *Server) reportBudget(w http.ResponseWriter, r *http.Request, caller, namespace, name string) {
	run, ok := s.loadBudgetRun(w, r, caller, namespace, name)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req budgetReport
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	state := agentorcav1alpha1.RunBudgetState(req.State)
	if state != agentorcav1alpha1.RunBudgetWarning && state != agentorcav1alpha1.RunBudgetExceeded {
		http.Error(w, `state must be "warning" or "exceeded"`, http.StatusBadRequest)
		return
	}

	patch := client.MergeFrom(run.DeepCopy())
	b := run.Status.Budget
	if b == nil {
		b = &agentorcav1alpha1.RunBudgetStatus{State: agentorcav1alpha1.RunBudgetOK, CapUSD: run.Spec.Budget.MaxUSD}
		run.Status.Budget = b
	}
	// A late or retried "exceeded" from before an approved raise must not open
	// another approval: the spend it reports is already under the cap.
	if state == agentorcav1alpha1.RunBudgetExceeded && req.SpentUSD != "" {
		spent, errS := strconv.ParseFloat(req.SpentUSD, 64)
		capUSD, errC := strconv.ParseFloat(b.CapUSD, 64)
		if errS == nil && errC == nil && spent < capUSD {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// A warning never overrides an exceeded budget that is still awaiting a decision.
	if b.State != agentorcav1alpha1.RunBudgetExceeded || state != agentorcav1alpha1.RunBudgetWarning {
		b.State = state
	}
	if req.SpentUSD != "" {
		b.SpentUSD = req.SpentUSD
	}
	now := metav1.Now()
	b.UpdatedAt = &now
	if err := s.crdClient.Status().Patch(r.Context(), run, patch); err != nil {
		http.Error(w, fmt.Sprintf("updating AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}
	slog.Info("AgentRun budget reported", "run", name, "namespace", namespace, "state", state, "spentUSD", req.SpentUSD)
	w.WriteHeader(http.StatusNoContent)
}

// getBudget tells the router the cap it must enforce.
func (s *Server) getBudget(w http.ResponseWriter, r *http.Request, caller, namespace, name string) {
	run, ok := s.loadBudgetRun(w, r, caller, namespace, name)
	if !ok {
		return
	}
	view := budgetView{
		State:    string(agentorcav1alpha1.RunBudgetOK),
		CapUSD:   run.Spec.Budget.MaxUSD,
		Terminal: postgresql.IsTerminalPhase(run.Status.Phase),
	}
	if b := run.Status.Budget; b != nil {
		view.State, view.CapUSD, view.SpentUSD = string(b.State), b.CapUSD, b.SpentUSD
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}
