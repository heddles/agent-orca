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
	"net/http"

	"github.com/heddles/agent-orca/internal/dispatch"
)

// Automatic agent routing in the browser:
//
//	GET  /api/routing             viewer: whether an agent can be picked, and the default
//	POST /api/routing/run         runner: run the agent picked for the request
//
// The run takes the same body as POST /api/agents/{ns}/{name}/run, and
// ?namespace= limits the pick to one namespace.

const sourceUI = "ui"

type routingView struct {
	Auto   bool   `json:"auto"`
	Reason string `json:"reason,omitempty"`
	// DefaultAgent is the tenant's, which an unsure request goes to. HasDefault
	// is false when there is none, or the caller may not run it.
	DefaultAgent string `json:"defaultAgent,omitempty"`
	HasDefault   bool   `json:"hasDefault"`
}

func (s *UIServer) registerRoutingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/routing", s.handleUIRouting)
	mux.HandleFunc("/api/routing/run", s.handleUIRoutingRun)
}

// routingRequest is what the dispatcher is asked for a browser's caller. With a
// namespace, only its agents may be picked or fallen back to.
func (s *UIServer) routingRequest(r *http.Request, t *TenantIdentity, namespace string) (dispatch.Request, error) {
	req := dispatch.Request{Tenant: dispatchTenant(t), Source: sourceUI}
	if namespace == "" {
		return req, nil
	}
	list, err := listAgentsInNamespaces(r.Context(), s.crdClient, t)
	if err != nil {
		return req, err
	}
	req.Only = []dispatch.Ref{}
	for _, a := range list.Items {
		if a.Namespace == namespace {
			req.Only = append(req.Only, dispatch.Ref{Namespace: a.Namespace, Name: a.Name})
		}
	}
	return req, nil
}

func (s *UIServer) handleUIRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := TenantFromContext(r.Context())
	if !ok || tenant == nil || s.dispatcher == nil {
		jsonResponse(w, routingView{Reason: "automatic routing needs a signed-in tenant member"})
		return
	}
	req, err := s.routingRequest(r, tenant, r.URL.Query().Get("namespace"))
	if err != nil {
		jsonError(w, "listing agents failed", http.StatusInternalServerError)
		return
	}
	over, err := s.dispatcher.Overview(r.Context(), req)
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	jsonResponse(w, routingView{
		Auto: over.Off == "", Reason: over.Off, DefaultAgent: over.DefaultAgent, HasDefault: over.HasDefault,
	})
}

func (s *UIServer) handleUIRoutingRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := TenantFromContext(r.Context())
	if !ok || tenant == nil || s.dispatcher == nil {
		jsonError(w, "automatic routing needs a signed-in tenant member", http.StatusBadRequest)
		return
	}
	req, err := s.routingRequest(r, tenant, r.URL.Query().Get("namespace"))
	if err != nil {
		jsonError(w, "listing agents failed", http.StatusInternalServerError)
		return
	}
	acp := s.acpHelper()
	acp.dispatcher, acp.dispatchAs = s.dispatcher, &req
	acp.handleAgentRun(w, r, tenant, nil)
}
