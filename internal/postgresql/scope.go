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

package postgresql

// Tenant scope. Every query on a tenant table runs inside a transaction that
// sets the row-level-security settings from migration 0010, so the database
// enforces the tenant even if a query forgets its tenant_id filter.
//
// A call's scope is one of:
//
//   - a tenant ID: the call sees and writes only that tenant's rows;
//   - PlatformScope: the explicit bypass, for operator-internal paths
//     (controllers, the router-facing internal API, retention) and platform
//     admins. It sees every tenant's rows;
//   - nothing: the call fails with ErrNoTenantScope. A store never falls back
//     to "all rows".
//
// The scope comes from the call's context (WithTenant, WithPlatform) or, for
// a Store made by AsPlatform, from the store itself. A scope in the context
// wins over the store's default, so an API handler can narrow a platform store.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/heddles/agent-orca/internal/tenancy"
)

// PlatformScope is the scope value that selects the RLS bypass. It is not a
// well-formed tenant ID (those start with "tnt_"), so no tenant can have it.
const PlatformScope = "*"

// ErrNoTenantScope means a store call carried neither a tenant ID nor the
// platform scope.
var ErrNoTenantScope = errors.New("postgres: no tenant scope on the call")

type scopeKey struct{}

// WithTenant scopes ctx to one tenant. An empty or malformed ID yields a scope
// that every store call rejects, never a wider one.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	if !tenancy.ValidID(tenantID) {
		tenantID = ""
	}
	return context.WithValue(ctx, scopeKey{}, tenantID)
}

// WithPlatform scopes ctx to the platform: operator-internal and platform-admin
// callers that legitimately span tenants.
func WithPlatform(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, PlatformScope)
}

// AsPlatform returns a view of the store whose calls default to the platform
// scope. The operator hands it to controllers and internal APIs; a call that
// carries a tenant scope in its context still narrows to that tenant.
func (s *Store) AsPlatform() *Store {
	if s == nil {
		return nil
	}
	c := *s
	c.defaultScope = PlatformScope
	return &c
}

// scopeOf resolves the scope of a call: the context's, else the store's.
func (s *Store) scopeOf(ctx context.Context) (string, error) {
	if v, ok := ctx.Value(scopeKey{}).(string); ok {
		if v == "" {
			return "", fmt.Errorf("%w: the tenant ID is empty or malformed", ErrNoTenantScope)
		}
		return v, nil
	}
	if s.defaultScope != "" {
		return s.defaultScope, nil
	}
	return "", ErrNoTenantScope
}

// dbtx is the part of *sql.Tx the store methods use.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scoped runs fn in a transaction with the call's RLS settings applied and
// hands fn the resolved scope so it can add its tenant_id filter. Everything
// fn reads must be scanned before it returns: the transaction ends with it.
func (s *Store) scoped(ctx context.Context, fn func(tx dbtx, scope string) error) error {
	scope, err := s.scopeOf(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := applyScope(ctx, tx, scope); err != nil {
		return err
	}
	if err := fn(tx, scope); err != nil {
		return err
	}
	return tx.Commit()
}

// applyScope sets the transaction-local RLS settings (set_config's third
// argument true), which end with the transaction and so cannot leak to the
// next user of a pooled connection.
func applyScope(ctx context.Context, tx dbtx, scope string) error {
	tenant, platform := scope, ""
	if scope == PlatformScope {
		tenant, platform = "", "on"
	}
	_, err := tx.ExecContext(ctx,
		`SELECT set_config('agentorca.tenant_id', $1, true), set_config('agentorca.platform', $2, true)`,
		tenant, platform)
	if err != nil {
		return fmt.Errorf("setting the tenant scope: %w", err)
	}
	return nil
}

// tenantFilter returns the "tenant_id = $n" condition for a query. A tenant
// scope always gets one. The platform scope gets none unless the caller asked
// for one tenant's rows (want).
func tenantFilter(scope, want string, add func(any) string) (string, bool) {
	switch {
	case scope != PlatformScope:
		return "tenant_id = " + add(scope), true
	case want != "":
		return "tenant_id = " + add(want), true
	}
	return "", false
}

// tenantArg is tenantFilter for queries that number their own parameters: the
// tenant_id to compare against, and whether to compare at all.
func tenantArg(scope, want string) (string, bool) {
	if scope != PlatformScope {
		return scope, true
	}
	return want, want != ""
}

// rowTenant picks the tenant_id to store on a row: the row's own, which must
// not contradict a tenant scope, else the tenant scope. A platform-scoped row
// with no tenant is stored unattributed (NULL), visible only to the platform.
func rowTenant(scope, own string) (string, error) {
	if scope == PlatformScope {
		return own, nil
	}
	if own != "" && own != scope {
		return "", fmt.Errorf("postgres: row belongs to tenant %s but the call is scoped to %s", own, scope)
	}
	return scope, nil
}

// inScopeValue is scoped for a scope the caller names rather than the call's
// context, for operator-internal jobs such as the backfill.
func (s *Store) inScopeValue(ctx context.Context, scope string, fn func(tx dbtx, scope string) error) error {
	return s.scoped(context.WithValue(ctx, scopeKey{}, scope), fn)
}

// ScopeFrom returns the scope a context carries: a tenant ID, PlatformScope, or
// "" for a scope every call rejects. ok is false when the context has none.
func ScopeFrom(ctx context.Context) (scope string, ok bool) {
	scope, ok = ctx.Value(scopeKey{}).(string)
	return scope, ok
}
