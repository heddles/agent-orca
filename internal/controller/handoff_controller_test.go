package controller

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func TestHandoffReconciler(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(2*time.Hour), now.Add(-time.Hour)
	pending := func(exp time.Time, extra map[string]string) map[string]string {
		a := map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI, tenancy.AnnotationHandoff: "pending", tenancy.AnnotationHandoffExpires: exp.Format(time.RFC3339)}
		maps.Copy(a, extra)
		return a
	}
	argo := map[string]string{"argocd.argoproj.io/tracking-id": "shop:agentorca.agentorca.io/Agent:ns/a"}
	tests := []struct {
		name        string
		annotations map[string]string
		labels      map[string]string
		wantOwner   string // annotation value after
		wantHandoff bool
		wantEvent   string
		wantRequeue bool
	}{
		{name: "pending and not adopted waits for the expiry", annotations: pending(future, nil), wantOwner: tenancy.OwnerUI, wantHandoff: true, wantRequeue: true},
		{name: "adopted by Argo flips to Git", annotations: pending(future, argo), wantOwner: "", wantEvent: EventReasonHandoffCompleted},
		{name: "adopted by Flux flips to Git", annotations: pending(future, nil), labels: map[string]string{
			"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, wantOwner: "", wantEvent: EventReasonHandoffCompleted},
		{name: "adopted with the manifest's owner: git keeps it and drops the handoff", annotations: map[string]string{
			tenancy.AnnotationOwner: "git", tenancy.AnnotationHandoff: "pending", tenancy.AnnotationHandoffExpires: future.Format(time.RFC3339)},
			wantOwner: "git", wantEvent: EventReasonHandoffCompleted},
		{name: "adopted after the expiry still counts", annotations: pending(past, argo), wantOwner: "", wantEvent: EventReasonHandoffCompleted},
		{name: "expired and not adopted is cleared, ownership stays", annotations: pending(past, nil), wantOwner: tenancy.OwnerUI, wantEvent: EventReasonHandoffExpired},
		{name: "malformed handoff is cleared", annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI, tenancy.AnnotationHandoff: "pending"},
			wantOwner: tenancy.OwnerUI, wantEvent: EventReasonHandoffExpired},
		{name: "no handoff: nothing to do", annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}, wantOwner: tenancy.OwnerUI},
		{name: "a Git object with no handoff is left alone", annotations: argo, wantOwner: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			a := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", Annotations: tc.annotations, Labels: tc.labels}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a).Build()
			rec := record.NewFakeRecorder(5)
			r := &HandoffReconciler{Client: c, Kind: "Agent", Recorder: rec, Now: func() time.Time { return now }}
			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(a)})
			if err != nil {
				t.Fatal(err)
			}
			var got agentorcav1alpha1.Agent
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(a), &got); err != nil {
				t.Fatal(err)
			}
			if got.Annotations[tenancy.AnnotationOwner] != tc.wantOwner {
				t.Errorf("owner = %q, want %q", got.Annotations[tenancy.AnnotationOwner], tc.wantOwner)
			}
			if has := tenancy.HandoffOf(&got).Requested; has != tc.wantHandoff {
				t.Errorf("handoff annotations present = %v, want %v", has, tc.wantHandoff)
			}
			if (res.RequeueAfter > 0) != tc.wantRequeue {
				t.Errorf("requeue = %v, want %v", res.RequeueAfter, tc.wantRequeue)
			}
			if tc.wantRequeue && res.RequeueAfter < 2*time.Hour {
				t.Errorf("requeue %v is before the expiry", res.RequeueAfter)
			}
			select {
			case ev := <-rec.Events:
				if tc.wantEvent == "" || !strings.Contains(ev, tc.wantEvent) {
					t.Errorf("event %q, want %q", ev, tc.wantEvent)
				}
			default:
				if tc.wantEvent != "" {
					t.Errorf("no event, want %s", tc.wantEvent)
				}
			}
		})
	}
}
