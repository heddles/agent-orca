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

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limits of a webhook trigger.
const (
	DefaultTriggerRatePerMin = 30
	MaxTriggerRatePerMin     = 600
	MaxTriggersPerAgent      = 20
	maxTriggerNameLen        = 80
	maxTriggerPathLen        = 200
	triggerIDPrefix          = "trg_"
)

var (
	// ErrTriggerNotFound: no trigger with that id in the call's scope.
	ErrTriggerNotFound = errors.New("trigger not found")
	// ErrInvalidTrigger: a trigger the table would not take.
	ErrInvalidTrigger = errors.New("invalid trigger")
	// ErrTriggerExists: the agent already has a trigger with that name.
	ErrTriggerExists = errors.New("the agent already has a trigger with that name")
	// ErrTooManyTriggers: the agent is at MaxTriggersPerAgent.
	ErrTooManyTriggers = errors.New("the agent has the most triggers it may have")
)

// Trigger is a webhook trigger (table triggers, migration 0018).
type Trigger struct {
	// ID is the public id in the hook URL.
	ID        string `json:"id"`
	TenantID  string `json:"tenantId,omitempty"`
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Name      string `json:"name"`
	// SigningKey is hex(sha256(secret)), the key a request is signed with. It is
	// never sent to a browser.
	SigningKey string `json:"-"`
	// InputPath is an optional dotted path into the JSON body (issue.title);
	// empty means the raw body is the input.
	InputPath string `json:"inputPath"`
	// RateLimitPerMin caps the requests per minute of this trigger.
	RateLimitPerMin int        `json:"rateLimitPerMin"`
	Enabled         bool       `json:"enabled"`
	CreatedBy       string     `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	LastFiredAt     *time.Time `json:"lastFiredAt,omitempty"`
	FireCount       int64      `json:"fireCount"`
}

// NewTriggerCredentials makes a public trigger id and a secret. Only the id
// and SigningKeyFor(secret) are stored.
func NewTriggerCredentials() (id, secret string, err error) {
	var b [48]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	return triggerIDPrefix + hex.EncodeToString(b[:16]), "whsec_" + hex.EncodeToString(b[16:]), nil
}

// SigningKeyFor is the key a sender signs with for a secret: hex(sha256(secret)).
func SigningKeyFor(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ValidTriggerID reports whether id has the shape NewTriggerCredentials makes.
func ValidTriggerID(id string) bool {
	rest, ok := strings.CutPrefix(id, triggerIDPrefix)
	if !ok || len(rest) != 32 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// Validate reports why t cannot be stored.
func (t Trigger) Validate() error {
	switch {
	case t.Namespace == "" || !evalObjectNameRe.MatchString(t.Agent):
		return fmt.Errorf("%w: a trigger belongs to a namespace and an agent", ErrInvalidTrigger)
	case strings.TrimSpace(t.Name) == "" || len(t.Name) > maxTriggerNameLen:
		return fmt.Errorf("%w: name is required and at most %d characters", ErrInvalidTrigger, maxTriggerNameLen)
	case len(t.InputPath) > maxTriggerPathLen:
		return fmt.Errorf("%w: inputPath is at most %d characters", ErrInvalidTrigger, maxTriggerPathLen)
	case t.RateLimitPerMin < 1 || t.RateLimitPerMin > MaxTriggerRatePerMin:
		return fmt.Errorf("%w: rateLimitPerMin must be between 1 and %d", ErrInvalidTrigger, MaxTriggerRatePerMin)
	case !ValidTriggerID(t.ID) || t.SigningKey == "":
		return fmt.Errorf("%w: id and signing key are required", ErrInvalidTrigger)
	case t.CreatedBy == "":
		return fmt.Errorf("%w: created by is required", ErrInvalidTrigger)
	}
	return nil
}

const triggerCols = `t.id, t.tenant_id, t.namespace, t.agent, t.name, t.secret_hash, t.input_path, t.rate_limit_per_min,
	t.enabled, t.created_by, t.created_at, t.last_fired_at, t.fire_count`

func scanTrigger(row interface{ Scan(...any) error }) (Trigger, error) {
	var t Trigger
	var fired sql.NullTime
	err := row.Scan(&t.ID, &t.TenantID, &t.Namespace, &t.Agent, &t.Name, &t.SigningKey, &t.InputPath, &t.RateLimitPerMin,
		&t.Enabled, &t.CreatedBy, &t.CreatedAt, &fired, &t.FireCount)
	if fired.Valid {
		v := fired.Time
		t.LastFiredAt = &v
	}
	return t, err
}

func triggerNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTriggerNotFound
	}
	return err
}

func wrapTrigger(what string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{ErrTriggerNotFound, ErrInvalidTrigger, ErrTriggerExists, ErrTooManyTriggers, ErrNoTenantScope} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// CreateTrigger stores a trigger under the call's tenant.
func (s *Store) CreateTrigger(ctx context.Context, t Trigger) (Trigger, error) {
	if err := t.Validate(); err != nil {
		return Trigger{}, err
	}
	var out Trigger
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, t.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM triggers WHERE tenant_id = $1 AND namespace = $2 AND agent = $3`,
			tenant, t.Namespace, t.Agent).Scan(&n); err != nil {
			return err
		}
		if n >= MaxTriggersPerAgent {
			return ErrTooManyTriggers
		}
		var dup bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM triggers WHERE tenant_id = $1 AND namespace = $2 AND agent = $3 AND name = $4)`,
			tenant, t.Namespace, t.Agent, t.Name).Scan(&dup); err != nil {
			return err
		}
		if dup {
			return ErrTriggerExists
		}
		out, err = scanTrigger(tx.QueryRowContext(ctx, `
			INSERT INTO triggers AS t (id, tenant_id, namespace, agent, name, secret_hash, input_path, rate_limit_per_min, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+triggerCols,
			t.ID, tenant, t.Namespace, t.Agent, t.Name, t.SigningKey, t.InputPath, t.RateLimitPerMin, t.CreatedBy))
		return err
	})
	if err != nil {
		return Trigger{}, wrapTrigger("creating the trigger", err)
	}
	return out, nil
}

