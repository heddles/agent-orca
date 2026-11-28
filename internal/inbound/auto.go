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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// Automatic places (docs/inbound-channels.md): in a place of a shared app that
// an administrator set to pick automatically, a message that names no agent and
// continues no thread goes to the one that fits what it asks. Only agents the
// sender may ask there are in the running. The pick is made after the rate
// limits, because it can cost money.

// refusedUndecided: no agent could be picked, so the sender was asked to name one.
const refusedUndecided = "agent_undecided"

// AgentRef names an agent.
type AgentRef struct{ Namespace, Name string }

// PickRequest asks which of Among should take a message.
type PickRequest struct {
	TenantID string
	// Text is what the sender wrote.
	Text  string
	Among []AgentRef
	// Default is the place's, when the sender may ask it.
	Default *AgentRef
}

// Picked is the agent that takes a message.
type Picked struct {
	Agent AgentRef
	// Routing records how it was picked, for the run; the pipeline does not
	// read it. Empty when there was nothing to pick.
	Routing string
}

var (
	// ErrNoPicker means automatic routing is not available to the tenant. The
	// place's default answers, as in any other place.
	ErrNoPicker = errors.New("automatic routing is not available")
	// ErrNoneToPick means none of the agents can be run.
	ErrNoneToPick = errors.New("no agent can be picked")
	// ErrUndecided means no agent could be picked with enough confidence and
	// there is no default to take the message.
	ErrUndecided = errors.New("no agent could be picked")
)

// Picker picks agents. The implementation lives next to the other run starters
// (internal/apiserver).
type Picker interface {
	Pick(ctx context.Context, r PickRequest) (Picked, error)
}

// autoAt reports whether the app picks automatically where m was written.
func (s Settings) autoAt(m Message) bool {
	place := postgresql.DirectPlace
	if m.Mode != ModeDirect {
		place = normalize(m.Channel)
	}
	return slices.ContainsFunc(s.AutoPlaces, func(p string) bool { return normalize(p) == place })
}

// askable are the candidates whose own lists admit the sender of m, and, when
// there are none, why the one that would have answered refuses it.
func askable(c Connection, in []candidate, caps Capabilities, m Message) (out []candidate, why string) {
	for _, cand := range in {
		refusal := c.as(cand.route).Settings.Refusal(caps, m)
		if refusal == "" {
			out = append(out, cand)
		} else if why == "" || cand.isDefault {
			why = refusal
		}
	}
	if len(out) > 0 {
		why = ""
	}
	return out, why
}

func defaultOf(in []candidate) (postgresql.InboundRoute, bool) {
	for _, c := range in {
		if c.isDefault {
			return c.route, true
		}
	}
	return postgresql.InboundRoute{}, false
}

// pickRoute has an agent picked among choices, all of which the sender may ask,
// and returns the connection as that agent's. With no choices to make, c is
// returned as it is. reason is why the message goes to no agent; the sender has
// then been told what to do, where there is something to do.
func (p *Pipeline) pickRoute(ctx context.Context, c Connection, m Message, choices []candidate) (
	out Connection, routing, reason string) {
	if len(choices) == 0 {
		return c, "", ""
	}
	req := PickRequest{TenantID: c.TenantID, Text: m.Text}
	names := make([]string, 0, len(choices))
	for _, cand := range choices {
		ref := AgentRef{Namespace: cand.route.Namespace, Name: cand.route.Agent}
		req.Among = append(req.Among, ref)
		if cand.isDefault && req.Default == nil {
			req.Default = &ref
		}
		names = append(names, ref.Name)
	}

	err := ErrNoPicker
	var picked Picked
	if p.Picker != nil {
		picked, err = p.Picker.Pick(ctx, req)
	}
	switch {
	case err == nil:
		for _, cand := range choices {
			if cand.route.Namespace == picked.Agent.Namespace && cand.route.Agent == picked.Agent.Name {
				return c.as(cand.route), picked.Routing, ""
			}
		}
		err = fmt.Errorf("the pick %s/%s is not one of the choices", picked.Agent.Namespace, picked.Agent.Name)
	case errors.Is(err, ErrNoneToPick):
		return c, "", refusedNoAgent
	}
	if !errors.Is(err, ErrUndecided) {
		if !errors.Is(err, ErrNoPicker) {
			slog.Warn("picking an agent for a message", "connection", c.ID, "err", err)
		}
		if r, ok := defaultOf(choices); ok {
			return c.as(r), "", ""
		}
	}
	p.notify(ctx, c, m, "I could not tell which agent should take this. Start your message with the name of one: "+
		strings.Join(names, ", ")+".")
	return c, "", refusedUndecided
}

// pickedNote goes ahead of the first answer of a thread an agent was picked
// for, so the sender knows who has it and how to ask another.
func pickedNote(agent, text string) string {
	return fmt.Sprintf("Picked %s for this. To ask another agent, start your message with its name.\n\n%s", agent, text)
}
