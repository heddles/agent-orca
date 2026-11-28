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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch"
)

// ACPRouting is the answer to GET /routing: whether a run may ask for "auto",
// and the agents the caller could name. It is an agent-orca extension that
// `aoctl acp serve` offers its editor choices from.
type ACPRouting struct {
	Auto bool `json:"auto"`
	// Reason says why not, when Auto is false.
	Reason string            `json:"reason,omitempty"`
	Agents []ACPRoutingAgent `json:"agents"`
}

// ACPRoutingAgent is an agent the caller could name.
type ACPRoutingAgent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (s *ACPServer) handleRouting(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		writeACPError(w, "invalid_input", "GET required", http.StatusMethodNotAllowed)
		return
	}
	if s.dispatcher == nil {
		writeACPDispatchError(w, fmt.Errorf("%w: this server does not route requests", dispatch.ErrOff))
		return
	}
	over, err := s.dispatcher.Overview(r.Context(), dispatch.Request{Tenant: dispatchTenant(tenant), Source: sourceACP})
	if err != nil {
		writeACPDispatchError(w, err)
		return
	}
	resp := ACPRouting{Auto: over.Off == "", Reason: over.Off, Agents: []ACPRoutingAgent{}}
	for _, o := range over.Agents {
		resp.Agents = append(resp.Agents, ACPRoutingAgent{Name: o.Ref.Name, Description: routingDescription(o.Agent)})
	}
	jsonResponse(w, resp)
}

// routingDescription is one line about an agent, from what its owner wrote for
// people or for routing. The system prompt is not one.
func routingDescription(a *agentorcav1alpha1.Agent) string {
	if a.Spec.Routing != nil && a.Spec.Routing.Description != "" {
		return a.Spec.Routing.Description
	}
	return a.Annotations[AnnotationDescription]
}

// sessionAgentKey is where the agent a session is with is stored. Like a chain
// key it belongs to one person, because a session ID is the client's choice.
func sessionAgentKey(ctx context.Context, sessionID string) string {
	user := ""
	if t, ok := TenantFromContext(ctx); ok && t != nil {
		user = t.UserID
	}
	sum := sha256.Sum256([]byte(user + "\x00" + sessionID))
	return "acp-agent:" + hex.EncodeToString(sum[:])
}

// rememberSessionAgent notes who a session is with, so that a later "auto" run
// in it stays there. It is kept as a checkpoint whose LastRunRef holds the
// agent's namespace/name.
func (s *ACPServer) rememberSessionAgent(ctx context.Context, sessionID string, run *agentorcav1alpha1.AgentRun) {
	key := sessionAgentKey(ctx, sessionID)
	cp := &agentorcav1alpha1.Checkpoint{SessionID: key, LastRunRef: run.Namespace + "/" + run.Spec.AgentRef}
	if _, err := sessionStore(ctx, s.checkpoint).Save(ctx, cp); err != nil {
		slog.Warn("failed to remember an ACP session's agent", "session", sessionID, "run", run.Name, "err", err)
	}
}

// pickSessionAgent picks the agent for an "auto" run. A session stays with its
// agent for as long as the caller may run it.
func (s *ACPServer) pickSessionAgent(ctx context.Context, tenant *TenantIdentity, text, sessionID string) (
	*agentorcav1alpha1.Agent, *RoutingInfo, error) {
	req := dispatch.Request{Tenant: dispatchTenant(tenant), Source: sourceACP}
	if s.dispatchAs != nil {
		req = *s.dispatchAs
	}
	req.Text = text
	if sessionID != "" && s.checkpoint != nil {
		cp, err := sessionStore(ctx, s.checkpoint).Load(ctx, sessionAgentKey(ctx, sessionID))
		if err != nil {
			slog.Warn("failed to read an ACP session's agent", "session", sessionID, "err", err)
		} else if cp != nil {
			if ns, name, ok := strings.Cut(cp.LastRunRef, "/"); ok {
				req.Current = &dispatch.Ref{Namespace: ns, Name: name}
			}
		}
	}
	return pickAgent(ctx, s.crdClient, s.dispatcher, req)
}
