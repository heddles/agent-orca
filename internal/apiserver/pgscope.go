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

	"github.com/heddles/agent-orca/internal/checkpoint"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// pgScope scopes ctx for the PostgreSQL store from the caller's identity, so
// row-level security keeps a tenant to its own rows. This is plumbing: the
// handlers still apply their namespace filters.
//
//   - a tenant with an ID: that tenant. This includes Kubernetes ServiceAccount
//     tokens, which resolve to a TenantConfig and carry its ID (see
//     resolveServiceAccountTenant);
//   - no identity at all: the platform scope. That is authentication disabled
//     or the UI proxy's service-account flow, the only callers that already see
//     every namespace;
//   - any identity present without a tenant ID (a legacy token of an
//     unregistered tenant, or an empty identity): a scope that every store call
//     rejects. It is the empty tenant, not an absent scope, so a store that
//     defaults to the platform (AsPlatform) still refuses the call.
func pgScope(ctx context.Context) context.Context {
	t, ok := TenantFromContext(ctx)
	switch {
	case !ok || t == nil:
		return postgresql.WithPlatform(ctx)
	case t.TenantID != "":
		return postgresql.WithTenant(ctx, t.TenantID)
	}
	return postgresql.WithTenant(ctx, "")
}

// sessionStore returns the session-checkpoint store scoped to the caller's
// tenant. Session IDs are chosen by clients, so without the tenant scope two
// tenants using the same ID would share a conversation chain.
func sessionStore(ctx context.Context, cp checkpoint.Store) checkpoint.Store {
	rs, ok := cp.(*checkpoint.RedisStore)
	if !ok {
		return cp
	}
	if t, ok := TenantFromContext(ctx); ok && t != nil && t.TenantID != "" {
		return rs.ForTenant(t.TenantID)
	}
	return cp
}
