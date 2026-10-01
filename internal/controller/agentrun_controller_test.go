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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

var _ = Describe("AgentRun Controller", func() {
	It("fails a run whose referenced Agent does not exist", func() {
		ctx := context.Background()
		name := fmt.Sprintf("test-run-%d", time.Now().UnixNano())
		namespacedName := types.NamespacedName{Name: name, Namespace: "default"}
		reconciler := &AgentRunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		By("creating an AgentRun referencing a missing Agent")
		run := &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.AgentRunSpec{
				AgentRef: "no-such-agent",
				Input:    "what is 2+2?",
			},
		}
		Expect(k8sClient.Create(ctx, run)).To(Succeed())

		By("first reconcile adds the finalizer and requeues")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Requeue).To(BeTrue())

		By("second reconcile fails the run because the Agent is missing")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, namespacedName, run)).To(Succeed())
		Expect(run.Status.Phase).To(Equal(agentorcav1alpha1.AgentRunPhaseFailed))
		Expect(run.Status.LastRestartReason).To(ContainSubstring(`agent "no-such-agent" not found`))
		Expect(run.Status.CompletionTime).NotTo(BeNil())
	})
})
