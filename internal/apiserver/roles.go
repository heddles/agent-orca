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
	"net/http"
	"slices"

	"github.com/heddles/agent-orca/internal/tenancy"
)

// Roles inside a tenant. The identity carries the caller's cumulative roles,
// resolved on each request from the groups and the live TenantConfig (see
// ExternalAuth.withRoles). Each handler that reads, runs or changes something
// names the lowest role it needs and asks requireRole before doing anything:
//
//	viewer   read
//	runner   start, cancel and chat with runs, answer clarifications, feedback
//	builder  create, update and delete resources
//	admin    ModelProvider, ModelSelector, MCPServer and Tool writes, approval
//	         decisions and the audit log
//
// A request with no identity (authentication disabled, or the UI proxy's
// service-account flow) is unchanged: it is not subject to roles.

// hasRole reports whether the caller in ctx holds role.
func hasRole(ctx context.Context, role tenancy.Role) bool {
	t, ok := TenantFromContext(ctx)
	if !ok || t == nil {
		return true
	}
	return slices.Contains(t.Roles, string(role))
}

// requireRole answers 403 when the caller lacks role and reports whether the
// handler may go on. The tenant's own roles are named in the answer so the
// caller knows what to ask an admin for.
func requireRole(w http.ResponseWriter, r *http.Request, role tenancy.Role) bool {
	if hasRole(r.Context(), role) {
		return true
	}
	http.Error(w, `{"error":"this needs the `+string(role)+` role in your tenant"}`, http.StatusForbidden)
	return false
}

// adminOnlyKinds are the resource kinds whose writes need the admin role
// rather than builder: they decide which models, servers and tools every agent
// in the tenant can reach, and where credentials go.
var adminOnlyKinds = map[string]bool{
	"ModelProvider": true,
	"ModelSelector": true,
	"MCPServer":     true,
	"Tool":          true,
}

// writeRoleFor is the role needed to create, update or delete an object of kind.
func writeRoleFor(kind string) tenancy.Role {
	if adminOnlyKinds[kind] {
		return tenancy.RoleAdmin
	}
	return tenancy.RoleBuilder
}

// requireRunnerACP is requireRole(runner) with the ACP error envelope.
func requireRunnerACP(w http.ResponseWriter, r *http.Request) bool {
	if hasRole(r.Context(), tenancy.RoleRunner) {
		return true
	}
	writeACPError(w, "forbidden", "this needs the runner role in your tenant", http.StatusForbidden)
	return false
}

// requireViewer is the gate in front of every authenticated route: an identity
// that holds no role at all (a tenant whose spec.roles lists none of the
// caller's groups and sets no defaultRole) may not even read. /api/me is the
// one exception, so the UI can tell such a user why nothing loads.
func requireViewer(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/api/me" || hasRole(r.Context(), tenancy.RoleViewer) {
		return true
	}
	http.Error(w, `{"error":"your groups give you no role in this tenant; ask a tenant admin"}`, http.StatusForbidden)
	return false
}
