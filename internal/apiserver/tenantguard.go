package apiserver

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// This file is the API side of tenant isolation. Every list, get, write and
// delete on every API surface goes through it: an object is visible only when
// its agentorca.io/tenant-id label matches the caller's tenant ID (or it is
// platform-shared, read-only) AND its namespace is one the caller may use. An
// object that fails either test is reported as not found, so existence does not
// leak. The name label agentorca.io/tenant is display only and decides nothing.

// callerOf converts an identity to the form the access check takes.
func callerOf(t *TenantIdentity) tenancy.Caller {
	return tenancy.Caller{TenantID: t.TenantID, TenantName: t.TenantName, Namespaces: tenantNamespaces(t)}
}

// accessTo decides what the caller in ctx may do with obj. Without a tenant
// identity (authentication disabled, or the UI proxy's service-account flow)
// there is no tenant to compare and the object is the caller's own, as before.
func accessTo(ctx context.Context, obj client.Object, kind string) tenancy.Access {
	t, ok := TenantFromContext(ctx)
	if !ok || t == nil {
		return tenancy.AccessOwn
	}
	return tenancy.CheckAccess(callerOf(t), obj, kind)
}

// canWrite reports whether the caller may read and change obj.
func canWrite(ctx context.Context, obj client.Object, kind string) bool {
	return accessTo(ctx, obj, kind) == tenancy.AccessOwn
}

// canRead reports whether the caller may read obj: its own, or platform-shared.
func canRead(ctx context.Context, obj client.Object, kind string) bool {
	return accessTo(ctx, obj, kind) != tenancy.AccessNone
}

// runVisibleTo is the check for a run named in a request: the tenant owns it.
func runVisibleTo(t *TenantIdentity, run client.Object) bool {
	return tenancy.CheckAccess(callerOf(t), run, "AgentRun") == tenancy.AccessOwn
}

// tenantOwnedListKind returns the kind of a list of tenant-owned objects, or
// "" for any other list (routing insights, for one, are not labelled).
func tenantOwnedListKind(list client.ObjectList) string {
	switch list.(type) {
	case *agentorcav1alpha1.AgentList:
		return "Agent"
	case *agentorcav1alpha1.ToolList:
		return "Tool"
	case *agentorcav1alpha1.MCPServerList:
		return "MCPServer"
	case *agentorcav1alpha1.KnowledgeBaseList:
		return "KnowledgeBase"
	case *agentorcav1alpha1.ModelSelectorList:
		return "ModelSelector"
	case *agentorcav1alpha1.ModelProviderList:
		return "ModelProvider"
	case *agentorcav1alpha1.AgentDeploymentList:
		return "AgentDeployment"
	case *agentorcav1alpha1.AgentWorkflowList:
		return kindAgentWorkflow
	case *agentorcav1alpha1.AgentRunList:
		return kindAgentRun
	case *agentorcav1alpha1.AgentScheduleList:
		return kindAgentSchedule
	}
	return ""
}

// filterReadable drops the items of a tenant-owned list that the caller may not
// read. Other lists are left alone.
func filterReadable(ctx context.Context, list client.ObjectList) error {
	t, ok := TenantFromContext(ctx)
	kind := tenantOwnedListKind(list)
	if !ok || t == nil || kind == "" {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	kept := make([]runtime.Object, 0, len(items))
	for _, it := range items {
		if obj, ok := it.(client.Object); ok && !canRead(ctx, obj, kind) {
			continue
		}
		kept = append(kept, it)
	}
	return meta.SetList(list, kept)
}

// stampOwner gives an object created on the caller's behalf the caller's tenant
// ID. Whatever ownership the object carried is dropped first, so a request body
// can never choose its tenant. With no tenant identity (authentication
// disabled) the object is left unstamped for the namespace binding to label.
func stampOwner(ctx context.Context, obj client.Object) {
	tenancy.StripOwnership(obj)
	if t, ok := TenantFromContext(ctx); ok && t != nil {
		tenancy.Stamp(obj, t.TenantID)
	}
}

// namespaceAllowed reports whether the caller may use ns. Without a tenant
// identity every namespace is allowed, as before.
func namespaceAllowed(ctx context.Context, ns string) bool {
	t, ok := TenantFromContext(ctx)
	if !ok || t == nil {
		return true
	}
	return slices.Contains(tenantNamespaces(t), ns)
}
