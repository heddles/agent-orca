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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// DirectPlace is the place of direct messages.
const DirectPlace = ""

// Shared reports whether c is a chat app several agents share, rather than one
// agent's own channel.
func (c InboundConnection) Shared() bool { return c.Namespace == "" && c.Agent == "" }

// InboundPlace is somewhere an agent answers: a channel id, or DirectPlace.
type InboundPlace struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Default: this agent answers messages there that name no agent.
	Default bool `json:"default"`
}

// InboundRoute is one agent's share of a chat app.
type InboundRoute struct {
	Namespace    string         `json:"namespace"`
	Agent        string         `json:"agent"`
	AllowSenders []string       `json:"allowSenders"`
	Approvers    []string       `json:"approvers"`
	Places       []InboundPlace `json:"places"`
}

// ErrRouteNotFound: the agent does not answer there.
var ErrRouteNotFound = errors.New("the agent does not answer there")

// ListInboundRoutes returns a connection's routes, in a stable order.
func (s *Store) ListInboundRoutes(ctx context.Context, connectionID string) ([]InboundRoute, error) {
	out := []InboundRoute{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		out, err = listRoutes(ctx, tx, scope, connectionID)
		return err
	})
	return out, wrapInbound("listing routes", err)
}

func listRoutes(ctx context.Context, tx dbtx, scope, connectionID string) ([]InboundRoute, error) {
	out := []InboundRoute{}
	index := map[string]int{}
	rows, err := tx.QueryContext(ctx, `
		SELECT namespace, agent, allow_senders, approvers FROM inbound_routes
		WHERE connection_id = $1 AND ($2::text = '*' OR tenant_id = $2) ORDER BY created_at, namespace, agent`, connectionID, scope)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r := InboundRoute{Places: []InboundPlace{}}
		var allow, approve []byte
		if err := rows.Scan(&r.Namespace, &r.Agent, &allow, &approve); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := errors.Join(json.Unmarshal(allow, &r.AllowSenders), json.Unmarshal(approve, &r.Approvers)); err != nil {
			_ = rows.Close()
			return nil, err
		}
		index[r.Namespace+"/"+r.Agent] = len(out)
		out = append(out, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	places, err := tx.QueryContext(ctx, `
		SELECT namespace, agent, place, place_name, is_default FROM inbound_places
		WHERE connection_id = $1 AND ($2::text = '*' OR tenant_id = $2) ORDER BY added_at, place`, connectionID, scope)
	if err != nil {
		return nil, err
	}
	defer func() { _ = places.Close() }()
	for places.Next() {
		var ns, agent string
		var p InboundPlace
		if err := places.Scan(&ns, &agent, &p.ID, &p.Name, &p.Default); err != nil {
			return nil, err
		}
		if i, ok := index[ns+"/"+agent]; ok {
			out[i].Places = append(out[i].Places, p)
		}
	}
	return out, places.Err()
}

// lockShared locks the connection row, so changes to its routes happen one at a
// time and defaults stay consistent, and returns the connection's tenant.
func lockShared(ctx context.Context, tx dbtx, scope, connectionID string) (string, error) {
	c, err := scanInbound(tx.QueryRowContext(ctx, `SELECT `+inboundCols+` FROM inbound_connections c
		WHERE c.id = $1 AND ($2::text = '*' OR c.tenant_id = $2) FOR UPDATE`, connectionID, scope))
	if err != nil {
		return "", err
	}
	if !c.Shared() {
		return "", fmt.Errorf("%w: only a shared chat app has routes", ErrInvalidInbound)
	}
	return c.TenantID, nil
}

// fillDefaults gives every place that has agents but no default to the agent
// added there first.
func fillDefaults(ctx context.Context, tx dbtx, tenant, connectionID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE inbound_places p SET is_default = TRUE
		FROM (
			SELECT DISTINCT ON (place) place, namespace, agent FROM inbound_places
			WHERE tenant_id = $1 AND connection_id = $2 ORDER BY place, added_at, namespace, agent
		) first
		WHERE p.tenant_id = $1 AND p.connection_id = $2 AND p.place = first.place AND p.namespace = first.namespace AND p.agent = first.agent
		  AND NOT EXISTS (SELECT 1 FROM inbound_places d WHERE d.tenant_id = $1 AND d.connection_id = $2 AND d.place = p.place AND d.is_default)`,
		tenant, connectionID)
	return err
}

// SaveInboundRoute creates or replaces an agent's route. A place it already had
// keeps its default; a place nobody else is in becomes its default; joining a
// place never takes the default from another agent.
func (s *Store) SaveInboundRoute(ctx context.Context, connectionID string, r InboundRoute, by string) error {
	return wrapInbound("saving the route", s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := lockShared(ctx, tx, scope, connectionID)
		if err != nil {
			return err
		}
		allow, err := json.Marshal(orEmpty(r.AllowSenders))
		if err != nil {
			return err
		}
		approve, err := json.Marshal(orEmpty(r.Approvers))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inbound_routes (tenant_id, connection_id, namespace, agent, allow_senders, approvers, created_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7)
			ON CONFLICT (tenant_id, connection_id, namespace, agent) DO UPDATE SET allow_senders = EXCLUDED.allow_senders, approvers = EXCLUDED.approvers`,
			tenant, connectionID, r.Namespace, r.Agent, string(allow), string(approve), by); err != nil {
			return err
		}
		keep := make([]string, 0, len(r.Places))
		for _, p := range r.Places {
			keep = append(keep, p.ID)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO inbound_places (tenant_id, connection_id, place, place_name, namespace, agent)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (tenant_id, connection_id, place, namespace, agent) DO UPDATE
				  SET place_name = CASE WHEN EXCLUDED.place_name <> '' THEN EXCLUDED.place_name ELSE inbound_places.place_name END`,
				tenant, connectionID, p.ID, p.Name, r.Namespace, r.Agent); err != nil {
				return err
			}
		}
		// pgTextArray of nothing is an empty array, which keeps no place.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM inbound_places WHERE tenant_id = $1 AND connection_id = $2 AND namespace = $3 AND agent = $4
			  AND NOT (place = ANY($5::text[]))`, tenant, connectionID, r.Namespace, r.Agent, pgTextArray(keep)); err != nil {
			return err
		}
		return fillDefaults(ctx, tx, tenant, connectionID)
	}))
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

