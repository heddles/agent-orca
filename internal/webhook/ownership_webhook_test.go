package webhook

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	argoSA    = "system:serviceaccount:argocd:argocd-application-controller"
	fluxSA    = "system:serviceaccount:flux-system:kustomize-controller"
	operator  = "system:serviceaccount:agent-orca-system:agent-orca"
	otherNsSA = "system:serviceaccount:agent-orca-system:something-else"
)

var ownershipNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func ownershipValidator(t *testing.T) (*OwnershipValidator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rec := record.NewFakeRecorder(10)
	return &OwnershipValidator{
		Decoder:                admission.NewDecoder(scheme),
		Writers:                tenancy.DefaultGitOpsWriters(),
		OperatorNamespace:      "agent-orca-system",
		OperatorServiceAccount: "agent-orca",
		Recorder:               rec,
		Now:                    func() time.Time { return ownershipNow },
	}, rec
}

func agentWith(labels, annotations map[string]string) *agentorcav1alpha1.Agent {
	l := map[string]string{tenancy.LabelTenantName: "acme"}
	maps.Copy(l, labels)
	return &agentorcav1alpha1.Agent{
		TypeMeta:   metav1.TypeMeta{APIVersion: agentorcav1alpha1.GroupVersion.String(), Kind: "Agent"},
		ObjectMeta: metav1.ObjectMeta{Name: "support-triage", Namespace: "team-a", UID: "u-1", Labels: l, Annotations: annotations},
	}
}

var (
	uiOwned   = map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}
	argoOwned = map[string]string{"argocd.argoproj.io/tracking-id": "shop:agentorca.agentorca.io/Agent:team-a/support-triage"}
	fluxLabel = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
)

func handoff(expires time.Time) map[string]string {
	return map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI, tenancy.AnnotationHandoff: "pending", tenancy.AnnotationHandoffExpires: expires.Format(time.RFC3339)}
}

// ownReq builds an admission request; old is nil for a create.
func ownReq(t *testing.T, op admissionv1.Operation, user, fieldManager string, old, obj *agentorcav1alpha1.Agent) admission.Request {
	t.Helper()
	raw := func(o *agentorcav1alpha1.Agent) runtime.RawExtension {
		if o == nil {
			return runtime.RawExtension{}
		}
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: b}
	}
	opts := runtime.RawExtension{}
	if fieldManager != "" {
		opts.Raw = []byte(`{"fieldManager":"` + fieldManager + `"}`)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op, Kind: metav1.GroupVersionKind{Group: agentorcav1alpha1.GroupVersion.Group, Version: "v1alpha1", Kind: "Agent"},
		UserInfo: authenticationv1.UserInfo{Username: user}, Object: raw(obj), OldObject: raw(old), Options: opts,
	}}
}

