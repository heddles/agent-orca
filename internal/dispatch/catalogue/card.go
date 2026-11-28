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

// Package catalogue describes agents to whatever picks one for a request. Every
// agent gets a routing card built from what it already declares, so none needs
// setting up for it; spec.routing improves a card.
package catalogue

import (
	"strings"
	"unicode/utf8"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// OptOutLabel set to "false" keeps an agent from ever being picked.
const OptOutLabel = "agentorca.io/dispatch"

const (
	// promptRunes is how much of a system prompt a card carries. The decider
	// measured in hack/dispatch-eval used this much.
	promptRunes = 600
	maxTools    = 20
	maxKBs      = 10
	partRunes   = 160
	// maxCardExamples is how many examples a card carries. A model reads every
	// candidate's card on every decision, so they cost on each one. The local
	// decider is given all of them separately.
	maxCardExamples = 5
	// maxCardBytes keeps a card inside decision.MaxCriterionBytes.
	maxCardBytes = 4000
)

// Routable reports whether the agent may be picked.
func Routable(agent *agentorcav1alpha1.Agent) bool {
	return !strings.EqualFold(strings.TrimSpace(agent.Labels[OptOutLabel]), "false")
}

// Related are the objects an agent refers to that say what it can do. Entries the
// agent does not refer to are ignored; missing ones only make the card shorter.
type Related struct {
	Tools          []agentorcav1alpha1.Tool
	KnowledgeBases []agentorcav1alpha1.KnowledgeBase
}

// Card is the text a decision about the agent is made on. With a description in
// spec.routing it is the name and that description, plus any "use when" lines,
// aliases and the first examples. Without one it is built from the opening of
// the system prompt, the tools and the knowledge bases.
func Card(agent *agentorcav1alpha1.Agent, related Related) string {
	routing := agent.Spec.Routing
	if routing == nil {
		routing = &agentorcav1alpha1.AgentRoutingSpec{}
	}
	var b strings.Builder
	b.WriteString(agent.Name)
	description := oneLine(routing.Description)
	if description != "" {
		b.WriteString(": " + description)
	} else if prompt := oneLine(agent.Spec.SystemPrompt); prompt != "" {
		b.WriteString(": " + clip(prompt, promptRunes))
	}
	if list := join(routing.UseWhen, "; "); list != "" {
		b.WriteString("\nUse when: " + list)
	}
	if list := join(routing.Aliases, ", "); list != "" {
		b.WriteString("\nAlso called: " + list)
	}
	if list := join(routing.Examples[:min(len(routing.Examples), maxCardExamples)], "; "); list != "" {
		b.WriteString("\nExample requests: " + list)
	}
	if description == "" {
		if list := strings.Join(toolLines(agent, related.Tools), "; "); list != "" {
			b.WriteString("\nTools: " + list)
		}
		if list := strings.Join(knowledgeLines(agent, related.KnowledgeBases), "; "); list != "" {
			b.WriteString("\nKnowledge: " + list)
		}
	}
	return clipBytes(b.String(), maxCardBytes)
}

// KnowledgeBaseNames are the knowledge bases the agent refers to. The structured
// references replace the plain list, as they do at run time.
func KnowledgeBaseNames(agent *agentorcav1alpha1.Agent) []string {
	if len(agent.Spec.KnowledgeBaseRefs) == 0 {
		return agent.Spec.KnowledgeBases
	}
	names := make([]string, 0, len(agent.Spec.KnowledgeBaseRefs))
	for _, ref := range agent.Spec.KnowledgeBaseRefs {
		names = append(names, ref.Name)
	}
	return names
}

// toolLines follow the order the agent lists its tools in, so a card does not
// change with the order objects are listed in.
func toolLines(agent *agentorcav1alpha1.Agent, tools []agentorcav1alpha1.Tool) []string {
	described := map[string]string{}
	for _, t := range tools {
		if t.Namespace == agent.Namespace && t.Spec.Schema != nil {
			described[t.Name] = oneLine(t.Spec.Schema.Description)
		}
	}
	return lines(agent.Spec.Tools, described, maxTools)
}

func knowledgeLines(agent *agentorcav1alpha1.Agent, kbs []agentorcav1alpha1.KnowledgeBase) []string {
	described := map[string]string{}
	for _, kb := range kbs {
		if kb.Namespace == agent.Namespace {
			described[kb.Name] = oneLine(kb.Spec.Description)
		}
	}
	return lines(KnowledgeBaseNames(agent), described, maxKBs)
}

func lines(names []string, described map[string]string, limit int) []string {
	out := make([]string, 0, min(len(names), limit))
	for _, n := range names {
		if len(out) == limit {
			break
		}
		if d := described[n]; d != "" {
			n += " (" + clip(d, partRunes) + ")"
		}
		out = append(out, n)
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func join(items []string, sep string) string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		if i = oneLine(i); i != "" {
			out = append(out, i)
		}
	}
	return strings.Join(out, sep)
}

func clip(s string, runes int) string {
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	return string([]rune(s)[:runes])
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
