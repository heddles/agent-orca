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
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// The token endpoint of the router-facing internal API (docs/mcp-oauth.md):
//
//	POST /agentrun/{ns}/{run}/mcp-token  {"server": "<MCPServer name>"}
//	  -> {"accessToken", "expiresAt", "identity"}   no refresh token, ever
//
// It authenticates like the budget and approval endpoints: the run pod's
// ServiceAccount token (audience agentorca/model-router) through TokenReview. On
// top of the namespace check those use, the caller must be the ServiceAccount of
// the run's own agent, so a pod of one agent cannot ask for another agent's
// connection. Whose connection it is comes from the AgentRun and Agent objects
// (Agent.status.publishedBy), never from the request body, which names only the
// server. The run must still be active, the server must be a per-user (maintainer or invoker) mode
// server that lists the agent in allowedAgents and that the agent uses, and the
// connection's tenant is the agent's tenant (the store's row-level security
// enforces it a second time).

const (
	maxMCPTokenBody = 1 << 10
	errCodeBadReq   = "bad_request"
	errCodeForbid   = "forbidden"
)

// mcpTokenBroker is the part of connections.Service this endpoint uses.
type mcpTokenBroker interface {
	AccessToken(ctx context.Context, req connections.TokenRequest) (connections.AccessToken, error)
}

// WithMCPTokens turns the endpoint on. Without it the endpoint answers 503.
func (s *Server) WithMCPTokens(b mcpTokenBroker) *Server {
	s.mcpTokens = b
	return s
}

func tokenFail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// agentServiceAccount is the ServiceAccount the run pods of an agent use.
func agentServiceAccount(agent *agentorcav1alpha1.Agent) string {
	if agent.Spec.ServiceAccountRef != nil {
		return agent.Spec.ServiceAccountRef.Name
	}
	return security.AgentSAName(agent.Name)
}

func (s *Server) issueMCPToken(w http.ResponseWriter, r *http.Request, caller, namespace, runName string) { //nolint:gocyclo
	if s.mcpTokens == nil {
		tokenFail(w, http.StatusServiceUnavailable, connections.CodeUnavailable, "per-user connections are not set up on this platform")
		return
	}
	if !callerInNamespace(caller, namespace) {
		tokenFail(w, http.StatusForbidden, errCodeForbid, "forbidden")
		return
	}
	var req struct {
		Server string `json:"server"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPTokenBody))
	if err != nil || json.Unmarshal(body, &req) != nil || len(validation.IsDNS1123Subdomain(req.Server)) > 0 {
		tokenFail(w, http.StatusBadRequest, errCodeBadReq, `the body must be {"server": "<MCPServer name>"}`)
		return
	}
	ctx := r.Context()
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: runName}, &run); err != nil {
		tokenFail(w, http.StatusNotFound, errCodeForbid, "not found")
		return
	}
	if postgresql.IsTerminalPhase(run.Status.Phase) {
		tokenFail(w, http.StatusConflict, connections.CodeUnavailable, "the run has ended")
		return
	}
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: run.Spec.AgentRef}, &agent); err != nil {
		tokenFail(w, http.StatusNotFound, errCodeForbid, "not found")
		return
	}
	// The caller is this run's agent's pod, not any pod of the namespace.
	if caller != "system:serviceaccount:"+namespace+":"+agentServiceAccount(&agent) {
		tokenFail(w, http.StatusForbidden, errCodeForbid, "forbidden")
		return
	}
	tenantID, _ := tenancy.TenantOf(&agent)
	if runTenant, _ := tenancy.TenantOf(&run); tenantID == "" || (runTenant != "" && runTenant != tenantID) {
		tokenFail(w, http.StatusForbidden, errCodeForbid, "the run and its agent do not belong to one tenant")
		return
	}
	var server agentorcav1alpha1.MCPServer
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: req.Server}, &server); err != nil {
		tokenFail(w, http.StatusNotFound, errCodeForbid, "not found")
		return
	}
	var oa *agentorcav1alpha1.MCPOAuthConfig
	if server.Spec.Auth != nil {
		oa = server.Spec.Auth.OAuth
	}
	mode := agentorcav1alpha1.MCPConnectionShared
	if oa != nil {
		mode = oa.Mode()
	}
	if mode != agentorcav1alpha1.MCPConnectionMaintainer && mode != agentorcav1alpha1.MCPConnectionInvoker {
		tokenFail(w, http.StatusConflict, connections.CodeUnavailable, "this server does not use a per-user connection")
		return
	}
	if serverTenant, platform := tenancy.TenantOf(&server); !platform && serverTenant != "" && serverTenant != tenantID {
		tokenFail(w, http.StatusForbidden, errCodeForbid, "forbidden")
		return
	}
	if !slices.Contains(server.Spec.AllowedAgents, agent.Name) || !s.agentUsesServer(ctx, &agent, server.Name) {
		tokenFail(w, http.StatusForbidden, errCodeForbid, "the agent may not use this server")
		return
	}
	// Whose connection: from the objects, never the request.
	var owner, label string
	if mode == agentorcav1alpha1.MCPConnectionInvoker {
		user, why := invokerOf(&run)
		if user == "" {
			// Fail closed: no fallback to the maintainer, a shared credential or anyone else.
			slog.Info("MCP invoker connection refused", "run", runName, "namespace", namespace, "server", server.Name, "reason", why)
			tokenFail(w, http.StatusConflict, connections.CodeRequired,
				"this run was not started by a person, so there is no account to use for "+server.Name+"; start it yourself from the UI")
			return
		}
		owner, label = user, " (invoker)"
	} else {
		owner, label = agent.Status.PublishedBy, " (maintainer)"
		if owner == "" {
			tokenFail(w, http.StatusConflict, connections.CodeUnavailable,
				"this agent has not been published by a user, so there is no connection owner; publish it from the builder")
			return
		}
	}
	tok, err := s.mcpTokens.AccessToken(postgresql.WithTenant(ctx, tenantID), connections.TokenRequest{
		TenantID: tenantID, UserID: owner,
		Server: connections.ServerRef{Namespace: server.Namespace, Name: server.Name, UID: string(server.UID), URL: server.Spec.URL,
			AppRef: oa.AppRef, UserScopes: oa.UserScopes},
	})
	if err != nil {
		if te := connections.AsTokenError(err); te != nil {
			slog.Info("MCP connection token refused", "run", runName, "namespace", namespace, "server", server.Name, "code", te.Code)
			tokenFail(w, http.StatusConflict, te.Code, te.Message)
			return
		}
		slog.Error("MCP connection token failed", "run", runName, "namespace", namespace, "server", server.Name, "err", err)
		tokenFail(w, http.StatusBadGateway, connections.CodeUnavailable, "the connection token could not be obtained")
		return
	}
	// The token is not logged, here or anywhere: only who it was for.
	slog.Info("MCP connection token issued", "run", runName, "namespace", namespace, "server", server.Name, "mode", mode, "identity", owner)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accessToken": tok.Token.Reveal(),
		"expiresAt":   tok.ExpiresAt.UTC().Format(time.RFC3339),
		"identity":    tok.AccountLabel + label,
	})
}

// agentUsesServer reports whether one of the agent's tools belongs to the server.
func (s *Server) agentUsesServer(ctx context.Context, agent *agentorcav1alpha1.Agent, server string) bool {
	for _, name := range agent.Spec.Tools {
		var tool agentorcav1alpha1.Tool
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &tool); err != nil {
			continue
		}
		for _, ref := range tool.OwnerReferences {
			if ref.Kind == kindMCPServer && ref.Name == server {
				return true
			}
		}
	}
	return false
}
