package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Tenant ownership of tenant-owned objects, in two webhooks that cover the same
// kinds:
//
//   - TenantLabeler (mutating) stamps agentorca.io/tenant-id on an object that
//     arrives without one, from the single tenant that claims its namespace.
//     This is how GitOps-applied manifests get the label.
//   - TenantIsolationValidator rejects a missing label, a changed label, a
//     label that no tenant claiming the namespace holds, platform scope set by
//     anyone but a platform admin, and references to another tenant's objects.
//
// In audit mode (tenancy.ModeAudit, the default) a violation is logged,
// counted and returned to the client as an admission warning, but allowed.

// +kubebuilder:webhook:path=/mutate-agentorca-agentorca-io-v1alpha1-tenantlabel,mutating=true,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=agents;tools;mcpservers;knowledgebases;modelselectors;modelproviders;agentdeployments;agentworkflows;agentruns;agentschedules,verbs=create;update,versions=v1alpha1,name=mtenantlabel.kb.io,admissionReviewVersions=v1

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-tenantisolation,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=agents;tools;mcpservers;knowledgebases;modelselectors;modelproviders;agentdeployments;agentworkflows;agentruns;agentschedules,verbs=create;update,versions=v1alpha1,name=vtenantisolation.kb.io,admissionReviewVersions=v1

// tenantObject is the part of an admission request both handlers read.
type tenantObject struct {
	metav1.ObjectMeta `json:"metadata"`
}

func decodeMeta(raw []byte) (*tenantObject, error) {
	var o tenantObject
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// TenantLabeler stamps the tenant-id label from the namespace's tenant binding.
type TenantLabeler struct {
	// TenantConfigs reads TenantConfigs. The manager's cached client is fine.
	TenantConfigs client.Reader
}

// Handle implements admission.Handler.
func (l *TenantLabeler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation == admissionv1.Delete {
		return admission.Allowed("ok")
	}
	obj, err := decodeMeta(req.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !obj.DeletionTimestamp.IsZero() {
		return admission.Allowed("ok")
	}
	id, platform := tenancy.TenantOf(obj)

	// An update that drops the label (kubectl replace, a Git tool that strips
	// labels it does not know) keeps the old object's labels. A changed value is
	// left for the validator to reject.
	if req.Operation == admissionv1.Update && len(req.OldObject.Raw) > 0 {
		old, err := decodeMeta(req.OldObject.Raw)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		oldID, oldPlatform := tenancy.TenantOf(old)
		set := map[string]string{}
		if id == "" && oldID != "" {
			set[tenancy.LabelTenantID] = oldID
		}
		if !platform && oldPlatform && obj.GetLabels()[tenancy.LabelScope] == "" {
			set[tenancy.LabelScope] = tenancy.ScopePlatform
		}
		if len(set) > 0 {
			return patchLabels(req, set)
		}
	}
	if id != "" || platform {
		return admission.Allowed("ok")
	}

	var tcs agentorcav1alpha1.TenantConfigList
	if err := l.TenantConfigs.List(ctx, &tcs); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("listing TenantConfigs: %w", err))
	}
	ids, _ := tenancy.NamespaceClaimants(tcs.Items, req.Namespace)
	if len(ids) != 1 {
		// Unbound or shared namespace: the validator decides.
		return admission.Allowed("ok")
	}
	return patchLabels(req, map[string]string{tenancy.LabelTenantID: ids[0]})
}

// patchLabels returns a JSON patch that adds the labels to the object.
func patchLabels(req admission.Request, set map[string]string) admission.Response {
	var doc map[string]any
	if err := json.Unmarshal(req.Object.Raw, &doc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
		meta["labels"] = labels
	}
	for k, v := range set {
		labels[k] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, out)
}

// TenantIsolationValidator checks label and reference rules.
type TenantIsolationValidator struct {
	Decoder admission.Decoder
	// TenantConfigs reads TenantConfigs (cached is fine).
	TenantConfigs client.Reader
	// Reader reads reference targets, Secrets included. Use the API reader so
	// no informer is started for Secrets.
	Reader client.Reader
	// AdminGroups may mark objects platform-shared.
	AdminGroups []string
	// OperatorNamespace identifies the operator's ServiceAccount
	// (system:serviceaccount:<ns>:*), which creates children of platform-shared
	// objects and so passes platform scope along.
	OperatorNamespace string
}

