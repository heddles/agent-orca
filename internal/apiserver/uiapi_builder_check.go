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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// "Check with my cases": POST /api/builder/check (builder, audited).
//
// The body carries the draft Agent, saved or not, and the dataset to run it on:
//
//	{"agent": {...Agent object...}, "datasetId": 7, "budgetUsd": 0.25, "compareWithPublished": true}
//
// The draft goes through the same dry-run write as ?dryRun=true (webhooks, CRD
// schema, ownership, tenant stamping) and nothing is stored. A draft that fails
// is answered like a failed dry run (4xx {"valid": false, "errors": [...]}) and
// no experiment is made. A valid draft is queued as an experiment of purpose
// "check", the existing experiment runner's work: one arm carries the draft's
// spec inline, and when the agent is published a second arm runs the live agent
// as the baseline. Check experiments are not listed and never count for a gate.
// The budget, the number of cases, the concurrency and the starts per user are
// capped (BuilderConfig).

const (
	maxCheckBody     = 256 << 10
	checkDraftArm    = "draft"
	checkLiveArm     = "published"
	checkTimeoutSecs = 300
)

type checkRequest struct {
	Agent     json.RawMessage `json:"agent"`
	DatasetID int64           `json:"datasetId"`
	BudgetUSD float64         `json:"budgetUsd"`
	// CompareWithPublished defaults to true: when the agent is published its live
	// version runs beside the draft.
	CompareWithPublished *bool `json:"compareWithPublished"`
}

// checkResponse is the body of a started check. Follow it with
// GET /api/evals/experiments/{id}, /results and /summary.
type checkResponse struct {
	Valid      bool           `json:"valid"`
	Experiment experimentView `json:"experiment"`
	// ComparedWithPublished is true when the live, published agent runs as the
	// first (baseline) arm.
	ComparedWithPublished bool `json:"comparedWithPublished"`
	// Capped is true when the case cap applies: only the dataset's first cases run.
	Capped bool `json:"capped"`
}

func (s *UIServer) handleBuilderCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, ok := s.experimentPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	if s.crdClient == nil {
		jsonError(w, "the cluster is not available", http.StatusServiceUnavailable)
		return
	}
	var req checkRequest
	if !readJSONBody(w, r, maxCheckBody, &req) {
		return
	}
	if req.DatasetID <= 0 || len(req.Agent) == 0 {
		jsonError(w, "agent and datasetId are required", http.StatusBadRequest)
		return
	}
	cfg := s.builderCfg
	budget := req.BudgetUSD
	if budget == 0 {
		budget = cfg.checkBudget()
	}
	if budget < 0 || budget > cfg.checkMaxBudget() {
		jsonError(w, fmt.Sprintf("budgetUsd must be above 0 and at most %g", cfg.checkMaxBudget()), http.StatusBadRequest)
		return
	}
	var meta struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if json.Unmarshal(req.Agent, &meta) != nil || meta.Metadata.Name == "" {
		jsonError(w, "the draft needs a name", http.StatusBadRequest)
		return
	}
	ns, name := meta.Metadata.Namespace, meta.Metadata.Name
	if ns == "" {
		jsonError(w, "the draft needs a workspace (metadata.namespace)", http.StatusBadRequest)
		return
	}
	if !namespaceAllowed(r.Context(), ns) {
		jsonError(w, "namespace not allowed", http.StatusForbidden)
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
	if d.Namespace != ns {
		jsonError(w, fmt.Sprintf("the dataset is in workspace %s and the draft in %s: a check runs the draft where the dataset is", d.Namespace, ns), http.StatusBadRequest)
		return
	}
	if wait, ok := s.checkLimiter.allow("check:"+tenantKeyOf(r)+":"+actorOf(r.Context()), cfg.checkPerHour(), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		jsonError(w, fmt.Sprintf("you can start %d checks an hour; try again in %d minutes", cfg.checkPerHour(), int(wait.Minutes())+1), http.StatusTooManyRequests)
		return
	}

	agent, ok := s.dryRunAgent(w, r, ns, name, req.Agent)
	if !ok {
		return
	}
	specJSON, err := json.Marshal(agent.Spec)
	if err != nil {
		jsonError(w, "the draft cannot be read", http.StatusInternalServerError)
		return
	}

	arms := []postgresql.EvalArm{{Label: checkDraftArm, Agent: name, Spec: specJSON}}
	published := false
	if req.CompareWithPublished == nil || *req.CompareWithPublished {
		published = s.agentIsPublished(r, ns, name)
	}
	if published {
		arms = append([]postgresql.EvalArm{{Label: checkLiveArm, Agent: name}}, arms...)
	}
	e := postgresql.EvalExperiment{
		DatasetID: d.ID, Namespace: ns, Name: "check: " + name, Purpose: postgresql.PurposeCheck, Arms: arms,
		BudgetUSD: budget, RunBudgetUSD: min(budget, 0.5), TimeoutSeconds: checkTimeoutSecs,
		Concurrency: cfg.checkConcurrency(), MaxCases: cfg.checkCases(), CreatedBy: actorOf(r.Context()),
	}
	created, warnings, ok := s.queueExperiment(w, r, store, d, e)
	if !ok {
		return
	}
	view := experimentViewOf(created)
	view.Warnings = warnings
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(checkResponse{Valid: true, Experiment: view, ComparedWithPublished: published, Capped: created.MaxCases > 0})
}

