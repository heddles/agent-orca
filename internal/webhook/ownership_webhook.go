package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// OwnershipPath is the validating webhook's path.
const OwnershipPath = "/validate-agentorca-agentorca-io-v1alpha1-ownership"

// The ownership webhook keeps one writer per object: the UI or Git. It covers
// the kinds the UI API can write, with its own failurePolicy: Fail. With Ignore,
// an unavailable webhook would let a GitOps sync take over a UI-owned object
// without anyone noticing.
//
// Who is writing is decided from the request, in this order:
//
//  1. a configured GitOps writer: the user is one of the GitOps service accounts
//     (Argo CD application controller, Flux kustomize and helm controllers), or
//     the request's field manager is one of theirs;
//  2. the operator's service account with any field manager but agent-orca-ui:
//     controllers (status, finalizers, tenant label stamping, owner migration,
//     the handoff controller). Never blocked;
//  3. the field manager agent-orca-ui: the UI API, which runs as the operator's
//     service account. Denied on Git-owned objects, as a backstop to the API's
//     own check;
//  4. anyone else, a human with kubectl.
//
// The rules, on the object as it is stored (the old object on an update) and,
// for a create, the object in the request:
//
//	GitOps writer, UI-owned object      denied, with an Event, unless a handoff is
//	                                    pending and unexpired
//	GitOps writer, anything else        allowed
//	UI field manager, Git-owned object  denied
//	human, UI-owned object              allowed, with a warning
//
// The webhook never changes ownership. The handoff controller does, after the
// GitOps tool has adopted the object.

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-ownership,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=agents;tools;mcpservers;knowledgebases;modelselectors;modelproviders;agentdeployments;agentworkflows;agentschedules,verbs=create;update,versions=v1alpha1,name=vownership.kb.io,admissionReviewVersions=v1

// Event reasons the webhook and the handoff controller record on an object.
const (
	EventReasonGitOpsBlocked = "GitOpsWriteBlocked"
	EventReasonUIBlocked     = "UIWriteBlocked"
)

// OwnershipValidator enforces the ownership rules.
type OwnershipValidator struct {
	Decoder admission.Decoder
	// Writers are the GitOps identities.
	Writers tenancy.GitOpsWriters
	// Detect says which labels count as Git tracking.
	Detect tenancy.DetectOptions
	// OperatorNamespace and OperatorServiceAccount identify the operator's own
	// writes. Without the service account name every service account in the
	// namespace counts.
	OperatorNamespace      string
	OperatorServiceAccount string
	// TenantConfigs names the tenant in a denial. Optional.
	TenantConfigs client.Reader
	// Recorder records the Event on the object. Optional.
	Recorder record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// ownershipVerdict is what the rules decide about one request.
type ownershipVerdict struct {
	// Deny is the denial message, "" when allowed.
	Deny string
	// Warning is an admission warning for an allowed request.
	Warning string
	// Reason is the Event reason to record on a denial.
	Reason string
}

type writeOptions struct {
	FieldManager string `json:"fieldManager"`
}

func (v *OwnershipValidator) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *OwnershipValidator) isOperator(user string) bool {
	if v.OperatorNamespace == "" {
		return false
	}
	if v.OperatorServiceAccount != "" {
		return user == "system:serviceaccount:"+v.OperatorNamespace+":"+v.OperatorServiceAccount
	}
	return strings.HasPrefix(user, "system:serviceaccount:"+v.OperatorNamespace+":")
}

