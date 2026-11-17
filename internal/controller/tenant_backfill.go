package controller

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// TenantBackfill is a one-shot pass, run when the operator starts, that gives
// objects created before tenant IDs their agentorca.io/tenant-id label. A
// tenant-owned object takes the ID of the tenant its old agentorca.io/tenant
// name label points at (when that tenant claims the namespace), else of the
// single tenant that claims its namespace. Children that controllers created
// earlier (pods, Secrets, ConfigMaps, Services, PVCs, Jobs, StatefulSets) take
// the ID of the parent that owns them. An object it cannot attribute is logged
// and counted, and left alone.
//
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;patch
//
// It is safe to run on every start: it only touches objects without the label.
type TenantBackfill struct {
	// Client reads and patches. Reads of Secrets and the other child kinds go
	// through it, so pass a client that does not start informers for them (the
	// manager's API reader wrapped in a writer) in production.
	Client client.Client
}

// BackfillStats is what one pass did.
type BackfillStats struct {
	Stamped      int
	Unattributed int
}

// NeedLeaderElection makes only the leader run the pass.
func (b *TenantBackfill) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (b *TenantBackfill) Start(ctx context.Context) error {
	stats, err := b.Run(ctx)
	if err != nil {
		// A failed pass is retried on the next start; it must not stop the operator.
		slog.Error("tenant label backfill failed", "err", err)
		return nil
	}
	slog.Info("tenant label backfill done", "stamped", stats.Stamped, "unattributed", stats.Unattributed)
	return nil
}

// tenantOwnedLists are the kinds API users and GitOps create.
func tenantOwnedLists() []client.ObjectList {
	return []client.ObjectList{
		&agentorcav1alpha1.AgentList{},
		&agentorcav1alpha1.ToolList{},
		&agentorcav1alpha1.MCPServerList{},
		&agentorcav1alpha1.KnowledgeBaseList{},
		&agentorcav1alpha1.ModelSelectorList{},
		&agentorcav1alpha1.ModelProviderList{},
		&agentorcav1alpha1.AgentDeploymentList{},
		&agentorcav1alpha1.AgentWorkflowList{},
		&agentorcav1alpha1.AgentRunList{},
	}
}

// childLists are the kinds controllers create, parents before their children
// (a Pod is owned by a Job or StatefulSet that is itself owned by a CR).
func childLists() []client.ObjectList {
	return []client.ObjectList{
		&batchv1.JobList{},
		&appsv1.StatefulSetList{},
		&appsv1.DeploymentList{},
		&corev1.PodList{},
		&corev1.SecretList{},
		&corev1.ConfigMapList{},
		&corev1.ServiceList{},
		&corev1.PersistentVolumeClaimList{},
	}
}

// Run does one pass.
func (b *TenantBackfill) Run(ctx context.Context) (BackfillStats, error) {
	var stats BackfillStats
	var tcs agentorcav1alpha1.TenantConfigList
	if err := b.Client.List(ctx, &tcs); err != nil {
		return stats, fmt.Errorf("listing TenantConfigs: %w", err)
	}
	idByName := map[string]*agentorcav1alpha1.TenantConfig{}
	claimedNamespaces := map[string]struct{}{}
	for i := range tcs.Items {
		tc := &tcs.Items[i]
		if !tc.DeletionTimestamp.IsZero() {
			continue
		}
		idByName[tc.Name] = tc
		for _, ns := range tc.Spec.AllowedNamespaces {
			claimedNamespaces[ns] = struct{}{}
		}
	}

	// ownerTenant records the tenant of each stamped or already-labelled owner,
	// by UID, for the children pass.
	ownerTenant := map[types.UID]string{}

	for _, list := range tenantOwnedLists() {
		if err := b.List(ctx, list, nil); err != nil {
			return stats, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return stats, err
		}
		for _, it := range items {
			obj, ok := it.(client.Object)
			if !ok {
				continue
			}
			id, platform := tenancy.TenantOf(obj)
			if id == "" && !platform {
				id = attribute(tcs.Items, idByName, obj)
				if id == "" {
					stats.Unattributed++
					tenancy.Violation(tenancy.RuleBackfillUnknown, fmt.Sprintf("%T", obj), obj.GetNamespace(), obj.GetName(),
						"no single tenant claims the namespace and no usable "+tenancy.LabelTenantName+" label: label it by hand")
					continue
				}
				if err := b.stamp(ctx, obj, id); err != nil {
					return stats, err
				}
				stats.Stamped++
			}
			if id != "" {
				ownerTenant[obj.GetUID()] = id
			}
		}
	}

	// Children, only in namespaces a tenant claims: elsewhere there is no tenant
	// to give them.
	namespaces := make([]string, 0, len(claimedNamespaces))
	for ns := range claimedNamespaces {
		namespaces = append(namespaces, ns)
	}
	slices.Sort(namespaces)
	for _, proto := range childLists() {
		for _, ns := range namespaces {
			list := proto.DeepCopyObject().(client.ObjectList)
			if err := b.List(ctx, list, []client.ListOption{client.InNamespace(ns)}); err != nil {
				// Children are best effort: a kind the operator may not list is
				// skipped, not a reason to abandon the rest.
				slog.Warn("tenant label backfill: skipping child kind", "namespace", ns, "err", err)
				continue
			}
			items, err := meta.ExtractList(list)
			if err != nil {
				return stats, err
			}
			for _, it := range items {
				if obj, ok := it.(client.Object); ok {
					b.backfillChild(ctx, obj, ownerTenant, &stats)
				}
			}
		}
	}
	return stats, nil
}