func TestOwnershipWebhookRules(t *testing.T) {
	future, past := ownershipNow.Add(time.Hour), ownershipNow.Add(-time.Hour)
	tests := []struct {
		name         string
		op           admissionv1.Operation
		user, fm     string
		old, obj     *agentorcav1alpha1.Agent
		deny         bool
		warn         bool
		wantInDenial []string
		wantEvent    string
	}{
		// GitOps writers against UI-owned objects.
		{name: "argo SA update of a UI-owned object is denied", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned), deny: true, wantEvent: EventReasonGitOpsBlocked,
			wantInDenial: []string{`Agent "support-triage"`, "owned by the agent-orca UI (tenant acme)", "export it from the UI and mark it for handoff, or rename your manifest"}},
		{name: "argo field manager alone is enough", op: admissionv1.Update, user: "someone", fm: "argocd-controller",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned), deny: true, wantEvent: EventReasonGitOpsBlocked},
		{name: "flux kustomize SA is denied", op: admissionv1.Update, user: fluxSA,
			old: agentWith(nil, uiOwned), obj: agentWith(fluxLabel, uiOwned), deny: true, wantEvent: EventReasonGitOpsBlocked},
		{name: "flux helm-controller field manager is denied", op: admissionv1.Update, user: "x", fm: "helm-controller",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned), deny: true, wantEvent: EventReasonGitOpsBlocked},
		{name: "argo create that claims UI ownership is denied", op: admissionv1.Create, user: argoSA, fm: "argocd-controller",
			obj: agentWith(nil, uiOwned), deny: true},
		{name: "argo create of a fresh object is allowed", op: admissionv1.Create, user: argoSA, fm: "argocd-controller",
			obj: agentWith(nil, argoOwned)},
		{name: "argo update of a Git-owned object is allowed", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, argoOwned), obj: agentWith(nil, argoOwned)},
		{name: "argo update of an object with no owner is allowed", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, nil), obj: agentWith(nil, argoOwned)},
		{name: "argo update of a stale ui annotation under tracking is allowed", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, mergeMaps(uiOwned, argoOwned)), obj: agentWith(nil, mergeMaps(uiOwned, argoOwned))},

		// Handoff.
		{name: "handoff pending lets argo adopt", op: admissionv1.Update, user: argoSA, fm: "argocd-controller",
			old: agentWith(nil, handoff(future)), obj: agentWith(nil, mergeMaps(handoff(future), argoOwned))},
		{name: "handoff pending lets flux adopt", op: admissionv1.Update, user: fluxSA, fm: "kustomize-controller",
			old: agentWith(nil, handoff(future)), obj: agentWith(fluxLabel, handoff(future))},
		{name: "expired handoff blocks again", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, handoff(past)), obj: agentWith(nil, handoff(past)), deny: true, wantEvent: EventReasonGitOpsBlocked},
		{name: "handoff without an expiry blocks", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI, tenancy.AnnotationHandoff: "pending"}),
			obj: agentWith(nil, uiOwned), deny: true},
		{name: "a request cannot hand itself off", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, uiOwned), obj: agentWith(nil, handoff(future)), deny: true},

		// Identity-independent: an unlisted writer that stamps Git tracking.
		{name: "unlisted writer applying Argo-tracked content is denied", op: admissionv1.Update, user: "system:serviceaccount:gitops:renamed-argo", fm: "my-argo",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, mergeMaps(uiOwned, argoOwned)), deny: true, wantEvent: EventReasonGitOpsBlocked,
			wantInDenial: []string{"owned by the agent-orca UI (tenant acme)", "argocd shop", "mark it for handoff"}},
		{name: "unlisted writer applying Flux-tracked content is denied", op: admissionv1.Update, user: "someone", fm: "my-flux",
			old: agentWith(nil, uiOwned), obj: agentWith(fluxLabel, uiOwned), deny: true},
		{name: "unlisted writer applying Helm metadata is denied", op: admissionv1.Update, user: "someone", fm: "helm",
			old: agentWith(nil, uiOwned), obj: agentWith(map[string]string{"app.kubernetes.io/managed-by": "Helm"}, mergeMaps(uiOwned, map[string]string{"meta.helm.sh/release-name": "r"})), deny: true},
		{name: "a human adding owner git without a handoff is told to export", op: admissionv1.Update, user: "alice", fm: "kubectl-edit",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, map[string]string{tenancy.AnnotationOwner: "git"}), deny: true,
			wantInDenial: []string{"use Export in the UI"}},
		{name: "unlisted writer with a pending handoff may adopt", op: admissionv1.Update, user: "system:serviceaccount:gitops:renamed-argo", fm: "my-argo",
			old: agentWith(nil, handoff(future)), obj: agentWith(nil, mergeMaps(handoff(future), argoOwned))},
		{name: "unlisted writer with an expired handoff is denied", op: admissionv1.Update, user: "someone", fm: "my-argo",
			old: agentWith(nil, handoff(past)), obj: agentWith(nil, mergeMaps(handoff(past), argoOwned)), deny: true},
		{name: "unlisted writer updating an already Git-owned object is allowed", op: admissionv1.Update, user: "someone", fm: "my-argo",
			old: agentWith(nil, argoOwned), obj: agentWith(nil, argoOwned)},
		{name: "operator adding tracking metadata is not blocked", op: admissionv1.Update, user: operator, fm: "manager",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, mergeMaps(uiOwned, argoOwned))},

		// Operator and controllers.
		{name: "operator status or finalizer write is allowed", op: admissionv1.Update, user: operator, fm: "manager",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned)},
		{name: "operator tenant label stamping on a Git-owned object is allowed", op: admissionv1.Update, user: operator,
			old: agentWith(nil, argoOwned), obj: agentWith(map[string]string{tenancy.LabelTenantID: "tnt_1"}, argoOwned)},
		{name: "operator owner migration is allowed", op: admissionv1.Update, user: operator,
			old: agentWith(nil, nil), obj: agentWith(nil, uiOwned)},
		{name: "operator handoff flip is allowed", op: admissionv1.Update, user: operator, fm: "agent-orca-handoff",
			old: agentWith(nil, mergeMaps(handoff(future), argoOwned)), obj: agentWith(nil, argoOwned)},

		// The UI API.
		{name: "UI write of a UI-owned object is allowed", op: admissionv1.Update, user: operator, fm: tenancy.FieldManagerUI,
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned)},
		{name: "UI create is allowed", op: admissionv1.Create, user: operator, fm: tenancy.FieldManagerUI, obj: agentWith(nil, uiOwned)},
		{name: "UI write of an unowned object is left to the API check", op: admissionv1.Update, user: operator, fm: tenancy.FieldManagerUI,
			old: agentWith(nil, nil), obj: agentWith(nil, nil)},
		{name: "UI write of an Argo-owned object is denied", op: admissionv1.Update, user: operator, fm: tenancy.FieldManagerUI,
			old: agentWith(nil, argoOwned), obj: agentWith(nil, argoOwned), deny: true, wantEvent: EventReasonUIBlocked,
			wantInDenial: []string{"owned by Git", "argocd shop", "duplicate"}},
		{name: "UI write of a Flux-owned object is denied", op: admissionv1.Update, user: operator, fm: tenancy.FieldManagerUI,
			old: agentWith(fluxLabel, nil), obj: agentWith(fluxLabel, nil), deny: true, wantEvent: EventReasonUIBlocked},
		{name: "UI write of an explicitly git-owned object is denied", op: admissionv1.Update, user: operator, fm: tenancy.FieldManagerUI,
			old: agentWith(nil, map[string]string{tenancy.AnnotationOwner: "git"}), obj: agentWith(nil, uiOwned), deny: true},
		{name: "UI create of an object that claims Git tracking is denied", op: admissionv1.Create, user: operator, fm: tenancy.FieldManagerUI,
			obj: agentWith(nil, mergeMaps(uiOwned, argoOwned)), deny: true},
		{name: "UI field manager from another user is held to the same rule", op: admissionv1.Update, user: "mallory", fm: tenancy.FieldManagerUI,
			old: agentWith(nil, argoOwned), obj: agentWith(nil, argoOwned), deny: true},

		// Humans.
		{name: "human edit of a UI-owned object warns", op: admissionv1.Update, user: "alice", fm: "kubectl-edit",
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned), warn: true},
		{name: "human create of a UI-owned object warns", op: admissionv1.Create, user: "alice", obj: agentWith(nil, uiOwned), warn: true},
		{name: "human edit of a Git-owned object is allowed without a warning", op: admissionv1.Update, user: "alice",
			old: agentWith(nil, argoOwned), obj: agentWith(nil, argoOwned)},
		{name: "human edit of an unowned object is allowed", op: admissionv1.Update, user: "alice", old: agentWith(nil, nil), obj: agentWith(nil, nil)},
		{name: "another service account in the operator namespace is not the operator", op: admissionv1.Update, user: otherNsSA,
			old: agentWith(nil, uiOwned), obj: agentWith(nil, uiOwned), warn: true},

		// Deletion in progress.
		{name: "finalizer removal on a UI-owned object by Argo is not blocked", op: admissionv1.Update, user: argoSA,
			old: agentWith(nil, uiOwned), obj: deleting(agentWith(nil, uiOwned))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, rec := ownershipValidator(t)
			resp := v.Handle(context.Background(), ownReq(t, tc.op, tc.user, tc.fm, tc.old, tc.obj))
			if resp.Allowed == tc.deny {
				t.Fatalf("allowed = %v (%s), want %v", resp.Allowed, resp.Result.Message, !tc.deny)
			}
			for _, want := range tc.wantInDenial {
				if !strings.Contains(resp.Result.Message, want) {
					t.Errorf("denial %q does not contain %q", resp.Result.Message, want)
				}
			}
			if got := len(resp.Warnings) > 0; got != tc.warn {
				t.Errorf("warnings = %v, want a warning: %v", resp.Warnings, tc.warn)
			}
			if tc.warn && !strings.Contains(resp.Warnings[0], "agent-orca UI (tenant acme)") {
				t.Errorf("warning = %q", resp.Warnings[0])
			}
			select {
			case ev := <-rec.Events:
				if !tc.deny || tc.old == nil || !strings.Contains(ev, tc.wantEvent) {
					t.Errorf("event %q, want reason %q", ev, tc.wantEvent)
				}
			default:
				if tc.deny && tc.old != nil {
					t.Errorf("no event recorded, want %s", tc.wantEvent)
				}
			}
		})
	}
}

