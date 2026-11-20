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
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Limits of a chat widget key.
const (
	DefaultWidgetRatePerMin   = 30
	MaxWidgetRatePerMin       = 600
	DefaultWidgetBudgetPerDay = 5.0
	MaxWidgetBudgetPerDay     = 10000.0
	MaxWidgetKeysPerAgent     = 10
	MaxWidgetOrigins          = 10
	widgetKeyPrefix           = "wgt_"
)

var (
	// ErrWidgetKeyNotFound: no widget key with that id in the call's scope.
	ErrWidgetKeyNotFound = errors.New("widget key not found")
	// ErrInvalidWidgetKey: a key the table would not take.
	ErrInvalidWidgetKey = errors.New("invalid widget key")
	// ErrWidgetKeyExists: the agent already has a key with that name.
	ErrWidgetKeyExists = errors.New("the agent already has a widget key with that name")
	// ErrTooManyWidgetKeys: the agent is at MaxWidgetKeysPerAgent.
	ErrTooManyWidgetKeys = errors.New("the agent has the most widget keys it may have")
)

// WidgetKey is a chat widget key (table widget_keys, migration 0019).
type WidgetKey struct {
	// ID is the public key in the embed snippet.
	ID        string `json:"id"`
	TenantID  string `json:"tenantId,omitempty"`
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Name      string `json:"name"`
	// AllowedOrigins are the page origins (scheme://host[:port]) the key answers to.
	AllowedOrigins []string `json:"allowedOrigins"`
	// RateLimitPerMin caps the messages per minute across every visitor of the key.
	RateLimitPerMin int `json:"rateLimitPerMin"`
	// BudgetUSDPerDay caps the spend of the key's runs per UTC day.
	BudgetUSDPerDay float64   `json:"budgetUsdPerDay"`
	Enabled         bool      `json:"enabled"`
	CreatedBy       string    `json:"createdBy"`
	CreatedAt       time.Time `json:"createdAt"`
}

// NewWidgetKeyID makes a public widget key id.
func NewWidgetKeyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return widgetKeyPrefix + hex.EncodeToString(b[:]), nil
}

