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

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch"
)

// AutoAgent, where a request names an agent, asks for one to be picked. An
// Agent that is really called this keeps the name.
const AutoAgent = "auto"

// AnnotationDispatch records on a run how its agent was picked: a RoutingInfo
// as JSON. A run whose caller named the agent has none.
const AnnotationDispatch = "agentorca.io/dispatch"

// The entry points, as decision records name them.
const (
	sourceAPI = "api"
	sourceACP = "acp"
)

// AgentDispatcher picks agents. *dispatch.Dispatcher is one.
type AgentDispatcher interface {
	Dispatch(ctx context.Context, req dispatch.Request) (dispatch.Result, error)
	Overview(ctx context.Context, req dispatch.Request) (dispatch.Overview, error)
}

// RoutingInfo says how an agent was picked for a request that named none.
type RoutingInfo struct {
	// Method is sticky, only, decided or fallback.
	Method string `json:"method"`
	// Reason says why the pick is not simply a sure router's.
	Reason string `json:"reason,omitempty"`
	// Confidence is the router's in its own first choice, which for a fallback
	// is not the agent that got the request.
	Confidence float64 `json:"confidence,omitempty"`
	// Considered are the router's most probable agents.
	Considered []RoutingCandidate `json:"considered,omitempty"`
	Escalated  bool               `json:"escalated,omitempty"`
	DecisionID string             `json:"decisionId,omitempty"`
}

// RoutingCandidate is an agent the router considered.
type RoutingCandidate struct {
	Agent       string  `json:"agent"`
	Probability float64 `json:"probability"`
}

func routingInfo(r dispatch.Result) *RoutingInfo {
	info := &RoutingInfo{
		Method: string(r.Method), Reason: r.Reason, Confidence: r.Confidence,
		Escalated: r.Escalated, DecisionID: r.ID,
	}
	for _, c := range r.Top {
		info.Considered = append(info.Considered, RoutingCandidate{Agent: c.Agent.Name, Probability: c.Probability})
	}
	return info
}

// markDispatched records on run how its agent was picked, if it was.
func markDispatched(run *agentorcav1alpha1.AgentRun, info *RoutingInfo) {
	if info == nil {
		return
	}
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}
	run.Annotations[AnnotationDispatch] = mustJSON(info)
}

// routingOf is how run's agent was picked, or nil.
func routingOf(run *agentorcav1alpha1.AgentRun) *RoutingInfo {
	var info RoutingInfo
	if err := json.Unmarshal([]byte(run.Annotations[AnnotationDispatch]), &info); err != nil || info.Method == "" {
		return nil
	}
	return &info
}

func dispatchTenant(t *TenantIdentity) dispatch.Tenant {
	if t == nil {
		return dispatch.Tenant{}
	}
	return dispatch.Tenant{ID: t.TenantID, Name: t.TenantName, Namespaces: tenantNamespaces(t), AllowedAgents: t.AllowedAgents}
}

// asksForAuto reports whether naming name asks for an agent to be picked. When
// the agents cannot be listed it does not, and looking the name up reports why.
func asksForAuto(ctx context.Context, cl client.Client, t *TenantIdentity, name string) bool {
	if name != AutoAgent || t == nil {
		return false
	}
	list, err := listAgentsInNamespaces(ctx, cl, t)
	if err != nil {
		return false
	}
	for i := range list.Items {
		if list.Items[i].Name == AutoAgent {
			return false
		}
	}
	return true
}

// pickAgent has the dispatcher pick the agent for req.
func pickAgent(ctx context.Context, cl client.Client, d AgentDispatcher, req dispatch.Request) (
	*agentorcav1alpha1.Agent, *RoutingInfo, error) {
	if d == nil {
		return nil, nil, fmt.Errorf("%w: this server does not route requests", dispatch.ErrOff)
	}
	res, err := d.Dispatch(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var agent agentorcav1alpha1.Agent
	if err := cl.Get(ctx, client.ObjectKey{Namespace: res.Agent.Namespace, Name: res.Agent.Name}, &agent); err != nil {
		return nil, nil, fmt.Errorf("reading the agent that was picked: %w", err)
	}
	return &agent, routingInfo(res), nil
}

// dispatchProblem is what to tell a caller about a request no agent was picked
// for. agents are the ones they may name instead, when the answer is to name one.
func dispatchProblem(err error) (status int, code, message string, agents []string) {
	var undecided *dispatch.UndecidedError
	switch {
	case errors.As(err, &undecided):
		for _, a := range undecided.Agents {
			agents = append(agents, a.Name)
		}
		return http.StatusUnprocessableEntity, "agent_undecided", undecided.Error(), agents
	case errors.Is(err, dispatch.ErrOff):
		return http.StatusBadRequest, "auto_unavailable", err.Error() + "; name an agent", nil
	case errors.Is(err, dispatch.ErrNoAgents):
		return http.StatusNotFound, "no_agents", err.Error(), nil
	}
	slog.Error("picking an agent", "err", err)
	return http.StatusInternalServerError, "server_error", "an agent could not be picked; try again or name one", nil
}

// writeDispatchError answers a REST caller no agent was picked for.
func writeDispatchError(w http.ResponseWriter, err error) {
	status, code, message, agents := dispatchProblem(err)
	body := map[string]any{"error": message, "code": code}
	if len(agents) > 0 {
		body["agents"] = agents
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeACPDispatchError is writeDispatchError in the ACP error envelope.
func writeACPDispatchError(w http.ResponseWriter, err error) {
	status, code, message, agents := dispatchProblem(err)
	body := ACPErr{Code: code, Message: message}
	if len(agents) > 0 {
		body.Data = map[string]any{"agents": agents}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
