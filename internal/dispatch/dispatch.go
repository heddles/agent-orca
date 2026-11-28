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

// Package dispatch picks the agent for a request that names none.
//
// It only ever picks among the agents the caller could have named. A decider
// proposes; the dispatcher takes the proposal when the decider is sure enough,
// and otherwise sends the request to a default agent. Every pick is recorded,
// without the request.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heddles/agent-orca/internal/dispatch/decision"
	"github.com/heddles/agent-orca/internal/dispatch/llmdecider"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/redact"
)

// How an agent was picked.
type Method string

const (
	// MethodSticky keeps the agent a conversation already has.
	MethodSticky Method = "sticky"
	// MethodOnly picks the one agent there is to pick.
	MethodOnly Method = "only"
	// MethodDecided takes a decider's pick.
	MethodDecided Method = "decided"
	// MethodFallback sends the request to a default agent.
	MethodFallback Method = "fallback"
	// methodRefused is how a request that went nowhere is recorded.
	methodRefused Method = "refused"
)

// Why a decider's pick was not simply taken.
const (
	ReasonLowConfidence = "low_confidence"
	ReasonNoAnswer      = "no_answer"
	ReasonDeciderError  = "decider_error"
	ReasonTooManyAgents = "too_many_agents"
	ReasonEmptyRequest  = "empty_request"
	// ReasonNoDefault marks a pick taken below the threshold, but at or above
	// the floor, because there is no default agent to send the request to.
	ReasonNoDefault = "no_default"
)

const (
	// DefaultThreshold is how sure a decider must be. On the benchmark's public
	// sets the local decider's picks at 0.8 or more were right 90 to 99 times in
	// 100 (hack/dispatch-eval/bench).
	DefaultThreshold = 0.8
	// floor is how sure a decider must be when there is no default agent.
	floor = 0.5

	topCandidates = 5
	questionID    = "agent"
	instructions  = "Which agent should handle this request?"
	recordTimeout = 2 * time.Second
)

// ErrOff means automatic routing is not available to the tenant.
var ErrOff = errors.New("automatic agent routing is not available")

// ErrNoAgents means the caller has no agent that could be picked.
var ErrNoAgents = errors.New("there is no agent that automatic routing can pick")

// UndecidedError means no agent could be picked with enough confidence and there
// is no default agent to take the request. The caller must name one.
type UndecidedError struct {
	Reason string
	// Agents are the ones the caller may name.
	Agents []Ref
}

func (e *UndecidedError) Error() string {
	const most = 20
	names := make([]string, 0, min(len(e.Agents), most))
	for _, a := range e.Agents[:min(len(e.Agents), most)] {
		names = append(names, a.Name)
	}
	list := strings.Join(names, ", ")
	if len(e.Agents) > most {
		list += fmt.Sprintf(" and %d more", len(e.Agents)-most)
	}
	return "no agent could be picked for this request, and the tenant has no default agent for it; name one of: " + list
}

// Tenant is who is asking, as the entry point authenticated them.
type Tenant struct {
	ID, Name string
	// Namespaces are the ones the caller may use, in the order a name is looked
	// up in them.
	Namespaces []string
	// AllowedAgents is spec.allowedAgents; empty means all.
	AllowedAgents []string
}

// Ref names an Agent.
type Ref struct{ Namespace, Name string }

func (r Ref) String() string { return r.Namespace + "/" + r.Name }

// Request is a request that names no agent.
type Request struct {
	Tenant Tenant
	// Text is what the user asked.
	Text string
	// Source is the entry point: api, acp, chat or ui.
	Source string
	// Current is the agent already on this conversation. It keeps the request
	// for as long as the caller may run it.
	Current *Ref
	// PlaceDefault is the default of where the request was made, such as a chat
	// channel. It stands in for the tenant's default agent.
	PlaceDefault *Ref
	// Only, when not nil, limits where the request may go at all, such as to the
	// agents that answer in a chat channel: it applies to the conversation's
	// agent and the defaults too. It never adds an agent.
	Only []Ref
}

// Candidate is an agent a decider considered.
type Candidate struct {
	Agent       Ref
	Probability float64
}

// Result is the agent picked, and how.
type Result struct {
	Agent  Ref
	Method Method
	// Reason says why the pick is not simply a sure decider's.
	Reason string
	// Confidence is the decider's in its own pick, which for a fallback is not
	// the agent the request went to.
	Confidence float64
	// Top are the most probable candidates.
	Top             []Candidate
	Provider, Model string
	Escalated       bool
	// ID identifies the stored record; empty when none was stored.
	ID string
}

// Records stores decisions and what they cost. *postgresql.Store is one.
type Records interface {
	InsertDispatchDecision(ctx context.Context, d postgresql.DispatchDecision) error
	DispatchSpendSince(ctx context.Context, tenantID string, since time.Time) (float64, error)
}

