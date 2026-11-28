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
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/catalogue"
	"github.com/heddles/agent-orca/internal/dispatch/decision"
	"github.com/heddles/agent-orca/internal/redact"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// pool is what one request may go to.
type pool struct {
	// runnable are the agents this request may go to by any route: the ones the
	// caller could name here.
	runnable map[Ref]*agentorcav1alpha1.Agent
	// routable are those of them that may be picked, in a stable order.
	routable []Ref
	// keys maps an option key of the decision question back to its agent.
	keys map[string]Ref
}

// candidates finds the agents the caller could name, by the rules a request
// that names one goes through: its namespaces, the tenant's ownership of the
// object, and the tenant's allowedAgents. A name in two namespaces means the
// agent in the first, as it does when a request names it. A template is copied,
// never run in place, and req.Only limits every route, not only what a decider
// is offered.
func (d *Dispatcher) candidates(ctx context.Context, req Request) (pool, error) {
	t := req.Tenant
	p := pool{runnable: map[Ref]*agentorcav1alpha1.Agent{}, keys: map[string]Ref{}}
	if len(t.Namespaces) == 0 {
		return p, errors.New("the tenant has no authorized namespaces")
	}
	caller := tenancy.Caller{TenantID: t.ID, TenantName: t.Name, Namespaces: t.Namespaces}
	named := map[string]bool{}
	for _, ns := range t.Namespaces {
		var list agentorcav1alpha1.AgentList
		if err := d.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return p, fmt.Errorf("listing agents: %w", err)
		}
		for i := range list.Items {
			a := &list.Items[i]
			if named[a.Name] || tenancy.CheckAccess(caller, a, "Agent") == tenancy.AccessNone {
				continue
			}
			named[a.Name] = true
			ref := Ref{Namespace: a.Namespace, Name: a.Name}
			if !tenancy.AgentAllowed(t.AllowedAgents, a.Name) || a.Labels[catalogue.TemplateLabel] == "true" ||
				req.Only != nil && !slices.Contains(req.Only, ref) {
				continue
			}
			p.runnable[ref] = a
			if catalogue.Routable(a) {
				p.routable = append(p.routable, ref)
				p.keys[ref.String()] = ref
			}
		}
	}
	slices.SortFunc(p.routable, func(a, b Ref) int { return strings.Compare(a.String(), b.String()) })
	return p, nil
}

// fallback is the agent an unsure request goes to: the default of where it was
// made, else the tenant's. Either must be one the caller could name.
func (p pool) fallback(place *Ref, tenantDefault string) (Ref, bool) {
	if place != nil {
		if _, ok := p.runnable[*place]; ok {
			return *place, true
		}
	}
	for ref := range p.runnable {
		if ref.Name == tenantDefault { // names are unique among the runnable
			return ref, true
		}
	}
	return Ref{}, false
}

// question describes the routable agents to a decider. Everything in it goes
// through redaction: the modelSelector decider sends it out of the cluster.
func (d *Dispatcher) question(ctx context.Context, p pool) (decision.Question, error) {
	q := decision.Question{
		Type: decision.TypeChoice, Instructions: instructions,
		Criteria: map[string]string{}, Texts: map[string]decision.OptionTexts{},
	}
	related := map[string]catalogue.Related{}
	for _, ref := range p.routable {
		if _, ok := related[ref.Namespace]; ok {
			continue
		}
		var tools agentorcav1alpha1.ToolList
		var kbs agentorcav1alpha1.KnowledgeBaseList
		if err := d.Client.List(ctx, &tools, client.InNamespace(ref.Namespace)); err != nil {
			return q, fmt.Errorf("listing tools: %w", err)
		}
		if err := d.Client.List(ctx, &kbs, client.InNamespace(ref.Namespace)); err != nil {
			return q, fmt.Errorf("listing knowledge bases: %w", err)
		}
		related[ref.Namespace] = catalogue.Related{Tools: tools.Items, KnowledgeBases: kbs.Items}
	}
	for _, ref := range p.routable {
		agent, rel := p.runnable[ref], related[ref.Namespace]
		texts := catalogue.Texts(agent, rel)
		q.Criteria[ref.String()] = clip(redact.String(catalogue.Card(agent, rel)), decision.MaxCriterionBytes)
		q.Texts[ref.String()] = decision.OptionTexts{
			Descriptions: redacted(texts.Descriptions), Examples: redacted(texts.Examples),
		}
	}
	boundExamples(q.Texts)
	return q, nil
}

func redacted(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = clip(redact.String(s), decision.MaxTextBytes); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// boundExamples keeps a question inside the limit on its texts. It lowers how
// many examples each agent keeps, the same for all, so none is favoured.
func boundExamples(texts map[string]decision.OptionTexts) {
	for most := decision.MaxExamples; ; most-- {
		total := 0
		for _, t := range texts {
			for _, s := range t.Descriptions {
				total += len(s)
			}
			for _, s := range t.Examples[:min(len(t.Examples), most)] {
				total += len(s)
			}
		}
		if total <= decision.MaxTotalTextBytes || most == 0 {
			for key, t := range texts {
				t.Examples = t.Examples[:min(len(t.Examples), most)]
				texts[key] = t
			}
			return
		}
	}
}
