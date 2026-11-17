package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

// Restore against a real API server with the tenant-isolation webhooks: the
// restored object goes through server-side apply and admission, keeps its
// tenant, a stale resourceVersion is a 409, and a deleted object comes back
// through the atomic create path.
func TestRestoreRevisionEnvtest(t *testing.T) { //nolint:gocyclo // one scenario on one API server
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

	for _, ns := range []string{"agent-orca-system", "a-only"} {
		must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	must(t, c.Create(ctx, &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "agent-orca-system"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			TenantID: isoA, AuthMode: "issued", AllowedNamespaces: []string{"a-only"},
			Issued: &agentorcav1alpha1.IssuedAuthConfig{ClientID: "acme", ClientSecretRef: agentorcav1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
		},
	}))

	revs := &fakeRevisions{}
	s := &UIServer{crdClient: c, audit: &fakeAudit{}, revisions: revs}
	me := &TenantIdentity{TenantID: isoA, TenantName: "acme", Namespace: "a-only", Namespaces: []string{"a-only"},
		UserID: builderUser, Roles: tenancy.RoleBuilder.Expand()}
	send := func(method, path, body string) *httptest.ResponseRecorder {
		var rec *httptest.ResponseRecorder
		// The retry covers the window before the webhook server accepts connections.
		for range 50 {
			req := asTenant(httptest.NewRequest(method, path, strings.NewReader(body)), me)
			rec = httptest.NewRecorder()
			if strings.Count(strings.Trim(strings.SplitN(path, "?", 2)[0], "/"), "/") >= 4 {
				s.handleResource(rec, req)
			} else {
				s.handleResourceCollection(rec, req)
			}
			if rec.Code != http.StatusInternalServerError {
				return rec
			}
			time.Sleep(200 * time.Millisecond)
		}
		return rec
	}
	agent := func() *agentorcav1alpha1.Agent {
		var a agentorcav1alpha1.Agent
		must(t, c.Get(ctx, client.ObjectKey{Namespace: "a-only", Name: "bot"}, &a))
		return &a
	}
	const base = "/api/resources/agents/a-only/bot"
	body := func(image, rv string) string {
		r := ""
		if rv != "" {
			r = `"resourceVersion":"` + rv + `",`
		}
		return `{"metadata":{` + r + `"name":"bot","namespace":"a-only"},"spec":{"modelSelectorRef":"sel","runtime":{"ociRef":"` + image + `"}}}`
	}

	if rec := send(http.MethodPost, "/api/resources/agents", body("registry.example/a:1", "")); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(http.MethodPut, base, body("registry.example/a:2", agent().ResourceVersion)); rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(revs.actions(), ","); got != "create,update" {
		t.Fatalf("revisions = %s", got)
	}

	// A stale version is refused and changes nothing.
	if rec := send(http.MethodPost, base+"/revisions/1/restore", `{"resourceVersion":"1"}`); rec.Code != http.StatusConflict {
		t.Errorf("stale restore = %d %s", rec.Code, rec.Body.String())
	}
	if agent().Spec.Runtime.OCIRef != "registry.example/a:2" {
		t.Error("a refused restore changed the agent")
	}

	// Restore revision 1 over the live agent, through the apply and the webhooks.
	rec := send(http.MethodPost, base+"/revisions/1/restore", `{"resourceVersion":"`+agent().ResourceVersion+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	got := agent()
	if got.Spec.Runtime.OCIRef != "registry.example/a:1" || got.Labels[tenancy.LabelTenantID] != isoA ||
		got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI {
		t.Errorf("agent after restore = %+v %+v", got.Spec.Runtime, got.ObjectMeta)
	}
	last := revs.rows[len(revs.rows)-1]
	if last.Action != postgresql.RevisionRestore || last.RestoredFrom == nil || *last.RestoredFrom != 1 {
		t.Errorf("restore revision = %+v", last)
	}

	// Delete, then restore recreates it.
	if rec := send(http.MethodDelete, base, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	rec = send(http.MethodPost, base+"/revisions/2/restore", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore of a deleted agent = %d %s", rec.Code, rec.Body.String())
	}
	got = agent()
	if got.Spec.Runtime.OCIRef != "registry.example/a:2" || got.Labels[tenancy.LabelTenantID] != isoA {
		t.Errorf("recreated agent = %+v %+v", got.Spec.Runtime, got.ObjectMeta)
	}
}
