package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

// envtestAssets returns the directory with etcd and kube-apiserver, or skips.
func envtestAssets(t *testing.T) string {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return "" // envtest reads it itself
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				return filepath.Join(base, e.Name())
			}
		}
	}
	t.Skip("no envtest binaries: run make setup-envtest or set KUBEBUILDER_ASSETS")
	return ""
}

func isolationWebhookConfigs() ([]*admissionregistrationv1.MutatingWebhookConfiguration, []*admissionregistrationv1.ValidatingWebhookConfiguration) {
	rules := []admissionregistrationv1.RuleWithOperations{{
		Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
		Rule: admissionregistrationv1.Rule{
			APIGroups: []string{"agentorca.agentorca.io"}, APIVersions: []string{"v1alpha1"},
			Resources: []string{"agents", "tools", "mcpservers", "knowledgebases", "modelselectors", "modelproviders", "agentdeployments", "agentworkflows", "agentruns"},
		},
	}}
	none := admissionregistrationv1.SideEffectClassNone
	fail := admissionregistrationv1.Fail
	svc := func(path string) admissionregistrationv1.WebhookClientConfig {
		return admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Path: new(path)}}
	}
	mut := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-label"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name: "mtenantlabel.kb.io", AdmissionReviewVersions: []string{"v1"}, SideEffects: &none, FailurePolicy: &fail,
			ClientConfig: svc("/mutate-agentorca-agentorca-io-v1alpha1-tenantlabel"), Rules: rules,
		}},
	}
	val := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-isolation"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "vtenantisolation.kb.io", AdmissionReviewVersions: []string{"v1"}, SideEffects: &none, FailurePolicy: &fail,
			ClientConfig: svc("/validate-agentorca-agentorca-io-v1alpha1-tenantisolation"), Rules: rules,
		}},
	}
	return []*admissionregistrationv1.MutatingWebhookConfiguration{mut}, []*admissionregistrationv1.ValidatingWebhookConfiguration{val}
}