// ValidWidgetKeyID reports whether id has the shape NewWidgetKeyID makes.
func ValidWidgetKeyID(id string) bool {
	rest, ok := strings.CutPrefix(id, widgetKeyPrefix)
	if !ok || len(rest) != 32 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// NormalizeOrigin returns origin as scheme://host[:port], lowercase, or an
// error. Only exact origins are accepted: no wildcards, paths or credentials.
// http is allowed for localhost only.
func NormalizeOrigin(origin string) (string, error) {
	o := strings.TrimSpace(origin)
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(o, "*") {
		return "", fmt.Errorf("%w: %q is not an origin such as https://www.example.com", ErrInvalidWidgetKey, origin)
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	switch {
	case scheme == "https":
	case scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"):
	default:
		return "", fmt.Errorf("%w: %q must be https (http is for localhost only)", ErrInvalidWidgetKey, origin)
	}
	return scheme + "://" + host, nil
}

// NormalizeOrigins normalizes and de-duplicates a list of origins.
func NormalizeOrigins(origins []string) ([]string, error) {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		n, err := NormalizeOrigin(o)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 || len(out) > MaxWidgetOrigins {
		return nil, fmt.Errorf("%w: give between 1 and %d allowed origins", ErrInvalidWidgetKey, MaxWidgetOrigins)
	}
	return out, nil
}

// OriginAllowed reports whether origin (as sent in the Origin header) is one of the key's.
func (k WidgetKey) OriginAllowed(origin string) bool {
	n, err := NormalizeOrigin(origin)
	return err == nil && slices.Contains(k.AllowedOrigins, n)
}

// Validate reports why k cannot be stored. Origins must already be normalized.
func (k WidgetKey) Validate() error {
	switch {
	case k.Namespace == "" || !evalObjectNameRe.MatchString(k.Agent):
		return fmt.Errorf("%w: a key belongs to a namespace and an agent", ErrInvalidWidgetKey)
	case strings.TrimSpace(k.Name) == "" || len(k.Name) > maxTriggerNameLen:
		return fmt.Errorf("%w: name is required and at most %d characters", ErrInvalidWidgetKey, maxTriggerNameLen)
	case k.RateLimitPerMin < 1 || k.RateLimitPerMin > MaxWidgetRatePerMin:
		return fmt.Errorf("%w: rateLimitPerMin must be between 1 and %d", ErrInvalidWidgetKey, MaxWidgetRatePerMin)
	case !(k.BudgetUSDPerDay > 0 && k.BudgetUSDPerDay <= MaxWidgetBudgetPerDay):
		return fmt.Errorf("%w: budgetUsdPerDay must be above 0 and at most %v", ErrInvalidWidgetKey, MaxWidgetBudgetPerDay)
	case !ValidWidgetKeyID(k.ID):
		return fmt.Errorf("%w: bad id", ErrInvalidWidgetKey)
	case k.CreatedBy == "":
		return fmt.Errorf("%w: created by is required", ErrInvalidWidgetKey)
	}
	if _, err := NormalizeOrigins(k.AllowedOrigins); err != nil {
		return err
	}
	return nil
}

const widgetKeyCols = `k.id, k.tenant_id, k.namespace, k.agent, k.name, k.allowed_origins, k.rate_limit_per_min,
	k.budget_usd_per_day::float8, k.enabled, k.created_by, k.created_at`

func scanWidgetKey(row interface{ Scan(...any) error }) (WidgetKey, error) {
	var k WidgetKey
	var origins string
	err := row.Scan(&k.ID, &k.TenantID, &k.Namespace, &k.Agent, &k.Name, &origins, &k.RateLimitPerMin,
		&k.BudgetUSDPerDay, &k.Enabled, &k.CreatedBy, &k.CreatedAt)
	k.AllowedOrigins = parsePgTextArray(origins)
	return k, err
}

// parsePgTextArray reads the text form of a text[] of simple values (origins
// contain no commas, quotes or braces; NormalizeOrigin guarantees it).
func parsePgTextArray(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.Trim(p, `"`)
	}
	return parts
}

func widgetKeyNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWidgetKeyNotFound
	}
	return err
}

func wrapWidgetKey(what string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{ErrWidgetKeyNotFound, ErrInvalidWidgetKey, ErrWidgetKeyExists, ErrTooManyWidgetKeys, ErrNoTenantScope} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// CreateWidgetKey stores a key under the call's tenant.
func (s *Store) CreateWidgetKey(ctx context.Context, k WidgetKey) (WidgetKey, error) {
	origins, err := NormalizeOrigins(k.AllowedOrigins)
	if err != nil {
		return WidgetKey{}, err
	}
	k.AllowedOrigins = origins
	if err := k.Validate(); err != nil {
		return WidgetKey{}, err
	}
	var out WidgetKey
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := rowTenant(scope, k.TenantID)
		if err != nil {
			return err
		}
		if tenant == "" {
			return ErrNoTenantScope
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM widget_keys WHERE tenant_id = $1 AND namespace = $2 AND agent = $3`,
			tenant, k.Namespace, k.Agent).Scan(&n); err != nil {
			return err
		}
		if n >= MaxWidgetKeysPerAgent {
			return ErrTooManyWidgetKeys
		}
		var dup bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM widget_keys WHERE tenant_id = $1 AND namespace = $2 AND agent = $3 AND name = $4)`,
			tenant, k.Namespace, k.Agent, k.Name).Scan(&dup); err != nil {
			return err
		}
		if dup {
			return ErrWidgetKeyExists
		}
		out, err = scanWidgetKey(tx.QueryRowContext(ctx, `
			INSERT INTO widget_keys AS k (id, tenant_id, namespace, agent, name, allowed_origins, rate_limit_per_min, budget_usd_per_day, created_by)
			VALUES ($1, $2, $3, $4, $5, $6::text[], $7, $8, $9)
			RETURNING `+widgetKeyCols,
			k.ID, tenant, k.Namespace, k.Agent, k.Name, pgTextArray(k.AllowedOrigins), k.RateLimitPerMin, k.BudgetUSDPerDay, k.CreatedBy))
		return err
	})
	return out, wrapWidgetKey("creating the widget key", err)
}