// Handle implements admission.Handler.
func (v *OwnershipValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := tenancy.NewUIObject(req.Kind.Kind)
	if obj == nil {
		return admission.Allowed("not a UI-managed kind")
	}
	if err := v.Decoder.DecodeRaw(req.Object, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var old client.Object
	if req.Operation == admissionv1.Update && len(req.OldObject.Raw) > 0 {
		old = tenancy.NewUIObject(req.Kind.Kind)
		if err := v.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}
	var opts writeOptions
	if len(req.Options.Raw) > 0 {
		_ = json.Unmarshal(req.Options.Raw, &opts) // an unreadable option is no field manager
	}

	verdict := v.decide(ctx, req.Kind.Kind, req.UserInfo.Username, opts.FieldManager, old, obj)
	if verdict.Deny == "" {
		resp := admission.Allowed("ok")
		if verdict.Warning != "" {
			resp = resp.WithWarnings(verdict.Warning)
		}
		return resp
	}
	// There is an object to hang the Event on only when it exists already.
	if old != nil && v.Recorder != nil {
		v.Recorder.Event(old, corev1.EventTypeWarning, verdict.Reason, verdict.Deny)
	}
	return admission.Denied(verdict.Deny)
}

// decide applies the rules. old is nil on a create.
func (v *OwnershipValidator) decide(ctx context.Context, kind, user, fieldManager string, old, obj client.Object) ownershipVerdict {
	if !obj.GetDeletionTimestamp().IsZero() {
		return ownershipVerdict{}
	}
	stored := obj
	if old != nil {
		stored = old
	}
	have := tenancy.Detect(stored, v.Detect)
	label := fmt.Sprintf("%s %q in namespace %q", kind, stored.GetName(), stored.GetNamespace())

	if by, gitops := v.Writers.Match(user, fieldManager); gitops {
		if have.Owner != tenancy.OwnerUI {
			return ownershipVerdict{}
		}
		if old != nil && tenancy.HandoffOf(old).Active(v.now()) {
			return ownershipVerdict{} // adoption: the handoff controller flips ownership afterwards
		}
		return ownershipVerdict{
			Reason: EventReasonGitOpsBlocked,
			Deny: fmt.Sprintf("%s is owned by the agent-orca UI (tenant %s). A write from %s was blocked: "+
				"export it from the UI and mark it for handoff, or rename your manifest.", label, v.tenantName(ctx, stored), by),
		}
	}
	if fieldManager != tenancy.FieldManagerUI {
		if v.isOperator(user) {
			return ownershipVerdict{}
		}
		// Identity-independent rule: a GitOps tool stamps its tracking metadata on
		// every object it applies, so a write that gives a UI-owned object Git
		// tracking is a takeover whoever sends it, even from a tool nobody listed
		// in ownership.gitopsWriters. An active handoff lets it through.
		if old != nil && have.Owner == tenancy.OwnerUI && !tenancy.HandoffOf(old).Active(v.now()) {
			if in := tenancy.Detect(obj, v.Detect); in.Owner == tenancy.OwnerGit {
				if in.Tool == tenancy.ToolGit {
					return ownershipVerdict{
						Reason: EventReasonGitOpsBlocked,
						Deny: fmt.Sprintf("%s is owned by the agent-orca UI (tenant %s). The write marks it as owned by Git (%s: git) "+
							"without a handoff: use Export in the UI, which sets the handoff, then apply the exported manifest.",
							label, v.tenantName(ctx, stored), tenancy.AnnotationOwner),
					}
				}
				return ownershipVerdict{
					Reason: EventReasonGitOpsBlocked,
					Deny: fmt.Sprintf("%s is owned by the agent-orca UI (tenant %s). A write carrying %s tracking metadata was blocked: "+
						"export it from the UI and mark it for handoff, or rename your manifest.", label, v.tenantName(ctx, stored), describeGit(in)),
				}
			}
		}
		if old != nil && tenancy.HandoffOf(old).Active(v.now()) && tenancy.Detect(obj, v.Detect).Owner == tenancy.OwnerGit {
			return ownershipVerdict{} // an adoption by a writer nobody listed
		}
		if have.Owner == tenancy.OwnerUI {
			return ownershipVerdict{Warning: fmt.Sprintf("%s is owned by the agent-orca UI (tenant %s): "+
				"your change will show up there, and the next UI edit may overwrite fields you set.", label, v.tenantName(ctx, stored))}
		}
		return ownershipVerdict{}
	}
	// The UI field manager. Both sides are checked: it must not write an object
	// that is Git-owned, nor turn one into a Git-tracked object.
	for _, o := range []client.Object{stored, obj} {
		if g := tenancy.Detect(o, v.Detect); g.Owner == tenancy.OwnerGit {
			return ownershipVerdict{
				Reason: EventReasonUIBlocked,
				Deny: fmt.Sprintf("%s is owned by Git (%s); the agent-orca UI cannot change it. Change it in Git, or duplicate it in the UI.",
					label, describeGit(g)),
			}
		}
	}
	return ownershipVerdict{}
}

func describeGit(o tenancy.Ownership) string {
	switch {
	case o.Tool != "" && o.App != "":
		return o.Tool + " " + o.App
	case o.Tool != "":
		return o.Tool
	}
	return "Git"
}

// tenantName is the name of the tenant that owns obj, for a message. It falls
// back to the tenant ID, then to "unknown".
func (v *OwnershipValidator) tenantName(ctx context.Context, obj client.Object) string {
	id, _ := tenancy.TenantOf(obj)
	if id != "" && v.TenantConfigs != nil {
		var tcs agentorcav1alpha1.TenantConfigList
		if err := v.TenantConfigs.List(ctx, &tcs); err == nil {
			for i := range tcs.Items {
				if tenancy.TenantIDOf(&tcs.Items[i]) == id {
					return tcs.Items[i].Name
				}
			}
		}
	}
	if n := obj.GetLabels()[tenancy.LabelTenantName]; n != "" {
		return n
	}
	if id != "" {
		return id
	}
	return "unknown"
}

// OwnershipConfig is what SetupOwnershipWebhook needs.
type OwnershipConfig struct {
	Writers                tenancy.GitOpsWriters
	Detect                 tenancy.DetectOptions
	OperatorNamespace      string
	OperatorServiceAccount string
}

// SetupOwnershipWebhook registers the ownership validating webhook.
func SetupOwnershipWebhook(mgr ctrl.Manager, cfg OwnershipConfig) error {
	mgr.GetWebhookServer().Register(OwnershipPath, &admission.Webhook{Handler: &OwnershipValidator{
		Decoder:                admission.NewDecoder(mgr.GetScheme()),
		Writers:                cfg.Writers,
		Detect:                 cfg.Detect,
		OperatorNamespace:      cfg.OperatorNamespace,
		OperatorServiceAccount: cfg.OperatorServiceAccount,
		TenantConfigs:          mgr.GetClient(),
		Recorder:               mgr.GetEventRecorderFor("agent-orca-ownership"), //nolint:staticcheck // same recorder API as the controllers
	}})
	return nil
}