// Handle implements admission.Handler.
func (v *TenantIsolationValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := tenancy.NewObject(req.Kind.Kind)
	if obj == nil {
		return admission.Allowed("not a tenant-owned kind")
	}
	if err := v.Decoder.DecodeRaw(req.Object, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	// Objects on their way out (finalizer removal) are not held to rules about
	// their future.
	if !obj.GetDeletionTimestamp().IsZero() {
		return admission.Allowed("ok")
	}
	var old client.Object
	if req.Operation == admissionv1.Update {
		old = tenancy.NewObject(req.Kind.Kind)
		if err := v.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	var tcs agentorcav1alpha1.TenantConfigList
	if err := v.TenantConfigs.List(ctx, &tcs); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("listing TenantConfigs: %w", err))
	}
	denials, warnings, err := v.check(ctx, obj, old, req.UserInfo.Username, req.UserInfo.Groups, req.Kind.Kind, tcs.Items)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if len(denials) > 0 {
		return admission.Denied(strings.Join(denials, "; ")).WithWarnings(warnings...)
	}
	return admission.Allowed("ok").WithWarnings(warnings...)
}

// check returns the violations that deny the request and the ones audit mode
// lets through (as warnings).
func (v *TenantIsolationValidator) check(ctx context.Context, obj, old client.Object, user string, groups []string, kind string, tcs []agentorcav1alpha1.TenantConfig) (denials, warnings []string, err error) {
	ns, name := obj.GetNamespace(), obj.GetName()
	report := func(rule, msg string) {
		if tenancy.Violation(rule, kind, ns, name, msg) {
			denials = append(denials, msg)
		} else {
			warnings = append(warnings, "tenant isolation (audit mode, would be denied): "+msg)
		}
	}

	id, platform := tenancy.TenantOf(obj)
	if old != nil {
		oldID, oldPlatform := tenancy.TenantOf(old)
		if oldID != "" && id != oldID {
			report(tenancy.RuleLabelImmutable, fmt.Sprintf("label %s is immutable once set (was %q)", tenancy.LabelTenantID, oldID))
		}
		if oldPlatform != platform {
			report(tenancy.RuleLabelImmutable, fmt.Sprintf("label %s is immutable once set", tenancy.LabelScope))
		}
	}
	if platform && (old == nil || !isPlatform(old)) && !v.mayMarkPlatform(user, groups) {
		report(tenancy.RuleScopeNotAdmin, fmt.Sprintf("only a platform admin (groups %v) may set %s=%s", v.AdminGroups, tenancy.LabelScope, tenancy.ScopePlatform))
	}
	if platform && id != "" {
		report(tenancy.RuleLabelNamespace, fmt.Sprintf("an object cannot carry both %s and %s=%s", tenancy.LabelTenantID, tenancy.LabelScope, tenancy.ScopePlatform))
	}

	ids, claims := tenancy.NamespaceClaimants(tcs, ns)
	switch {
	case id == "" && !platform:
		switch {
		case claims > 1:
			report(tenancy.RuleLabelMissing, fmt.Sprintf("namespace %q is shared by several tenants: set label %s", ns, tenancy.LabelTenantID))
		case claims == 0:
			report(tenancy.RuleLabelMissing, fmt.Sprintf("no tenant claims namespace %q: set label %s, or %s=%s for a platform-shared object", ns, tenancy.LabelTenantID, tenancy.LabelScope, tenancy.ScopePlatform))
		default:
			report(tenancy.RuleLabelMissing, fmt.Sprintf("the tenant that claims namespace %q has no ID yet: retry once its TenantConfig is registered", ns))
		}
	case id != "" && len(ids) > 0 && !slices.Contains(ids, id):
		report(tenancy.RuleLabelNamespace, fmt.Sprintf("label %s=%s: no tenant with that ID claims namespace %q", tenancy.LabelTenantID, id, ns))
	}

	violations, err := tenancy.CheckRefs(ctx, v.Reader, obj, tcs)
	if err != nil {
		return nil, nil, err
	}
	for _, rv := range violations {
		report(tenancy.RuleCrossTenantRef, rv.Error())
	}
	return denials, warnings, nil
}

func isPlatform(obj client.Object) bool {
	_, p := tenancy.TenantOf(obj)
	return p
}

func (v *TenantIsolationValidator) mayMarkPlatform(user string, groups []string) bool {
	if anyIn(groups, v.AdminGroups) {
		return true
	}
	return v.OperatorNamespace != "" && strings.HasPrefix(user, "system:serviceaccount:"+v.OperatorNamespace+":")
}

// SetupTenantIsolationWebhooks registers the tenant label and isolation
// webhooks. operatorNamespace is the namespace the operator runs in.
func SetupTenantIsolationWebhooks(mgr ctrl.Manager, adminGroups []string, operatorNamespace string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	srv := mgr.GetWebhookServer()
	srv.Register("/mutate-agentorca-agentorca-io-v1alpha1-tenantlabel",
		&admission.Webhook{Handler: &TenantLabeler{TenantConfigs: mgr.GetClient()}})
	srv.Register("/validate-agentorca-agentorca-io-v1alpha1-tenantisolation",
		&admission.Webhook{Handler: &TenantIsolationValidator{
			Decoder: decoder, TenantConfigs: mgr.GetClient(), Reader: mgr.GetAPIReader(),
			AdminGroups: adminGroups, OperatorNamespace: operatorNamespace,
		}})
	return nil
}
