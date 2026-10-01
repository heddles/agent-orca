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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
)

var _ = Describe("Agent Controller", func() {
	It("manages the agent ServiceAccount across create and delete", func() {
		ctx := context.Background()
		name := fmt.Sprintf("test-agent-%d", time.Now().UnixNano())
		namespacedName := types.NamespacedName{Name: name, Namespace: "default"}
		reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		By("creating an Agent")
		agent := &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.AgentSpec{
				ModelSelectorRef: "test-selector",
				Runtime: agentorcav1alpha1.AgentRuntime{
					OCIRef: "ghcr.io/test/agent:latest",
				},
			},
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())

		By("first reconcile adds the finalizer and requeues")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, namespacedName, agent)).To(Succeed())
		Expect(agent.Finalizers).To(ContainElement(agentFinalizer))

		By("second reconcile creates the managed ServiceAccount and reflects it in status")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		saName := security.AgentSAName(name)
		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, sa)).To(Succeed())
		Expect(sa.OwnerReferences).To(HaveLen(1))
		Expect(sa.OwnerReferences[0].Name).To(Equal(name))

		Expect(k8sClient.Get(ctx, namespacedName, agent)).To(Succeed())
		Expect(agent.Status.ServiceAccountName).To(Equal(saName))

		By("deleting the Agent cleans up the ServiceAccount and removes the finalizer")
		Expect(k8sClient.Delete(ctx, agent)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, &corev1.ServiceAccount{})
			return err != nil // ServiceAccount gone
		}).Should(BeTrue())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, namespacedName, &agentorcav1alpha1.Agent{})
			return err != nil // Agent gone (finalizer removed)
		}).Should(BeTrue())
	})
})
