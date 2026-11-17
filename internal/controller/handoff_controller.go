package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/heddles/agent-orca/internal/tenancy"
)

// Event reasons the handoff controller records.
const (
	EventReasonHandoffCompleted = "HandoffCompleted"
	EventReasonHandoffExpired   = "HandoffExpired"
)

// HandoffReconciler finishes and ends handoffs of UI-owned objects to Git.
//
// A builder exports an object from the UI, which marks it agentorca.io/handoff:
// pending until an expiry. While that is unexpired the ownership webhook lets a
// GitOps writer update the object; the webhook never changes ownership. This
// controller does:
//
//   - once the object carries Git tracking metadata (the GitOps tool adopted
//     it), it removes agentorca.io/owner: ui and both handoff annotations and
//     records an Event. The UI then shows the object as Git-owned;
//   - once the handoff has expired (or its annotations are malformed) without
//     adoption, it removes the handoff annotations, so GitOps writes are
//     blocked again, and records an Event.
//
// One reconciler is registered per kind the UI API writes.
//
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agents;tools;mcpservers;modelproviders;knowledgebases;modelselectors;agentdeployments;agentworkflows,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type HandoffReconciler struct {
	client.Client
	// Kind is the Kind name the reconciler watches.
	Kind     string
	Detect   tenancy.DetectOptions
	Recorder record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (r *HandoffReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile implements reconcile.Reconciler.
func (r *HandoffReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := tenancy.NewUIObject(r.Kind)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	h := tenancy.HandoffOf(obj)
	if !h.Requested {
		return ctrl.Result{}, nil
	}
	now := r.now()
	det := tenancy.Detect(obj, r.Detect)

	var reason, message string
	switch {
	case det.Owner == tenancy.OwnerGit:
		reason = EventReasonHandoffCompleted
		message = "handed off: now owned by Git"
		if det.Tool != "" && det.App != "" {
			message = fmt.Sprintf("handed off: now managed by %s %s", det.Tool, det.App)
		}
	case !h.Active(now):
		reason = EventReasonHandoffExpired
		message = "the handoff to Git expired before a GitOps tool adopted this object; the UI still owns it"
	default:
		return ctrl.Result{RequeueAfter: h.Expires.Sub(now) + time.Second}, nil
	}

	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object)) //nolint:forcetypeassert // DeepCopyObject of a client.Object
	tenancy.ClearHandoff(obj)
	if reason == EventReasonHandoffCompleted {
		if a := obj.GetAnnotations(); a[tenancy.AnnotationOwner] == tenancy.OwnerUI {
			delete(a, tenancy.AnnotationOwner)
			obj.SetAnnotations(a)
		}
	}
	if err := r.Patch(ctx, obj, patch); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(obj, corev1.EventTypeNormal, reason, message)
	}
	return ctrl.Result{}, nil
}

// SetupHandoffControllers registers a HandoffReconciler for each kind the UI
// API writes.
func SetupHandoffControllers(mgr ctrl.Manager, detect tenancy.DetectOptions) error {
	hasHandoff := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return tenancy.HandoffOf(o).Requested
	})
	for kind, newObj := range tenancy.UIKinds {
		r := &HandoffReconciler{
			Client:   mgr.GetClient(),
			Kind:     kind,
			Detect:   detect,
			Recorder: mgr.GetEventRecorderFor("agent-orca-handoff"), //nolint:staticcheck // same recorder API as the other controllers
		}
		if err := ctrl.NewControllerManagedBy(mgr).
			Named("handoff-"+strings.ToLower(kind)).
			For(newObj(), builder.WithPredicates(hasHandoff)).
			Complete(r); err != nil {
			return fmt.Errorf("setting up the %s handoff controller: %w", kind, err)
		}
	}
	return nil
}
