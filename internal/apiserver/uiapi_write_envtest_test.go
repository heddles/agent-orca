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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// The write path against a real API server: server-side apply with the
// agent-orca-ui field manager, field removal on edit, the API server's own
// resourceVersion conflict, and the admission webhooks still firing.
func TestUIWritePathEnvtest(t *testing.T) { //nolint:gocyclo // one scenario on one API server
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
	must(t, agentwebhook.SetupTenantIsolationWebhooks(mgr, nil, "agent-orca-system"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	must(t, err)
	isoMode(t, tenancy.ModeEnforce)

	const idA, idB = isoA, isoB
	for _, ns := range []string{"agent-orca-system", "a-only", "b-only"} {
		must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shared", Labels: map[string]string{tenancy.NamespaceModeLabel: tenancy.NamespaceModeShared},
	}}))
	for name, spec := range map[string]struct {
		id  string
		nss []string
	}{"acme": {idA, []string{"a-only", "shared"}}, "globex": {idB, []string{"b-only", "shared"}}} {
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
	me := &TenantIdentity{TenantID: idA, TenantName: "acme", Namespace: "a-only", Namespaces: []string{"a-only", "shared"},
		UserID: "ada", Roles: tenancy.RoleAdmin.Expand()}
	do := func(id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
		req := asTenant(httptest.NewRequest(method, path, strings.NewReader(body)), id)
		rec := httptest.NewRecorder()
		if strings.Count(strings.Trim(path, "/"), "/") >= 4 {
			s.handleResource(rec, req)
		} else {
			s.handleResourceCollection(rec, req)
		}
		return rec
	}
	// retry covers the window before the webhook server accepts connections.
	retryDo := func(id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
		var rec *httptest.ResponseRecorder
		for range 50 {
			if rec = do(id, method, path, body); rec.Code != http.StatusInternalServerError {
				return rec
			}
			time.Sleep(200 * time.Millisecond)
		}
		return rec
	}
	getTool := func(ns, name string) *agentorcav1alpha1.Tool {
		var tl agentorcav1alpha1.Tool
		must(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &tl))
		return &tl
	}

	// Create: owned by the UI, with the apply recorded under agent-orca-ui.
	rec := retryDo(me, http.MethodPost, "/api/resources/tools",
		`{"metadata":{"name":"calc","namespace":"ignored"},"spec":{"ociRef":"registry.example/calc:1","agentRef":"helper"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	tl := getTool("a-only", "calc")
	if tl.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI || tl.Labels[tenancy.LabelTenantID] != idA {
		t.Errorf("metadata = %v %v", tl.Annotations, tl.Labels)
	}
	var applied bool
	for _, mf := range tl.ManagedFields {
		if mf.Manager == tenancy.FieldManagerUI && mf.Operation == metav1.ManagedFieldsOperationApply {
			applied = true
		}
	}
	if !applied {
		t.Errorf("no Apply entry for %s in managedFields: %+v", tenancy.FieldManagerUI, tl.ManagedFields)
	}

	for _, mf := range tl.ManagedFields {
		if mf.Operation == metav1.ManagedFieldsOperationUpdate && mf.Manager == tenancy.FieldManagerUI {
			t.Errorf("the create left an Update entry that would keep fields alive: %+v", mf)
		}
	}
	// Creating the same name again is an AlreadyExists from the server: a 409,
	// never a takeover of the object.
	if rec = do(me, http.MethodPost, "/api/resources/tools", `{"metadata":{"name":"calc"},"spec":{"ociRef":"hijack"}}`); rec.Code != http.StatusConflict {
		t.Errorf("create over an existing object = %d %s", rec.Code, rec.Body.String())
	}
	if getTool("a-only", "calc").Spec.OCIRef == "hijack" {
		t.Error("the second create took the object over")
	}
	// Edit: a field the editor drops is removed, because the create was an apply too.
	rv := tl.ResourceVersion
	rec = do(me, http.MethodPut, "/api/resources/tools/a-only/calc",
		`{"metadata":{"resourceVersion":"`+rv+`"},"spec":{"ociRef":"registry.example/calc:2"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	tl = getTool("a-only", "calc")
	if tl.Spec.OCIRef != "registry.example/calc:2" || tl.Spec.AgentRef != "" {
		t.Errorf("spec after the edit = %+v; agentRef should be gone", tl.Spec)
	}
	if strings.Contains(rec.Body.String(), "managedFields") {
		t.Error("managedFields leaked into the response")
	}

	// The same stale version again: the API's own check agrees with ours.
	rec = do(me, http.MethodPut, "/api/resources/tools/a-only/calc", `{"metadata":{"resourceVersion":"`+rv+`"},"spec":{"ociRef":"x"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tl.ResourceVersion) {
		t.Errorf("stale update = %d %s", rec.Code, rec.Body.String())
	}
	stale := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": agentorcav1alpha1.GroupVersion.String(), "kind": "Tool",
		"metadata": map[string]any{"name": "calc", "namespace": "a-only", "resourceVersion": rv},
		"spec":     map[string]any{"ociRef": "x"},
	}}
	if err := s.applyResource(httptest.NewRequest(http.MethodPut, "/", nil).WithContext(ctx), stale); !k8serrors.IsConflict(err) {
		t.Errorf("the API server accepted a stale resourceVersion: %v", err)
	}

	// A controller's field is not taken over by an edit that does not mention it.
	base := getTool("a-only", "calc")
	patch := base.DeepCopy()
	patch.Labels = map[string]string{"set-by": "someone-else"}
	must(t, c.Patch(ctx, patch, client.MergeFrom(base), client.FieldOwner("some-controller")))
	tl = getTool("a-only", "calc")
	rec = do(me, http.MethodPut, "/api/resources/tools/a-only/calc",
		`{"metadata":{"resourceVersion":"`+tl.ResourceVersion+`"},"spec":{"ociRef":"registry.example/calc:3"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update after a foreign edit = %d %s", rec.Code, rec.Body.String())
	}
	if got := getTool("a-only", "calc").Labels["set-by"]; got != "someone-else" {
		t.Errorf("the UI removed another manager's label: %q", got)
	}

	// Webhooks fire on apply: a cross-tenant reference is denied, as a 4xx.
	must(t, c.Create(ctx, &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Name: "tool-b", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: idB},
	}}))
	shared := *me
	shared.Namespace = "shared"
	rec = do(&shared, http.MethodPost, "/api/resources/agents",
		`{"metadata":{"name":"thief"},"spec":{"modelSelectorRef":"sel","tools":["tool-b"],"runtime":{"ociRef":"registry.example/a:1"}}}`)
	if rec.Code < 400 || rec.Code >= 500 || !strings.Contains(rec.Body.String(), "belongs to tenant "+idB) {
		t.Errorf("cross-tenant reference = %d %s, want a 4xx naming the other tenant", rec.Code, rec.Body.String())
	}

	// Delete: only what the UI made.
	foreign := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "by-hand", Namespace: "a-only"}}
	must(t, c.Create(ctx, foreign))
	if rec = do(me, http.MethodDelete, "/api/resources/tools/a-only/by-hand", ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete of an object made by hand = %d %s", rec.Code, rec.Body.String())
	}
	if rec = do(me, http.MethodDelete, "/api/resources/tools/a-only/calc", ""); rec.Code != http.StatusOK {
		t.Errorf("delete of a UI object = %d %s", rec.Code, rec.Body.String())
	}
	if len(audit.events) != 4 { // create, two updates, delete; refused writes leave no event
		for _, e := range audit.events {
			t.Logf("audit: %+v", e)
		}
		t.Errorf("audit events = %d, want 4", len(audit.events))
	}
}
