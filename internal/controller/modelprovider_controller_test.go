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

// TestModelProviderValidate covers the pure validation branches directly:
// invalid specs are rejected by CRD admission (litellmModel has MinLength=1),
// so these paths cannot be driven through envtest.
func TestModelProviderValidate(t *testing.T) {
	tests := []struct {
		name        string
		spec        agentorcav1alpha1.ModelProviderSpec
		wantReady   bool
		wantMessage string
	}{
		{
			name: "valid provider",
			spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "anthropic/claude-sonnet-4-6",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "creds", Key: "api-key"},
			},
			wantReady:   true,
			wantMessage: "provider anthropic/claude-sonnet-4-6 validated",
		},
		{
			name:        "missing litellmModel",
			spec:        agentorcav1alpha1.ModelProviderSpec{CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "creds", Key: "api-key"}},
			wantMessage: "spec.litellmModel is required",
		},
		{
			name: "litellmModel without provider prefix",
			spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "claude",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "creds", Key: "api-key"},
			},
			wantMessage: `spec.litellmModel "claude" must be in <provider>/<model> format (e.g. anthropic/claude-sonnet-4-6)`,
		},
		{
			name: "missing credentialsRef.name",
			spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "anthropic/claude",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Key: "api-key"},
			},
			wantMessage: "spec.credentialsRef.name is required",
		},
		{
			name: "missing credentialsRef.key",
			spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "anthropic/claude",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "creds"},
			},
			wantMessage: "spec.credentialsRef.key is required",
		},
	}

	r := &ModelProviderReconciler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, message := r.validate(&agentorcav1alpha1.ModelProvider{Spec: tt.spec})
			if ready != tt.wantReady {
				t.Errorf("ready: got %v, want %v", ready, tt.wantReady)
			}
			if message != tt.wantMessage {
				t.Errorf("message: got %q, want %q", message, tt.wantMessage)
			}
		})
	}
}

var _ = Describe("ModelProvider Controller", func() {
	It("publishes validation results to status", func() {
		ctx := context.Background()
		name := fmt.Sprintf("test-modelprovider-%d", time.Now().UnixNano())
		namespacedName := types.NamespacedName{Name: name, Namespace: "default"}

		By("creating a valid ModelProvider")
		mp := &agentorcav1alpha1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentorcav1alpha1.ModelProviderSpec{
				LiteLLMModel:   "anthropic/claude-sonnet-4-6",
				CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "creds", Key: "api-key"},
			},
		}
		Expect(k8sClient.Create(ctx, mp)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, mp) }()

		By("reconciling the ModelProvider")
		reconciler := &ModelProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
		Expect(err).NotTo(HaveOccurred())

		updated := &agentorcav1alpha1.ModelProvider{}
		Expect(k8sClient.Get(ctx, namespacedName, updated)).To(Succeed())
		Expect(updated.Status.Ready).To(BeTrue())
		Expect(updated.Status.Message).To(Equal("provider anthropic/claude-sonnet-4-6 validated"))
		Expect(updated.Status.Conditions).To(HaveLen(1))
		Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
		Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
	})
})
