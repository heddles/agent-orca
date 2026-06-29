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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

var _ = Describe("MCPServer Controller", func() {
	const namespace = "default"

	ctx := context.Background()

	reconciler := func() *MCPServerReconciler {
		return &MCPServerReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	}

	reconcileAndExpectSuccess := func(name string) {
		_, err := reconciler().Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	deleteIfExists := func(obj client.Object) {
		err := k8sClient.Delete(ctx, obj)
		if err != nil && !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}

	Context("When creating an MCPServer with HTTP transport", func() {
		const serverName = "test-http-server"

		AfterEach(func() {
			deleteIfExists(&agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			// Child tools are garbage collected via owner references,
			// but clean up explicitly in tests since envtest GC may be async.
			deleteIfExists(&agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-add", Namespace: namespace},
			})
			deleteIfExists(&agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-multiply", Namespace: namespace},
			})
		})

		It("should create child Tool CRs for each declared tool", func() {
			server := &agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcv1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://calculator.default.svc:3000",
					Tools: []agentorcv1alpha1.MCPServerTool{
						{
							Name:        "add",
							Description: "Add two numbers",
							InputSchema: &runtime.RawExtension{
								Raw: []byte(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
							},
						},
						{
							Name:        "multiply",
							Description: "Multiply two numbers",
							InputSchema: &runtime.RawExtension{
								Raw: []byte(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			// Verify child Tools were created.
			var addTool agentorcv1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-add", Namespace: namespace,
			}, &addTool)).To(Succeed())

			Expect(addTool.Spec.Type).To(Equal(agentorcv1alpha1.ToolTypeMCP))
			Expect(addTool.Spec.MCPConfig).NotTo(BeNil())
			Expect(addTool.Spec.MCPConfig.Transport).To(Equal("http"))
			Expect(addTool.Spec.MCPConfig.URL).To(Equal("http://calculator.default.svc:3000"))
			Expect(addTool.Spec.Schema).NotTo(BeNil())
			Expect(addTool.Spec.Schema.Description).To(Equal("Add two numbers"))
			Expect(addTool.Labels[LabelManagedBy]).To(Equal(LabelManagedByMCPServer))
			Expect(addTool.Labels[LabelMCPServer]).To(Equal(serverName))

			var mulTool agentorcv1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-multiply", Namespace: namespace,
			}, &mulTool)).To(Succeed())
			Expect(mulTool.Spec.Schema.Description).To(Equal("Multiply two numbers"))

			// Verify owner reference is set.
			Expect(addTool.OwnerReferences).To(HaveLen(1))
			Expect(addTool.OwnerReferences[0].Name).To(Equal(serverName))

			// Verify MCPServer status.
			var updated agentorcv1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeTrue())
			Expect(updated.Status.ToolCount).To(Equal(2))
		})
	})

	Context("When creating an MCPServer with stdio transport", func() {
		const serverName = "test-stdio-server"

		AfterEach(func() {
			deleteIfExists(&agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			deleteIfExists(&agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-echo", Namespace: namespace},
			})
		})

		It("should create a child Tool with the OCIRef and args", func() {
			server := &agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcv1alpha1.MCPServerSpec{
					Transport: "stdio",
					OCIRef:    "ghcr.io/example/mcp-echo:v1",
					Args:      []string{"/usr/local/bin/mcp-echo", "--mode=stdio"},
					Env:       map[string]string{"LOG_LEVEL": "debug"},
					Tools: []agentorcv1alpha1.MCPServerTool{
						{
							Name:        "echo",
							Description: "Echo input back",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var tool agentorcv1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-echo", Namespace: namespace,
			}, &tool)).To(Succeed())

			Expect(tool.Spec.Type).To(Equal(agentorcv1alpha1.ToolTypeMCP))
			Expect(tool.Spec.OCIRef).To(Equal("ghcr.io/example/mcp-echo:v1"))
			Expect(tool.Spec.MCPConfig.Transport).To(Equal("stdio"))
			Expect(tool.Spec.MCPConfig.Args).To(Equal([]string{"/usr/local/bin/mcp-echo", "--mode=stdio"}))
			Expect(tool.Spec.MCPConfig.Env).To(HaveKeyWithValue("LOG_LEVEL", "debug"))
		})
	})

	Context("When validation fails", func() {
		const serverName = "test-invalid-server"

		AfterEach(func() {
			deleteIfExists(&agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
		})

		It("should set Ready=false when http transport has no URL", func() {
			server := &agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcv1alpha1.MCPServerSpec{
					Transport: "http",
					// URL intentionally omitted
					Tools: []agentorcv1alpha1.MCPServerTool{
						{Name: "foo", Description: "A tool"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var updated agentorcv1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeFalse())
			Expect(updated.Status.Message).To(ContainSubstring("spec.url is required"))
		})

		It("should set Ready=false when stdio transport has no OCIRef", func() {
			server := &agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcv1alpha1.MCPServerSpec{
					Transport: "stdio",
					// OCIRef intentionally omitted
					Tools: []agentorcv1alpha1.MCPServerTool{
						{Name: "bar", Description: "A tool"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var updated agentorcv1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeFalse())
			Expect(updated.Status.Message).To(ContainSubstring("spec.ociRef is required"))
		})
	})

	Context("When removing a tool from the MCPServer spec", func() {
		const serverName = "test-stale-cleanup"

		AfterEach(func() {
			deleteIfExists(&agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			deleteIfExists(&agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-keep", Namespace: namespace},
			})
			deleteIfExists(&agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-remove", Namespace: namespace},
			})
		})

		It("should delete the stale child Tool", func() {
			server := &agentorcv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcv1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://example.svc:3000",
					Tools: []agentorcv1alpha1.MCPServerTool{
						{Name: "keep", Description: "Stays"},
						{Name: "remove", Description: "Will be removed"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			// Both tools should exist.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-keep", Namespace: namespace,
			}, &agentorcv1alpha1.Tool{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-remove", Namespace: namespace,
			}, &agentorcv1alpha1.Tool{})).To(Succeed())

			// Remove one tool from the spec.
			var current agentorcv1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &current)).To(Succeed())
			current.Spec.Tools = []agentorcv1alpha1.MCPServerTool{
				{Name: "keep", Description: "Stays"},
			}
			Expect(k8sClient.Update(ctx, &current)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			// The kept tool should still exist.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-keep", Namespace: namespace,
			}, &agentorcv1alpha1.Tool{})).To(Succeed())

			// The removed tool should be gone.
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-remove", Namespace: namespace,
			}, &agentorcv1alpha1.Tool{})
			Expect(errors.IsNotFound(err)).To(BeTrue())

			// Status should reflect the new count.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &current)).To(Succeed())
			Expect(current.Status.ToolCount).To(Equal(1))
		})
	})

	Context("When reconciling a deleted MCPServer", func() {
		It("should not error on NotFound", func() {
			_, err := reconciler().Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent-server",
					Namespace: namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When a Tool with managed-by label is validated by ToolReconciler", func() {
		const toolName = "test-managed-tool"

		AfterEach(func() {
			deleteIfExists(&agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: namespace},
			})
		})

		It("should pass validation without spec.schema if managed by mcpserver", func() {
			tool := &agentorcv1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      toolName,
					Namespace: namespace,
					Labels: map[string]string{
						LabelManagedBy: LabelManagedByMCPServer,
						LabelMCPServer: "some-server",
					},
				},
				Spec: agentorcv1alpha1.ToolSpec{
					Type: agentorcv1alpha1.ToolTypeMCP,
					MCPConfig: &agentorcv1alpha1.MCPConfig{
						Transport: "http",
						URL:       "http://example.svc:3000",
					},
					// Schema intentionally omitted.
				},
			}
			Expect(k8sClient.Create(ctx, tool)).To(Succeed())

			toolReconciler := &ToolReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := toolReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: toolName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			var updated agentorcv1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: toolName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeTrue())
		})
	})
})
