/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tenancy

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// UIKinds are the kinds the UI API can write, by Kind name.
var UIKinds = map[string]func() client.Object{
	"Agent":           func() client.Object { return &agentorcav1alpha1.Agent{} },
	"Tool":            func() client.Object { return &agentorcav1alpha1.Tool{} },
	"MCPServer":       func() client.Object { return &agentorcav1alpha1.MCPServer{} },
	"ModelProvider":   func() client.Object { return &agentorcav1alpha1.ModelProvider{} },
	"KnowledgeBase":   func() client.Object { return &agentorcav1alpha1.KnowledgeBase{} },
	"ModelSelector":   func() client.Object { return &agentorcav1alpha1.ModelSelector{} },
	"AgentDeployment": func() client.Object { return &agentorcav1alpha1.AgentDeployment{} },
	"AgentWorkflow":   func() client.Object { return &agentorcav1alpha1.AgentWorkflow{} },
	"AgentSchedule":   func() client.Object { return &agentorcav1alpha1.AgentSchedule{} },
}

// NewUIObject returns an empty object of a UI-writable kind, nil for any other.
func NewUIObject(kind string) client.Object {
	if f, ok := UIKinds[kind]; ok {
		return f()
	}
	return nil
}

// Who owns an object, UI or Git, and how a handoff from one to the other works.
// The UI API, the ownership webhook and the handoff controller all read the
// same answer from Detect.

const (
	// OwnerGit marks an object a GitOps tool or a pipeline owns.
	OwnerGit = "git"
	// OwnerUnknown is the answer for an object with no ownership signal at all.
	OwnerUnknown = "unknown"

	// AnnotationHandoff is HandoffPending while the UI owner has exported the
	// object and allows a GitOps writer to adopt it.
	AnnotationHandoff = "agentorca.io/handoff"
	// HandoffPending is the only value AnnotationHandoff takes.
	HandoffPending = "pending"
	// AnnotationHandoffExpires is the RFC 3339 time the handoff stops allowing
	// GitOps writes.
	AnnotationHandoffExpires = "agentorca.io/handoff-expires"

	// AnnotationMaintainer is the catalog maintainer of an object. The
	// export keeps it.
	AnnotationMaintainer = "agentorca.io/maintainer"
)

// Tool names in Ownership.Tool.
const (
	ToolArgoCD        = "argocd"
	ToolFluxKustomize = "flux-kustomize"
	ToolFluxHelm      = "flux-helm"
	ToolHelm          = "helm"
	ToolGit           = "git"
)

// GitOps tracking metadata. Argo CD in annotation mode writes the annotation;
// in label mode it writes a label whose key is configurable. That label is
// also what Helm and many charts set, so it counts only when the operator is
// told Argo CD uses it.
const (
	argoTrackingAnnotation = "argocd.argoproj.io/tracking-id"
	// DefaultArgoTrackingLabel is Argo CD's default label tracking key.
	DefaultArgoTrackingLabel = "app.kubernetes.io/instance"
	fluxKustomizeName        = "kustomize.toolkit.fluxcd.io/name"
	fluxKustomizeNamespace   = "kustomize.toolkit.fluxcd.io/namespace"
	fluxHelmName             = "helm.toolkit.fluxcd.io/name"
	fluxHelmNamespace        = "helm.toolkit.fluxcd.io/namespace"
	helmManagedByLabel       = "app.kubernetes.io/managed-by"
	helmReleaseAnnotation    = "meta.helm.sh/release-name"
	helmReleaseNamespace     = "meta.helm.sh/release-namespace"
	kubectlLastApplied       = "kubectl.kubernetes.io/last-applied-configuration"
)

// DetectOptions configures Detect.
type DetectOptions struct {
	// ArgoLabelTracking says Argo CD tracks resources by label. Off by default.
	ArgoLabelTracking bool
	// ArgoTrackingLabelKey is that label; empty means DefaultArgoTrackingLabel.
	ArgoTrackingLabelKey string
}

func (o DetectOptions) argoLabel() string {
	if o.ArgoTrackingLabelKey == "" {
		return DefaultArgoTrackingLabel
	}
	return o.ArgoTrackingLabelKey
}

// Ownership is what Detect finds.
type Ownership struct {
	// Owner is OwnerUI, OwnerGit or OwnerUnknown.
	Owner string `json:"owner"`
	// Tool is the GitOps tool for a Git-owned object (the Tool* constants).
	Tool string `json:"tool,omitempty"`
	// App is the Argo CD application, Flux Kustomization or HelmRelease, or
	// Helm release that manages the object.
	App string `json:"app,omitempty"`
}