// ListTriggers returns the triggers of a namespace, optionally of one agent,
// oldest first. The platform scope sees every tenant's.
func (s *Store) ListTriggers(ctx context.Context, namespaces []string, agent string) ([]Trigger, error) {
	out := []Trigger{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+triggerCols+` FROM triggers t
			WHERE ($1::text = '*' OR t.tenant_id = $1)
			  AND (cardinality($2::text[]) = 0 OR t.namespace = ANY($2))
			  AND ($3 = '' OR t.agent = $3)
			ORDER BY t.created_at, t.id`, scope, pgTextArray(namespaces), agent)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			t, err := scanTrigger(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapTrigger("listing triggers", err)
	}
	return out, nil
}

// GetTrigger returns one trigger of the call's scope, or ErrTriggerNotFound.
// The webhook endpoint, which has no tenant yet, calls it with the platform
// scope: the unguessable id is the lookup key, and everything after it runs as
// the tenant the row names.
func (s *Store) GetTrigger(ctx context.Context, id string) (Trigger, error) {
	var t Trigger
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		t, err = scanTrigger(tx.QueryRowContext(ctx,
			`SELECT `+triggerCols+` FROM triggers t WHERE t.id = $1 AND ($2::text = '*' OR t.tenant_id = $2)`, id, scope))
		return triggerNotFound(err)
	})
	if err != nil {
		return Trigger{}, wrapTrigger("reading the trigger", err)
	}
	return t, nil
}

// SetTriggerEnabled switches a trigger on or off and returns it.
func (s *Store) SetTriggerEnabled(ctx context.Context, id string, enabled bool) (Trigger, error) {
	var t Trigger
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		t, err = scanTrigger(tx.QueryRowContext(ctx,
			`UPDATE triggers t SET enabled = $3 WHERE t.id = $1 AND ($2::text = '*' OR t.tenant_id = $2) RETURNING `+triggerCols,
			id, scope, enabled))
		return triggerNotFound(err)
	})
	if err != nil {
		return Trigger{}, wrapTrigger("updating the trigger", err)
	}
	return t, nil
}

// DeleteTrigger removes a trigger and returns it.
func (s *Store) DeleteTrigger(ctx context.Context, id string) (Trigger, error) {
	var t Trigger
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		t, err = scanTrigger(tx.QueryRowContext(ctx,
			`DELETE FROM triggers t WHERE t.id = $1 AND ($2::text = '*' OR t.tenant_id = $2) RETURNING `+triggerCols, id, scope))
		return triggerNotFound(err)
	})
	if err != nil {
		return Trigger{}, wrapTrigger("deleting the trigger", err)
	}
	return t, nil
}

// RecordTriggerFire stamps the time of a run a trigger started.
func (s *Store) RecordTriggerFire(ctx context.Context, id string, at time.Time) error {
	return wrapTrigger("recording the trigger fire", s.scoped(ctx, func(tx dbtx, scope string) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE triggers SET last_fired_at = $3, fire_count = fire_count + 1 WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`,
			id, scope, at)
		return err
	}))
}
