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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// TestToolValidate covers the pure validation branches directly: unknown tool
// types are rejected by CRD admission (the type field is an enum), so that
// path cannot be driven through envtest.
func TestToolValidate(t *testing.T) {
	schema := &agentorcav1alpha1.ToolSchema{Description: "test tool"}

	tests := []struct {
		name        string
		tool        *agentorcav1alpha1.Tool
		wantReady   bool
		wantMessage string
	}{
		{
			name: "valid regular tool",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolTypeRegular,
				OCIRef: "ghcr.io/test/tool:latest",
				Schema: schema,
			}},
			wantReady:   true,
			wantMessage: "tool regular/valid regular tool validated",
		},
		{
			name: "regular tool missing ociRef",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolTypeRegular,
				Schema: schema,
			}},
			wantMessage: "spec.ociRef is required for type=regular",
		},
		{
			name: "agent tool missing agentRef",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolTypeAgent,
				Schema: schema,
			}},
			wantMessage: "spec.agentRef is required for type=agent",
		},
		{
			name: "valid agent tool",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:     agentorcav1alpha1.ToolTypeAgent,
				AgentRef: "other-agent",
				Schema:   schema,
			}},
			wantReady:   true,
			wantMessage: "tool agent/valid agent tool validated",
		},
		{
			name: "mcp tool missing mcpConfig",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolTypeMCP,
				Schema: schema,
			}},
			wantMessage: "spec.mcpConfig is required for type=mcp",
		},
		{
			name: "valid remote mcp tool",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:      agentorcav1alpha1.ToolTypeMCP,
				MCPConfig: &agentorcav1alpha1.MCPConfig{Transport: "http", URL: "https://mcp.example.com/sse"},
				Schema:    schema,
			}},
			wantReady:   true,
			wantMessage: "tool mcp/valid remote mcp tool validated",
		},
		{
			name: "mcp stdio rejects auth config",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:      agentorcav1alpha1.ToolTypeMCP,
				OCIRef:    "ghcr.io/test/mcp:latest",
				MCPConfig: &agentorcav1alpha1.MCPConfig{Transport: "stdio", Auth: &agentorcav1alpha1.MCPAuthConfig{}},
				Schema:    schema,
			}},
			wantMessage: "spec.mcpConfig.auth is not supported for stdio transport; use envFrom for stdio credentials",
		},
		{
			name: "unknown tool type",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolType("bogus"),
				Schema: schema,
			}},
			wantMessage: `unknown tool type "bogus"`,
		},
		{
			name: "missing schema",
			tool: &agentorcav1alpha1.Tool{Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolTypeRegular,
				OCIRef: "ghcr.io/test/tool:latest",
			}},
			wantMessage: "spec.schema is required so the LLM knows how to call this tool",
		},
	}

	r := &ToolReconciler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.tool.Name = tt.name
			ready, message := r.validate(context.Background(), tt.tool)
			if ready != tt.wantReady {
				t.Errorf("ready: got %v, want %v", ready, tt.wantReady)
			}
			if message != tt.wantMessage {
				t.Errorf("message: got %q, want %q", message, tt.wantMessage)
			}
		})
	}
}

var _ = Describe("Tool Controller", func() {
	It("publishes validation results to status", func() {
		ctx := context.Background()
		name := fmt.Sprintf("test-tool-%d", time.Now().UnixNano())
		namespacedName := types.NamespacedName{Name: name, Namespace: "default"}

		By("creating a valid Tool")
		tool := &agentorcav1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.ToolSpec{
				Type:   agentorcav1alpha1.ToolTypeRegular,
				OCIRef: "ghcr.io/test/tool:latest",
				Schema: &agentorcav1alpha1.ToolSchema{Description: "test tool"},
			},
		}
		Expect(k8sClient.Create(ctx, tool)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, tool) }()

		By("reconciling the Tool")
		reconciler := &ToolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		updated := &agentorcav1alpha1.Tool{}
		Expect(k8sClient.Get(ctx, namespacedName, updated)).To(Succeed())
		Expect(updated.Status.Ready).To(BeTrue())
		Expect(updated.Status.Message).To(Equal(fmt.Sprintf("tool regular/%s validated", name)))
		Expect(updated.Status.Conditions).To(HaveLen(1))
		Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
		Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
	})
})
