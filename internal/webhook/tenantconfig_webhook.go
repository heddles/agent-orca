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
	"fmt"
	"net/http"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// TenantConfigValidator enforces tenant identity rules the CRD schema cannot:
// IDs are immutable and never silently shared, restoring a deleted tenant is an
// admin act, and a namespace belongs to one tenant unless marked shared.
type TenantConfigValidator struct {
	Decoder admission.Decoder
	// Reader reads TenantConfigs and Namespaces. Use the manager's API reader so
	// the check sees committed state and starts no informers.
	Reader client.Reader
	// Registry tells a deleted ID from one never used. Nil when Postgres is not
	// configured: every pinned ID is then treated as fresh, so deployments that
	// want restore protection need the registry.
	Registry tenancy.Registry
	// AdminGroups are the groups whose members may restore a deleted tenant ID.
	// Empty means nobody may.
	AdminGroups []string
}

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-tenantconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=tenantconfigs,verbs=create;update,versions=v1alpha1,name=vtenantconfig.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *TenantConfigValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var tc agentorcav1alpha1.TenantConfig
	if err := v.Decoder.DecodeRaw(req.Object, &tc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var old *agentorcav1alpha1.TenantConfig
	if req.Operation == admissionv1.Update {
		old = &agentorcav1alpha1.TenantConfig{}
		if err := v.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}
	if err := v.validate(ctx, &tc, old, req.UserInfo.Groups); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("ok")
}

func (v *TenantConfigValidator) validate(ctx context.Context, tc, old *agentorcav1alpha1.TenantConfig, groups []string) error {
	// A TenantConfig on its way out is being cleaned up (finalizer removal); do
	// not block that on rules about its future.
	if !tc.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := v.validateTenantID(ctx, tc, old, groups); err != nil {
		return err
	}
	return v.validateNamespaces(ctx, tc, old)
}

func (v *TenantConfigValidator) validateTenantID(ctx context.Context, tc, old *agentorcav1alpha1.TenantConfig, groups []string) error {
	id := tc.Spec.TenantID
	if old != nil && old.Spec.TenantID != "" && id != old.Spec.TenantID {
		return fmt.Errorf("spec.tenantID is immutable once set (was %q)", old.Spec.TenantID)
	}
	if id == "" {
		return nil
	}
	if !tenancy.ValidID(id) {
		return fmt.Errorf("spec.tenantID %q is not a tenant ID: want %q plus a UUIDv7", id, tenancy.IDPrefix)
	}
	// Pinning the ID a TenantConfig already has is how GitOps adopts a generated
	// one; any other first pin would rebind a live tenant to different data.
	if old != nil && old.Spec.TenantID == "" && old.Status.TenantID != "" && id != old.Status.TenantID {
		return fmt.Errorf("spec.tenantID %q differs from the tenant's current ID %q; a live tenant cannot be rebound", id, old.Status.TenantID)
	}

	var list agentorcav1alpha1.TenantConfigList
	if err := v.Reader.List(ctx, &list); err != nil {
		return fmt.Errorf("listing TenantConfigs: %w", err)
	}
	for i := range list.Items {
		other := &list.Items[i]
		if sameTenantConfig(other, tc) || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.TenantID == id || other.Status.TenantID == id {
			return fmt.Errorf("spec.tenantID %q is bound to TenantConfig %s/%s", id, other.Namespace, other.Name)
		}
	}

	if v.Registry == nil {
		return nil
	}
	rec, err := v.Registry.GetTenant(ctx, id)
	if err != nil {
		return fmt.Errorf("checking the tenant registry: %w", err)
	}
	if rec != nil && rec.State == tenancy.StateDeleted && !anyIn(groups, v.AdminGroups) {
		return fmt.Errorf("spec.tenantID %q belongs to a deleted tenant; restoring it exposes that tenant's data and needs a member of an admin group (%v)", id, v.AdminGroups)
	}
	return nil
}

// validateNamespaces rejects a namespace another TenantConfig already claims,
// unless the Namespace is marked shared. On update only namespaces added by
// this change are checked, so a setup that predates the rule can still be
// edited while it is migrated.
func (v *TenantConfigValidator) validateNamespaces(ctx context.Context, tc, old *agentorcav1alpha1.TenantConfig) error {
	claim := tc.Spec.AllowedNamespaces
	if old != nil {
		claim = slices.DeleteFunc(slices.Clone(claim), func(ns string) bool {
			return slices.Contains(old.Spec.AllowedNamespaces, ns)
		})
	}
	if len(claim) == 0 {
		return nil
	}
	var list agentorcav1alpha1.TenantConfigList
	if err := v.Reader.List(ctx, &list); err != nil {
		return fmt.Errorf("listing TenantConfigs: %w", err)
	}
	for _, ns := range claim {
		owner := ""
		for i := range list.Items {
			other := &list.Items[i]
			if !sameTenantConfig(other, tc) && other.DeletionTimestamp.IsZero() && slices.Contains(other.Spec.AllowedNamespaces, ns) {
				owner = other.Namespace + "/" + other.Name
				break
			}
		}
		if owner == "" {
			continue
		}
		shared, err := v.namespaceShared(ctx, ns)
		if err != nil {
			return err
		}
		if !shared {
			return fmt.Errorf("namespace %q is already claimed by TenantConfig %s; a platform admin can label the Namespace %s=%s to share it",
				ns, owner, tenancy.NamespaceModeLabel, tenancy.NamespaceModeShared)
		}
	}
	return nil
}

func (v *TenantConfigValidator) namespaceShared(ctx context.Context, name string) (bool, error) {
	var ns corev1.Namespace
	if err := v.Reader.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading Namespace %s: %w", name, err)
	}
	return ns.Labels[tenancy.NamespaceModeLabel] == tenancy.NamespaceModeShared, nil
}

func sameTenantConfig(a, b *agentorcav1alpha1.TenantConfig) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name
}

func anyIn(have, want []string) bool {
	return slices.ContainsFunc(have, func(g string) bool { return slices.Contains(want, g) })
}

// SetupTenantConfigWebhook registers the TenantConfig validating webhook.
func SetupTenantConfigWebhook(mgr ctrl.Manager, registry tenancy.Registry, adminGroups []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorca-agentorca-io-v1alpha1-tenantconfig",
		&admission.Webhook{Handler: &TenantConfigValidator{
			Decoder: decoder, Reader: mgr.GetAPIReader(), Registry: registry, AdminGroups: adminGroups,
		}},
	)
	return nil
}
