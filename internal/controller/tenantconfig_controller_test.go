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

package controller

import (
	"context"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	pinnedID  = "tnt_01935c7e-1111-7aaa-8bbb-000000000001"
	tenantKey = "acme"
	tenantNS  = "tenants"
)

func tenantConfigFor(uid string, mutate func(*agentorcav1alpha1.TenantConfig)) *agentorcav1alpha1.TenantConfig {
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: tenantKey, Namespace: tenantNS, UID: types.UID(uid)},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode:          "issued",
			AllowedNamespaces: []string{"acme-ns"},
			Issued: &agentorcav1alpha1.IssuedAuthConfig{
				ClientID:        "acme-client",
				ClientSecretRef: agentorcav1alpha1.SecretKeyRef{Name: "acme-secret", Key: "secret"},
			},
		},
	}
	if mutate != nil {
		mutate(tc)
	}
	return tc
}

func newTenantReconciler(t *testing.T, reg tenancy.Registry, objs ...client.Object) (*TenantConfigReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agentorcav1alpha1.TenantConfig{}).WithObjects(objs...).Build()
	return &TenantConfigReconciler{Client: c, Scheme: scheme, Registry: reg, OperatorNamespace: "agent-orca-system"}, c
}

func reconcileTenant(t *testing.T, r *TenantConfigReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tenantKey, Namespace: tenantNS}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getTenant(t *testing.T, c client.Client) *agentorcav1alpha1.TenantConfig {
	t.Helper()
	var tc agentorcav1alpha1.TenantConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: tenantKey, Namespace: tenantNS}, &tc); err != nil {
		t.Fatal(err)
	}
	return &tc
}

func TestTenantConfigAssignsAndMirrorsID(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(*agentorcav1alpha1.TenantConfig)
		wantID string // empty: any generated ID
	}{
		{name: "generated at creation"},
		{name: "pinned", mutate: func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.TenantID = pinnedID }, wantID: pinnedID},
		{name: "existing TenantConfig is migrated", mutate: func(tc *agentorcav1alpha1.TenantConfig) { tc.Status.Ready = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := tenancy.NewMemoryRegistry()
			r, c := newTenantReconciler(t, reg, tenantConfigFor("uid-1", tt.mutate))
			reconcileTenant(t, r)

			tc := getTenant(t, c)
			id := tc.Status.TenantID
			if !tenancy.ValidID(id) || (tt.wantID != "" && id != tt.wantID) {
				t.Fatalf("status.tenantID = %q, want valid (pinned %q)", id, tt.wantID)
			}
			rec, _ := reg.GetTenant(ctx, id)
			if rec == nil || rec.State != tenancy.StateActive || rec.UID != "uid-1" || rec.Name != tenantKey || rec.Namespace != tenantNS {
				t.Errorf("registry record = %+v", rec)
			}
			if !tc.Status.Ready {
				t.Errorf("tenant not ready: %s", tc.Status.Message)
			}

			reconcileTenant(t, r)
			if got := getTenant(t, c).Status.TenantID; got != id {
				t.Errorf("ID changed on second reconcile: %q -> %q", id, got)
			}
		})
	}
}

func TestTenantConfigRecreateGetsNewID(t *testing.T) {
	ctx := context.Background()
	reg := tenancy.NewMemoryRegistry()
	r, c := newTenantReconciler(t, reg, tenantConfigFor("uid-1", nil))
	reconcileTenant(t, r)
	oldID := getTenant(t, c).Status.TenantID

	if err := c.Delete(ctx, getTenant(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r) // finalizer records the deletion
	if err := c.Get(ctx, types.NamespacedName{Name: tenantKey, Namespace: tenantNS}, &agentorcav1alpha1.TenantConfig{}); !apierrors.IsNotFound(err) {
		t.Fatalf("TenantConfig still present after finalize: %v", err)
	}
	if rec, _ := reg.GetTenant(ctx, oldID); rec == nil || rec.State != tenancy.StateDeleted || rec.DeletedAt == nil {
		t.Fatalf("old record = %+v, want deleted", rec)
	}

	if err := c.Create(ctx, tenantConfigFor("uid-2", nil)); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	newID := getTenant(t, c).Status.TenantID
	if newID == "" || newID == oldID {
		t.Fatalf("recreated tenant ID = %q, want a new ID (old %q)", newID, oldID)
	}
	if rec, _ := reg.GetTenant(ctx, oldID); rec.State != tenancy.StateDeleted {
		t.Errorf("old ID revived: %+v", rec)
	}
}

func TestTenantConfigRestoreByPinning(t *testing.T) {
	ctx := context.Background()
	reg := tenancy.NewMemoryRegistry()
	now := time.Now()
	_ = reg.PutTenant(ctx, tenancy.Record{ID: pinnedID, Name: tenantKey, Namespace: tenantNS, UID: "uid-old", State: tenancy.StateDeleted, DeletedAt: &now})

	r, c := newTenantReconciler(t, reg, tenantConfigFor("uid-2", func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.TenantID = pinnedID }))
	reconcileTenant(t, r)

	if got := getTenant(t, c).Status.TenantID; got != pinnedID {
		t.Errorf("status.tenantID = %q, want the restored %q", got, pinnedID)
	}
	if rec, _ := reg.GetTenant(ctx, pinnedID); rec.State != tenancy.StateActive || rec.UID != "uid-2" || rec.DeletedAt != nil {
		t.Errorf("record = %+v, want active and owned by the new UID", rec)
	}
}

func TestTenantConfigPinnedIDInUseIsNotReady(t *testing.T) {
	ctx := context.Background()
	reg := tenancy.NewMemoryRegistry()
	_ = reg.PutTenant(ctx, tenancy.Record{ID: pinnedID, Name: "other", Namespace: tenantNS, UID: "uid-other", State: tenancy.StateActive})

	r, c := newTenantReconciler(t, reg, tenantConfigFor("uid-2", func(tc *agentorcav1alpha1.TenantConfig) { tc.Spec.TenantID = pinnedID }))
	reconcileTenant(t, r)

	tc := getTenant(t, c)
	if tc.Status.Ready || tc.Status.TenantID != "" {
		t.Errorf("status = %+v, want not ready and no ID", tc.Status)
	}
}

func TestTenantConfigWithoutRegistryStillGetsID(t *testing.T) {
	r, c := newTenantReconciler(t, nil, tenantConfigFor("uid-1", nil))
	reconcileTenant(t, r)
	id := getTenant(t, c).Status.TenantID
	if !tenancy.ValidID(id) {
		t.Fatalf("status.tenantID = %q", id)
	}
	reconcileTenant(t, r)
	if got := getTenant(t, c).Status.TenantID; got != id {
		t.Errorf("ID changed: %q -> %q", id, got)
	}
	if len(getTenant(t, c).Finalizers) != 0 {
		t.Error("finalizer added without a registry")
	}
}
