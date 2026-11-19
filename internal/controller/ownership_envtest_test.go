package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

func ownershipEnvtestAssets(t *testing.T) string {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				return filepath.Join(base, e.Name())
			}
		}
	}
	t.Skip("no envtest binaries: run make setup-envtest or set KUBEBUILDER_ASSETS")
	return ""
}

// warnings collects the admission warnings a client receives.
type warnings struct {
	mu   sync.Mutex
	list []string
}

func (w *warnings) HandleWarningHeader(_ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, text)
}

func (w *warnings) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.list
	w.list = nil
	return out
}

// The real ownership webhook and handoff controller against a real API server,
// with writers impersonated as the Argo CD and Flux service accounts and using
// their field managers.
func TestOwnershipEnvtest(t *testing.T) { //nolint:gocyclo // one scenario on one API server
	none, fail := admissionregistrationv1.SideEffectClassNone, admissionregistrationv1.Fail
	path := agentwebhook.OwnershipPath
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "ownership"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "vownership.kb.io", AdmissionReviewVersions: []string{"v1"}, SideEffects: &none, FailurePolicy: &fail,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Path: &path}},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups: []string{"agentorca.agentorca.io"}, APIVersions: []string{"v1alpha1"},
					Resources: []string{"agents", "tools", "mcpservers", "knowledgebases", "modelselectors", "modelproviders", "agentdeployments", "agentworkflows", "agentschedules"},
				},
			}},
		}},
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: ownershipEnvtestAssets(t),
		WebhookInstallOptions: envtest.WebhookInstallOptions{ValidatingWebhooks: []*admissionregistrationv1.ValidatingWebhookConfiguration{validating}},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	must(t, agentorcav1alpha1.AddToScheme(scheme))
	must(t, corev1.AddToScheme(scheme))

	// as returns a client for the user (a member of system:masters, so the API
	// server authorises it) with the warnings it receives.
	as := func(user string) (client.Client, *warnings) {
		w := &warnings{}
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:masters"}}
		c.WarningHandler = w
		cl, err := client.New(c, client.Options{Scheme: scheme})
		must(t, err)
		return cl, w
	}
	const operatorUser = "system:serviceaccount:agent-orca-system:agent-orca"

	// The manager, and so the handoff controller, run as the operator's service account.
	mgrCfg := rest.CopyConfig(cfg)
	mgrCfg.Impersonate = rest.ImpersonationConfig{UserName: operatorUser, Groups: []string{"system:masters"}}
	wo := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(mgrCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: wo.LocalServingHost, Port: wo.LocalServingPort, CertDir: wo.LocalServingCertDir,
		}),
	})
	must(t, err)
	must(t, agentwebhook.SetupOwnershipWebhook(mgr, agentwebhook.OwnershipConfig{
		Writers: tenancy.DefaultGitOpsWriters(), OperatorNamespace: "agent-orca-system", OperatorServiceAccount: "agent-orca",
	}))
	must(t, SetupHandoffControllers(mgr, tenancy.DetectOptions{}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	must(t, err)
	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}))

	argo, _ := as("system:serviceaccount:argocd:argocd-application-controller")
	flux, _ := as("system:serviceaccount:flux-system:kustomize-controller")
	ui, _ := as(operatorUser)
	human, humanWarnings := as("alice")
	const (
		argoFM  = "argocd-controller"
		fluxFM  = "kustomize-controller"
		humanFM = "kubectl-edit"
	)
	trackingID := "shop:agentorca.agentorca.io/Agent:team-a/%s"

	newAgent := func(name string, labels, annotations map[string]string) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels, Annotations: annotations},
			Spec: agentorcav1alpha1.AgentSpec{
				ModelSelectorRef: "sel", SystemPrompt: "v1",
				Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "registry.example/a:1"},
			},
		}
	}
	get := func(name string) *agentorcav1alpha1.Agent {
		t.Helper()
		var a agentorcav1alpha1.Agent
		must(t, admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, &a))
		return &a
	}
	// edit reads, changes and updates as c with field manager fm.
	edit := func(c client.Client, fm, name string, change func(*agentorcav1alpha1.Agent)) error {
		t.Helper()
		var a agentorcav1alpha1.Agent
		must(t, admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, &a))
		change(&a)
		return c.Update(ctx, &a, client.FieldOwner(fm))
	}
	setPrompt := func(p string) func(*agentorcav1alpha1.Agent) {
		return func(a *agentorcav1alpha1.Agent) { a.Spec.SystemPrompt = p }
	}
	// retry covers the window before the webhook server accepts connections.
	create := func(c client.Client, fm string, a *agentorcav1alpha1.Agent) error {
		var err error
		for range 50 {
			if err = c.Create(ctx, a, client.FieldOwner(fm)); err == nil || k8serrors.IsAlreadyExists(err) || k8serrors.IsForbidden(err) {
				return err
			}
			time.Sleep(200 * time.Millisecond)
		}
		return err
	}
	denied := func(err error, want ...string) {
		t.Helper()
		if err == nil || !k8serrors.IsForbidden(err) {
			t.Fatalf("err = %v, want a denial", err)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("denial %q does not contain %q", err, w)
			}
		}
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		for range 100 {
			if cond() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	hasEvent := func(name, reason string) bool {
		var evs corev1.EventList
		if err := admin.List(ctx, &evs, client.InNamespace("team-a")); err != nil {
			return false
		}
		for _, e := range evs.Items {
			if e.InvolvedObject.Name == name && e.Reason == reason {
				return true
			}
		}
		return false
	}
	uiOwned := map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}

	// A UI-created object, through the UI's own identity and field manager.
	if err := create(ui, tenancy.FieldManagerUI, newAgent("triage", nil, uiOwned)); err != nil {
		t.Fatalf("UI create: %v", err)
	}

	// Collision: Argo CD and Flux are denied, whether identified by service
	// account, by field manager alone, or both. The denial names the object, the
	// owner and the fix, and an Event lands on the object.
	t.Run("a GitOps collision is denied", func(t *testing.T) {
		denied(edit(argo, argoFM, "triage", setPrompt("from argo")),
			`Agent "triage"`, "owned by the agent-orca UI", "export it from the UI and mark it for handoff, or rename your manifest")
		denied(edit(flux, fluxFM, "triage", setPrompt("from flux")), "owned by the agent-orca UI")
		denied(edit(argo, "some-other-manager", "triage", setPrompt("sa only")), "owned by the agent-orca UI")
		denied(edit(human, argoFM, "triage", setPrompt("fm only")), "owned by the agent-orca UI")
		denied(edit(human, "helm-controller", "triage", setPrompt("helm fm")), "owned by the agent-orca UI")
		if got := get("triage").Spec.SystemPrompt; got != "v1" {
			t.Errorf("a denied write changed the object: %q", got)
		}
		eventually("the GitOpsWriteBlocked event", func() bool { return hasEvent("triage", agentwebhook.EventReasonGitOpsBlocked) })
		// Argo creating an object that claims UI ownership is denied too.
		denied(create(argo, argoFM, newAgent("squatter", nil, uiOwned)))
	})

	// An AgentSchedule is owned like the other kinds: a GitOps writer is denied
	// on a UI-owned one, and the UI is denied on a Git-owned one.
	t.Run("a GitOps writer is denied on a UI-owned schedule", func(t *testing.T) {
		newSchedule := func(name string, annotations map[string]string) *agentorcav1alpha1.AgentSchedule {
			return &agentorcav1alpha1.AgentSchedule{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Annotations: annotations},
				Spec: agentorcav1alpha1.AgentScheduleSpec{
					AgentRef: "triage", Schedule: "0 8 * * *", Input: "go", Budget: &agentorcav1alpha1.RunBudget{MaxUSD: "1"},
				},
			}
		}
		var err error
		for range 50 {
			if err = ui.Create(ctx, newSchedule("nightly", uiOwned), client.FieldOwner(tenancy.FieldManagerUI)); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		must(t, err)
		var sc agentorcav1alpha1.AgentSchedule
		must(t, admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "nightly"}, &sc))
		sc.Spec.Input = "from argo"
		denied(argo.Update(ctx, &sc, client.FieldOwner(argoFM)), `AgentSchedule "nightly"`, "owned by the agent-orca UI")
		denied(flux.Update(ctx, &sc, client.FieldOwner(fluxFM)), "owned by the agent-orca UI")
		denied(argo.Create(ctx, newSchedule("squatter", uiOwned), client.FieldOwner(argoFM)))

		// Argo's own schedule is Git-owned, and the UI may not touch it.
		git := map[string]string{"argocd.argoproj.io/tracking-id": "shop:agentorca.agentorca.io/AgentSchedule:team-a/synced"}
		must(t, argo.Create(ctx, newSchedule("synced", git), client.FieldOwner(argoFM)))
		must(t, admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "synced"}, &sc))
		sc.Spec.Suspend = true
		denied(ui.Update(ctx, &sc, client.FieldOwner(tenancy.FieldManagerUI)), "owned by Git")
	})

	// A GitOps tool nobody listed (renamed service account, custom field manager)
	// is still blocked, because what it applies carries tracking metadata.
	t.Run("an unlisted GitOps writer is denied by the tracking metadata it applies", func(t *testing.T) {
		stranger, _ := as("system:serviceaccount:gitops:renamed-controller")
		const strangerFM = "my-custom-gitops"
		must(t, create(ui, tenancy.FieldManagerUI, newAgent("unlisted", nil, uiOwned)))
		denied(edit(stranger, strangerFM, "unlisted", func(a *agentorcav1alpha1.Agent) {
			a.Annotations["argocd.argoproj.io/tracking-id"] = "shop:agentorca.agentorca.io/Agent:team-a/unlisted"
			a.Spec.SystemPrompt = "argo takeover"
		}), "owned by the agent-orca UI", "argocd shop")
		denied(edit(stranger, strangerFM, "unlisted", func(a *agentorcav1alpha1.Agent) {
			a.Labels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
			a.Spec.SystemPrompt = "flux takeover"
		}), "owned by the agent-orca UI", "flux-kustomize flux-system/apps")
		if got := get("unlisted"); got.Spec.SystemPrompt != "v1" || tenancy.Detect(got, tenancy.DetectOptions{}).Owner != tenancy.OwnerUI {
			t.Errorf("a denied write changed the object: %q %v", got.Spec.SystemPrompt, got.Annotations)
		}
		// With a handoff pending the same write is allowed, and ownership then flips.
		must(t, edit(ui, tenancy.FieldManagerUI, "unlisted", func(a *agentorcav1alpha1.Agent) { tenancy.SetHandoff(a, time.Now().Add(time.Hour)) }))
		must(t, edit(stranger, strangerFM, "unlisted", func(a *agentorcav1alpha1.Agent) {
			a.Annotations["argocd.argoproj.io/tracking-id"] = "shop:agentorca.agentorca.io/Agent:team-a/unlisted"
			a.Spec.SystemPrompt = "adopted"
		}))
		eventually("the flip", func() bool { return !tenancy.HandoffOf(get("unlisted")).Requested })
		if a := get("unlisted"); a.Spec.SystemPrompt != "adopted" || a.Annotations[tenancy.AnnotationOwner] != "" {
			t.Errorf("after adoption: %q %v", a.Spec.SystemPrompt, a.Annotations)
		}
	})

	t.Run("Git-owned objects are not affected", func(t *testing.T) {
		must(t, create(argo, argoFM, newAgent("synced", nil, map[string]string{"argocd.argoproj.io/tracking-id": "shop:agentorca.agentorca.io/Agent:team-a/synced"})))
		must(t, edit(argo, argoFM, "synced", setPrompt("v2")))
		must(t, create(flux, fluxFM, newAgent("fluxed", map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, nil)))
		must(t, edit(flux, fluxFM, "fluxed", setPrompt("v2")))
		// An object nobody owns is Git's to take: that is how existing GitOps users keep working.
		must(t, create(admin, "kubectl", newAgent("plain", nil, nil)))
		must(t, edit(argo, argoFM, "plain", setPrompt("v2")))
	})

	t.Run("a human edit of a UI-owned object warns", func(t *testing.T) {
		humanWarnings.take()
		must(t, edit(human, humanFM, "triage", setPrompt("hand fix")))
		w := humanWarnings.take()
		if len(w) != 1 || !strings.Contains(w[0], "owned by the agent-orca UI") {
			t.Errorf("warnings = %q", w)
		}
		if got := get("triage").Spec.SystemPrompt; got != "hand fix" {
			t.Errorf("prompt = %q", got)
		}
		// And a human edit of a Git-owned object does not.
		must(t, edit(human, humanFM, "synced", setPrompt("hand fix")))
		if w := humanWarnings.take(); len(w) != 0 {
			t.Errorf("warnings on a Git-owned object = %q", w)
		}
	})

	t.Run("the UI field manager is denied on a Git-owned object", func(t *testing.T) {
		denied(edit(ui, tenancy.FieldManagerUI, "synced", setPrompt("from ui")), "owned by Git", "argocd shop", "duplicate")
		denied(edit(ui, tenancy.FieldManagerUI, "fluxed", setPrompt("from ui")), "flux-kustomize flux-system/apps")
		denied(create(ui, tenancy.FieldManagerUI, newAgent("claims-git", nil, map[string]string{"argocd.argoproj.io/tracking-id": "shop:g/Agent:team-a/claims-git"})))
		eventually("the UIWriteBlocked event", func() bool { return hasEvent("synced", agentwebhook.EventReasonUIBlocked) })
		// The UI still edits its own.
		must(t, edit(ui, tenancy.FieldManagerUI, "triage", setPrompt("ui edit")))
	})

	t.Run("controllers are never blocked", func(t *testing.T) {
		// A finalizer and a label from the operator (the tenant label stamping and the
		// owner migration look like this), and a status write, on a UI-owned object.
		must(t, edit(ui, "agent-orca-controller", "triage", func(a *agentorcav1alpha1.Agent) {
			a.Labels = map[string]string{tenancy.LabelTenantID: "tnt_01935c7e-aaaa-7aaa-8bbb-00000000000a"}
			a.Finalizers = []string{"agentorca.io/finalizer"}
		}))
		a := get("triage")
		a.Status.ServiceAccountName = "triage-sa"
		must(t, ui.Status().Update(ctx, a))
		must(t, edit(ui, "agent-orca-controller", "triage", func(a *agentorcav1alpha1.Agent) { a.Finalizers = nil }))
		// The U1 owner migration: an unowned object gains the owner annotation.
		must(t, edit(ui, "agent-orca-migration", "plain", func(a *agentorcav1alpha1.Agent) {
			a.Annotations = map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}
		}))
		must(t, edit(ui, "agent-orca-migration", "plain", func(a *agentorcav1alpha1.Agent) { a.Annotations = nil }))
	})

	// Handoff to Argo CD: the UI marks the object, Argo adopts it, the controller flips it.
	t.Run("a handoff is adopted by Argo CD and then flipped", func(t *testing.T) {
		must(t, edit(ui, tenancy.FieldManagerUI, "triage", func(a *agentorcav1alpha1.Agent) {
			tenancy.SetHandoff(a, time.Now().Add(time.Hour))
		}))
		// The manifest as exported: owner git, plus what Argo adds.
		must(t, edit(argo, argoFM, "triage", func(a *agentorcav1alpha1.Agent) {
			a.Annotations[tenancy.AnnotationOwner] = tenancy.OwnerGit
			a.Annotations["argocd.argoproj.io/tracking-id"] = strings.Replace(trackingID, "%s", "triage", 1)
			a.Spec.SystemPrompt = "from git"
		}))
		eventually("the handoff to be flipped", func() bool {
			a := get("triage")
			return !tenancy.HandoffOf(a).Requested
		})
		a := get("triage")
		if a.Spec.SystemPrompt != "from git" || a.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerGit {
			t.Errorf("after adoption: prompt %q, annotations %v", a.Spec.SystemPrompt, a.Annotations)
		}
		if o := tenancy.Detect(a, tenancy.DetectOptions{}); o.Owner != tenancy.OwnerGit || o.Tool != tenancy.ToolArgoCD || o.App != "shop" {
			t.Errorf("ownership = %+v", o)
		}
		eventually("the HandoffCompleted event", func() bool { return hasEvent("triage", EventReasonHandoffCompleted) })
		// Git owns it now: Argo syncs it, the UI cannot write it.
		must(t, edit(argo, argoFM, "triage", setPrompt("synced again")))
		denied(edit(ui, tenancy.FieldManagerUI, "triage", setPrompt("from ui")), "owned by Git")
	})

	// Handoff to Flux, whose adoption is labels, and where the manifest has no owner annotation.
	t.Run("a handoff is adopted by Flux and the stale owner annotation removed", func(t *testing.T) {
		must(t, create(ui, tenancy.FieldManagerUI, newAgent("support", nil, uiOwned)))
		must(t, edit(ui, tenancy.FieldManagerUI, "support", func(a *agentorcav1alpha1.Agent) { tenancy.SetHandoff(a, time.Now().Add(time.Hour)) }))
		must(t, edit(flux, fluxFM, "support", func(a *agentorcav1alpha1.Agent) {
			a.Labels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
		}))
		eventually("the flip", func() bool { return get("support").Annotations[tenancy.AnnotationOwner] == "" })
		a := get("support")
		if tenancy.HandoffOf(a).Requested {
			t.Errorf("handoff annotations left: %v", a.Annotations)
		}
		if o := tenancy.Detect(a, tenancy.DetectOptions{}); o.Tool != tenancy.ToolFluxKustomize {
			t.Errorf("ownership = %+v", o)
		}
		denied(edit(ui, tenancy.FieldManagerUI, "support", setPrompt("from ui")), "owned by Git")
	})

	// An expired handoff is cleared and GitOps is blocked again.
	t.Run("a handoff expires", func(t *testing.T) {
		must(t, create(ui, tenancy.FieldManagerUI, newAgent("short-lived", nil, uiOwned)))
		must(t, edit(ui, tenancy.FieldManagerUI, "short-lived", func(a *agentorcav1alpha1.Agent) { tenancy.SetHandoff(a, time.Now().Add(3*time.Second)) }))
		// While it is pending Argo may write (and does not adopt: no tracking yet).
		must(t, edit(argo, argoFM, "short-lived", setPrompt("early")))
		eventually("the handoff to expire and be cleared", func() bool { return !tenancy.HandoffOf(get("short-lived")).Requested })
		a := get("short-lived")
		if a.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI {
			t.Errorf("an expired handoff changed ownership: %v", a.Annotations)
		}
		eventually("the HandoffExpired event", func() bool { return hasEvent("short-lived", EventReasonHandoffExpired) })
		denied(edit(argo, argoFM, "short-lived", setPrompt("late")), "owned by the agent-orca UI")
	})

	t.Run("a cancelled handoff blocks Argo again", func(t *testing.T) {
		must(t, create(ui, tenancy.FieldManagerUI, newAgent("changed-mind", nil, uiOwned)))
		must(t, edit(ui, tenancy.FieldManagerUI, "changed-mind", func(a *agentorcav1alpha1.Agent) { tenancy.SetHandoff(a, time.Now().Add(time.Hour)) }))
		must(t, edit(ui, tenancy.FieldManagerUI, "changed-mind", func(a *agentorcav1alpha1.Agent) { tenancy.ClearHandoff(a) }))
		denied(edit(argo, argoFM, "changed-mind", setPrompt("nope")), "owned by the agent-orca UI")
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
