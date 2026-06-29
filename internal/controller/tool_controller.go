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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

// ToolReconciler reconciles a Tool object.
type ToolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=tools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=tools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=tools/finalizers,verbs=update

func (r *ToolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tool agentorcv1alpha1.Tool
	if err := r.Get(ctx, req.NamespacedName, &tool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ready, message := r.validate(ctx, &tool)

	patch := client.MergeFrom(tool.DeepCopy())
	tool.Status.Ready = ready
	tool.Status.Message = message

	now := metav1.Now()
	condStatus := metav1.ConditionTrue
	if !ready {
		condStatus = metav1.ConditionFalse
	}
	setCondition(&tool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             "Validated",
		Message:            message,
		LastTransitionTime: now,
	})

	if err := r.Status().Patch(ctx, &tool, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching Tool status: %w", err)
	}
	return ctrl.Result{}, nil
}

// validate checks the Tool spec based on its type.
// Full OCI signature verification is performed at AgentRun creation time.
func (r *ToolReconciler) validate(_ context.Context, tool *agentorcv1alpha1.Tool) (bool, string) {
	switch tool.Spec.Type {
	case agentorcv1alpha1.ToolTypeAgent, "":
		if tool.Spec.Type == agentorcv1alpha1.ToolTypeAgent && tool.Spec.AgentRef == "" {
			return false, "spec.agentRef is required for type=agent"
		}
		fallthrough
	case agentorcv1alpha1.ToolTypeRegular:
		if tool.Spec.Type != agentorcv1alpha1.ToolTypeAgent && tool.Spec.OCIRef == "" {
			return false, "spec.ociRef is required for type=regular"
		}

	case agentorcv1alpha1.ToolTypeWasm:
		if tool.Spec.OCIRef == "" {
			return false, "spec.ociRef is required for type=wasm"
		}

	case agentorcv1alpha1.ToolTypeMCP:
		if tool.Spec.MCPConfig == nil {
			return false, "spec.mcpConfig is required for type=mcp"
		}
		// Remote MCP: URL required. Sidecar MCP: ociRef required.
		if tool.Spec.MCPConfig.URL == "" && tool.Spec.OCIRef == "" {
			return false, "type=mcp requires either spec.mcpConfig.url (remote) or spec.ociRef (sidecar)"
		}
		// Auth headers are only supported for HTTP/SSE transports.
		if tool.Spec.MCPConfig.Auth != nil && tool.Spec.MCPConfig.Transport == "stdio" {
			return false, "spec.mcpConfig.auth is not supported for stdio transport; use envFrom for stdio credentials"
		}

	default:
		return false, fmt.Sprintf("unknown tool type %q", tool.Spec.Type)
	}

	if tool.Spec.Schema == nil && tool.Labels[LabelManagedBy] != LabelManagedByMCPServer {
		return false, "spec.schema is required so the LLM knows how to call this tool"
	}

	return true, fmt.Sprintf("tool %s/%s validated", tool.Spec.Type, tool.Name)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ToolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcv1alpha1.Tool{}).
		Named("tool").
		Complete(r)
}
