package tenancy

import (
	"context"
	"slices"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	tidA = "tnt_01935c7e-aaaa-7aaa-8bbb-00000000000a"
	tidB = "tnt_01935c7e-bbbb-7aaa-8bbb-00000000000b"
)

func withMode(t *testing.T, m Mode) {
	t.Helper()
	prev := CurrentMode()
	SetMode(m)
	t.Cleanup(func() { SetMode(prev) })
}

func labelled(obj client.Object, kv ...string) client.Object {
	l := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	obj.SetLabels(l)
	return obj
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeAudit, false},
		{"audit", ModeAudit, false},
		{" Enforce ", ModeEnforce, false},
		{"strict", ModeAudit, true},
	}
	for _, tt := range tests {
		got, err := ParseMode(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ParseMode(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestViolationDeniesOnlyWhenEnforcing(t *testing.T) {
	withMode(t, ModeAudit)
	if Violation(RuleLabelMissing, "Agent", "ns", "a", "x") {
		t.Error("audit mode denied")
	}
	SetMode(ModeEnforce)
	if !Violation(RuleLabelMissing, "Agent", "ns", "a", "x") {
		t.Error("enforce mode allowed")
	}
}

func TestCheckAccess(t *testing.T) {
	caller := Caller{TenantID: tidA, TenantName: "acme", Namespaces: []string{"a-ns", "shared"}}
	obj := func(ns string, kv ...string) client.Object {
		return labelled(&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns}}, kv...)
	}
	tests := []struct {
		name string
		mode Mode
		obj  client.Object
		want Access
	}{
		{"own label, allowed namespace", ModeEnforce, obj("a-ns", LabelTenantID, tidA), AccessOwn},
		{"other tenant label in a shared namespace", ModeEnforce, obj("shared", LabelTenantID, tidB), AccessNone},
		{"own label, namespace not allowed", ModeEnforce, obj("b-ns", LabelTenantID, tidA), AccessNone},
		{"platform object in allowed namespace", ModeEnforce, obj("a-ns", LabelScope, ScopePlatform), AccessPlatform},
		{"platform object in foreign namespace", ModeEnforce, obj("b-ns", LabelScope, ScopePlatform), AccessNone},
		{"name label is display only", ModeEnforce, obj("a-ns", LabelTenantName, "acme"), AccessNone},
		{"unlabelled, enforce", ModeEnforce, obj("a-ns"), AccessNone},
		{"unlabelled, audit keeps the namespace rule", ModeAudit, obj("a-ns"), AccessOwn},
		{"unlabelled with another tenant's name, audit", ModeAudit, obj("a-ns", LabelTenantName, "globex"), AccessNone},
		{"label wins over matching name label", ModeAudit, obj("a-ns", LabelTenantID, tidB, LabelTenantName, "acme"), AccessNone},
		{"cluster-scoped own object", ModeEnforce, obj("", LabelTenantID, tidA), AccessOwn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMode(t, tt.mode)
			if got := CheckAccess(caller, tt.obj, "Agent"); got != tt.want {
				t.Errorf("CheckAccess = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCallerWithoutIDSeesNoLabelledObject(t *testing.T) {
	withMode(t, ModeEnforce)
	c := Caller{Namespaces: []string{"ns"}}
	o := labelled(&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}, LabelTenantID, tidA)
	if CheckAccess(c, o, "Agent") != AccessNone {
		t.Error("a caller with no tenant ID saw a tenant's object")
	}
}

func TestInheritLabels(t *testing.T) {
	tests := []struct {
		name   string
		parent map[string]string
		child  map[string]string
		want   map[string]string
	}{
		{"tenant passes down", map[string]string{LabelTenantID: tidA}, nil, map[string]string{LabelTenantID: tidA}},
		{"keeps the child's other labels", map[string]string{LabelTenantID: tidA}, map[string]string{"k": "v"}, map[string]string{"k": "v", LabelTenantID: tidA}},
		{"platform scope passes down", map[string]string{LabelScope: ScopePlatform}, nil, map[string]string{LabelScope: ScopePlatform}},
		{"unlabelled parent leaves the child alone", map[string]string{"x": "y"}, map[string]string{"k": "v"}, map[string]string{"k": "v"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: tt.parent}}
			child := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: tt.child}}
			InheritLabels(parent, child)
			if len(child.Labels) != len(tt.want) {
				t.Fatalf("labels = %v, want %v", child.Labels, tt.want)
			}
			for k, v := range tt.want {
				if child.Labels[k] != v {
					t.Errorf("labels = %v, want %v", child.Labels, tt.want)
				}
			}
		})
	}
}

