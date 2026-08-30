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

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
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
		labelWarmPool:      deployName,
		labelWarmStatus:    warmStatusClaimed,
		labelWarmRequests:  strconv.Itoa(served),
		"agentorca.io/run": name,
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
			_ = agentorcav1alpha1.AddToScheme(scheme)

			podName := "warm-1"
			var pod *corev1.Pod
			if tt.podIsWarm {
				pod = newWarmPodForReuse(podName, "red-commander", tt.servedAtClaim)
			} else {
				pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"}}
			}

			dep := &agentorcav1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "red-commander", Namespace: "default"},
				Spec:       agentorcav1alpha1.AgentDeploymentSpec{MaxRequestsPerPod: tt.maxRequests},
			}

			var deleted []string
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(pod, dep).
				WithStatusSubresource(&agentorcav1alpha1.AgentDeployment{}).
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
			run := &agentorcav1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default",
					Labels: map[string]string{"agentorca.io/deployment": "red-commander"}},
				Status: agentorcav1alpha1.AgentRunStatus{PodName: podName},
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
				if _, ok := got.Labels["agentorca.io/run"]; ok {
					t.Fatalf("warm pod should have agentorca.io/run label removed on return-to-idle, got %v", got.Labels)
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

// TestFindClaimedWarmPod verifies the split-brain safeguard: when a prior
// claim succeeded on a warm pod but the operator's POST response was lost
// (client-side timeout), findClaimedWarmPod locates the pod that already owns
// this run via its agentorca.io/run label, preventing a second pod from being
// created for the same run.
func TestFindClaimedWarmPod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentorcav1alpha1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-1",
			Namespace: "default",
			Labels: map[string]string{
				labelWarmPool:      "dep-1",
				"agentorca.io/run": "run-1",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-1",
			Namespace: "default",
			Labels:    map[string]string{"agentorca.io/deployment": "dep-1"},
		},
	}

	got := r.findClaimedWarmPod(context.Background(), run)
	if got != "warm-1" {
		t.Errorf("expected 'warm-1', got %q", got)
	}

	// A run with no deployment label should get no result.
	run2 := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-2",
			Namespace: "default",
		},
	}
	if got := r.findClaimedWarmPod(context.Background(), run2); got != "" {
		t.Errorf("expected empty for no-label run, got %q", got)
	}
}

// TestClaimWarmPod_SkipsWithoutDeploymentLabel verifies that claimWarmPod
// returns early (no HTTP call) when the run lacks the deployment label — this
// is the bug that caused ACP runs to always create fresh ephemeral pods instead
// of reusing warm ones.
func TestClaimWarmPod_SkipsWithoutDeploymentLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentorcav1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &AgentRunReconciler{Client: cl, Scheme: scheme}

	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentSpec{
			Runtime: agentorcav1alpha1.AgentRuntime{InputMode: "http"},
		},
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-1",
			Namespace: "default",
			Labels:    map[string]string{}, // no deployment label
		},
	}

	podName, err := r.claimWarmPod(context.Background(), run, agent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if podName != "" {
		t.Errorf("expected no pod claim without deployment label, got %q", podName)
	}
}