// Detect decides who owns obj. Git tracking metadata wins over an
// agentorca.io/owner: ui annotation: an object that a GitOps tool tracks is
// Git's, whatever else it says (that is also what an adopted handoff looks
// like until the handoff controller removes the stale annotation).
func Detect(obj metav1.Object, opts DetectOptions) Ownership {
	a, l := obj.GetAnnotations(), obj.GetLabels()
	if id, ok := a[argoTrackingAnnotation]; ok {
		// <app>:<group>/<kind>:<namespace>/<name>
		app, _, _ := strings.Cut(id, ":")
		return Ownership{Owner: OwnerGit, Tool: ToolArgoCD, App: app}
	}
	if n, ok := l[fluxKustomizeName]; ok && l[fluxKustomizeNamespace] != "" {
		return Ownership{Owner: OwnerGit, Tool: ToolFluxKustomize, App: l[fluxKustomizeNamespace] + "/" + n}
	}
	if n, ok := l[fluxHelmName]; ok {
		app := n
		if ns := l[fluxHelmNamespace]; ns != "" {
			app = ns + "/" + n
		}
		return Ownership{Owner: OwnerGit, Tool: ToolFluxHelm, App: app}
	}
	if l[helmManagedByLabel] == "Helm" {
		if rel, ok := a[helmReleaseAnnotation]; ok {
			return Ownership{Owner: OwnerGit, Tool: ToolHelm, App: rel}
		}
	}
	if opts.ArgoLabelTracking {
		if v, ok := l[opts.argoLabel()]; ok {
			return Ownership{Owner: OwnerGit, Tool: ToolArgoCD, App: v}
		}
	}
	switch a[AnnotationOwner] {
	case OwnerGit:
		return Ownership{Owner: OwnerGit, Tool: ToolGit}
	case OwnerUI:
		return Ownership{Owner: OwnerUI}
	}
	return Ownership{Owner: OwnerUnknown}
}

// StripGitTracking removes the GitOps tracking metadata Detect looks at, and
// kubectl's last-applied copy, so an object copied from a Git-owned source does
// not carry its source's tracking.
func StripGitTracking(obj metav1.Object, opts DetectOptions) {
	a, l := obj.GetAnnotations(), obj.GetLabels()
	for _, k := range []string{argoTrackingAnnotation, helmReleaseAnnotation, helmReleaseNamespace, kubectlLastApplied} {
		delete(a, k)
	}
	for _, k := range []string{fluxKustomizeName, fluxKustomizeNamespace, fluxHelmName, fluxHelmNamespace} {
		delete(l, k)
	}
	if l[helmManagedByLabel] == "Helm" {
		delete(l, helmManagedByLabel)
	}
	if opts.ArgoLabelTracking {
		delete(l, opts.argoLabel())
	}
	obj.SetAnnotations(a)
	obj.SetLabels(l)
}

// Handoff is the handoff state an object carries.
type Handoff struct {
	// Requested is true when either handoff annotation is present.
	Requested bool
	// Pending is the value of AnnotationHandoff being "pending".
	Pending bool
	// Expires is the parsed expiry; zero when missing or malformed.
	Expires time.Time
}

// HandoffOf reads the handoff annotations.
func HandoffOf(obj metav1.Object) Handoff {
	a := obj.GetAnnotations()
	h := Handoff{Pending: a[AnnotationHandoff] == HandoffPending}
	_, hasExp := a[AnnotationHandoffExpires]
	_, hasHand := a[AnnotationHandoff]
	h.Requested = hasExp || hasHand
	if t, err := time.Parse(time.RFC3339, a[AnnotationHandoffExpires]); err == nil {
		h.Expires = t
	}
	return h
}

// Active reports whether the handoff currently lets a GitOps writer adopt the
// object: pending, with an expiry that has not passed. A missing or malformed
// expiry is never active.
func (h Handoff) Active(now time.Time) bool {
	return h.Pending && !h.Expires.IsZero() && now.Before(h.Expires)
}

// Expired reports whether handoff annotations are present but no longer active.
func (h Handoff) Expired(now time.Time) bool { return h.Requested && !h.Active(now) }