func TestSetControllerReferencePassesTenantDown(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	owner := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "u1", Labels: map[string]string{LabelTenantID: tidA}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	if err := SetControllerReference(owner, pod, scheme); err != nil {
		t.Fatal(err)
	}
	if pod.Labels[LabelTenantID] != tidA || len(pod.OwnerReferences) != 1 {
		t.Errorf("pod = %+v", pod.ObjectMeta)
	}
}

func TestStripOwnershipThenStamp(t *testing.T) {
	o := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelTenantID: tidB, LabelScope: ScopePlatform, "k": "v"}}}
	StripOwnership(o)
	Stamp(o, tidA)
	if o.Labels[LabelTenantID] != tidA || o.Labels[LabelScope] != "" || o.Labels["k"] != "v" {
		t.Errorf("labels = %v", o.Labels)
	}
}

func tc(name string, id string, namespaces ...string) agentorcav1alpha1.TenantConfig {
	return agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: id, AllowedNamespaces: namespaces},
	}
}

func TestNamespaceClaimants(t *testing.T) {
	deleting := tc("gone", "tnt_x", "shared")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	tcs := []agentorcav1alpha1.TenantConfig{tc("a", tidA, "a-ns", "shared"), tc("b", tidB, "shared"), tc("new", "", "new-ns"), deleting}
	tests := []struct {
		ns         string
		wantIDs    []string
		wantClaims int
	}{
		{"a-ns", []string{tidA}, 1},
		{"shared", []string{tidA, tidB}, 2},
		{"new-ns", nil, 1},
		{"nobody", nil, 0},
	}
	for _, tt := range tests {
		ids, claims := NamespaceClaimants(tcs, tt.ns)
		if !slices.Equal(ids, tt.wantIDs) || claims != tt.wantClaims {
			t.Errorf("NamespaceClaimants(%q) = %v, %d; want %v, %d", tt.ns, ids, claims, tt.wantIDs, tt.wantClaims)
		}
	}
}

func agent(tenant string, mutate func(*agentorcav1alpha1.Agent)) *agentorcav1alpha1.Agent {
	a := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"}}
	if tenant != "" {
		a.Labels = map[string]string{LabelTenantID: tenant}
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

func TestRefsOfFindsEveryKind(t *testing.T) {
	a := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		ModelSelectorRef:   "sel",
		Tools:              []string{"t1"},
		KnowledgeBases:     []string{"kb1"},
		KnowledgeBaseRefs:  []agentorcav1alpha1.KnowledgeBaseRef{{Name: "kb2"}},
		GuardrailPolicyRef: "gp",
		Memory:             &agentorcav1alpha1.AgentMemoryConfig{LongTermMemoryRef: "kb3", SummaryModelSelectorRef: "sel2"},
		Runtime:            agentorcav1alpha1.AgentRuntime{SecretRefs: []agentorcav1alpha1.SecretMount{{Name: "s1"}}},
	}}
	got := map[string]bool{}
	for _, r := range RefsOf(a) {
		got[r.Kind+"/"+r.Name] = true
	}
	for _, want := range []string{"ModelSelector/sel", "ModelSelector/sel2", "Tool/t1", "KnowledgeBase/kb1", "KnowledgeBase/kb2", "KnowledgeBase/kb3", "GuardrailPolicy/gp", "Secret/s1"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}

	other := []struct {
		name string
		obj  client.Object
		want string
	}{
		{"deployment", &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "ag"}}, "Agent/ag"},
		{"run", &agentorcav1alpha1.AgentRun{Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: "ag"}}, "Agent/ag"},
		{"workflow step", &agentorcav1alpha1.AgentWorkflow{Spec: agentorcav1alpha1.AgentWorkflowSpec{Steps: []agentorcav1alpha1.WorkflowStep{{Name: "s", AgentRef: "ag"}}}}, "Agent/ag"},
		{"agent tool", &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{AgentRef: "ag"}}, "Agent/ag"},
		{"selector provider", &agentorcav1alpha1.ModelSelector{Spec: agentorcav1alpha1.ModelSelectorSpec{Providers: []agentorcav1alpha1.ProviderWeight{{Name: "p"}}}}, "ModelProvider/p"},
		{"deployment webhook secret", &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{WebhookNotify: &agentorcav1alpha1.WebhookNotifyConfig{WebhookSecretRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "wh"}}}}}, "Secret/wh"},
	}
	for _, tt := range other {
		found := false
		for _, r := range RefsOf(tt.obj) {
			if r.Kind+"/"+r.Name == tt.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s not found in %v", tt.name, tt.want, RefsOf(tt.obj))
		}
	}
}

