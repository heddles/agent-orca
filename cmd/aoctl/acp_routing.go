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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// This file is the bridge's automatic routing: `aoctl acp serve --agent auto`
// serves every agent the user may run. Each thread has a choice of agent,
// offered to the editor as a session config option (and as session modes, for
// editors that know only those), with Auto first. The server picks for Auto on
// the first prompt and keeps the thread there. What the agent may do on this
// computer is its own, so it is loaded per thread, for the agent that has it.

const (
	// autoAgent, as an agent's name, asks the server to pick one.
	autoAgent = "auto"
	// agentOptionID is the session config option that chooses the agent.
	agentOptionID = "agent"
	// zedAutoName is the editor's entry for automatic routing.
	zedAutoName = "Agent Orca"

	maxOptionDescription = 160

	routedFallback  = "fallback"
	routedOnly      = "only"
	routedDecided   = "decided"
	reasonNoDefault = "no_default"
	codeUndecided   = "agent_undecided"
)

// ACPRouting mirrors GET /routing.
type ACPRouting struct {
	Auto   bool              `json:"auto"`
	Reason string            `json:"reason,omitempty"`
	Agents []ACPRoutingAgent `json:"agents"`
}

// ACPRoutingAgent is an agent the user could name.
type ACPRoutingAgent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (r ACPRouting) has(name string) bool {
	return slices.ContainsFunc(r.Agents, func(a ACPRoutingAgent) bool { return a.Name == name })
}