// SetHandoff marks obj as pending handoff until expires.
func SetHandoff(obj metav1.Object, expires time.Time) {
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[AnnotationHandoff] = HandoffPending
	a[AnnotationHandoffExpires] = expires.UTC().Format(time.RFC3339)
	obj.SetAnnotations(a)
}

// ClearHandoff removes the handoff annotations.
func ClearHandoff(obj metav1.Object) {
	a := obj.GetAnnotations()
	delete(a, AnnotationHandoff)
	delete(a, AnnotationHandoffExpires)
	obj.SetAnnotations(a)
}

// GitOpsWriters identifies the writers that are GitOps tools.
type GitOpsWriters struct {
	// ServiceAccounts are full usernames (system:serviceaccount:<ns>:<name>).
	ServiceAccounts []string
	// FieldManagers are server-side apply and update field manager names.
	FieldManagers []string
}

// DefaultGitOpsWriters are the identities of Argo CD, Flux and Helm in their
// usual namespaces.
func DefaultGitOpsWriters() GitOpsWriters {
	return GitOpsWriters{
		ServiceAccounts: []string{
			"system:serviceaccount:argocd:argocd-application-controller",
			"system:serviceaccount:flux-system:kustomize-controller",
			"system:serviceaccount:flux-system:helm-controller",
		},
		FieldManagers: []string{"argocd-controller", "kustomize-controller", "helm-controller"},
	}
}

// ParseWriters builds GitOpsWriters from comma-separated lists. A service
// account may be a full username or namespace:name or namespace/name. An empty
// list keeps the default for that list.
func ParseWriters(serviceAccounts, fieldManagers string) GitOpsWriters {
	w := DefaultGitOpsWriters()
	if sas := splitList(serviceAccounts); len(sas) > 0 {
		w.ServiceAccounts = nil
		for _, sa := range sas {
			w.ServiceAccounts = append(w.ServiceAccounts, NormalizeServiceAccount(sa))
		}
	}
	if fms := splitList(fieldManagers); len(fms) > 0 {
		w.FieldManagers = fms
	}
	return w
}

// NormalizeServiceAccount turns namespace:name or namespace/name into a username.
func NormalizeServiceAccount(s string) string {
	if strings.HasPrefix(s, "system:serviceaccount:") {
		return s
	}
	return "system:serviceaccount:" + strings.Replace(s, "/", ":", 1)
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Match reports whether the request is from a configured GitOps writer, and
// says which identity gave it away.
func (w GitOpsWriters) Match(username, fieldManager string) (by string, ok bool) {
	if slices.Contains(w.ServiceAccounts, username) {
		return "service account " + username, true
	}
	for _, fm := range w.FieldManagers {
		if fm != "" && fm == fieldManager {
			return "field manager " + fieldManager, true
		}
	}
	return "", false
}

// Environment variables the ownership settings come from (Helm values under
// ownership). Unset means the default.
const (
	EnvGitOpsServiceAccounts = "OWNERSHIP_GITOPS_SERVICE_ACCOUNTS"
	EnvGitOpsFieldManagers   = "OWNERSHIP_GITOPS_FIELD_MANAGERS"
	EnvArgoLabelTracking     = "OWNERSHIP_ARGO_LABEL_TRACKING"
	EnvArgoTrackingLabel     = "OWNERSHIP_ARGO_TRACKING_LABEL"
	EnvHandoffTTL            = "OWNERSHIP_HANDOFF_TTL"

	// DefaultHandoffTTL is how long an exported object waits for Git to adopt it.
	DefaultHandoffTTL = 7 * 24 * time.Hour
)

// WritersFromEnv reads the GitOps writer identities.
func WritersFromEnv() GitOpsWriters {
	return ParseWriters(os.Getenv(EnvGitOpsServiceAccounts), os.Getenv(EnvGitOpsFieldManagers))
}

// DetectOptionsFromEnv reads the Argo CD label tracking settings.
func DetectOptionsFromEnv() DetectOptions {
	on, _ := strconv.ParseBool(os.Getenv(EnvArgoLabelTracking))
	return DetectOptions{ArgoLabelTracking: on, ArgoTrackingLabelKey: os.Getenv(EnvArgoTrackingLabel)}
}

// HandoffTTLFromEnv reads the handoff lifetime; an unset or invalid value is
// DefaultHandoffTTL.
func HandoffTTLFromEnv() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(EnvHandoffTTL)); err == nil && d > 0 {
		return d
	}
	return DefaultHandoffTTL
}
