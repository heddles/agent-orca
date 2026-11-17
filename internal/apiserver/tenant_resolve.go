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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	// tenantResolveTTL bounds how long a registry answer is reused. A suspended
	// or deleted tenant stops authenticating within this window even on a
	// replica that never saw the change; the replica that did see it drops the
	// cache at once (RefreshTenants).
	tenantResolveTTL = 15 * time.Second

	// tenantResolveTimeout bounds one registry lookup on the request path.
	tenantResolveTimeout = 5 * time.Second
)

var (
	// errTenantInactive means the token's tenant is unknown, suspended or
	// deleted. Callers answer 401.
	errTenantInactive = errors.New("tenant is unknown, suspended or deleted")
	// errLegacyTokenRefused means the token predates tenant IDs and the
	// deployment no longer maps those by name.
	errLegacyTokenRefused = errors.New("token carries no tid claim and legacy tokens are disabled")
)

type tenantCacheEntry struct {
	rec     *tenancy.Record
	expires time.Time
}

// SetTenantRegistry sets the registry tokens' tid claims are resolved against.
// With none, tid is checked against the TenantConfig's status mirror instead.
func (a *ExternalAuth) SetTenantRegistry(reg tenancy.Registry) {
	a.tidMu.Lock()
	defer a.tidMu.Unlock()
	a.tenantRegistry = reg
	a.tidCache = nil
}

// SetAllowLegacyTokens chooses whether tokens without a tid claim are accepted
// and mapped by tenant name. The zero value allows them.
func (a *ExternalAuth) SetAllowLegacyTokens(allow bool) { a.rejectLegacyTokens = !allow }

// resetTenantCache drops cached registry answers so a state change takes
// effect on this replica immediately.
func (a *ExternalAuth) resetTenantCache() {
	a.tidMu.Lock()
	a.tidCache = nil
	a.tidMu.Unlock()
}

// checkTenant confirms that the tenant a token names is still the live one.
// tid is the token's tenant ID and name its tenant name. A token without a tid
// predates tenant IDs: it is mapped by name when legacy tokens are allowed,
// because name alone cannot tell a recreated tenant from the original.
func (a *ExternalAuth) checkTenant(tid, name string) error {
	if tid == "" {
		if a.rejectLegacyTokens {
			return errLegacyTokenRefused
		}
		slog.Warn("accepting token without tid claim, mapped by tenant name; legacy tokens will stop being accepted", "tenant", name)
		return nil
	}

	a.tidMu.Lock()
	reg := a.tenantRegistry
	a.tidMu.Unlock()
	if reg == nil {
		// No registry (no Postgres): the TenantConfig status is the only record.
		if tc := a.tenantConfigFor(name); tc == nil || tc.Status.TenantID != tid {
			return errTenantInactive
		}
		return nil
	}

	rec, err := a.lookupTenant(reg, tid)
	if err != nil {
		return fmt.Errorf("resolving tenant: %w", err)
	}
	if rec == nil || rec.State != tenancy.StateActive || (name != "" && rec.Name != name) {
		return errTenantInactive
	}
	return nil
}

func (a *ExternalAuth) lookupTenant(reg tenancy.Registry, tid string) (*tenancy.Record, error) {
	now := time.Now()
	a.tidMu.Lock()
	if e, ok := a.tidCache[tid]; ok && now.Before(e.expires) {
		a.tidMu.Unlock()
		return e.rec, nil
	}
	a.tidMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), tenantResolveTimeout)
	defer cancel()
	rec, err := reg.GetTenant(ctx, tid)
	if err != nil {
		return nil, err
	}

	a.tidMu.Lock()
	if a.tidCache == nil {
		a.tidCache = map[string]tenantCacheEntry{}
	}
	a.tidCache[tid] = tenantCacheEntry{rec: rec, expires: now.Add(tenantResolveTTL)}
	a.tidMu.Unlock()
	return rec, nil
}