// tenantKeyOf is the tenant part of a per-user limit key.
func tenantKeyOf(r *http.Request) string {
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		return quotaKey(t)
	}
	return ""
}

// agentIsPublished reports whether the live agent has an AgentDeployment that runs it.
func (s *UIServer) agentIsPublished(r *http.Request, ns, name string) bool {
	var deps agentorcav1alpha1.AgentDeploymentList
	if err := s.crdClient.List(r.Context(), &deps, client.InNamespace(ns)); err != nil {
		return false
	}
	var live agentorcav1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil || !canRead(r.Context(), &live, kindAgent) {
		return false
	}
	for i := range deps.Items {
		if deps.Items[i].Spec.AgentRef == name {
			return true
		}
	}
	return false
}

// dryRunAgent validates a draft Agent through the dry-run write, as a create when
// the agent does not exist and as an update when it does. On failure it has
// answered the request (a failed dry run) and reports false. On success it
// returns the object as the server would store it, defaults applied.
func (s *UIServer) dryRunAgent(w http.ResponseWriter, r *http.Request, ns, name string, draft json.RawMessage) (*agentorcav1alpha1.Agent, bool) {
	info := crdKinds["agents"]
	method, path := http.MethodPost, "/api/resources/agents"
	body := draft
	var live agentorcav1alpha1.Agent
	err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live)
	if err == nil {
		method, path = http.MethodPut, "/api/resources/agents/"+ns+"/"+name
		var m map[string]any
		if json.Unmarshal(draft, &m) != nil || m == nil {
			jsonError(w, "the draft must be an Agent object", http.StatusBadRequest)
			return nil, false
		}
		md, _ := m["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
			m["metadata"] = md
		}
		md["resourceVersion"] = live.ResourceVersion
		body, _ = json.Marshal(m)
	}
	dr := r.Clone(r.Context())
	dr.Method = method
	dr.URL = r.URL.ResolveReference(mustURL(path + "?dryRun=true"))
	dr.Body = io.NopCloser(bytes.NewReader(body))
	dr.ContentLength = int64(len(body))
	dr = dr.WithContext(withDryRun(dr.Context()))
	rec := &dryRunWriter{header: http.Header{}, status: http.StatusOK}
	if method == http.MethodPost {
		s.createResource(rec, dr, info)
	} else {
		s.updateResource(rec, dr, info, ns, name)
	}
	if rec.status >= http.StatusBadRequest {
		rec.finish(w)
		return nil, false
	}
	var out agentorcav1alpha1.Agent
	if err := json.Unmarshal(rec.body.Bytes(), &out); err != nil {
		jsonError(w, "the draft could not be read back", http.StatusInternalServerError)
		return nil, false
	}
	return &out, true
}
