package catalogue

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/decision"
)

func agent(spec agentorcav1alpha1.AgentSpec) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "net-chat", Namespace: "ops"}, Spec: spec}
}

func tool(namespace, name, description string) agentorcav1alpha1.Tool {
	return agentorcav1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentorcav1alpha1.ToolSpec{Schema: &agentorcav1alpha1.ToolSchema{Description: description}},
	}
}

func TestCardNeedsNoSetup(t *testing.T) {
	a := agent(agentorcav1alpha1.AgentSpec{
		SystemPrompt:      "You diagnose\n   the network.",
		Tools:             []string{"flows", "unknown"},
		KnowledgeBases:    []string{"ignored"},
		KnowledgeBaseRefs: []agentorcav1alpha1.KnowledgeBaseRef{{Name: "runbooks"}},
	})
	got := Card(a, Related{
		Tools: []agentorcav1alpha1.Tool{
			tool("ops", "flows", "Reads flow logs"),
			tool("other", "unknown", "another namespace's description"),
			tool("ops", "unreferenced", "not this agent's"),
		},
		KnowledgeBases: []agentorcav1alpha1.KnowledgeBase{{
			ObjectMeta: metav1.ObjectMeta{Name: "runbooks", Namespace: "ops"},
			Spec:       agentorcav1alpha1.KnowledgeBaseSpec{Description: "Network runbooks"},
		}},
	})
	want := "net-chat: You diagnose the network.\nTools: flows (Reads flow logs); unknown\nKnowledge: runbooks (Network runbooks)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestADescriptionReplacesWhatIsDerived(t *testing.T) {
	got := Card(agent(agentorcav1alpha1.AgentSpec{
		SystemPrompt: "internal instructions",
		Tools:        []string{"flows"},
		Routing: &agentorcav1alpha1.AgentRoutingSpec{
			Description: "Network problems.", Aliases: []string{"netops", " "}, Examples: []string{"why is it slow", "dns fails"},
		},
	}), Related{})
	want := "net-chat: Network problems.\nAlso called: netops\nExample requests: why is it slow; dns fails"
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestACardListsUseWhenLinesAndOnlyTheFirstExamples(t *testing.T) {
	got := Card(agent(agentorcav1alpha1.AgentSpec{
		Routing: &agentorcav1alpha1.AgentRoutingSpec{
			Description: "Network problems.", UseWhen: []string{"dns fails", "a service\nis unreachable"},
			Examples: []string{"1", "2", "3", "4", "5", "6"},
		},
	}), Related{})
	want := "net-chat: Network problems.\nUse when: dns fails; a service is unreachable\nExample requests: 1; 2; 3; 4; 5"
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestTextsKeepEachPieceApart(t *testing.T) {
	long := strings.Repeat("é", 300)
	a := agent(agentorcav1alpha1.AgentSpec{Routing: &agentorcav1alpha1.AgentRoutingSpec{
		Description: "Network problems.", Aliases: []string{"netops"},
		UseWhen: []string{"dns\nfails", " "}, Examples: []string{"why is it slow", long},
	}})
	got := Texts(a, Related{})
	want := decision.OptionTexts{
		Descriptions: []string{"net-chat: Network problems.\nAlso called: netops", "net-chat: dns fails"},
		Examples:     []string{"why is it slow", long},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if len(a.Spec.Routing.UseWhen) != 2 {
		t.Error("the agent was modified")
	}
	// The pieces are valid in a request, whatever the agent declares.
	q := decision.Question{Type: decision.TypeChoice, Criteria: map[string]string{"a": Card(a, Related{})},
		Texts: map[string]decision.OptionTexts{"a": got}}
	if err := (decision.Request{State: "x", Questions: map[string]decision.Question{"q": q}}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestExamplesAloneKeepThePrompt(t *testing.T) {
	got := Card(agent(agentorcav1alpha1.AgentSpec{
		SystemPrompt: "Network.", Routing: &agentorcav1alpha1.AgentRoutingSpec{Examples: []string{"dns fails"}},
	}), Related{})
	if got != "net-chat: Network.\nExample requests: dns fails" {
		t.Errorf("got %q", got)
	}
}

func TestAnAgentThatDeclaresNothingIsStillNamed(t *testing.T) {
	if got := Card(agent(agentorcav1alpha1.AgentSpec{}), Related{}); got != "net-chat" {
		t.Errorf("got %q", got)
	}
}

func TestCardsAreBounded(t *testing.T) {
	tools := make([]string, 100)
	related := Related{}
	for i := range tools {
		tools[i] = strings.Repeat("é", 100)
		related.Tools = append(related.Tools, tool("ops", tools[i], strings.Repeat("é", 1000)))
	}
	got := Card(agent(agentorcav1alpha1.AgentSpec{SystemPrompt: strings.Repeat("é", 100000), Tools: tools}), related)
	if len(got) > decision.MaxCriterionBytes || !utf8.ValidString(got) {
		t.Errorf("%d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestRoutable(t *testing.T) {
	a := agent(agentorcav1alpha1.AgentSpec{})
	if !Routable(a) {
		t.Error("an agent is routable by default")
	}
	a.Labels = map[string]string{OptOutLabel: "False"}
	if Routable(a) {
		t.Error("the opt-out label was ignored")
	}
}
