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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func qdrantPod(name, ns, kb string, phase corev1.PodPhase, qdrantReady bool) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "qdrant"}
	if qdrantReady {
		cs.Ready = true
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels: map[string]string{"agentorca.io/knowledgebase": kb}},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

func TestQdrantReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	r := &KnowledgeBaseReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			qdrantPod("kb-qdrant-red-tactics", "red-team", "red-tactics", corev1.PodRunning, true),
			qdrantPod("pending", "red-team", "pending-kb", corev1.PodPending, false),
			qdrantPod("warming", "red-team", "warming-kb", corev1.PodRunning, false),
			qdrantPod("other-ns", "blue-team", "red-tactics", corev1.PodRunning, true),
		).Build()}

	tests := []struct {
		name, ns, kb string
		want         bool
	}{
		{name: "ready pod", ns: "red-team", kb: "red-tactics", want: true},
		{name: "no pods for kb", ns: "red-team", kb: "missing-kb", want: false},
		{name: "pending pod", ns: "red-team", kb: "pending-kb", want: false},
		{name: "running but qdrant not ready", ns: "red-team", kb: "warming-kb", want: false},
		{name: "pod in other namespace not visible", ns: "red-team", kb: "red-tactics-other-ns", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.qdrantReady(context.Background(), tt.ns, tt.kb); got != tt.want {
				t.Fatalf("qdrantReady(%s,%s) = %v, want %v", tt.ns, tt.kb, got, tt.want)
			}
		})
	}
}
