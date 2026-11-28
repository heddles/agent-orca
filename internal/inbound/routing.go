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

package inbound

import (
	"context"
	"strings"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// Shared chat apps: one connection, several agents (docs/inbound-channels.md).
// Which agent answers a message is decided here, before anything else is
// checked; the connection then takes that agent's identity and lists, so every
// later check (who may ask, who may approve, the run) is that agent's.

// RouteStore lists the routes of a shared connection.
type RouteStore interface {
	ListInboundRoutes(ctx context.Context, connectionID string) ([]postgresql.InboundRoute, error)
}

// refusedNoAgent: no agent answers where the message was written.
const refusedNoAgent = "no_agent_here"

// shared reports whether the connection routes between agents.
func (c Connection) shared() bool { return c.Namespace == "" && c.Agent == "" }

// as returns the connection as one agent's route sees it. Places were already
// matched by routing, so no channel list applies; nobody is admitted by group.
func (c Connection) as(r postgresql.InboundRoute) Connection {
	c.Namespace, c.Agent, c.routed = r.Namespace, r.Agent, true
	c.Settings.AllowSenders = normalizeAll(r.AllowSenders)
	c.Settings.AllowGroups = nil
	c.Settings.AllowChannels = nil
	c.Settings.Approvers = normalizeAll(r.Approvers)
	return c
}

func normalizeAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = normalize(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

type candidate struct {
	route     postgresql.InboundRoute
	isDefault bool
}

// candidates are the routes that answer where m was written.
func candidates(routes []postgresql.InboundRoute, m Message) []candidate {
	place := postgresql.DirectPlace
	if m.Mode != ModeDirect {
		if place = normalize(m.Channel); place == "" {
			return nil
		}
	}
	var out []candidate
	for _, r := range routes {
		for _, p := range r.Places {
			if normalize(p.ID) == place {
				out = append(out, candidate{route: r, isDefault: p.Default})
				break
			}
		}
	}
	return out
}

// addressed reports whether text starts with an agent's name ("net-chat why is
// it slow", "net-chat: why"), and returns the rest. The name alone is not a
// request, so it is left as it is.
func addressed(text, agent string) (rest string, ok bool) {
	trimmed := strings.TrimSpace(text)
	i := strings.IndexAny(trimmed, " \t\n:,")
	if i < 0 || !strings.EqualFold(trimmed[:i], agent) {
		return text, false
	}
	rest = strings.TrimSpace(strings.TrimLeft(trimmed[i:], " \t\n:,"))
	if rest == "" {
		return text, false
	}
	return rest, true
}

// How a message came to its agent.
type routedBy int

const (
	routedNowhere routedBy = iota
	routedByName
	routedByThread
	routedByDefault
)

// route picks the agent for a message among in, those that answer where it was
// written: the one it names; else, in a thread, the one that answered there
// last; else the place's default. threadAgent is asked only when it is needed.
func route(in []candidate, m Message, threadAgent func() (ns, agent string)) (postgresql.InboundRoute, string, routedBy) {
	if len(in) == 0 {
		return postgresql.InboundRoute{}, m.Text, routedNowhere
	}
	// With agents of the same name in two namespaces the default one wins.
	for _, wantDefault := range []bool{true, false} {
		for _, c := range in {
			if c.isDefault == wantDefault {
				if rest, ok := addressed(m.Text, c.route.Agent); ok {
					return c.route, rest, routedByName
				}
			}
		}
	}
	if ns, agent := threadAgent(); agent != "" {
		for _, c := range in {
			if c.route.Namespace == ns && c.route.Agent == agent {
				return c.route, m.Text, routedByThread
			}
		}
	}
	if r, ok := defaultOf(in); ok {
		return r, m.Text, routedByDefault
	}
	return postgresql.InboundRoute{}, m.Text, routedNowhere
}

// routeMessage turns a shared connection into the one for the agent that
// answers m, and takes the agent's name off the text. why is a ledger reason
// when no agent answers. A connection of one agent is returned unchanged. In an
// automatic place, a message with no agent of its own is returned with the
// candidates to pick among, and the connection unchanged.
func (p *Pipeline) routeMessage(ctx context.Context, c Connection, m *Message, threadKey string) (
	out Connection, why string, toPick []candidate, err error) {
	if !c.shared() {
		return c, "", nil, nil
	}
	if p.Routes == nil {
		return c, refusedNoAgent, nil, nil
	}
	scope := postgresql.WithTenant(ctx, c.TenantID)
	routes, err := p.Routes.ListInboundRoutes(scope, c.ID)
	if err != nil {
		return c, "", nil, err
	}
	in := candidates(routes, *m)
	r, text, by := route(in, *m, func() (string, string) {
		if threadKey == "" {
			return "", ""
		}
		ns, name, err := p.Store.LatestInboundRunInThread(scope, c.ID, threadKey)
		if err != nil || name == "" {
			return "", ""
		}
		st, err := p.Runs.RunState(ctx, ns, name)
		if err != nil || !st.Found {
			return "", ""
		}
		return ns, st.Agent
	})
	if len(in) > 0 && (by == routedByDefault || by == routedNowhere) && c.Settings.autoAt(*m) {
		return c, "", in, nil
	}
	if by == routedNowhere {
		return c, refusedNoAgent, nil, nil
	}
	m.Text = text
	return c.as(r), "", nil, nil
}

// priorRun is the run a message in a thread follows on from: the thread's latest.
// It decides what the message can answer or approve, so on a shared app it counts
// only when it is of the agent the message was routed to. Otherwise naming
// another agent in the thread would put that agent's approvers over this run's
// tool calls, or answer its question.
func (p *Pipeline) priorRun(ctx context.Context, c Connection, threadKey string) string {
	ns, name, err := p.Store.LatestInboundRunInThread(postgresql.WithTenant(ctx, c.TenantID), c.ID, threadKey)
	if err != nil || name == "" {
		return ""
	}
	if !c.routed {
		return name
	}
	st, err := p.Runs.RunState(ctx, ns, name)
	if err != nil || !st.Found || st.Agent != c.Agent || ns != c.Namespace {
		return ""
	}
	return name
}

// admit routes a message and checks it against the lists of whoever answers it.
// why is the ledger reason when it is refused. When toPick is not empty an agent
// is still to be picked among them; the sender may ask each of them.
func (p *Pipeline) admit(ctx context.Context, c Connection, m *Message, caps Capabilities, threadKey string) (
	out Connection, why string, toPick []candidate, err error) {
	routed, why, toPick, err := p.routeMessage(ctx, c, m, threadKey)
	if err != nil || why != "" {
		return routed, why, nil, err
	}
	if len(toPick) == 0 {
		return routed, routed.Settings.Refusal(caps, *m), nil, nil
	}
	switch allowed, refusal := askable(c, toPick, caps, *m); len(allowed) {
	case 0:
		return c, refusal, nil, nil
	case 1:
		return c.as(allowed[0].route), "", nil, nil
	default:
		return c, "", allowed, nil
	}
}

// routeOfRun is the connection as the agent of a run sees it, for what is said
// about a run after the message that started it.
func (p *Pipeline) routeOfRun(ctx context.Context, c Connection, ns, name string) Connection {
	if !c.shared() || p.Routes == nil {
		return c
	}
	st, err := p.Runs.RunState(ctx, ns, name)
	if err != nil || !st.Found {
		return c
	}
	routes, err := p.Routes.ListInboundRoutes(postgresql.WithTenant(ctx, c.TenantID), c.ID)
	if err != nil {
		return c
	}
	for _, r := range routes {
		if r.Namespace == ns && r.Agent == st.Agent {
			return c.as(r)
		}
	}
	return c
}