// DeleteInboundRoute removes an agent from a chat app. Defaults it held pass to
// the agent that has been there longest.
func (s *Store) DeleteInboundRoute(ctx context.Context, connectionID, namespace, agent string) error {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := lockShared(ctx, tx, scope, connectionID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM inbound_routes WHERE tenant_id = $1 AND connection_id = $2 AND namespace = $3 AND agent = $4`,
			tenant, connectionID, namespace, agent)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(err, ErrRouteNotFound)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbound_places WHERE tenant_id = $1 AND connection_id = $2 AND namespace = $3 AND agent = $4`,
			tenant, connectionID, namespace, agent); err != nil {
			return err
		}
		return fillDefaults(ctx, tx, tenant, connectionID)
	})
	if errors.Is(err, ErrRouteNotFound) {
		return ErrRouteNotFound
	}
	return wrapInbound("removing the route", err)
}

// SetInboundDefault makes an agent that is in a place its default.
func (s *Store) SetInboundDefault(ctx context.Context, connectionID, place, namespace, agent string) error {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, err := lockShared(ctx, tx, scope, connectionID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE inbound_places SET is_default = FALSE WHERE tenant_id = $1 AND connection_id = $2 AND place = $3`,
			tenant, connectionID, place); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE inbound_places SET is_default = TRUE
			WHERE tenant_id = $1 AND connection_id = $2 AND place = $3 AND namespace = $4 AND agent = $5`,
			tenant, connectionID, place, namespace, agent)
		if err != nil {
			return err
		}
		// Rolled back with the transaction, so the old default stays.
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(err, ErrRouteNotFound)
		}
		return nil
	})
	if errors.Is(err, ErrRouteNotFound) {
		return ErrRouteNotFound
	}
	return wrapInbound("setting the default agent", err)
}
