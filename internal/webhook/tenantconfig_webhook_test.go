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

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	idLive    = "tnt_01935c7e-1111-7aaa-8bbb-000000000001"
	idDeleted = "tnt_01935c7e-2222-7aaa-8bbb-000000000002"
	idFresh   = "tnt_01935c7e-3333-7aaa-8bbb-000000000003"
	adminGrp  = "platform-admins"
)

func tc(name, ns string, mutate func(*agentorcav1alpha1.TenantConfig)) *agentorcav1alpha1.TenantConfig {
	t := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode:          "issued",
			AllowedNamespaces: []string{name + "-ns"},
		},
	}
	if mutate != nil {
		mutate(t)
	}
	return t
}

func TestTenantConfigValidate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	pin := func(id string) func(*agentorcav1alpha1.TenantConfig) {
		return func(t *agentorcav1alpha1.TenantConfig) { t.Spec.TenantID = id }
	}
	claimNS := func(ns ...string) func(*agentorcav1alpha1.TenantConfig) {
		return func(t *agentorcav1alpha1.TenantConfig) { t.Spec.AllowedNamespaces = ns }
	}
	sharedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared-ns", Labels: map[string]string{tenancy.NamespaceModeLabel: tenancy.NamespaceModeShared}}}
	privateNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "contested-ns"}}
	existing := tc("existing", "tenants", func(t *agentorcav1alpha1.TenantConfig) {
		t.Spec.TenantID = idLive
		t.Status.TenantID = idLive
		t.Spec.AllowedNamespaces = []string{"contested-ns", "shared-ns"}
	})
	gone := tc("gone", "tenants", func(t *agentorcav1alpha1.TenantConfig) {
		t.Status.TenantID = idFresh
		t.DeletionTimestamp = &metav1.Time{Time: now}
		t.Finalizers = []string{"test"}
	})

	tests := []struct {
		name    string
		obj     *agentorcav1alpha1.TenantConfig
		old     *agentorcav1alpha1.TenantConfig
		groups  []string
		noReg   bool
		wantErr string // substring; empty means allowed
	}{
		// spec.tenantID format
		{name: "no ID is fine", obj: tc("new", "tenants", nil)},
		{name: "well-formed pinned ID", obj: tc("new", "tenants", pin(idFresh))},
		{name: "malformed ID", obj: tc("new", "tenants", pin("acme")), wantErr: "is not a tenant ID"},
		{name: "UUIDv4 is not an ID", obj: tc("new", "tenants", pin("tnt_01935c7e-1111-4aaa-8bbb-000000000001")), wantErr: "is not a tenant ID"},
		{name: "missing prefix", obj: tc("new", "tenants", pin("01935c7e-1111-7aaa-8bbb-000000000001")), wantErr: "is not a tenant ID"},

		// immutability
		{name: "unchanged ID on update", obj: tc("new", "tenants", pin(idFresh)), old: tc("new", "tenants", pin(idFresh))},
		{name: "changed ID on update", obj: tc("new", "tenants", pin(idLive+"x")), old: tc("new", "tenants", pin(idFresh)), wantErr: "immutable"},
		{name: "ID removed on update", obj: tc("new", "tenants", nil), old: tc("new", "tenants", pin(idFresh)), wantErr: "immutable"},
		{
			name: "first pin adopting the generated ID",
			obj:  tc("new", "tenants", pin(idFresh)),
			old:  tc("new", "tenants", func(t *agentorcav1alpha1.TenantConfig) { t.Status.TenantID = idFresh }),
		},
		{
			name: "first pin of a different ID would rebind a live tenant",
			obj:  tc("new", "tenants", pin(idFresh)),
			old:  tc("new", "tenants", func(t *agentorcav1alpha1.TenantConfig) { t.Status.TenantID = idLive + "x" }),

			wantErr: "cannot be rebound",
		},

		// bound to another live TenantConfig
		{name: "ID pinned by another live TenantConfig (spec)", obj: tc("new", "tenants", pin(idLive)), wantErr: "is bound to TenantConfig tenants/existing"},
		{name: "ID pinned by another TenantConfig in another namespace", obj: tc("new", "other", pin(idLive)), wantErr: "is bound to TenantConfig tenants/existing"},
		{name: "ID the TenantConfig already owns", obj: existing.DeepCopy(), old: existing.DeepCopy()},
		{name: "ID of a TenantConfig being deleted may be taken", obj: tc("new", "tenants", pin(idFresh))},

		// restoring a deleted ID
		{name: "restore a deleted ID as a non-admin", obj: tc("new", "tenants", pin(idDeleted)), wantErr: "belongs to a deleted tenant"},
		{name: "restore a deleted ID as another group", obj: tc("new", "tenants", pin(idDeleted)), groups: []string{"developers"}, wantErr: "belongs to a deleted tenant"},
		{name: "restore a deleted ID as an admin", obj: tc("new", "tenants", pin(idDeleted)), groups: []string{"developers", adminGrp}},
		{name: "unknown ID needs no admin", obj: tc("new", "tenants", pin(idFresh)), groups: nil},
		{name: "without a registry every pin is fresh", obj: tc("new", "tenants", pin(idDeleted)), noReg: true},

		// namespace claims
		{name: "unclaimed namespace", obj: tc("new", "tenants", claimNS("free-ns"))},
		{name: "namespace claimed by another tenant", obj: tc("new", "tenants", claimNS("contested-ns")), wantErr: `namespace "contested-ns" is already claimed by TenantConfig tenants/existing`},
		{name: "claimed namespace marked shared", obj: tc("new", "tenants", claimNS("shared-ns"))},
		{name: "one of several namespaces contested", obj: tc("new", "tenants", claimNS("free-ns", "contested-ns")), wantErr: "contested-ns"},
		{name: "namespace claimed by a tenant being deleted", obj: tc("new", "tenants", claimNS("gone-ns")), old: nil},
		{name: "re-applying the existing tenant keeps its namespaces", obj: existing.DeepCopy(), old: existing.DeepCopy()},
		{
			name: "update that adds a contested namespace",
			obj:  tc("new", "tenants", claimNS("free-ns", "contested-ns")),
			old:  tc("new", "tenants", claimNS("free-ns")),

			wantErr: "contested-ns",
		},
		{
			name: "update that keeps a namespace shared before the rule existed",
			obj: tc("new", "tenants", func(t *agentorcav1alpha1.TenantConfig) {
				t.Spec.AllowedNamespaces = []string{"contested-ns"}
				t.Spec.BudgetPerDayUSD = "5"
			}),
			old: tc("new", "tenants", claimNS("contested-ns")),
		},
		{name: "a TenantConfig being deleted is not validated", obj: tc("new", "tenants", func(t *agentorcav1alpha1.TenantConfig) {
			t.Spec.TenantID = "garbage"
			t.DeletionTimestamp = &metav1.Time{Time: now}
			t.Finalizers = []string{"test"}
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing.DeepCopy(), gone.DeepCopy(), sharedNS.DeepCopy(), privateNS.DeepCopy()).Build()
			reg := tenancy.NewMemoryRegistry()
			_ = reg.PutTenant(context.Background(), tenancy.Record{ID: idDeleted, Name: "old", Namespace: "tenants", State: tenancy.StateDeleted, DeletedAt: &now})
			_ = reg.PutTenant(context.Background(), tenancy.Record{ID: idLive, Name: "existing", Namespace: "tenants", State: tenancy.StateActive})

			v := &TenantConfigValidator{Reader: c, AdminGroups: []string{adminGrp}}
			if !tt.noReg {
				v.Registry = reg
			}
			err := v.validate(context.Background(), tt.obj, tt.old, tt.groups)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("denied: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("allowed, want error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestTenantConfigRestoreWithNoAdminGroupConfigured(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	reg := tenancy.NewMemoryRegistry()
	now := time.Now()
	_ = reg.PutTenant(context.Background(), tenancy.Record{ID: idDeleted, Name: "old", State: tenancy.StateDeleted, DeletedAt: &now})
	var c client.Reader = fake.NewClientBuilder().WithScheme(scheme).Build()

	v := &TenantConfigValidator{Reader: c, Registry: reg}
	obj := tc("new", "tenants", func(t *agentorcav1alpha1.TenantConfig) { t.Spec.TenantID = idDeleted })
	if err := v.validate(context.Background(), obj, nil, []string{adminGrp, "system:masters"}); err == nil {
		t.Error("restore allowed with no admin group configured")
	}
}