func TestOwnershipWebhookLabelTracking(t *testing.T) {
	v, _ := ownershipValidator(t)
	instance := map[string]string{"app.kubernetes.io/instance": "shop"}
	// Without label tracking the instance label says nothing: a UI-owned object
	// that carries it is still UI-owned, and a UI write goes through.
	resp := v.Handle(context.Background(), ownReq(t, admissionv1.Update, operator, tenancy.FieldManagerUI,
		agentWith(instance, uiOwned), agentWith(instance, uiOwned)))
	if !resp.Allowed {
		t.Errorf("instance label alone blocked the UI: %s", resp.Result.Message)
	}
	v.Detect = tenancy.DetectOptions{ArgoLabelTracking: true}
	resp = v.Handle(context.Background(), ownReq(t, admissionv1.Update, operator, tenancy.FieldManagerUI,
		agentWith(instance, nil), agentWith(instance, nil)))
	if resp.Allowed {
		t.Error("with label tracking the instance label should mark the object as Argo-owned")
	}
}

func TestOwnershipWebhookCustomWriters(t *testing.T) {
	v, _ := ownershipValidator(t)
	v.Writers = tenancy.ParseWriters("gitops:argo", "my-argo")
	deniedFor := func(user, fm string) bool {
		return !v.Handle(context.Background(), ownReq(t, admissionv1.Update, user, fm, agentWith(nil, uiOwned), agentWith(nil, uiOwned))).Allowed
	}
	if !deniedFor("system:serviceaccount:gitops:argo", "") || !deniedFor("x", "my-argo") {
		t.Error("custom writers are not denied")
	}
	if deniedFor(argoSA, "argocd-controller") {
		t.Error("a configured list replaces the defaults, but the default Argo CD was still denied")
	}
}

func TestOwnershipWebhookOtherKindsAreNotItsBusiness(t *testing.T) {
	v, _ := ownershipValidator(t)
	req := ownReq(t, admissionv1.Update, argoSA, "", agentWith(nil, uiOwned), agentWith(nil, uiOwned))
	req.Kind.Kind = "AgentRun"
	if !v.Handle(context.Background(), req).Allowed {
		t.Error("AgentRun is not a UI-writable kind")
	}
}

func deleting(a *agentorcav1alpha1.Agent) *agentorcav1alpha1.Agent {
	now := metav1.NewTime(ownershipNow)
	a.DeletionTimestamp = &now
	return a
}

func mergeMaps(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}
