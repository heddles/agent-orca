package tenancy

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func objWith(labels, annotations map[string]string) *metav1.ObjectMeta {
	return &metav1.ObjectMeta{Name: "x", Namespace: "ns", Labels: labels, Annotations: annotations}
}

func TestDetect(t *testing.T) {
	label := DetectOptions{ArgoLabelTracking: true}
	custom := DetectOptions{ArgoLabelTracking: true, ArgoTrackingLabelKey: "argocd.argoproj.io/instance"}
	tests := []struct {
		name string
		obj  *metav1.ObjectMeta
		opts DetectOptions
		want Ownership
	}{
		{"nothing", objWith(nil, nil), DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"ui annotation", objWith(nil, map[string]string{AnnotationOwner: OwnerUI}), DetectOptions{}, Ownership{Owner: OwnerUI}},
		{"explicit git", objWith(nil, map[string]string{AnnotationOwner: OwnerGit}), DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolGit}},
		{"unrecognised owner value", objWith(nil, map[string]string{AnnotationOwner: "robot"}), DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"argo annotation", objWith(nil, map[string]string{"argocd.argoproj.io/tracking-id": "shop:agentorca.agentorca.io/Agent:ns/x"}),
			DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolArgoCD, App: "shop"}},
		{"argo annotation beats ui annotation", objWith(nil, map[string]string{"argocd.argoproj.io/tracking-id": "shop:g/Agent:ns/x", AnnotationOwner: OwnerUI}),
			DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolArgoCD, App: "shop"}},
		{"instance label alone is not Argo by default", objWith(map[string]string{"app.kubernetes.io/instance": "shop"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"instance label with label tracking", objWith(map[string]string{"app.kubernetes.io/instance": "shop"}, nil),
			label, Ownership{Owner: OwnerGit, Tool: ToolArgoCD, App: "shop"}},
		{"instance label with label tracking on a custom key is not it", objWith(map[string]string{"app.kubernetes.io/instance": "shop"}, nil),
			custom, Ownership{Owner: OwnerUnknown}},
		{"custom tracking key", objWith(map[string]string{"argocd.argoproj.io/instance": "shop"}, nil),
			custom, Ownership{Owner: OwnerGit, Tool: ToolArgoCD, App: "shop"}},
		{"custom key set but tracking off", objWith(map[string]string{"argocd.argoproj.io/instance": "shop"}, nil),
			DetectOptions{ArgoTrackingLabelKey: "argocd.argoproj.io/instance"}, Ownership{Owner: OwnerUnknown}},
		{"flux kustomize", objWith(map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolFluxKustomize, App: "flux-system/apps"}},
		{"flux kustomize needs the namespace label too", objWith(map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"flux helm", objWith(map[string]string{"helm.toolkit.fluxcd.io/name": "shop", "helm.toolkit.fluxcd.io/namespace": "prod"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolFluxHelm, App: "prod/shop"}},
		{"flux helm name only", objWith(map[string]string{"helm.toolkit.fluxcd.io/name": "shop"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolFluxHelm, App: "shop"}},
		{"helm", objWith(map[string]string{"app.kubernetes.io/managed-by": "Helm"}, map[string]string{"meta.helm.sh/release-name": "shop"}),
			DetectOptions{}, Ownership{Owner: OwnerGit, Tool: ToolHelm, App: "shop"}},
		{"helm label without release annotation", objWith(map[string]string{"app.kubernetes.io/managed-by": "Helm"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"release annotation without helm label", objWith(nil, map[string]string{"meta.helm.sh/release-name": "shop"}),
			DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"controller managed-by is not helm", objWith(map[string]string{"app.kubernetes.io/managed-by": "agent-orca"}, nil),
			DetectOptions{}, Ownership{Owner: OwnerUnknown}},
		{"helm beats the instance label under label tracking",
			objWith(map[string]string{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "other"}, map[string]string{"meta.helm.sh/release-name": "shop"}),
			label, Ownership{Owner: OwnerGit, Tool: ToolHelm, App: "shop"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.obj, tc.opts); got != tc.want {
				t.Errorf("Detect = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStripGitTracking(t *testing.T) {
	o := objWith(
		map[string]string{"kustomize.toolkit.fluxcd.io/name": "a", "kustomize.toolkit.fluxcd.io/namespace": "b", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "i", "keep": "me"},
		map[string]string{"argocd.argoproj.io/tracking-id": "a:b", "meta.helm.sh/release-name": "r", "kubectl.kubernetes.io/last-applied-configuration": "{}", "note": "hi"})
	StripGitTracking(o, DetectOptions{ArgoLabelTracking: true})
	if got := Detect(o, DetectOptions{ArgoLabelTracking: true}); got.Owner != OwnerUnknown {
		t.Errorf("still detected after stripping: %+v", got)
	}
	if o.Labels["keep"] != "me" || o.Annotations["note"] != "hi" || len(o.Labels) != 1 || len(o.Annotations) != 1 {
		t.Errorf("stripped too much or too little: %v %v", o.Labels, o.Annotations)
	}
}

func TestHandoff(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)
	tests := []struct {
		name                 string
		a                    map[string]string
		requested, act, expd bool
	}{
		{"none", nil, false, false, false},
		{"pending, unexpired", map[string]string{AnnotationHandoff: "pending", AnnotationHandoffExpires: future}, true, true, false},
		{"pending, expired", map[string]string{AnnotationHandoff: "pending", AnnotationHandoffExpires: past}, true, false, true},
		{"pending, no expiry", map[string]string{AnnotationHandoff: "pending"}, true, false, true},
		{"pending, bad expiry", map[string]string{AnnotationHandoff: "pending", AnnotationHandoffExpires: "tomorrow"}, true, false, true},
		{"other value", map[string]string{AnnotationHandoff: "done", AnnotationHandoffExpires: future}, true, false, true},
		{"expiry only", map[string]string{AnnotationHandoffExpires: future}, true, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := HandoffOf(objWith(nil, tc.a))
			if h.Requested != tc.requested || h.Active(now) != tc.act || h.Expired(now) != tc.expd {
				t.Errorf("requested/active/expired = %v/%v/%v, want %v/%v/%v", h.Requested, h.Active(now), h.Expired(now), tc.requested, tc.act, tc.expd)
			}
		})
	}
	o := objWith(nil, nil)
	SetHandoff(o, now.Add(time.Hour))
	if !HandoffOf(o).Active(now) {
		t.Error("SetHandoff did not set an active handoff")
	}
	ClearHandoff(o)
	if HandoffOf(o).Requested {
		t.Error("ClearHandoff left annotations")
	}
}

func TestWriters(t *testing.T) {
	w := ParseWriters("", "")
	for _, tc := range []struct {
		user, fm string
		want     bool
	}{
		{"system:serviceaccount:argocd:argocd-application-controller", "", true},
		{"system:serviceaccount:flux-system:kustomize-controller", "", true},
		{"system:serviceaccount:flux-system:helm-controller", "", true},
		{"alice", "argocd-controller", true},
		{"alice", "kustomize-controller", true},
		{"alice", "helm-controller", true},
		{"alice", "kubectl-edit", false},
		{"system:serviceaccount:argocd:argocd-server", "", false},
		{"alice", "", false},
	} {
		if _, got := w.Match(tc.user, tc.fm); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.user, tc.fm, got, tc.want)
		}
	}
	custom := ParseWriters("gitops:argo, ci/deployer ,system:serviceaccount:x:y", "my-argo")
	want := []string{"system:serviceaccount:gitops:argo", "system:serviceaccount:ci:deployer", "system:serviceaccount:x:y"}
	for i, sa := range want {
		if custom.ServiceAccounts[i] != sa {
			t.Errorf("service account %d = %q, want %q", i, custom.ServiceAccounts[i], sa)
		}
	}
	if _, ok := custom.Match("alice", "argocd-controller"); ok {
		t.Error("a configured list must replace the default field managers")
	}
	if _, ok := custom.Match("alice", "my-argo"); !ok {
		t.Error("custom field manager not matched")
	}
}