// Two tenants, one shared namespace, a real API server and the real webhooks:
// a cross-tenant reference is rejected, and a cross-tenant get is a 404.
func TestTenantIsolationEnvtest(t *testing.T) {
	assets := envtestAssets(t)
	mutating, validating := isolationWebhookConfigs()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
		WebhookInstallOptions: envtest.WebhookInstallOptions{MutatingWebhooks: mutating, ValidatingWebhooks: validating},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	_ = admissionv1.AddToScheme(scheme)

	wo := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: wo.LocalServingHost, Port: wo.LocalServingPort, CertDir: wo.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agentwebhook.SetupTenantIsolationWebhooks(mgr, nil, "agent-orca-system"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	prev := tenancy.CurrentMode()
	tenancy.SetMode(tenancy.ModeEnforce)
	t.Cleanup(func() { tenancy.SetMode(prev) })

	const idA, idB = "tnt_01935c7e-aaaa-7aaa-8bbb-00000000000a", "tnt_01935c7e-bbbb-7aaa-8bbb-00000000000b"
	for _, ns := range []string{"agent-orca-system", "a-only", "b-only"} {
		must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shared", Labels: map[string]string{tenancy.NamespaceModeLabel: tenancy.NamespaceModeShared},
	}}))
	tenantConfig := func(name, id string, ns ...string) *agentorcav1alpha1.TenantConfig {
		return &agentorcav1alpha1.TenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-orca-system"},
			Spec: agentorcav1alpha1.TenantConfigSpec{
				TenantID: id, AuthMode: "issued", AllowedNamespaces: ns,
				Issued: &agentorcav1alpha1.IssuedAuthConfig{ClientID: name, ClientSecretRef: agentorcav1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
			},
		}
	}
	must(t, c.Create(ctx, tenantConfig("acme", idA, "a-only", "shared")))
	must(t, c.Create(ctx, tenantConfig("globex", idB, "b-only", "shared")))

	tool := func(ns, name string, kv ...string) *agentorcav1alpha1.Tool {
		o := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if len(kv) > 0 {
			o.Labels = map[string]string{}
			for i := 0; i < len(kv); i += 2 {
				o.Labels[kv[i]] = kv[i+1]
			}
		}
		return o
	}
	agent := func(ns, name, usesTool string, kv ...string) *agentorcav1alpha1.Agent {
		a := &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: agentorcav1alpha1.AgentSpec{
				ModelSelectorRef: "sel", Tools: []string{usesTool},
				Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "registry.example/agent:1"},
			},
		}
		if len(kv) > 0 {
			a.Labels = map[string]string{}
			for i := 0; i < len(kv); i += 2 {
				a.Labels[kv[i]] = kv[i+1]
			}
		}
		return a
	}
	// retry covers the window before the webhook server accepts connections.
	create := func(obj client.Object) error {
		var err error
		for range 50 {
			if err = c.Create(ctx, obj); err == nil || strings.Contains(err.Error(), "denied the request") {
				return err
			}
			time.Sleep(200 * time.Millisecond)
		}
		return err
	}

	// A namespace with one owner labels its objects.
	soloTool := tool("a-only", "solo")
	must(t, create(soloTool))
	if got := soloTool.Labels[tenancy.LabelTenantID]; got != idA {
		t.Errorf("object in a single-owner namespace got tenant-id %q, want %q", got, idA)
	}
	// A shared namespace needs an explicit label.
	if err := create(tool("shared", "unlabelled")); err == nil || !strings.Contains(err.Error(), "shared by several tenants") {
		t.Errorf("unlabelled object in a shared namespace: err = %v", err)
	}
	must(t, create(tool("shared", "tool-a", tenancy.LabelTenantID, idA)))
	must(t, create(tool("shared", "tool-b", tenancy.LabelTenantID, idB)))
	// A label for a tenant that does not claim the namespace is refused.
	if err := create(tool("a-only", "spoof", tenancy.LabelTenantID, idB)); err == nil {
		t.Error("tenant B's label was accepted in tenant A's namespace")
	}

	// Cross-tenant reference: rejected. Same-tenant: fine.
	if err := create(agent("shared", "uses-b", "tool-b", tenancy.LabelTenantID, idA)); err == nil || !strings.Contains(err.Error(), "belongs to tenant "+idB) {
		t.Errorf("agent of A using B's tool: err = %v", err)
	}
	must(t, create(agent("shared", "uses-a", "tool-a", tenancy.LabelTenantID, idA)))

	// The label is immutable.
	var ta agentorcav1alpha1.Tool
	must(t, c.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "tool-a"}, &ta))
	ta.Labels[tenancy.LabelTenantID] = idB
	if err := c.Update(ctx, &ta); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("changing the tenant-id label: err = %v", err)
	}

	// Audit mode lets the same reference through.
	tenancy.SetMode(tenancy.ModeAudit)
	must(t, create(agent("shared", "uses-b-audit", "tool-b", tenancy.LabelTenantID, idA)))
	tenancy.SetMode(tenancy.ModeEnforce)

	// The API: tenant A cannot read tenant B's tool in the shared namespace.
	s := &UIServer{crdClient: c}
	identity := &TenantIdentity{Roles: testAdminRoles, TenantID: idA, TenantName: "acme", Namespace: "shared", Namespaces: []string{"a-only", "shared"}}
	get := func(name string) int {
		rec := httptest.NewRecorder()
		s.handleResource(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/resources/tools/shared/"+name, nil), identity))
		return rec.Code
	}
	if code := get("tool-b"); code != http.StatusNotFound {
		t.Errorf("GET tenant B's tool = %d, want 404", code)
	}
	if code := get("tool-a"); code != http.StatusOK {
		t.Errorf("GET own tool = %d, want 200", code)
	}
	var list agentorcav1alpha1.ToolList
	if err := listCRDsMultiNamespace(asTenant(httptest.NewRequest(http.MethodGet, "/", nil), identity).Context(), c, &list, ""); err != nil {
		t.Fatal(err)
	}
	for _, it := range list.Items {
		if it.Labels[tenancy.LabelTenantID] != idA {
			t.Errorf("list leaked %s/%s (tenant %s)", it.Namespace, it.Name, it.Labels[tenancy.LabelTenantID])
		}
	}
	if len(list.Items) != 2 { // solo and tool-a
		t.Errorf("list = %d tools, want 2", len(list.Items))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
