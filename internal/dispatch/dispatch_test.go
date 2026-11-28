package dispatch

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/catalogue"
	"github.com/heddles/agent-orca/internal/dispatch/decision"
	"github.com/heddles/agent-orca/internal/evals/judge"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/redact"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const tenantID = "tnt_01935c7e-0000-7000-8000-00000000000a"

type option func(*agentorcav1alpha1.Agent)

func labelled(k, v string) option {
	return func(a *agentorcav1alpha1.Agent) { a.Labels[k] = v }
}

func agent(ns, name string, opts ...option) *agentorcav1alpha1.Agent {
	a := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{tenancy.LabelTenantID: tenantID}},
		Spec:       agentorcav1alpha1.AgentSpec{SystemPrompt: "You are " + name},
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func tenantConfig(dispatch *agentorcav1alpha1.TenantDispatch, budget string) *agentorcav1alpha1.TenantConfig {
	return &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: "acme"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AllowedNamespaces: []string{"a", "b"}, Dispatch: dispatch, BudgetPerDayUSD: budget,
		},
		Status: agentorcav1alpha1.TenantConfigStatus{TenantID: tenantID},
	}
}

// said is a decider that picks key with a confidence.
type said struct {
	mu         sync.Mutex
	key        string
	confidence float64
	err        error
	cost       float64
	asked      []decision.Request
}

func (s *said) Decide(_ context.Context, req decision.Request) (decision.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, req)
	res := decision.Response{Provider: "local", Model: "m", Usage: decision.Usage{InputTokens: 7, CostUSD: s.cost}}
	if s.err != nil {
		return res, s.err
	}
	res.Answers = map[string]decision.Answer{questionID: {Type: decision.TypeChoice, Choice: s.key, Confidence: s.confidence}}
	return res, nil
}

type records struct {
	mu    sync.Mutex
	rows  []postgresql.DispatchDecision
	spent float64
	err   error
}

func (r *records) InsertDispatchDecision(_ context.Context, d postgresql.DispatchDecision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.rows = append(r.rows, d)
	return nil
}

func (r *records) DispatchSpendSince(context.Context, string, time.Time) (float64, error) {
	return r.spent, r.err
}

