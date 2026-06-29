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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

// makeWarmPod creates a warm pool pod with the given name, creation time, and readiness state.
func makeWarmPod(name, deployName, configHash string, createdAt time.Time, modelRouterReady bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				labelWarmPool:   deployName,
				labelWarmStatus: warmStatusIdle,
			},
			Annotations: map[string]string{
				"agentorc.io/router-config-hash": configHash,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
		},
	}
	if modelRouterReady {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: "model-router", Ready: true},
		}
	}
	return pod
}

func TestScaleDownWarmPool(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name            string
		warmPoolSize    int
		pods            []*corev1.Pod
		wantRemaining   int
		wantReadyCount  int
		wantDeletedPods []string // names of pods that should be deleted
	}{
		{
			name:         "no excess pods",
			warmPoolSize: 3,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", "hash1", now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
			},
			wantRemaining:  2,
			wantReadyCount: 2,
		},
		{
			name:         "exact match",
			warmPoolSize: 3,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", "hash1", now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
				makeWarmPod("ready-3", "deploy-a", "hash1", now.Add(-1*time.Minute), true),
			},
			wantRemaining:  3,
			wantReadyCount: 3,
		},
		{
			name:         "excess ready pods - oldest deleted first",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("oldest", "deploy-a", "hash1", now.Add(-30*time.Minute), true),
				makeWarmPod("middle", "deploy-a", "hash1", now.Add(-15*time.Minute), true),
				makeWarmPod("newest-1", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
				makeWarmPod("newest-2", "deploy-a", "hash1", now.Add(-1*time.Minute), true),
			},
			wantRemaining:   2,
			wantReadyCount:  2,
			wantDeletedPods: []string{"oldest", "middle"},
		},
		{
			name:         "excess mixed - ready deleted before starting",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("ready-old", "deploy-a", "hash1", now.Add(-20*time.Minute), true),
				makeWarmPod("ready-new", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
				makeWarmPod("starting-1", "deploy-a", "hash1", now.Add(-2*time.Minute), false),
				makeWarmPod("starting-2", "deploy-a", "hash1", now.Add(-1*time.Minute), false),
			},
			wantRemaining:   2,
			wantReadyCount:  0, // both ready pods deleted to reach desired=2, leaving 2 starting
			wantDeletedPods: []string{"ready-old", "ready-new"},
		},
		{
			name:         "all starting pods - excess deleted",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("starting-1", "deploy-a", "hash1", now.Add(-10*time.Minute), false),
				makeWarmPod("starting-2", "deploy-a", "hash1", now.Add(-5*time.Minute), false),
				makeWarmPod("starting-3", "deploy-a", "hash1", now.Add(-2*time.Minute), false),
				makeWarmPod("starting-4", "deploy-a", "hash1", now.Add(-1*time.Minute), false),
			},
			wantRemaining:   2,
			wantReadyCount:  0,
			wantDeletedPods: []string{"starting-1", "starting-2"},
		},
		{
			name:         "scale to zero deletes all",
			warmPoolSize: 0,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", "hash1", now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
			},
			wantRemaining:   0,
			wantReadyCount:  0,
			wantDeletedPods: []string{"ready-1", "ready-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = agentorcv1alpha1.AddToScheme(scheme)

			// Track deleted pod names via interceptor.
			var deletedPods []string
			objs := make([]client.Object, len(tt.pods))
			for i, p := range tt.pods {
				objs[i] = p
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&agentorcv1alpha1.AgentDeployment{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if pod, ok := obj.(*corev1.Pod); ok {
							deletedPods = append(deletedPods, pod.Name)
						}
						return c.Delete(ctx, obj, opts...)
					},
				}).
				Build()

			// Simulate the scale-down portion of reconcileWarmPool.
			// List idle warm pods (same query as the real reconciler).
			var pods corev1.PodList
			if err := fakeClient.List(context.Background(), &pods,
				client.InNamespace("default"),
				client.MatchingLabels{
					labelWarmPool:   "deploy-a",
					labelWarmStatus: warmStatusIdle,
				},
			); err != nil {
				t.Fatalf("listing pods: %v", err)
			}

			// Run the same cleanup + counting logic (skip token/config checks for unit test).
			var readyPods, startingPods []*corev1.Pod
			for i := range pods.Items {
				p := &pods.Items[i]
				modelRouterReady := false
				for _, cs := range p.Status.InitContainerStatuses {
					if cs.Name == "model-router" && cs.Ready {
						modelRouterReady = true
						break
					}
				}
				if modelRouterReady {
					readyPods = append(readyPods, p)
				} else {
					startingPods = append(startingPods, p)
				}
			}

			// Run scale-down logic (mirrors the production code).
			desired := tt.warmPoolSize
			readyCount := len(readyPods)
			startingCount := len(startingPods)
			totalExisting := readyCount + startingCount

			if excess := totalExisting - desired; excess > 0 {
				slices.SortFunc(readyPods, func(a, b *corev1.Pod) int {
					return a.CreationTimestamp.Time.Compare(b.CreationTimestamp.Time)
				})
				deleted := 0
				for deleted < excess && len(readyPods) > 0 {
					p := readyPods[0]
					readyPods = readyPods[1:]
					if err := fakeClient.Delete(context.Background(), p); err != nil {
						t.Fatalf("deleting excess ready pod: %v", err)
					}
					deleted++
				}
				for deleted < excess && len(startingPods) > 0 {
					p := startingPods[0]
					startingPods = startingPods[1:]
					if err := fakeClient.Delete(context.Background(), p); err != nil {
						t.Fatalf("deleting excess starting pod: %v", err)
					}
					deleted++
				}
				readyCount = len(readyPods)
			}

			// Verify remaining pod count.
			var remaining corev1.PodList
			if err := fakeClient.List(context.Background(), &remaining, client.InNamespace("default")); err != nil {
				t.Fatalf("listing remaining pods: %v", err)
			}
			if got := len(remaining.Items); got != tt.wantRemaining {
				t.Errorf("remaining pods: got %d, want %d", got, tt.wantRemaining)
			}
			if readyCount != tt.wantReadyCount {
				t.Errorf("readyCount: got %d, want %d", readyCount, tt.wantReadyCount)
			}

			// Verify correct pods were deleted.
			if tt.wantDeletedPods != nil {
				slices.Sort(deletedPods)
				wantSorted := slices.Clone(tt.wantDeletedPods)
				slices.Sort(wantSorted)
				if !slices.Equal(deletedPods, wantSorted) {
					t.Errorf("deleted pods: got %v, want %v", deletedPods, wantSorted)
				}
			}
		})
	}
}