// Dispatcher picks agents.
type Dispatcher struct {
	// Client reads agents, tenants, selectors and providers.
	Client client.Client
	// Local is the cluster's decision service, or nil when none is deployed.
	Local decision.Decider
	// Providers resolves the providers of a tenant's ModelSelector.
	Providers llmdecider.ProviderResolver
	// Records may be nil: nothing is stored, and the cost of earlier decision
	// calls is then not counted against a budget.
	Records Records
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Dispatch picks the agent for req. It returns ErrOff, ErrNoAgents or an
// *UndecidedError when there is none to run.
func (d *Dispatcher) Dispatch(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	set, err := d.settings(ctx, req.Tenant)
	if err != nil {
		return Result{}, err
	}
	if err := d.available(set); err != nil {
		return Result{}, err
	}
	pool, err := d.candidates(ctx, req)
	if err != nil {
		return Result{}, err
	}
	// A conversation keeps its agent, even one that is never newly picked.
	if req.Current != nil {
		if _, ok := pool.runnable[*req.Current]; ok {
			res := Result{Agent: *req.Current, Method: MethodSticky}
			observe(res, time.Since(start))
			return res, nil
		}
	}
	if len(pool.routable) == 0 {
		return Result{}, ErrNoAgents
	}

	var out outcome
	switch {
	case len(pool.routable) == 1:
		out = outcome{res: Result{Agent: pool.routable[0], Method: MethodOnly, Confidence: 1}}
	default:
		out = d.decide(ctx, req, set, pool)
		out.resolve(set, req, pool)
	}
	out.res.ID = d.record(ctx, req, out, len(pool.routable), time.Since(start))
	observe(out.res, time.Since(start))
	if out.res.Method == methodRefused {
		return Result{}, &UndecidedError{Reason: out.res.Reason, Agents: pool.routable}
	}
	return out.res, nil
}

// outcome is what a decider said and what became of it.
type outcome struct {
	res Result
	// pick is the decider's choice, when it gave one.
	pick  *Ref
	usage decision.Usage
}

func (d *Dispatcher) decide(ctx context.Context, req Request, set settings, pool pool) outcome {
	var out outcome
	text := clip(strings.TrimSpace(strings.ToValidUTF8(redact.String(req.Text), "")), decision.MaxStateBytes)
	if text == "" {
		out.res.Reason = ReasonEmptyRequest
		return out
	}
	if len(pool.routable) > decision.MaxOptions {
		out.res.Reason = ReasonTooManyAgents
		return out
	}
	question, err := d.question(ctx, pool)
	if err != nil {
		slog.Error("dispatch: describing the candidates", "tenant", req.Tenant.ID, "err", err)
		out.res.Reason = ReasonDeciderError
		return out
	}
	res, err := d.decider(req.Tenant, set).Decide(ctx, decision.Request{
		Tenant: req.Tenant.ID, State: text, Questions: map[string]decision.Question{questionID: question},
	})
	// A call that failed may still have been charged.
	out.usage = res.Usage
	out.res.Provider, out.res.Model, out.res.Escalated = res.Provider, res.Model, res.Escalated
	if err != nil {
		slog.Warn("dispatch: the decider failed", "tenant", req.Tenant.ID, "err", err)
		out.res.Reason = ReasonDeciderError
		return out
	}
	answer, ok := res.ChoiceAnswer(questionID, question)
	if !ok {
		out.res.Reason = ReasonNoAnswer
		return out
	}
	picked := pool.keys[answer.Choice]
	out.pick, out.res.Confidence = &picked, answer.Confidence
	for key, p := range answer.Probabilities {
		out.res.Top = append(out.res.Top, Candidate{Agent: pool.keys[key], Probability: p})
	}
	if len(out.res.Top) == 0 {
		out.res.Top = []Candidate{{Agent: picked, Probability: answer.Confidence}}
	}
	slices.SortFunc(out.res.Top, func(a, b Candidate) int {
		switch {
		case a.Probability > b.Probability:
			return -1
		case a.Probability < b.Probability:
			return 1
		}
		return strings.Compare(a.Agent.String(), b.Agent.String())
	})
	out.res.Top = out.res.Top[:min(len(out.res.Top), topCandidates)]
	if answer.Confidence < set.threshold {
		out.res.Reason = ReasonLowConfidence
	}
	return out
}

// resolve turns what the decider said into where the request goes: its pick
// when it is sure; else the default agent, if the caller may run it; else its
// pick if that clears the floor; else nowhere.
func (o *outcome) resolve(set settings, req Request, pool pool) {
	if o.pick != nil && o.res.Reason == "" {
		o.res.Agent, o.res.Method = *o.pick, MethodDecided
		return
	}
	if fallback, ok := pool.fallback(req.PlaceDefault, set.defaultAgent); ok {
		o.res.Agent, o.res.Method = fallback, MethodFallback
		return
	}
	if o.pick != nil && o.res.Confidence >= floor {
		o.res.Agent, o.res.Method, o.res.Reason = *o.pick, MethodDecided, ReasonNoDefault
		return
	}
	o.res.Method = methodRefused
}

// record stores the outcome and returns its ID, or "" when it was not stored. A
// failure to store never stops a request.
func (d *Dispatcher) record(ctx context.Context, req Request, out outcome, candidates int, took time.Duration) string {
	if d.Records == nil || req.Tenant.ID == "" {
		return ""
	}
	r := out.res
	row := postgresql.DispatchDecision{
		ID: uuid.NewString(), TenantID: req.Tenant.ID, Source: req.Source,
		Method: string(r.Method), Reason: r.Reason,
		AgentNamespace: r.Agent.Namespace, Agent: r.Agent.Name,
		Confidence: r.Confidence, CandidateCount: candidates,
		Provider: r.Provider, Model: r.Model, Escalated: r.Escalated,
		InputTokens: out.usage.InputTokens, OutputTokens: out.usage.OutputTokens, CostUSD: out.usage.CostUSD,
		Latency: took,
	}
	for _, c := range r.Top {
		row.Candidates = append(row.Candidates, postgresql.DispatchCandidate{
			Namespace: c.Agent.Namespace, Agent: c.Agent.Name, Probability: c.Probability,
		})
	}
	// The cost was incurred even if the caller has gone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := d.Records.InsertDispatchDecision(postgresql.WithTenant(ctx, req.Tenant.ID), row); err != nil {
		recordFailures.Inc()
		slog.Error("dispatch: storing a decision", "tenant", req.Tenant.ID, "costUsd", row.CostUSD, "err", err)
		return ""
	}
	return row.ID
}

// clip cuts s to at most n bytes without splitting a rune.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