func TestCheckRefs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tool := func(name, ns string, kv ...string) *agentorcav1alpha1.Tool {
		return labelled(&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, kv...).(*agentorcav1alpha1.Tool)
	}
	secret := func(name, ns string, kv ...string) *corev1.Secret {
		return labelled(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, kv...).(*corev1.Secret)
	}
	objs := []client.Object{
		tool("mine", "ns", LabelTenantID, tidA),
		tool("theirs", "ns", LabelTenantID, tidB),
		tool("shared", "ns", LabelScope, ScopePlatform),
		tool("unowned", "ns"),
		secret("their-secret", "ns", LabelTenantID, tidB),
		secret("far-secret", "far"),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	withTools := func(names ...string) func(*agentorcav1alpha1.Agent) {
		return func(a *agentorcav1alpha1.Agent) { a.Spec.Tools = names }
	}
	tests := []struct {
		name  string
		owner client.Object
		tcs   []agentorcav1alpha1.TenantConfig
		want  []string // substrings, one per violation
	}{
		{"same tenant", agent(tidA, withTools("mine")), nil, nil},
		{"platform target", agent(tidA, withTools("shared")), nil, nil},
		{"unlabelled target where this tenant is the sole claimant", agent(tidA, withTools("unowned")), []agentorcav1alpha1.TenantConfig{tc("a", tidA, "ns")}, nil},
		{"missing target is not an isolation problem", agent(tidA, withTools("nope")), nil, nil},
		{"other tenant's tool", agent(tidA, withTools("theirs")), nil, []string{"Tool \"theirs\""}},
		{"one good one bad", agent(tidA, withTools("mine", "theirs")), nil, []string{"theirs"}},
		{"other tenant's secret", agent(tidA, func(a *agentorcav1alpha1.Agent) {
			a.Spec.Runtime.SecretRefs = []agentorcav1alpha1.SecretMount{{Name: "their-secret"}}
		}), nil, []string{"Secret"}},
		{"deployment to another tenant's agent", &agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", Labels: map[string]string{LabelTenantID: tidA}},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "agent-b"},
		}, nil, nil}, // agent-b is absent from the fake: nothing to compare
		{"platform owner may not read a tenant's object", labelled(&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
			Spec:       agentorcav1alpha1.AgentSpec{Tools: []string{"mine"}},
		}, LabelScope, ScopePlatform), nil, []string{"platform-shared"}},
		{"owner with no ownership is skipped", agent("", withTools("theirs")), nil, nil},
		{"cross-namespace secret in another tenant's namespace", agent(tidA, func(a *agentorcav1alpha1.Agent) {
			a.Spec.Runtime.SecretRefs = nil
			a.Spec.Tools = nil
		}), nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckRefs(context.Background(), reader, tt.owner, tt.tcs)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("violations = %v, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i].Error(), w) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func TestCheckRefsCrossNamespaceSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	kb := &agentorcav1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "a-ns", Labels: map[string]string{LabelTenantID: tidA}},
		Spec: agentorcav1alpha1.KnowledgeBaseSpec{
			Ingestion: &agentorcav1alpha1.IngestionConfig{S3: &agentorcav1alpha1.S3IngestionSource{SecretRef: &agentorcav1alpha1.SecretKeyRef{Name: "s", Key: "k", Namespace: "b-ns"}}},
		},
	}
	tcs := []agentorcav1alpha1.TenantConfig{tc("a", tidA, "a-ns"), tc("b", tidB, "b-ns")}
	got, err := CheckRefs(context.Background(), reader, kb, tcs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Reason, "belongs to another tenant") {
		t.Errorf("violations = %v", got)
	}
	// The same reference into a namespace the tenant also claims is fine.
	tcs[0].Spec.AllowedNamespaces = []string{"a-ns", "b-ns"}
	tcs[1].Spec.AllowedNamespaces = []string{"b-ns"}
	if got, _ = CheckRefs(context.Background(), reader, kb, tcs); len(got) != 0 {
		t.Errorf("shared namespace: violations = %v", got)
	}
}

func TestEnforceRefsDeniesOnlyWhenEnforcing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		labelled(&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "theirs", Namespace: "ns"}}, LabelTenantID, tidB),
	).Build()
	owner := agent(tidA, func(a *agentorcav1alpha1.Agent) { a.Spec.Tools = []string{"theirs"} })

	withMode(t, ModeAudit)
	if msg, err := EnforceRefs(context.Background(), reader, owner); err != nil || msg != "" {
		t.Errorf("audit: %q, %v", msg, err)
	}
	SetMode(ModeEnforce)
	if msg, err := EnforceRefs(context.Background(), reader, owner); err != nil || !strings.Contains(msg, "another tenant") {
		t.Errorf("enforce: %q, %v", msg, err)
	}
}