// ListWidgetKeys returns the keys of the given namespaces (all when empty),
// optionally of one agent, oldest first.
func (s *Store) ListWidgetKeys(ctx context.Context, namespaces []string, agent string) ([]WidgetKey, error) {
	out := []WidgetKey{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+widgetKeyCols+` FROM widget_keys k
			WHERE ($1::text = '*' OR k.tenant_id = $1)
			  AND (cardinality($2::text[]) = 0 OR k.namespace = ANY($2))
			  AND ($3 = '' OR k.agent = $3)
			ORDER BY k.created_at, k.id`, scope, pgTextArray(namespaces), agent)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			k, err := scanWidgetKey(rows)
			if err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapWidgetKey("listing widget keys", err)
	}
	return out, nil
}

// GetWidgetKey returns one key of the call's scope. The public widget
// endpoints, which have no tenant yet, call it with the platform scope.
func (s *Store) GetWidgetKey(ctx context.Context, id string) (WidgetKey, error) {
	var k WidgetKey
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		k, err = scanWidgetKey(tx.QueryRowContext(ctx,
			`SELECT `+widgetKeyCols+` FROM widget_keys k WHERE k.id = $1 AND ($2::text = '*' OR k.tenant_id = $2)`, id, scope))
		return widgetKeyNotFound(err)
	})
	if err != nil {
		return WidgetKey{}, wrapWidgetKey("reading the widget key", err)
	}
	return k, nil
}

// WidgetKeyPatch is a partial update; nil fields are left alone.
type WidgetKeyPatch struct {
	Enabled         *bool
	AllowedOrigins  []string
	RateLimitPerMin *int
	BudgetUSDPerDay *float64
}

// UpdateWidgetKey applies patch and returns the key.
func (s *Store) UpdateWidgetKey(ctx context.Context, id string, p WidgetKeyPatch) (WidgetKey, error) {
	var k WidgetKey
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, err := scanWidgetKey(tx.QueryRowContext(ctx,
			`SELECT `+widgetKeyCols+` FROM widget_keys k WHERE k.id = $1 AND ($2::text = '*' OR k.tenant_id = $2) FOR UPDATE`, id, scope))
		if err != nil {
			return widgetKeyNotFound(err)
		}
		if p.Enabled != nil {
			cur.Enabled = *p.Enabled
		}
		if p.AllowedOrigins != nil {
			if cur.AllowedOrigins, err = NormalizeOrigins(p.AllowedOrigins); err != nil {
				return err
			}
		}
		if p.RateLimitPerMin != nil {
			cur.RateLimitPerMin = *p.RateLimitPerMin
		}
		if p.BudgetUSDPerDay != nil {
			cur.BudgetUSDPerDay = *p.BudgetUSDPerDay
		}
		if err := cur.Validate(); err != nil {
			return err
		}
		k, err = scanWidgetKey(tx.QueryRowContext(ctx, `
			UPDATE widget_keys k SET enabled = $2, allowed_origins = $3::text[], rate_limit_per_min = $4, budget_usd_per_day = $5
			WHERE k.id = $1 RETURNING `+widgetKeyCols,
			id, cur.Enabled, pgTextArray(cur.AllowedOrigins), cur.RateLimitPerMin, cur.BudgetUSDPerDay))
		return err
	})
	return k, wrapWidgetKey("updating the widget key", err)
}

// DeleteWidgetKey removes a key and returns it.
func (s *Store) DeleteWidgetKey(ctx context.Context, id string) (WidgetKey, error) {
	var k WidgetKey
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		k, err = scanWidgetKey(tx.QueryRowContext(ctx,
			`DELETE FROM widget_keys k WHERE k.id = $1 AND ($2::text = '*' OR k.tenant_id = $2) RETURNING `+widgetKeyCols, id, scope))
		return widgetKeyNotFound(err)
	})
	return k, wrapWidgetKey("deleting the widget key", err)
}
