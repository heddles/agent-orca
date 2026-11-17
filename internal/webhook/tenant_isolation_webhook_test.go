package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	tenA = "tnt_01935c7e-aaaa-7aaa-8bbb-00000000000a"
	tenB = "tnt_01935c7e-bbbb-7aaa-8bbb-00000000000b"
)

func isolationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func tenantCfg(name, id string, namespaces ...string) *agentorcav1alpha1.TenantConfig {
	return &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-orca-system"},
		Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: id, AuthMode: "issued", AllowedNamespaces: namespaces},
	}
}

func toolIn(ns, name string, kv ...string) *agentorcav1alpha1.Tool {
	t := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if len(kv) > 0 {
		t.Labels = map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			t.Labels[kv[i]] = kv[i+1]
		}
	}
	return t
}

func request(t *testing.T, op admissionv1.Operation, kind, ns string, obj, old client.Object, user authenticationv1.UserInfo) admission.Request {
	t.Helper()
	raw := func(o client.Object) runtime.RawExtension {
		if o == nil {
			return runtime.RawExtension{}
		}
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: b}
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op, Namespace: ns,
		Kind:   metav1.GroupVersionKind{Group: "agentorca.agentorca.io", Version: "v1alpha1", Kind: kind},
		Object: raw(obj), OldObject: raw(old), UserInfo: user,
	}}
}

func withMode(t *testing.T, m tenancy.Mode) {
	t.Helper()
	prev := tenancy.CurrentMode()
	tenancy.SetMode(m)
	t.Cleanup(func() { tenancy.SetMode(prev) })
}

func TestTenantLabeler(t *testing.T) {
	tcs := []client.Object{
		tenantCfg("a", tenA, "a-ns", "shared-ns"),
		tenantCfg("b", tenB, "b-ns", "shared-ns"),
	}
	reader := fake.NewClientBuilder().WithScheme(isolationScheme(t)).WithObjects(tcs...).Build()
	l := &TenantLabeler{TenantConfigs: reader}

	tests := []struct {
		name    string
		op      admissionv1.Operation
		obj     client.Object
		old     client.Object
		wantTID string // label the patch sets; "" means no patch
	}{
		{"single-owner namespace is stamped", admissionv1.Create, toolIn("a-ns", "t"), nil, tenA},
		{"other tenant's namespace gets that tenant", admissionv1.Create, toolIn("b-ns", "t"), nil, tenB},
		{"shared namespace is left to the validator", admissionv1.Create, toolIn("shared-ns", "t"), nil, ""},
		{"unclaimed namespace is left to the validator", admissionv1.Create, toolIn("nobody", "t"), nil, ""},
		{"explicit label is kept", admissionv1.Create, toolIn("a-ns", "t", tenancy.LabelTenantID, tenB), nil, ""},
		{"platform scope is not stamped", admissionv1.Create, toolIn("a-ns", "t", tenancy.LabelScope, tenancy.ScopePlatform), nil, ""},
		{"update that drops the label gets it back", admissionv1.Update, toolIn("shared-ns", "t"), toolIn("shared-ns", "t", tenancy.LabelTenantID, tenB), tenB},
		{"update on a legacy object is stamped from the namespace", admissionv1.Update, toolIn("a-ns", "t"), toolIn("a-ns", "t"), tenA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := l.Handle(context.Background(), request(t, tt.op, "Tool", tt.obj.GetNamespace(), tt.obj, tt.old, authenticationv1.UserInfo{}))
			if !resp.Allowed {
				t.Fatalf("denied: %v", resp.Result)
			}
			got := ""
			for _, p := range resp.Patches {
				if m, ok := p.Value.(map[string]any); ok {
					if v, ok := m[tenancy.LabelTenantID].(string); ok {
						got = v
					}
				}
				if p.Path == "/metadata/labels/agentorca.io~1tenant-id" {
					got, _ = p.Value.(string)
				}
			}
			if got != tt.wantTID {
				t.Errorf("stamped %q, want %q (patches %+v)", got, tt.wantTID, resp.Patches)
			}
		})
	}
}