// backfillChild labels one child object: operator-shared objects as
// platform-shared, the rest from their controller owner. A failure is logged and
// skipped; children are best effort.
func (b *TenantBackfill) backfillChild(ctx context.Context, obj client.Object, ownerTenant map[types.UID]string, stats *BackfillStats) {
	id, platform := tenancy.TenantOf(obj)
	if id == "" && !platform && isOperatorSharedObject(obj) {
		// Operator-managed objects that every tenant of the namespace use are
		// platform-shared, so references to them stay valid in enforce mode.
		if err := b.stampPlatform(ctx, obj); err != nil {
			slog.Warn("tenant label backfill: labelling shared object failed", "err", err)
			return
		}
		stats.Stamped++
		return
	}
	if id == "" && !platform {
		ref := metaOwner(obj)
		if ref == "" {
			return
		}
		if id = ownerTenant[ref]; id == "" {
			return
		}
		if err := b.stamp(ctx, obj, id); err != nil {
			slog.Warn("tenant label backfill: labelling child failed", "err", err)
			return
		}
		stats.Stamped++
	}
	if id != "" {
		ownerTenant[obj.GetUID()] = id
	}
}

// List lists into list, optionally with options.
func (b *TenantBackfill) List(ctx context.Context, list client.ObjectList, opts []client.ListOption) error {
	if err := b.Client.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("listing %T: %w", list, err)
	}
	return nil
}

func (b *TenantBackfill) stamp(ctx context.Context, obj client.Object, id string) error {
	base := obj.DeepCopyObject().(client.Object)
	tenancy.Stamp(obj, id)
	if err := b.Client.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("labelling %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// isOperatorSharedObject reports whether obj is one of the per-namespace objects
// the operator creates for all tenants of the namespace.
func isOperatorSharedObject(obj client.Object) bool {
	_, isSecret := obj.(*corev1.Secret)
	return isSecret && obj.GetName() == hindsightKeySecretName
}

func (b *TenantBackfill) stampPlatform(ctx context.Context, obj client.Object) error {
	base := obj.DeepCopyObject().(client.Object)
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[tenancy.LabelScope] = tenancy.ScopePlatform
	obj.SetLabels(l)
	if err := b.Client.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("labelling %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// metaOwner returns the UID of obj's controller owner, or "".
func metaOwner(obj client.Object) types.UID {
	for _, r := range obj.GetOwnerReferences() {
		if r.Controller != nil && *r.Controller {
			return r.UID
		}
	}
	return ""
}

// attribute decides the tenant of an unlabelled object. The old name label wins
// when that tenant claims the object's namespace; otherwise the namespace must
// have exactly one claimant with an ID.
func attribute(tcs []agentorcav1alpha1.TenantConfig, byName map[string]*agentorcav1alpha1.TenantConfig, obj client.Object) string {
	ns := obj.GetNamespace()
	if name := obj.GetLabels()[tenancy.LabelTenantName]; name != "" {
		if tc, ok := byName[name]; ok && slices.Contains(tc.Spec.AllowedNamespaces, ns) {
			if id := tenancy.TenantIDOf(tc); id != "" {
				return id
			}
		}
	}
	if ids, _ := tenancy.NamespaceClaimants(tcs, ns); len(ids) == 1 {
		return ids[0]
	}
	return ""
}

// readerClient lists through a reader and writes through a client, so the
// backfill can list Secrets and Pods without the manager starting a cluster-wide
// informer for them.
type readerClient struct {
	client.Client
	reader client.Reader
}

func (c readerClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

// NewBackfillClient returns a client that reads with reader and writes with c.
func NewBackfillClient(c client.Client, reader client.Reader) client.Client {
	return readerClient{Client: c, reader: reader}
}
