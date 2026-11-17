package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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

// Dry run and "use template" against a real API server with the admission
// webhooks installed: a webhook denial surfaces in errors and stores nothing,
// and a template copy has the right labels and references.
func TestBuilderEndpointsEnvtest(t *testing.T) { //nolint:gocyclo // one scenario on one API server
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
	must(t, agentorcav1alpha1.AddToScheme(scheme))
	must(t, corev1.AddToScheme(scheme))
	wo := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: wo.LocalServingHost, Port: wo.LocalServingPort, CertDir: wo.LocalServingCertDir,
		}),
	})
	must(t, err)
	must(t, agentwebhook.SetupTenantIsolationWebhooks(mgr, []string{"system:masters"}, "agent-orca-system"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	must(t, err)
	isoMode(t, tenancy.ModeEnforce)
	tenancy.SetPlatformNamespaces([]string{"agent-orca-system"})
	t.Cleanup(func() { tenancy.SetPlatformNamespaces(nil) })

	for _, ns := range []string{"agent-orca-system", "a-only", "b-only"} {
		must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shared", Labels: map[string]string{tenancy.NamespaceModeLabel: tenancy.NamespaceModeShared},
	}}))
	for name, spec := range map[string]struct {
		id  string
		nss []string
	}{"acme": {isoA, []string{"a-only", "shared"}}, "globex": {isoB, []string{"b-only", "shared"}}} {
		must(t, c.Create(ctx, &agentorcav1alpha1.TenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-orca-system"},
			Spec: agentorcav1alpha1.TenantConfigSpec{
				TenantID: spec.id, AuthMode: "issued", AllowedNamespaces: spec.nss,
				Issued: &agentorcav1alpha1.IssuedAuthConfig{ClientID: name, ClientSecretRef: agentorcav1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
			},
		}))
	}
	audit := &fakeAudit{}
	s := &UIServer{crdClient: c, audit: audit}
	mux := http.NewServeMux()
	s.registerBuilderRoutes(mux)
	me := &TenantIdentity{TenantID: isoA, TenantName: "acme", Namespace: "shared", Namespaces: []string{"shared", "a-only"},
		UserID: builderUser, Roles: tenancy.RoleAdmin.Expand()}
	send := func(method, path, body string) *httptest.ResponseRecorder {
		var rec *httptest.ResponseRecorder
		// The retry covers the window before the webhook server accepts connections.
		for range 50 {
			req := asTenant(httptest.NewRequest(method, path, strings.NewReader(body)), me)
			rec = httptest.NewRecorder()
			if strings.HasPrefix(path, "/api/resources") {
				if strings.Count(strings.Trim(strings.SplitN(path, "?", 2)[0], "/"), "/") >= 4 {
					s.handleResource(rec, req)
				} else {
					s.handleResourceCollection(rec, req)
				}
			} else {
				mux.ServeHTTP(rec, req)
			}
			if rec.Code != http.StatusInternalServerError {
				return rec
			}
			time.Sleep(200 * time.Millisecond)
		}
		return rec
	}

	// Dry run: a real webhook denial is reported in errors, and nothing is stored.
	must(t, c.Create(ctx, &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Name: "tool-b", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoB},
	}}))
	const agentBody = `{"metadata":{"name":"drafty"},"spec":{"modelSelectorRef":"sel","tools":["TOOL"],"runtime":{"ociRef":"registry.example/a:1"}}}`
	rec := send(http.MethodPost, "/api/resources/agents?dryRun=true", strings.Replace(agentBody, "TOOL", "tool-b", 1))
	if rec.Code < 400 || rec.Code >= 500 || !strings.Contains(rec.Body.String(), `"valid":false`) ||
		!strings.Contains(rec.Body.String(), "belongs to tenant "+isoB) || strings.Contains(rec.Body.String(), "denied the request") {
		t.Errorf("dry run of a cross-tenant reference = %d %s", rec.Code, rec.Body.String())
	}
	rec = send(http.MethodPost, "/api/resources/agents?dryRun=true", strings.Replace(agentBody, "TOOL", "pub-agent", 1))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":true`) || !strings.Contains(rec.Body.String(), isoA) {
		t.Errorf("dry run of a valid agent = %d %s", rec.Code, rec.Body.String())
	}
	var drafty agentorcav1alpha1.Agent
	if err := c.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "drafty"}, &drafty); !k8serrors.IsNotFound(err) {
		t.Errorf("a dry run stored the agent: %v", err)
	}
	if len(audit.events) != 0 {
		t.Errorf("a dry run wrote audit rows: %+v", audit.events)
	}
	// A dry run of a name that exists is the same 409 a real create gives.
	must(t, c.Create(ctx, &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "drafty", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA}},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "sel", Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "x"}},
	}))
	if rec = send(http.MethodPost, "/api/resources/agents?dryRun=true", strings.Replace(agentBody, "TOOL", "pub-agent", 1)); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"valid":false`) {
		t.Errorf("dry run over an existing name = %d %s", rec.Code, rec.Body.String())
	}

	// Use a template: the platform's objects are created as the operator would.
	grp := map[string]string{tenancy.LabelScope: tenancy.ScopePlatform, LabelTemplateGroup: "tg"}
	tplLabels := map[string]string{tenancy.LabelScope: tenancy.ScopePlatform, LabelTemplateGroup: "tg", LabelTemplate: "true"}
	must(t, c.Create(ctx, &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "tg-tool", Namespace: "agent-orca-system", Labels: grp}}))
	must(t, c.Create(ctx, &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "tg", Namespace: "agent-orca-system", Labels: tplLabels, Annotations: map[string]string{AnnotationMaintainer: "Platform"}},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: builderSelector, Tools: []string{"tg-tool"}, Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "registry.example/a:1"}},
	}))
	must(t, c.Create(ctx, &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "tg-chat", Namespace: "agent-orca-system", Labels: grp},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "tg"},
	}))
	if rec = send(http.MethodPost, "/api/templates/agent-orca-system/tg/use", `{"name":"copy"}`); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), `ModelSelector \"default\" does not exist in namespace shared`) {
		t.Fatalf("use without a selector = %d %s", rec.Code, rec.Body.String())
	}
	var none agentorcav1alpha1.Tool
	if err := c.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "copy-tool"}, &none); !k8serrors.IsNotFound(err) || len(audit.events) != 0 {
		t.Errorf("a refused use created something: %v %+v", err, audit.events)
	}
	must(t, c.Create(ctx, &agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA}},
		Spec: agentorcav1alpha1.ModelSelectorSpec{Providers: []agentorcav1alpha1.ProviderWeight{{Name: "p"}}}}))
	// b-only is not acme's namespace, so the copy lands in its first one.
	rec = send(http.MethodPost, "/api/templates/agent-orca-system/tg/use", `{"name":"copy","namespace":"b-only"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("use template = %d %s", rec.Code, rec.Body.String())
	}
	var agent agentorcav1alpha1.Agent
	must(t, c.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "copy"}, &agent))
	var dep agentorcav1alpha1.AgentDeployment
	must(t, c.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "copy-deploy"}, &dep))
	var tool agentorcav1alpha1.Tool
	must(t, c.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "copy-tool"}, &tool))
	if !slices.Equal(agent.Spec.Tools, []string{"copy-tool"}) || dep.Spec.AgentRef != "copy" || agent.Spec.ModelSelectorRef != builderSelector {
		t.Errorf("references: agent %+v, deployment %+v", agent.Spec, dep.Spec)
	}
	for _, o := range []client.Object{&agent, &dep, &tool} {
		l, a := o.GetLabels(), o.GetAnnotations()
		if l[tenancy.LabelTenantID] != isoA || l[tenancy.LabelScope] != "" || l[LabelTemplate] != "" || l[LabelTemplateGroup] != "" ||
			a[tenancy.AnnotationOwner] != tenancy.OwnerUI || a[tenancy.AnnotationLastModifiedBy] != builderUser {
			t.Errorf("%T labels %v annotations %v", o, l, a)
		}
	}
	if agent.Annotations[AnnotationMaintainer] != "Platform" {
		t.Errorf("maintainer = %v", agent.Annotations)
	}
	if len(audit.events) != 3 {
		t.Errorf("audit = %+v", audit.events)
	}
	// A second use with a taken name leaves nothing behind.
	if rec = send(http.MethodPost, "/api/templates/agent-orca-system/tg/use", `{"name":"copy"}`); rec.Code != http.StatusConflict {
		t.Errorf("use over an existing name = %d %s", rec.Code, rec.Body.String())
	}
	if len(audit.events) != 3 {
		t.Errorf("a refused use wrote audit rows: %+v", audit.events)
	}
}