func TestTenantIsolationValidator(t *testing.T) {
	scheme := isolationScheme(t)
	existing := []client.Object{
		tenantCfg("a", tenA, "a-ns", "shared-ns"),
		tenantCfg("b", tenB, "b-ns", "shared-ns"),
		tenantCfg("fresh", "", "fresh-ns"),
		toolIn("shared-ns", "tool-b", tenancy.LabelTenantID, tenB),
		toolIn("shared-ns", "tool-a", tenancy.LabelTenantID, tenA),
		toolIn("shared-ns", "tool-platform", tenancy.LabelScope, tenancy.ScopePlatform),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing...).Build()
	v := &TenantIsolationValidator{
		Decoder: admission.NewDecoder(scheme), TenantConfigs: reader, Reader: reader,
		AdminGroups: []string{adminGrp}, OperatorNamespace: "agent-orca-system",
	}
	admin := authenticationv1.UserInfo{Username: "root", Groups: []string{adminGrp}}
	operator := authenticationv1.UserInfo{Username: "system:serviceaccount:agent-orca-system:agent-orca"}
	plain := authenticationv1.UserInfo{Username: "dev", Groups: []string{"devs"}}

	agentIn := func(ns string, tools []string, kv ...string) *agentorcav1alpha1.Agent {
		a := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns}, Spec: agentorcav1alpha1.AgentSpec{Tools: tools}}
		if len(kv) > 0 {
			a.Labels = map[string]string{}
			for i := 0; i < len(kv); i += 2 {
				a.Labels[kv[i]] = kv[i+1]
			}
		}
		return a
	}

	tests := []struct {
		name     string
		op       admissionv1.Operation
		kind     string
		obj, old client.Object
		user     authenticationv1.UserInfo
		// wantRule is the violation expected; every rule is checked in enforce
		// mode (denied) and audit mode (allowed with a warning).
		wantRule string
	}{
		{name: "labelled for the namespace's tenant", op: admissionv1.Create, kind: "Agent", obj: agentIn("a-ns", nil, tenancy.LabelTenantID, tenA), user: plain},
		{name: "label held by a claimant of a shared namespace", op: admissionv1.Create, kind: "Agent", obj: agentIn("shared-ns", nil, tenancy.LabelTenantID, tenB), user: plain},
		{name: "unlabelled in a shared namespace", op: admissionv1.Create, kind: "Agent", obj: agentIn("shared-ns", nil), user: plain, wantRule: tenancy.RuleLabelMissing},
		{name: "unlabelled in an unclaimed namespace", op: admissionv1.Create, kind: "Agent", obj: agentIn("nobody", nil), user: plain, wantRule: tenancy.RuleLabelMissing},
		{name: "unlabelled where the claimant has no ID yet", op: admissionv1.Create, kind: "Agent", obj: agentIn("fresh-ns", nil), user: plain, wantRule: tenancy.RuleLabelMissing},
		{name: "label of a tenant that does not claim the namespace", op: admissionv1.Create, kind: "Agent", obj: agentIn("a-ns", nil, tenancy.LabelTenantID, tenB), user: plain, wantRule: tenancy.RuleLabelNamespace},
		{name: "label may not change", op: admissionv1.Update, kind: "Agent",
			obj: agentIn("shared-ns", nil, tenancy.LabelTenantID, tenB), old: agentIn("shared-ns", nil, tenancy.LabelTenantID, tenA), user: plain, wantRule: tenancy.RuleLabelImmutable},
		{name: "label may not be removed", op: admissionv1.Update, kind: "Agent",
			obj: agentIn("shared-ns", nil, tenancy.LabelScope, tenancy.ScopePlatform), old: agentIn("shared-ns", nil, tenancy.LabelTenantID, tenA), user: admin, wantRule: tenancy.RuleLabelImmutable},
		{name: "same label on update", op: admissionv1.Update, kind: "Agent",
			obj: agentIn("shared-ns", nil, tenancy.LabelTenantID, tenA), old: agentIn("shared-ns", nil, tenancy.LabelTenantID, tenA), user: plain},
		{name: "platform scope by a tenant user", op: admissionv1.Create, kind: "Agent", obj: agentIn("a-ns", nil, tenancy.LabelScope, tenancy.ScopePlatform), user: plain, wantRule: tenancy.RuleScopeNotAdmin},
		{name: "platform scope by an admin", op: admissionv1.Create, kind: "Agent", obj: agentIn("a-ns", nil, tenancy.LabelScope, tenancy.ScopePlatform), user: admin},
		{name: "platform scope passed down by the operator", op: admissionv1.Create, kind: "Agent", obj: agentIn("a-ns", nil, tenancy.LabelScope, tenancy.ScopePlatform), user: operator},
		{name: "tenant ID and platform scope together", op: admissionv1.Create, kind: "Agent", obj: agentIn("a-ns", nil, tenancy.LabelScope, tenancy.ScopePlatform, tenancy.LabelTenantID, tenA), user: admin, wantRule: tenancy.RuleLabelNamespace},
		{name: "reference to the tenant's own tool", op: admissionv1.Create, kind: "Agent", obj: agentIn("shared-ns", []string{"tool-a"}, tenancy.LabelTenantID, tenA), user: plain},
		{name: "reference to a platform tool", op: admissionv1.Create, kind: "Agent", obj: agentIn("shared-ns", []string{"tool-platform"}, tenancy.LabelTenantID, tenA), user: plain},
		{name: "reference to another tenant's tool", op: admissionv1.Create, kind: "Agent", obj: agentIn("shared-ns", []string{"tool-b"}, tenancy.LabelTenantID, tenA), user: plain, wantRule: tenancy.RuleCrossTenantRef},
		{name: "deployment of another tenant's agent", op: admissionv1.Create, kind: "AgentDeployment",
			obj: &agentorcav1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "shared-ns", Labels: map[string]string{tenancy.LabelTenantID: tenA}},
				Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "agent-b"},
			}, user: plain},
		{name: "not a tenant-owned kind", op: admissionv1.Create, kind: "TenantConfig", obj: tenantCfg("x", "", "x-ns"), user: plain},
	}
	for _, tt := range tests {
		for _, mode := range []tenancy.Mode{tenancy.ModeEnforce, tenancy.ModeAudit} {
			t.Run(tt.name+"/"+string(mode), func(t *testing.T) {
				withMode(t, mode)
				resp := v.Handle(context.Background(), request(t, tt.op, tt.kind, tt.obj.GetNamespace(), tt.obj, tt.old, tt.user))
				switch {
				case tt.wantRule == "":
					if !resp.Allowed || len(resp.Warnings) > 0 {
						t.Errorf("want a clean allow, got allowed=%v warnings=%v result=%v", resp.Allowed, resp.Warnings, resp.Result)
					}
				case mode == tenancy.ModeEnforce:
					if resp.Allowed {
						t.Errorf("rule %s: allowed in enforce mode", tt.wantRule)
					}
				default:
					if !resp.Allowed || len(resp.Warnings) == 0 || !strings.Contains(resp.Warnings[0], "audit mode") {
						t.Errorf("rule %s: audit mode should allow with a warning, got allowed=%v warnings=%v", tt.wantRule, resp.Allowed, resp.Warnings)
					}
				}
			})
		}
	}
}
