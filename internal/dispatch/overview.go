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

package dispatch

import (
	"context"
	"errors"
	"slices"
	"strings"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Overview is what automatic routing would offer a caller.
type Overview struct {
	// Off says why a request that names no agent would be refused; empty when
	// it would not be.
	Off string
	// Agents are the ones the caller could name, in a stable order.
	Agents []Option
	// DefaultAgent is the tenant's, whether or not the caller may run it.
	DefaultAgent string
	// HasDefault is whether an unsure request of this caller has an agent to
	// fall back to.
	HasDefault bool
}

// Option is an agent the caller could name.
type Option struct {
	Ref   Ref
	Agent *agentorcav1alpha1.Agent
	// Routable is whether the agent can be newly picked.
	Routable bool
}

// Overview tells an entry point what to offer: whether automatic routing is
// available to the tenant, and the agents req's caller could name. req needs no
// text.
func (d *Dispatcher) Overview(ctx context.Context, req Request) (Overview, error) {
	var out Overview
	set, err := d.settings(ctx, req.Tenant)
	if err == nil {
		err = d.available(set)
	}
	switch {
	case errors.Is(err, ErrOff):
		out.Off = err.Error()
	case err != nil:
		return out, err
	}
	pool, err := d.candidates(ctx, req)
	if err != nil {
		return out, err
	}
	for ref, agent := range pool.runnable {
		out.Agents = append(out.Agents, Option{Ref: ref, Agent: agent, Routable: slices.Contains(pool.routable, ref)})
	}
	slices.SortFunc(out.Agents, func(a, b Option) int { return strings.Compare(a.Ref.String(), b.Ref.String()) })
	if out.Off == "" && len(pool.routable) == 0 {
		out.Off = ErrNoAgents.Error()
	}
	out.DefaultAgent = set.defaultAgent
	_, out.HasDefault = pool.fallback(req.PlaceDefault, set.defaultAgent)
	return out, nil
}
