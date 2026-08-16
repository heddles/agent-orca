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

	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

func TestWarmRequestCount(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want int
	}{
		{name: "absent", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: nil}}, want: 0},
		{name: "zero", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelWarmRequests: "0"}}}, want: 0},
		{name: "five", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelWarmRequests: "5"}}}, want: 5},
		{name: "garbage", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelWarmRequests: "abc"}}}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warmRequestCount(tt.pod); got != tt.want {
				t.Fatalf("warmRequestCount=%d want %d", got, tt.want)
			}
		})
	}
}

// TestShouldDeletePodOnCompletion locks in the fix for the bug where the operator
// destroyed a warm pod the moment an HTTP/chat run produced output (even mid-session).
// One-shot pods (no warm-pool label) must still be deleted; warm pods must be preserved
// so they can be returned to the idle pool by reconcileRunPodOnTerminal.
func TestShouldDeletePodOnCompletion(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "one-shot pod (no warm label) is deleted", pod: &corev1.Pod{}, want: true},
		{name: "warm pod is preserved", pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelWarmPool: "dep"}}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDeletePodOnCompletion(tt.pod); got != tt.want {
				t.Fatalf("shouldDeletePodOnCompletion=%v want %v", got, tt.want)
			}
		})
	}
}

// newWarmPodForReuse builds a warm pod in the claimed state with a served-request count.
func newWarmPodForReuse(name, deployName string, served int) *corev1.Pod {
	labels := map[string]string{
		labelWarmPool:     deployName,
		labelWarmStatus:   warmStatusClaimed,
		labelWarmRequests: strconv.Itoa(served),
		"agentorc.io/run": name,
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "model-router"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
}

func TestReconcileRunPodOnTerminal(t *testing.T) {
	tests := []struct {
		name               string
		maxRequests        int // AgentDeployment.MaxRequestsPerPod
		servedAtClaim      int // warm-requests label value at completion time
		podIsWarm          bool
		wantDeleted        bool
		wantReturnedToIdle bool
	}{
		{name: "one-shot pod is always deleted", maxRequests: 0, podIsWarm: false, wantDeleted: true, wantReturnedToIdle: false},
		{name: "warm pod reused when max=0 (default)", maxRequests: 0, podIsWarm: true, servedAtClaim: 1, wantDeleted: false, wantReturnedToIdle: true},
		{name: "warm pod reused when served < cap", maxRequests: 5, podIsWarm: true, servedAtClaim: 3, wantDeleted: false, wantReturnedToIdle: true},
		{name: "warm pod recycled when served == cap", maxRequests: 5, podIsWarm: true, servedAtClaim: 5, wantDeleted: true, wantReturnedToIdle: false},
		{name: "warm pod recycled when served > cap", maxRequests: 5, podIsWarm: true, servedAtClaim: 6, wantDeleted: true, wantReturnedToIdle: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = agentorcv1alpha1.AddToScheme(scheme)

			podName := "warm-1"
			var pod *corev1.Pod
			if tt.podIsWarm {
				pod = newWarmPodForReuse(podName, "red-commander", tt.servedAtClaim)
			} else {
				pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"}}
			}

			dep := &agentorcv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "red-commander", Namespace: "default"},
				Spec:       agentorcv1alpha1.AgentDeploymentSpec{MaxRequestsPerPod: tt.maxRequests},
			}

			var deleted []string
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(pod, dep).
				WithStatusSubresource(&agentorcv1alpha1.AgentDeployment{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if p, ok := obj.(*corev1.Pod); ok {
							deleted = append(deleted, p.Name)
						}
						return c.Delete(ctx, obj, opts...)
					},
				}).
				Build()

			r := &AgentRunReconciler{Client: fc, Scheme: scheme}
			run := &agentorcv1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default",
					Labels: map[string]string{"agentorc.io/deployment": "red-commander"}},
				Status: agentorcv1alpha1.AgentRunStatus{PodName: podName},
			}

			r.reconcileRunPodOnTerminal(context.Background(), run)

			// Re-fetch the pod to inspect its post-completion state.
			var got corev1.Pod
			if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: podName}, &got); err == nil {
				// Pod still exists -> must have been returned to idle, not deleted.
				if tt.wantDeleted {
					t.Fatalf("pod %q should have been deleted, but still exists; labels=%v", podName, got.Labels)
				}
				if got.Labels[labelWarmStatus] != warmStatusIdle {
					t.Fatalf("warm pod warm-status = %q, want idle", got.Labels[labelWarmStatus])
				}
				if _, ok := got.Labels["agentorc.io/run"]; ok {
					t.Fatalf("warm pod should have agentorc.io/run label removed on return-to-idle, got %v", got.Labels)
				}
				if !tt.wantReturnedToIdle {
					t.Fatalf("pod %q was returned to idle, but test expected deletion", podName)
				}
			} else if !tt.wantDeleted {
				t.Fatalf("pod %q was deleted but test expected it to be returned to idle", podName)
			}

			if tt.wantDeleted && len(deleted) == 0 {
				t.Fatalf("expected pod to be deleted, but no delete occurred (deleted=%v)", deleted)
			}
			if !tt.wantDeleted && len(deleted) != 0 {
				t.Fatalf("expected pod to be retained, but it was deleted: %v", deleted)
			}
		})
	}
}