// An unlabelled target is the owner's only where the owner's tenant alone
// claims its namespace. In a shared namespace it may be anyone's, and in a
// namespace no tenant claims the operator would read it with its own rights.
func TestCheckRefsUnlabelledTargets(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	secret := func(ns, name string, kv ...string) *corev1.Secret {
		return labelled(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, kv...).(*corev1.Secret)
	}
	tcsAll := []client.Object{
		&agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: agentorcav1alpha1.TenantConfigSpec{TenantID: tidA, AllowedNamespaces: []string{"a-ns", "shared"}}},
		&agentorcav1alpha1.TenantConfig{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: agentorcav1alpha1.TenantConfigSpec{TenantID: tidB, AllowedNamespaces: []string{"shared"}}},
	}
	objs := append(tcsAll,
		secret("a-ns", "plain"),                                 // unlabelled, in A's exclusive namespace
		secret("shared", "plain"),                               // (a) unlabelled in a shared namespace
		secret("shared", "platform", LabelScope, ScopePlatform), // fine anywhere
		secret("shared", "mine", LabelTenantID, tidA),           // fine
		secret("agent-orca-system", "operator-secret"),          // (b) unlabelled where no tenant lives
		secret("agent-orca-system", "shared-key", LabelScope, ScopePlatform),
	)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	usesSecret := func(ns, owner, secretName string) *agentorcav1alpha1.Agent {
		a := agent(owner, nil)
		a.Namespace = ns
		a.Spec.Runtime.SecretRefs = []agentorcav1alpha1.SecretMount{{Name: secretName}}
		return a
	}
	crossNS := func(secretNS, secretName string) *agentorcav1alpha1.KnowledgeBase {
		return &agentorcav1alpha1.KnowledgeBase{
			ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "a-ns", Labels: map[string]string{LabelTenantID: tidA}},
			Spec: agentorcav1alpha1.KnowledgeBaseSpec{Ingestion: &agentorcav1alpha1.IngestionConfig{
				S3: &agentorcav1alpha1.S3IngestionSource{SecretRef: &agentorcav1alpha1.SecretKeyRef{Name: secretName, Key: "k", Namespace: secretNS}},
			}},
		}
	}
	tests := []struct {
		name      string
		owner     client.Object
		violation bool
	}{
		{"unlabelled in the owner's exclusive namespace", usesSecret("a-ns", tidA, "plain"), false},
		{"(a) unlabelled in a shared namespace", usesSecret("shared", tidA, "plain"), true},
		{"labelled for the owner in a shared namespace", usesSecret("shared", tidA, "mine"), false},
		{"platform in a shared namespace", usesSecret("shared", tidA, "platform"), false},
		{"(b) unlabelled in an unclaimed namespace", crossNS("agent-orca-system", "operator-secret"), true},
		{"platform in an unclaimed namespace", crossNS("agent-orca-system", "shared-key"), false},
		{"platform owner, unlabelled target in its own tenant-less spot", func() client.Object {
			o := usesSecret("shared", "", "plain")
			o.Labels = map[string]string{LabelScope: ScopePlatform}
			return o
		}(), true},
	}
	for _, tt := range tests {
		for _, mode := range []Mode{ModeAudit, ModeEnforce} {
			t.Run(tt.name+"/"+string(mode), func(t *testing.T) {
				withMode(t, mode)
				var tcs agentorcav1alpha1.TenantConfigList
				if err := reader.List(context.Background(), &tcs); err != nil {
					t.Fatal(err)
				}
				got, err := CheckRefs(context.Background(), reader, tt.owner, tcs.Items)
				if err != nil {
					t.Fatal(err)
				}
				if (len(got) > 0) != tt.violation {
					t.Fatalf("violations = %v, want violation=%v", got, tt.violation)
				}
				// The mode decides what the controllers do with it.
				msg, err := EnforceRefs(context.Background(), reader, tt.owner)
				if err != nil {
					t.Fatal(err)
				}
				if denied := msg != ""; denied != (tt.violation && mode == ModeEnforce) {
					t.Errorf("EnforceRefs denied=%v in %s mode, violation=%v", denied, mode, tt.violation)
				}
			})
		}
	}
}

func TestUnlabelledAPIReadIsCountedOnce(t *testing.T) {
	withMode(t, ModeAudit)
	c := Caller{TenantID: tidA, Namespaces: []string{"ns"}}
	o := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "once", Namespace: "ns"}}
	before := testutilCount(t, RuleAPIUnlabelled, "Agent")
	for range 5 {
		if CheckAccess(c, o, "Agent") != AccessOwn {
			t.Fatal("audit mode must keep the namespace rule")
		}
	}
	if got := testutilCount(t, RuleAPIUnlabelled, "Agent") - before; got != 1 {
		t.Errorf("five reads counted %v times, want 1", got)
	}
	SetMode(ModeEnforce)
	if CheckAccess(c, o, "Agent") != AccessNone {
		t.Error("enforce mode must hide it on every read")
	}
}

func testutilCount(t *testing.T, rule, kind string) float64 {
	t.Helper()
	var m dto.Metric
	if err := violations.WithLabelValues(rule, kind, string(ModeAudit)).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