// ACPRoutingInfo mirrors how the server says it picked a run's agent.
type ACPRoutingInfo struct {
	Method     string  `json:"method"`
	Reason     string  `json:"reason,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// acpAPIError is a refusal of the ACP API.
type acpAPIError struct {
	Status        int
	Code, Message string
	// Agents are the ones to choose from, when none could be picked.
	Agents []string
	text   string
}

func (e *acpAPIError) Error() string { return e.text }

// GetRouting asks whether runs may name the agent "auto", and what could be
// named.
func (c *Client) GetRouting(ctx context.Context) (ACPRouting, error) {
	var out ACPRouting
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.ACP, "/")+"/routing", nil)
	if err != nil {
		return out, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONBodyBytes))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &e) != nil || e.Message == "" {
			e.Message = "this agent-orca server may be too old for automatic routing"
		}
		return out, fmt.Errorf("asking about automatic routing (HTTP %d): %s", resp.StatusCode, e.Message)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// startRouting prepares `--agent auto`. It reports false when there is an agent
// really called auto, which is then served like any other.
func (b *acpBridge) startRouting(ctx context.Context) (bool, error) {
	routing, err := b.client.GetRouting(ctx)
	if err != nil {
		if _, merr := b.client.GetAgentManifest(ctx, autoAgent); merr == nil {
			return false, nil
		}
		return false, err
	}
	if routing.has(autoAgent) {
		return false, nil
	}
	if !routing.Auto {
		return false, fmt.Errorf("%s — to use one agent instead, run `aoctl acp setup --editor zed --agent <name>`",
			routing.Reason)
	}
	b.mu.Lock()
	b.routed, b.routing = true, routing
	b.mu.Unlock()
	return true, nil
}

func (b *acpBridge) isRouted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.routed
}

// agentOf is the agent a session is with and what it may do here. In routed
// mode a session that has no agent yet may do nothing.
func (b *acpBridge) agentOf(sessionID string) (string, ACPAgentManifest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.routed {
		return b.agentName, b.manifest
	}
	if s := b.sessions[sessionID]; s != nil && s.Agent != "" {
		return s.Agent, s.manifest
	}
	return b.agentName, ACPAgentManifest{}
}

// chosenAgent is what a session's next prompt names: the agent it chose, or
// auto.
func (b *acpBridge) chosenAgent(sessionID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.routed {
		return b.agentName
	}
	if s := b.sessions[sessionID]; s != nil && s.Choice != "" {
		return s.Choice
	}
	return autoAgent
}

// mayResume reports whether a prompt may answer the run the session is waiting
// on: not when the user has since chosen another agent.
func (b *acpBridge) mayResume(sessionID string) bool {
	choice := b.chosenAgent(sessionID)
	if !b.isRouted() || choice == autoAgent {
		return true
	}
	name, _ := b.agentOf(sessionID)
	return choice == name
}

// takeAgent records that agent has the session's run: it loads what the agent
// may do here, and tells the user when the thread was routed or changed hands.
// Nothing the user allowed one agent carries to another.
func (b *acpBridge) takeAgent(ctx context.Context, sessionID, runID, agent string, how *ACPRoutingInfo) {
	man, err := b.client.GetAgentManifest(ctx, agent)
	if err != nil {
		b.srv.logf("acp: reading the manifest of %q: %v — it gets no access to this computer for this turn\n",
			agent, err)
		man = ACPAgentManifest{}
	}
	b.mu.Lock()
	s := b.sessions[sessionID]
	if s == nil {
		s = &acpSession{SessionID: sessionID}
		b.sessions[sessionID] = s
	}
	before := s.Agent
	if before != agent {
		// A turn of the last agent that is still running must not be judged by
		// the new one's policy.
		if s.Cancel != nil {
			s.Cancel()
		}
		s.WritesAllowed, s.allowedCommands = false, nil
	}
	s.Agent, s.manifest = agent, man
	b.mu.Unlock()

	if note := routingNote(before, agent, how); note != "" {
		b.emitMessageChunk(sessionID, "route_"+runID, note+"\n\n", nil)
	}
}

// routingNote is what to tell the user about who has the thread, if anything.
func routingNote(before, agent string, how *ACPRoutingInfo) string {
	if before == agent {
		return ""
	}
	note := ""
	switch {
	case how != nil && how.Method == routedFallback:
		note = fmt.Sprintf("No clear match, so this went to the default agent, **%s**.", agent)
	case how != nil && how.Method == routedOnly:
		note = fmt.Sprintf("**%s** is the only agent there is to pick.", agent)
	case how != nil && how.Method == routedDecided && how.Reason == reasonNoDefault:
		note = fmt.Sprintf("Routed to **%s** (%.0f%% sure; there is no default agent).", agent, how.Confidence*100)
	case how != nil && how.Method == routedDecided:
		note = fmt.Sprintf("Routed to **%s** (%.0f%% sure).", agent, how.Confidence*100)
	case before != "":
		note = fmt.Sprintf("Now with **%s**.", agent)
	}
	if note != "" && before != "" {
		note += " It does not see what other agents said in this thread."
	}
	return note
}

// --- the choice of agent, as the editor sees it ---

type acpConfigChoice struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpConfigOption struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	Category     string            `json:"category"`
	Type         string            `json:"type"`
	CurrentValue string            `json:"currentValue"`
	Options      []acpConfigChoice `json:"options"`
}

type acpMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpModeState struct {
	CurrentModeID  string    `json:"currentModeId"`
	AvailableModes []acpMode `json:"availableModes"`
}

// agentChoices are Auto and the agents the user may name, plus the session's
// own choice should it have gone since.
func agentChoices(routing ACPRouting, current string) []acpConfigChoice {
	out := []acpConfigChoice{{
		Value: autoAgent, Name: "Auto",
		Description: "Picks an agent for the thread from your first message, and stays with it",
	}}
	for _, a := range routing.Agents {
		d := []rune(strings.Join(strings.Fields(a.Description), " "))
		if len(d) > maxOptionDescription {
			d = append(d[:maxOptionDescription-1], '…')
		}
		out = append(out, acpConfigChoice{Value: a.Name, Name: a.Name, Description: string(d)})
	}
	if current != autoAgent && !routing.has(current) {
		out = append(out, acpConfigChoice{Value: current, Name: current})
	}
	return out
}

// sessionChoices is what a session offers in both of ACP's forms. The spec asks
// agents to send both while clients move from modes to config options.
func (b *acpBridge) sessionChoices(sessionID string) ([]acpConfigOption, *acpModeState) {
	current := b.chosenAgent(sessionID)
	b.mu.Lock()
	choices := agentChoices(b.routing, current)
	b.mu.Unlock()
	modes := &acpModeState{CurrentModeID: current}
	for _, c := range choices {
		modes.AvailableModes = append(modes.AvailableModes, acpMode{ID: c.Value, Name: c.Name, Description: c.Description})
	}
	return []acpConfigOption{{
		ID: agentOptionID, Name: "Agent", Description: "Who handles this thread",
		Category: "mode", Type: "select", CurrentValue: current, Options: choices,
	}}, modes
}

// refreshRouting rereads the agents the user may name, for a new thread. The
// last answer stands when it cannot.
func (b *acpBridge) refreshRouting(ctx context.Context) {
	routing, err := b.client.GetRouting(ctx)
	if err != nil {
		b.srv.logf("acp: refreshing the list of agents: %v\n", err)
		return
	}
	b.mu.Lock()
	b.routing = routing
	b.mu.Unlock()
}

// chooseAgent sets which agent a session's next prompt names.
func (b *acpBridge) chooseAgent(sessionID, choice string) error {
	if !b.isRouted() {
		return errors.New("this thread has one agent")
	}
	current := b.chosenAgent(sessionID)
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[sessionID]
	if s == nil {
		return fmt.Errorf("unknown session %q", sessionID)
	}
	if !slices.ContainsFunc(agentChoices(b.routing, current), func(c acpConfigChoice) bool { return c.Value == choice }) {
		return fmt.Errorf("%q is not one of the agents to choose from", choice)
	}
	s.Choice = choice
	return nil
}

func handleSetConfigOption(_ context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     any    `json:"value"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	value, ok := p.Value.(string)
	if p.ConfigID != agentOptionID || !ok {
		return nil, fmt.Errorf("unknown config option %q", p.ConfigID)
	}
	if err := b.chooseAgent(p.SessionID, value); err != nil {
		return nil, err
	}
	options, _ := b.sessionChoices(p.SessionID)
	return map[string]any{"configOptions": options}, nil
}

func handleSetMode(_ context.Context, b *acpBridge, req jsonrpcRequest) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		ModeID    string `json:"modeId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if err := b.chooseAgent(p.SessionID, p.ModeID); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// undecidedNote is what to show when no agent could be picked: how to go on.
func undecidedNote(err error) (string, bool) {
	var refusal *acpAPIError
	if !errors.As(err, &refusal) || refusal.Code != codeUndecided {
		return "", false
	}
	list := ""
	if len(refusal.Agents) > 0 {
		list = ": " + strings.Join(refusal.Agents, ", ")
	}
	return "No agent could be picked for this. Choose one in the agent menu and send it again" + list + ".", true
}
