package apiserver

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// publishedFixture holds one agent of every kind the published rule tells apart.
// None but "deployed-draft" has an AgentDeployment.
func publishedFixture(t *testing.T) *UIServer {
	t.Helper()
	meta := func(name string, ann, labels map[string]string) metav1.ObjectMeta {
		l := map[string]string{tenancy.LabelTenantID: "tnt_a"}
		maps.Copy(l, labels)
		return metav1.ObjectMeta{Name: name, Namespace: insightsNS, Labels: l, Annotations: ann}
	}
	agent := func(name string, ann, labels map[string]string, spec agentorcav1alpha1.AgentSpec) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{ObjectMeta: meta(name, ann, labels), Spec: spec}
	}
	git := map[string]string{"argocd.argoproj.io/tracking-id": "app:agentorca.agentorca.io/Agent:" + insightsNS + "/x"}
	draft := map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}
	ok := agentorcav1alpha1.AgentSpec{ModelSelectorRef: "deep", Tools: []string{"ledger", "_done"}, KnowledgeBases: []string{"books"}}
	with := func(f func(*agentorcav1alpha1.AgentSpec)) agentorcav1alpha1.AgentSpec {
		s := *ok.DeepCopy()
		f(&s)
		return s
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		&agentorcav1alpha1.ModelSelector{ObjectMeta: meta("deep", nil, nil)},
		&agentorcav1alpha1.Tool{ObjectMeta: meta("ledger", nil, nil)},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: meta("books", nil, nil)},
		agent("from-git", git, nil, ok),
		agent("applied-by-hand", nil, nil, ok),
		agent("ui-draft", draft, nil, ok),
		agent("deployed-draft", draft, nil, ok),
		agent("a-template", git, map[string]string{LabelTemplate: templateMarker}, ok),
		agent("no-selector", git, nil, with(func(s *agentorcav1alpha1.AgentSpec) { s.ModelSelectorRef = "gone" })),
		agent("no-tool", git, nil, with(func(s *agentorcav1alpha1.AgentSpec) { s.Tools = []string{"missing-tool"} })),
		agent("no-knowledge", git, nil, with(func(s *agentorcav1alpha1.AgentSpec) { s.KnowledgeBases = []string{"missing-kb"} })),
		&agentorcav1alpha1.AgentDeployment{ObjectMeta: meta("deployed-draft", nil, nil),
			Spec: agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "deployed-draft"}},
	).Build()
	return &UIServer{crdClient: cl}
}

// publishedWant is each agent's expected state, and what the reason must name.
var publishedWant = map[string]struct {
	published, deployed bool
	reason              string
}{
	"from-git":        {published: true},
	"applied-by-hand": {published: true},
	"deployed-draft":  {published: true, deployed: true},
	"ui-draft":        {reason: "draft"},
	"a-template":      {reason: "template"},
	"no-selector":     {reason: `ModelSelector "gone"`},
	"no-tool":         {reason: `Tool "missing-tool"`},
	"no-knowledge":    {reason: `KnowledgeBase "missing-kb"`},
}

func TestCatalogCallsAnAgentPublishedWhenItCanRun(t *testing.T) {
	s := publishedFixture(t)
	rec := agentsReq(s, catalogTenant, http.MethodGet, "/api/catalog", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body)
	}
	var got []catalogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(publishedWant) {
		t.Fatalf("got %d agents: %s", len(got), rec.Body)
	}
	for _, e := range got {
		want := publishedWant[e.Name]
		if e.Published != want.published || e.Conversational != want.deployed {
			t.Errorf("%s: published=%v conversational=%v, want %v and %v (%s)", e.Name, e.Published, e.Conversational, want.published, want.deployed, e.NotPublishedReason)
		}
		if (want.reason == "") != (e.NotPublishedReason == "") || !strings.Contains(e.NotPublishedReason, want.reason) {
			t.Errorf("%s: reason %q, want one naming %q", e.Name, e.NotPublishedReason, want.reason)
		}
	}
}

// The channel, trigger and chat app gates and inbound messages ask about one
// agent; they must agree with the catalog.
func TestOneAgentsPublishedStateMatchesTheCatalog(t *testing.T) {
	s := publishedFixture(t)
	ctx := context.Background()
	for name, want := range publishedWant {
		var a agentorcav1alpha1.Agent
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: insightsNS, Name: name}, &a); err != nil {
			t.Fatal(err)
		}
		reason, err := s.notPublishedReason(ctx, &a)
		if err != nil {
			t.Fatal(err)
		}
		if (reason == "") != want.published || !strings.Contains(reason, want.reason) {
			t.Errorf("%s: reason %q, want published=%v naming %q", name, reason, want.published, want.reason)
		}
	}
}