func kube(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{agentorcav1alpha1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func tenant() Tenant { return Tenant{ID: tenantID, Name: "acme", Namespaces: []string{"a", "b"}} }

func ref(s string) Ref {
	ns, name, _ := strings.Cut(s, "/")
	return Ref{ns, name}
}

func TestWhereARequestGoes(t *testing.T) {
	def := &agentorcav1alpha1.TenantDispatch{DefaultAgent: "helpdesk"}
	place := ref("b/channel-default")
	for name, tc := range map[string]struct {
		spec    *agentorcav1alpha1.TenantDispatch
		pick    string
		sure    float64
		err     error
		request func(*Request)
		agent   string
		method  Method
		reason  string
	}{
		"a sure pick is taken":              {spec: def, pick: "a/ops", sure: 0.8, agent: "a/ops", method: MethodDecided},
		"an unsure pick goes to default":    {spec: def, pick: "a/ops", sure: 0.79, agent: "a/helpdesk", method: MethodFallback, reason: ReasonLowConfidence},
		"a failed decider goes to default":  {spec: def, err: errors.New("down"), agent: "a/helpdesk", method: MethodFallback, reason: ReasonDeciderError},
		"an unoffered pick goes to default": {spec: def, pick: "z/secret", sure: 1, agent: "a/helpdesk", method: MethodFallback, reason: ReasonNoAnswer},
		"a tenant can raise the bar": {spec: &agentorcav1alpha1.TenantDispatch{DefaultAgent: "helpdesk", MinConfidencePercent: new(int32(95))},
			pick: "a/ops", sure: 0.9, agent: "a/helpdesk", method: MethodFallback, reason: ReasonLowConfidence},
		"a place's default stands in": {spec: def, pick: "a/ops", sure: 0.1, agent: "b/channel-default", method: MethodFallback,
			reason: ReasonLowConfidence, request: func(r *Request) { r.PlaceDefault = &place }},
		"with no default a fair pick is taken": {pick: "a/ops", sure: 0.5, agent: "a/ops", method: MethodDecided, reason: ReasonNoDefault},
		"an unknown default counts as none": {spec: &agentorcav1alpha1.TenantDispatch{DefaultAgent: "gone"}, pick: "a/ops", sure: 0.6,
			agent: "a/ops", method: MethodDecided, reason: ReasonNoDefault},
		"an empty request goes to default": {spec: def, pick: "a/ops", sure: 1, agent: "a/helpdesk", method: MethodFallback,
			reason: ReasonEmptyRequest, request: func(r *Request) { r.Text = "  " }},
	} {
		local := &said{key: tc.pick, confidence: tc.sure, err: tc.err}
		store := &records{}
		d := &Dispatcher{Client: kube(t, tenantConfig(tc.spec, ""), agent("a", "ops"), agent("a", "helpdesk"), agent("b", "channel-default")),
			Local: local, Records: store}
		req := Request{Tenant: tenant(), Text: "a pod is down", Source: "api"}
		if tc.request != nil {
			tc.request(&req)
		}
		got, err := d.Dispatch(context.Background(), req)
		if err != nil || got.Agent != ref(tc.agent) || got.Method != tc.method || got.Reason != tc.reason {
			t.Errorf("%s: got %s %s %q, %v", name, got.Agent, got.Method, got.Reason, err)
			continue
		}
		if len(store.rows) != 1 || store.rows[0].ID != got.ID || store.rows[0].Agent != got.Agent.Name ||
			store.rows[0].Method != string(tc.method) || store.rows[0].CandidateCount != 3 {
			t.Errorf("%s: recorded %+v", name, store.rows)
		}
	}
}

func TestWithNothingToFallBackOnAnUnsureRequestIsRefused(t *testing.T) {
	store := &records{}
	d := &Dispatcher{Client: kube(t, tenantConfig(nil, ""), agent("a", "ops"), agent("a", "writer")),
		Local: &said{key: "a/ops", confidence: 0.49}, Records: store}
	_, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"})
	var undecided *UndecidedError
	if !errors.As(err, &undecided) || undecided.Reason != ReasonLowConfidence {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(undecided.Agents, []Ref{ref("a/ops"), ref("a/writer")}) || !strings.Contains(err.Error(), "ops, writer") {
		t.Errorf("the error must name the agents: %v", err)
	}
	if len(store.rows) != 1 || store.rows[0].Method != "refused" || store.rows[0].Agent != "" {
		t.Errorf("recorded %+v", store.rows)
	}
}

// Nothing a decider or a default says can send a request to an agent the caller
// could not have named.
func TestDispatchNeverWidensAccess(t *testing.T) {
	objects := []client.Object{
		agent("a", "ops"), agent("a", "writer"),
		agent("a", "not-allowed"),
		agent("a", "other-tenant", labelled(tenancy.LabelTenantID, "tnt_01935c7e-0000-7000-8000-00000000000b")),
		agent("c", "other-namespace"),
		agent("a", "template", labelled(catalogue.TemplateLabel, "true")),
		agent("a", "opted-out", labelled(catalogue.OptOutLabel, "false")),
	}
	caller := tenant()
	caller.AllowedAgents = []string{"ops", "writer", "other-tenant", "other-namespace", "template", "opted-out"}
	forbidden := []string{"a/not-allowed", "a/other-tenant", "c/other-namespace", "a/template"}
	tenancy.SetMode(tenancy.ModeEnforce)
	defer tenancy.SetMode(tenancy.ModeAudit)

	local := &said{key: "a/ops", confidence: 0.1}
	for _, target := range forbidden {
		want := ref(target)
		spec := &agentorcav1alpha1.TenantDispatch{DefaultAgent: want.Name}
		d := &Dispatcher{Client: kube(t, append(objects, tenantConfig(spec, ""))...), Local: local}
		got, err := d.Dispatch(context.Background(), Request{Tenant: caller, Text: "x", Current: &want, PlaceDefault: &want})
		if err == nil && got.Agent == want {
			t.Errorf("%s was reached by %s", target, got.Method)
		}
	}
	criteria := local.asked[0].Questions[questionID].Criteria
	offered := make([]string, 0, len(criteria))
	for key := range criteria {
		offered = append(offered, key)
	}
	slices.Sort(offered)
	if !slices.Equal(offered, []string{"a/ops", "a/writer"}) {
		t.Errorf("offered %v", offered)
	}
}

func TestAnOptedOutAgentIsNeverPickedButCanBeTheDefaultOrKeepAConversation(t *testing.T) {
	quiet := ref("a/helpdesk")
	d := &Dispatcher{Client: kube(t, tenantConfig(&agentorcav1alpha1.TenantDispatch{DefaultAgent: "helpdesk"}, ""),
		agent("a", "ops"), agent("a", "writer"), agent("a", "helpdesk", labelled(catalogue.OptOutLabel, "false"))),
		Local: &said{key: "a/ops", confidence: 0.1}}
	got, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"})
	if err != nil || got.Agent != quiet || got.Method != MethodFallback {
		t.Errorf("default: %+v, %v", got, err)
	}
	got, err = d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x", Current: &quiet})
	if err != nil || got.Method != MethodSticky {
		t.Errorf("sticky: %+v, %v", got, err)
	}
}

func TestOnlyLimitsEveryRoute(t *testing.T) {
	local := &said{key: "a/ops", confidence: 0.1}
	elsewhere := ref("a/helpdesk")
	d := &Dispatcher{Client: kube(t, tenantConfig(&agentorcav1alpha1.TenantDispatch{DefaultAgent: "helpdesk"}, ""),
		agent("a", "ops"), agent("a", "writer"), agent("a", "helpdesk")), Local: local}
	req := Request{Tenant: tenant(), Text: "x", Current: &elsewhere, PlaceDefault: &elsewhere,
		Only: []Ref{ref("a/ops"), ref("a/writer"), ref("c/not-there")}}
	got, err := d.Dispatch(context.Background(), req)
	var undecided *UndecidedError
	if !errors.As(err, &undecided) {
		t.Fatalf("the channel does not answer with helpdesk: got %+v, %v", got, err)
	}
	if n := len(local.asked[0].Questions[questionID].Criteria); n != 2 {
		t.Errorf("offered %d", n)
	}
	req.Only = []Ref{}
	if _, err := d.Dispatch(context.Background(), req); !errors.Is(err, ErrNoAgents) {
		t.Errorf("an empty limit allows nothing: %v", err)
	}
}

func TestStickyAndOnlyNeedNoDecision(t *testing.T) {
	local := &said{err: errors.New("must not be asked")}
	store := &records{}
	current := ref("b/writer")
	d := &Dispatcher{Client: kube(t, tenantConfig(nil, ""), agent("a", "ops"), agent("b", "writer")), Local: local, Records: store}
	got, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x", Current: &current})
	if err != nil || got.Agent != current || got.Method != MethodSticky || len(store.rows) != 0 {
		t.Errorf("sticky: %+v, %v, %d rows", got, err, len(store.rows))
	}
	d.Client = kube(t, tenantConfig(nil, ""), agent("a", "ops"))
	got, err = d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x", Current: &current})
	if err != nil || got.Agent != ref("a/ops") || got.Method != MethodOnly || len(local.asked) != 0 {
		t.Errorf("one agent, and the conversation's is gone: %+v, %v", got, err)
	}
}

func TestANameInTwoNamespacesMeansTheFirst(t *testing.T) {
	local := &said{key: "a/ops", confidence: 1}
	d := &Dispatcher{Client: kube(t, tenantConfig(nil, ""), agent("a", "ops"), agent("b", "ops"), agent("b", "writer")), Local: local}
	if _, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, offered := local.asked[0].Questions[questionID].Criteria["b/ops"]; offered {
		t.Error("an agent that cannot be named was offered")
	}
}

func TestWhenAutoIsOff(t *testing.T) {
	off := false
	judged := &agentorcav1alpha1.TenantDispatch{ModelSelectorRef: &agentorcav1alpha1.DispatchModelSelectorRef{Name: "s"}}
	for name, tc := range map[string]struct {
		spec   *agentorcav1alpha1.TenantDispatch
		local  decision.Decider
		caller Tenant
	}{
		"turned off":       {spec: &agentorcav1alpha1.TenantDispatch{Enabled: &off}, local: &said{}, caller: tenant()},
		"no local service": {caller: tenant()},
		"a selector alone does not send requests out": {spec: judged, caller: tenant()},
		"judge, no selector":                          {spec: &agentorcav1alpha1.TenantDispatch{Decider: "modelSelector"}, local: &said{}, caller: tenant()},
		"no tenant config":                            {local: &said{}, caller: Tenant{ID: "tnt_01935c7e-0000-7000-8000-00000000000c", Namespaces: []string{"a"}}},
		"no tenant":                                   {local: &said{}, caller: Tenant{Namespaces: []string{"a"}}},
	} {
		d := &Dispatcher{Client: kube(t, tenantConfig(tc.spec, ""), agent("a", "ops")), Local: tc.local}
		if _, err := d.Dispatch(context.Background(), Request{Tenant: tc.caller, Text: "x"}); !errors.Is(err, ErrOff) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestSecretsAreMaskedBeforeAnyDecider(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	redact.Register(secret)
	defer redact.Forget(secret)
	local := &said{key: "a/ops", confidence: 1}
	leaky := agent("a", "ops")
	leaky.Spec.Routing = &agentorcav1alpha1.AgentRoutingSpec{
		Description: "uses " + secret, UseWhen: []string{secret}, Examples: []string{"try " + secret}}
	d := &Dispatcher{Client: kube(t, tenantConfig(nil, ""), leaky, agent("a", "writer")), Local: local}
	if _, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "my key is " + secret}); err != nil {
		t.Fatal(err)
	}
	asked := local.asked[0]
	q := asked.Questions[questionID]
	all := asked.State + q.Criteria["a/ops"] + strings.Join(q.Texts["a/ops"].Descriptions, "") + strings.Join(q.Texts["a/ops"].Examples, "")
	if strings.Contains(all, secret) {
		t.Errorf("a secret reached the decider: %q", all)
	}
}

func TestExamplesAreTrimmedEvenlyToFitARequest(t *testing.T) {
	texts := map[string]decision.OptionTexts{}
	for i := range decision.MaxOptions {
		examples := make([]string, decision.MaxExamples)
		for j := range examples {
			examples[j] = strings.Repeat("a", decision.MaxTextBytes)
		}
		texts[strings.Repeat("k", i+1)] = decision.OptionTexts{Descriptions: []string{"d"}, Examples: examples}
	}
	boundExamples(texts)
	q := decision.Question{Type: decision.TypeChoice, Criteria: map[string]string{}, Texts: texts}
	kept := len(texts["k"].Examples)
	for k, v := range texts {
		q.Criteria[k] = ""
		if len(v.Examples) != kept {
			t.Fatalf("unequal: %d and %d", len(v.Examples), kept)
		}
	}
	if kept == 0 || kept == decision.MaxExamples {
		t.Errorf("kept %d", kept)
	}
	if err := (decision.Request{State: "x", Questions: map[string]decision.Question{"q": q}}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestAStoreFailureDoesNotStopARequest(t *testing.T) {
	d := &Dispatcher{Client: kube(t, tenantConfig(nil, ""), agent("a", "ops"), agent("a", "writer")),
		Local: &said{key: "a/ops", confidence: 1}, Records: &records{err: errors.New("db down")}}
	got, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"})
	if err != nil || got.Agent != ref("a/ops") || got.ID != "" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// priced is a ModelProvider that always picks a/writer.
type priced struct{ calls *int }

func (p priced) Resolve(context.Context, tenancy.Caller, string, string) (judge.Provider, error) {
	return p, nil
}
func (p priced) Complete(context.Context, judge.Request) (judge.Reply, error) {
	*p.calls++
	return judge.Reply{Text: `{"choice":"a/writer","confidence":0.9}`, CostUSD: 0.01}, nil
}
func (p priced) PriceUSD(int, int) float64 { return 0.01 }

func selector() *agentorcav1alpha1.ModelSelector {
	return &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s", Labels: map[string]string{tenancy.LabelTenantID: tenantID}},
		Spec:       agentorcav1alpha1.ModelSelectorSpec{Providers: []agentorcav1alpha1.ProviderWeight{{Name: "p", Weight: 1}}},
	}
}

func judged(escalate bool, decider string) *agentorcav1alpha1.TenantDispatch {
	return &agentorcav1alpha1.TenantDispatch{Decider: decider, Escalate: escalate,
		ModelSelectorRef: &agentorcav1alpha1.DispatchModelSelectorRef{Name: "s"}, DefaultAgent: "ops"}
}

func TestOnlyATenantThatAskedHasRequestsSentToItsModel(t *testing.T) {
	for name, tc := range map[string]struct {
		spec  *agentorcav1alpha1.TenantDispatch
		sure  float64
		calls int
		agent string
	}{
		"local, unsure, no escalate": {judged(false, "local"), 0.1, 0, "a/ops"},
		"local, sure, escalate":      {judged(true, "local"), 0.9, 0, "a/ops"},
		"local, unsure, escalate":    {judged(true, "local"), 0.1, 1, "a/writer"},
		"modelSelector decides":      {judged(false, "modelSelector"), 0.9, 1, "a/writer"},
	} {
		calls := 0
		store := &records{}
		d := &Dispatcher{Client: kube(t, tenantConfig(tc.spec, ""), selector(), agent("a", "ops"), agent("a", "writer")),
			Local: &said{key: "a/ops", confidence: tc.sure}, Providers: priced{&calls}, Records: store}
		got, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"})
		if err != nil || calls != tc.calls || got.Agent != ref(tc.agent) {
			t.Errorf("%s: got %+v, %v, %d model calls", name, got, err, calls)
			continue
		}
		if cost := store.rows[0].CostUSD; cost != float64(tc.calls)*0.01 {
			t.Errorf("%s: recorded cost %v", name, cost)
		}
	}
}

func TestADecisionCallStaysInsideTheBudget(t *testing.T) {
	spentOnRuns := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "r", Labels: map[string]string{tenancy.LabelTenantID: tenantID},
			CreationTimestamp: metav1.Now()},
		Status: agentorcav1alpha1.AgentRunStatus{SpendUSD: "0.50"},
	}
	for name, tc := range map[string]struct {
		budget       string
		spentOnCalls float64
		storeErr     error
		calls        int
	}{
		"room":                       {budget: "1.00", spentOnCalls: 0.4, calls: 1},
		"runs and calls count":       {budget: "1.00", spentOnCalls: 0.495, calls: 0},
		"unknown spend, no call":     {budget: "1.00", storeErr: errors.New("db down"), calls: 0},
		"no budget, nothing to read": {budget: "", storeErr: errors.New("db down"), calls: 1},
	} {
		calls := 0
		d := &Dispatcher{Client: kube(t, tenantConfig(judged(false, "modelSelector"), tc.budget), selector(),
			spentOnRuns.DeepCopy(), agent("a", "ops"), agent("a", "writer")),
			Providers: priced{&calls}, Records: &records{spent: tc.spentOnCalls, err: tc.storeErr}}
		got, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"})
		if err != nil || calls != tc.calls {
			t.Errorf("%s: %d calls, %+v, %v", name, calls, got, err)
		}
		if calls == 0 && (got.Method != MethodFallback || got.Reason != ReasonDeciderError) {
			t.Errorf("%s: without a call the request goes to the default: %+v", name, got)
		}
	}
}

func TestWarmAsksOnlyTheLocalDeciderAndRecordsNothing(t *testing.T) {
	local := &said{key: "a/ops", confidence: 1}
	store := &records{}
	d := &Dispatcher{Client: kube(t, tenantConfig(judged(true, "local"), ""), selector(), agent("a", "ops"), agent("a", "writer")),
		Local: local, Records: store, Providers: priced{new(int)}}
	d.Warm(context.Background())
	if len(local.asked) != 1 || local.asked[0].State != warmState || len(store.rows) != 0 {
		t.Fatalf("asked %d times, %d rows", len(local.asked), len(store.rows))
	}
	// What was warmed is what a real request asks about, so it is already prepared.
	if _, err := d.Dispatch(context.Background(), Request{Tenant: tenant(), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	a, b := local.asked[0], local.asked[1]
	if a.Tenant != b.Tenant || !reflect.DeepEqual(a.Questions, b.Questions) {
		t.Errorf("warmed %+v, asked %+v", a.Questions, b.Questions)
	}
	local.asked = nil
	d.Client = kube(t, tenantConfig(judged(false, "modelSelector"), ""), agent("a", "ops"), agent("a", "writer"))
	d.Warm(context.Background())
	if len(local.asked) != 0 {
		t.Error("a tenant that does not use the local decider was warmed")
	}
}
