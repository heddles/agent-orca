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

// Package tenancy defines a tenant's stable identity: the ID format, the
// registry that is the source of truth for which IDs exist and in what state,
// and the rule that keeps the registry in step with TenantConfigs. A tenant
// name is only a label an operator can reuse; the ID is never reused.
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	// IDPrefix starts every tenant ID.
	IDPrefix = "tnt_"

	// NamespaceModeLabel marks a Namespace that several TenantConfigs may claim.
	NamespaceModeLabel = "agentorca.io/namespace-mode"
	// NamespaceModeShared is the NamespaceModeLabel value that allows sharing.
	NamespaceModeShared = "shared"

	// RegistryFinalizer holds a TenantConfig until the registry records its
	// deletion; without it the reconciler could not tell which ID to retire.
	RegistryFinalizer = "agentorca.io/tenant-registry"
)

// State is a tenant's lifecycle state in the registry.
type State string

const (
	StateActive    State = "active"
	StateSuspended State = "suspended"
	StateDeleted   State = "deleted"
)

var idPattern = regexp.MustCompile(`^tnt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NewID returns a fresh tenant ID: "tnt_" plus a UUIDv7.
func NewID() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generating tenant ID: %w", err)
	}
	return IDPrefix + u.String(), nil
}

// ValidID reports whether s is a well-formed tenant ID.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Record is one tenant in the registry.
type Record struct {
	ID        string
	Name      string
	Namespace string
	// UID is the Kubernetes UID of the TenantConfig that owns the ID. It tells a
	// recreated TenantConfig apart from the one it replaced.
	UID       string
	State     State
	CreatedAt time.Time
	DeletedAt *time.Time
}

// Registry stores tenant records. Implementations must be safe for concurrent
// use.
type Registry interface {
	// GetTenant returns the record for id, or nil when there is none.
	GetTenant(ctx context.Context, id string) (*Record, error)
	// PutTenant inserts or replaces the record with r.ID. CreatedAt is kept when the
	// record already exists.
	PutTenant(ctx context.Context, r Record) error
	// LiveTenantsByName returns the records that are not deleted for a TenantConfig's
	// namespace and name.
	LiveTenantsByName(ctx context.Context, namespace, name string) ([]Record, error)
}

// ErrIDInUse means a pinned ID belongs to another live TenantConfig.
var ErrIDInUse = errors.New("tenant ID is bound to another live TenantConfig")

// Claim describes the TenantConfig being synced.
type Claim struct {
	Name, Namespace, UID string
	// Pinned is spec.tenantID.
	Pinned string
	// Current is status.tenantID, the mirror of the last sync.
	Current string
}

// Sync makes the registry hold an active record for the claimed TenantConfig
// and returns its ID. A pinned ID wins; otherwise the mirrored ID is kept, and
// only when there is neither (or the mirrored ID has been deleted) does a
// TenantConfig get a new one. That is what keeps a same-name recreate from
// inheriting the old tenant's data.
func Sync(ctx context.Context, reg Registry, c Claim, now time.Time) (string, error) {
	id := c.Pinned
	if id == "" {
		id = c.Current
	}
	if id != "" {
		rec, err := reg.GetTenant(ctx, id)
		if err != nil {
			return "", err
		}
		switch {
		case rec == nil:
			// Pinned fresh, or the registry was rebuilt from the mirror.
			if err := reg.PutTenant(ctx, Record{ID: id, Name: c.Name, Namespace: c.Namespace, UID: c.UID, State: StateActive, CreatedAt: now}); err != nil {
				return "", err
			}
		case rec.State == StateDeleted && c.Pinned != "":
			// Explicit restore; the webhook has already checked the requester.
			rec.Name, rec.Namespace, rec.UID, rec.State, rec.DeletedAt = c.Name, c.Namespace, c.UID, StateActive, nil
			if err := reg.PutTenant(ctx, *rec); err != nil {
				return "", err
			}
		case rec.State == StateDeleted:
			id = "" // a deleted mirror is never revived implicitly
		default:
			if rec.UID != "" && rec.UID != c.UID {
				return "", fmt.Errorf("%w: %s", ErrIDInUse, id)
			}
			if rec.Name != c.Name || rec.Namespace != c.Namespace || rec.UID != c.UID {
				rec.Name, rec.Namespace, rec.UID = c.Name, c.Namespace, c.UID
				if err := reg.PutTenant(ctx, *rec); err != nil {
					return "", err
				}
			}
		}
	}
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return "", err
		}
		if err := reg.PutTenant(ctx, Record{ID: id, Name: c.Name, Namespace: c.Namespace, UID: c.UID, State: StateActive, CreatedAt: now}); err != nil {
			return "", err
		}
	}
	return id, retireOrphans(ctx, reg, c, id, now)
}

// retireOrphans marks deleted any other live record for the same namespace and
// name: a TenantConfig whose finalizer was force-removed never reported its
// deletion, and its sessions must not outlive the replacement.
func retireOrphans(ctx context.Context, reg Registry, c Claim, keep string, now time.Time) error {
	live, err := reg.LiveTenantsByName(ctx, c.Namespace, c.Name)
	if err != nil {
		return err
	}
	for _, r := range live {
		if r.ID == keep {
			continue
		}
		r.State, r.DeletedAt = StateDeleted, &now
		if err := reg.PutTenant(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// MarkDeleted records that the tenant's TenantConfig is gone. It is idempotent.
func MarkDeleted(ctx context.Context, reg Registry, id string, now time.Time) error {
	rec, err := reg.GetTenant(ctx, id)
	if err != nil || rec == nil || rec.State == StateDeleted {
		return err
	}
	rec.State, rec.DeletedAt = StateDeleted, &now
	return reg.PutTenant(ctx, *rec)
}
