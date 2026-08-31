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

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
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
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			// Child tools are garbage collected via owner references,
			// but clean up explicitly in tests since envtest GC may be async.
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-add", Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-multiply", Namespace: namespace},
			})
		})

		It("should create child Tool CRs for each declared tool", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://calculator.default.svc:3000",
					Tools: []agentorcav1alpha1.MCPServerTool{
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
			var addTool agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-add", Namespace: namespace,
			}, &addTool)).To(Succeed())

			Expect(addTool.Spec.Type).To(Equal(agentorcav1alpha1.ToolTypeMCP))
			Expect(addTool.Spec.MCPConfig).NotTo(BeNil())
			Expect(addTool.Spec.MCPConfig.Transport).To(Equal("http"))
			Expect(addTool.Spec.MCPConfig.URL).To(Equal("http://calculator.default.svc:3000"))
			Expect(addTool.Spec.Schema).NotTo(BeNil())
			Expect(addTool.Spec.Schema.Description).To(Equal("Add two numbers"))
			Expect(addTool.Labels[LabelManagedBy]).To(Equal(LabelManagedByMCPServer))
			Expect(addTool.Labels[LabelMCPServer]).To(Equal(serverName))

			var mulTool agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-multiply", Namespace: namespace,
			}, &mulTool)).To(Succeed())
			Expect(mulTool.Spec.Schema.Description).To(Equal("Multiply two numbers"))

			// Verify owner reference is set.
			Expect(addTool.OwnerReferences).To(HaveLen(1))
			Expect(addTool.OwnerReferences[0].Name).To(Equal(serverName))

			// Verify MCPServer status.
			var updated agentorcav1alpha1.MCPServer
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
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-echo", Namespace: namespace},
			})
		})

		It("should create a child Tool with the OCIRef and args", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "stdio",
					OCIRef:    "ghcr.io/example/mcp-echo:v1",
					Args:      []string{"/usr/local/bin/mcp-echo", "--mode=stdio"},
					Env:       map[string]string{"LOG_LEVEL": "debug"},
					Tools: []agentorcav1alpha1.MCPServerTool{
						{
							Name:        "echo",
							Description: "Echo input back",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var tool agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-echo", Namespace: namespace,
			}, &tool)).To(Succeed())

			Expect(tool.Spec.Type).To(Equal(agentorcav1alpha1.ToolTypeMCP))
			Expect(tool.Spec.OCIRef).To(Equal("ghcr.io/example/mcp-echo:v1"))
			Expect(tool.Spec.MCPConfig.Transport).To(Equal("stdio"))
			Expect(tool.Spec.MCPConfig.Args).To(Equal([]string{"/usr/local/bin/mcp-echo", "--mode=stdio"}))
			Expect(tool.Spec.MCPConfig.Env).To(HaveKeyWithValue("LOG_LEVEL", "debug"))
		})
	})

	Context("When validation fails", func() {
		const serverName = "test-invalid-server"

		AfterEach(func() {
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
		})

		It("should set Ready=false when http transport has no URL", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					// URL intentionally omitted
					Tools: []agentorcav1alpha1.MCPServerTool{
						{Name: "foo", Description: "A tool"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeFalse())
			Expect(updated.Status.Message).To(ContainSubstring("spec.url is required"))
		})

		It("should set Ready=false when stdio transport has no OCIRef", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "stdio",
					// OCIRef intentionally omitted
					Tools: []agentorcav1alpha1.MCPServerTool{
						{Name: "bar", Description: "A tool"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var updated agentorcav1alpha1.MCPServer
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
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-keep", Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-remove", Namespace: namespace},
			})
		})

		It("should delete the stale child Tool", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://example.svc:3000",
					Tools: []agentorcav1alpha1.MCPServerTool{
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
			}, &agentorcav1alpha1.Tool{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-remove", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})).To(Succeed())

			// Remove one tool from the spec.
			var current agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &current)).To(Succeed())
			current.Spec.Tools = []agentorcav1alpha1.MCPServerTool{
				{Name: "keep", Description: "Stays"},
			}
			Expect(k8sClient.Update(ctx, &current)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			// The kept tool should still exist.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-keep", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})).To(Succeed())

			// The removed tool should be gone.
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-remove", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})
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
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: namespace},
			})
		})

		It("should pass validation without spec.schema if managed by mcpserver", func() {
			tool := &agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      toolName,
					Namespace: namespace,
					Labels: map[string]string{
						LabelManagedBy: LabelManagedByMCPServer,
						LabelMCPServer: "some-server",
					},
				},
				Spec: agentorcav1alpha1.ToolSpec{
					Type: agentorcav1alpha1.ToolTypeMCP,
					MCPConfig: &agentorcav1alpha1.MCPConfig{
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

			var updated agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: toolName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeTrue())
		})
	})

	Context("When creating an MCPServer in discovery mode (empty tools)", func() {
		const serverName = "test-discovery-server"

		AfterEach(func() {
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			// The connector/marker Tool CR is named after the MCPServer.
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
		})

		It("should create a single connector Tool CR named after the server", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport:       "http",
					URL:             "http://calculator.default.svc:3000",
					Discoverability: agentorcav1alpha1.MCPServerDiscoverabilityEnabled,
					// Tools intentionally omitted.
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var marker agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &marker)).To(Succeed())

			Expect(marker.Spec.Type).To(Equal(agentorcav1alpha1.ToolTypeMCP))
			Expect(marker.Spec.MCPConfig).NotTo(BeNil())
			Expect(marker.Spec.MCPConfig.Transport).To(Equal("http"))
			Expect(marker.Spec.MCPConfig.URL).To(Equal("http://calculator.default.svc:3000"))
			Expect(marker.Spec.Schema).To(BeNil())
			Expect(marker.Labels[LabelManagedBy]).To(Equal(LabelManagedByMCPServer))
			Expect(marker.Labels[LabelMCPServer]).To(Equal(serverName))
			Expect(marker.OwnerReferences).To(HaveLen(1))
			Expect(marker.OwnerReferences[0].Name).To(Equal(serverName))

			// Exactly one child Tool CR should exist for this server.
			var children agentorcav1alpha1.ToolList
			Expect(k8sClient.List(ctx, &children,
				client.InNamespace(namespace),
				client.MatchingLabels{LabelManagedBy: LabelManagedByMCPServer, LabelMCPServer: serverName},
			)).To(Succeed())
			Expect(children.Items).To(HaveLen(1))

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeTrue())
			Expect(updated.Status.Discovering).To(BeTrue())
			Expect(updated.Status.ToolCount).To(Equal(1))
		})

		It("should default to discovery mode when discoverability is omitted", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://calculator.default.svc:3000",
					// Discoverability and Tools both omitted → defaults to enabled.
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var marker agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &marker)).To(Succeed())
			Expect(marker.Spec.Type).To(Equal(agentorcav1alpha1.ToolTypeMCP))

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeTrue())
			Expect(updated.Status.Discovering).To(BeTrue())
		})
	})

	Context("When validating discovery mode boundaries", func() {
		const serverName = "test-discovery-validation"

		AfterEach(func() {
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-foo", Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
		})

		It("should set Ready=false when disabled with empty spec.tools", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport:       "http",
					URL:             "http://example.svc:3000",
					Discoverability: agentorcav1alpha1.MCPServerDiscoverabilityDisabled,
					// Tools intentionally omitted.
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeFalse())
			Expect(updated.Status.Message).To(ContainSubstring("spec.tools must declare at least one tool"))
		})

		It("should accept enabled with explicit spec.tools (backward compatible)", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport:       "http",
					URL:             "http://example.svc:3000",
					Discoverability: agentorcav1alpha1.MCPServerDiscoverabilityEnabled,
					Tools: []agentorcav1alpha1.MCPServerTool{
						{Name: "foo", Description: "A tool"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			// Explicit declaration under enabled -> per-tool CR, not the marker.
			var foo agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-foo", Namespace: namespace,
			}, &foo)).To(Succeed())
			Expect(foo.Labels[LabelMCPServer]).To(Equal(serverName))

			// No connector marker (named after the server) should exist.
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})
			Expect(errors.IsNotFound(err)).To(BeTrue())

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeTrue())
			Expect(updated.Status.Discovering).To(BeFalse())
		})

		It("should flag duplicate tool names even in default (enabled) mode", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://example.svc:3000",
					Tools: []agentorcav1alpha1.MCPServerTool{
						{Name: "foo"},
						{Name: "foo"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Ready).To(BeFalse())
			Expect(updated.Status.Message).To(ContainSubstring("duplicate tool name"))
		})
	})

	Context("When switching an MCPServer from explicit tools to discovery mode", func() {
		const serverName = "test-migration-server"

		AfterEach(func() {
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-alpha", Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName + "-beta", Namespace: namespace},
			})
			deleteIfExists(&agentorcav1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
		})

		It("should replace per-tool CRs with the connector Tool CR", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://example.svc:3000",
					Tools: []agentorcav1alpha1.MCPServerTool{
						{Name: "alpha", Description: "Alpha"},
						{Name: "beta", Description: "Beta"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())
			reconcileAndExpectSuccess(serverName)

			// Both per-tool CRs exist.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-alpha", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-beta", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})).To(Succeed())

			// Switch to discovery mode.
			var current agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &current)).To(Succeed())
			current.Spec.Discoverability = agentorcav1alpha1.MCPServerDiscoverabilityEnabled
			current.Spec.Tools = nil
			Expect(k8sClient.Update(ctx, &current)).To(Succeed())

			reconcileAndExpectSuccess(serverName)

			// The per-tool CRs are gone...
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-alpha", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName + "-beta", Namespace: namespace,
			}, &agentorcav1alpha1.Tool{})
			Expect(errors.IsNotFound(err)).To(BeTrue())

			// ...and the connector marker exists, named after the server.
			var marker agentorcav1alpha1.Tool
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &marker)).To(Succeed())
			Expect(marker.Spec.Type).To(Equal(agentorcav1alpha1.ToolTypeMCP))
			Expect(marker.Labels[LabelMCPServer]).To(Equal(serverName))

			var updated agentorcav1alpha1.MCPServer
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: serverName, Namespace: namespace,
			}, &updated)).To(Succeed())
			Expect(updated.Status.Discovering).To(BeTrue())
		})
	})

	Context("When an agent references a stale per-tool name under discovery mode", func() {
		const serverName = "test-hint-server"

		AfterEach(func() {
			deleteIfExists(&agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: namespace},
			})
		})

		It("should return a hint to reference the server by name", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport: "http",
					URL:       "http://example.svc:3000",
					// Discoverability defaults to enabled; Tools omitted => discovery mode.
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())
			reconcileAndExpectSuccess(serverName)

			// A stale per-tool reference like "github-mcp-<tool>" should yield an
			// actionable hint pointing the agent at the connector/server name.
			hint := mcpToolResolutionHint(ctx, k8sClient, namespace, serverName+"-get-file-contents")
			Expect(hint).To(ContainSubstring("auto-discovery mode"))
			Expect(hint).To(ContainSubstring("reference the server by name \"" + serverName + "\""))
		})

		It("should return no hint for a non-discovery server", func() {
			server := &agentorcav1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverName,
					Namespace: namespace,
				},
				Spec: agentorcav1alpha1.MCPServerSpec{
					Transport:       "http",
					URL:             "http://example.svc:3000",
					Discoverability: agentorcav1alpha1.MCPServerDiscoverabilityDisabled,
					Tools: []agentorcav1alpha1.MCPServerTool{
						{Name: "foo", Description: "A tool"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, server)).To(Succeed())
			reconcileAndExpectSuccess(serverName)

			Expect(mcpToolResolutionHint(ctx, k8sClient, namespace, serverName+"-get-file-contents")).To(BeEmpty())
		})
	})
})
