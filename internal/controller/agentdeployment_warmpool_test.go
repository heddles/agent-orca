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

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

// makeWarmPod creates an IDLE warm pool pod with the given name, creation time, and readiness.
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
				"agentorca.io/router-config-hash": configHash,
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

// makeClaimedWarmPod builds a warm pod currently bound to a run (must not be deleted by scale-down).
func makeClaimedWarmPod(name, deployName, configHash string, createdAt time.Time, ready bool) *corev1.Pod {
	p := makeWarmPod(name, deployName, configHash, createdAt, ready)
	p.Labels[labelWarmStatus] = warmStatusClaimed
	return p
}

// makeTerminatingWarmPod builds a warm pod already receiving a deletion (must be ignored, not recounted).
func makeTerminatingWarmPod(name, deployName, configHash string, createdAt time.Time, ready bool) *corev1.Pod {
	p := makeWarmPod(name, deployName, configHash, createdAt, ready)
	p.DeletionTimestamp = &metav1.Time{Time: createdAt}
	p.Finalizers = []string{"test.agentorca.io/warm-pool"} // required for a terminating object to be admitted by the fake client
	return p
}

// warmPodModelRouterReady mirrors the readiness check in reconcileWarmPool.
func warmPodModelRouterReady(p *corev1.Pod) bool {
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.Name == "model-router" && cs.Ready { //nolint:goconst
			return true
		}
	}
	return false
}

// TestScaleDownWarmPool exercises the scale-down portion of reconcileWarmPool using the
// SAME partitioning accounting (warmPoolSize = total idle + in-use; claimed pods count
// toward the total and are never deleted; terminating pods are ignored). It is a
// faithful mirror, not the real reconciler, because reconcileWarmPool requires the full
// AgentDeploymentReconciler wiring (SA tokens, buildWarmPod, etc.).
func TestScaleDownWarmPool(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name            string
		warmPoolSize    int
		pods            []*corev1.Pod
		wantRemaining   int // pods left in the fake client after scale-down
		wantReadyCount  int // idle-ready pods remaining after scale-down
		wantDeletedPods []string
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
			name:         "excess mixed - not-yet-ready starting pods deleted first",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("ready-old", "deploy-a", "hash1", now.Add(-20*time.Minute), true),
				makeWarmPod("ready-new", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
				makeWarmPod("starting-1", "deploy-a", "hash1", now.Add(-2*time.Minute), false),
				makeWarmPod("starting-2", "deploy-a", "hash1", now.Add(-1*time.Minute), false),
			},
			wantRemaining:   2,
			wantReadyCount:  2, // both ready pods kept; starting ones pruned
			wantDeletedPods: []string{"starting-1", "starting-2"},
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
			name:         "scale to zero deletes all idle pods",
			warmPoolSize: 0,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", "hash1", now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
			},
			wantRemaining:   0,
			wantReadyCount:  0,
			wantDeletedPods: []string{"ready-1", "ready-2"},
		},
		{
			// Regression: a claimed (in-use) warm pod must count toward the desired
			// total and must NOT be deleted. Under the old idle-only accounting this
			// case deleted nothing (claimed pods were invisible); the pool instead
			// over-provisioned and deleted the just-returned pod. Here total=3 vs
			// desired=2 => one excess IDLE pod is pruned, the claimed pod survives.
			name:         "claimed pod counts toward total and survives scale-down",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("idle-1", "deploy-a", "hash1", now.Add(-10*time.Minute), true),
				makeWarmPod("idle-2", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
				makeClaimedWarmPod("claimed", "deploy-a", "hash1", now.Add(-2*time.Minute), true),
			},
			wantRemaining:   2,
			wantReadyCount:  1,
			wantDeletedPods: []string{"idle-1"},
		},
		{
			// Regression: a pod already being terminated must be ignored by the
			// count (not recounted as excess and re-deleted on every reconcile).
			name:         "terminating pod is ignored, not recounted",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("idle-1", "deploy-a", "hash1", now.Add(-10*time.Minute), true),
				makeTerminatingWarmPod("terminating", "deploy-a", "hash1", now.Add(-5*time.Minute), true),
			},
			wantRemaining:   2,
			wantReadyCount:  1,
			wantDeletedPods: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = agentorcav1alpha1.AddToScheme(scheme)

			var deletedPods []string
			objs := make([]client.Object, len(tt.pods))
			for i, p := range tt.pods {
				objs[i] = p
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&agentorcav1alpha1.AgentDeployment{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if pod, ok := obj.(*corev1.Pod); ok {
							deletedPods = append(deletedPods, pod.Name)
						}
						return c.Delete(ctx, obj, opts...)
					},
				}).
				Build()

			// Mirror the scale-down accounting of reconcileWarmPool.
			var pods corev1.PodList
			if err := fakeClient.List(context.Background(), &pods,
				client.InNamespace("default"),
				client.MatchingLabels{labelWarmPool: "deploy-a"},
			); err != nil {
				t.Fatalf("listing pods: %v", err)
			}

			var idleReady, idleStarting, claimed []*corev1.Pod
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.DeletionTimestamp != nil {
					continue // terminating pods are ignored
				}
				if p.Labels[labelWarmStatus] == warmStatusClaimed {
					claimed = append(claimed, p)
					continue
				}
				if warmPodModelRouterReady(p) {
					idleReady = append(idleReady, p)
				} else {
					idleStarting = append(idleStarting, p)
				}
			}

			desired := tt.warmPoolSize
			readyCount := len(idleReady)
			startingCount := len(idleStarting)
			totalExisting := readyCount + startingCount + len(claimed)

			if excess := totalExisting - desired; excess > 0 {
				slices.SortFunc(idleStarting, func(a, b *corev1.Pod) int {
					return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
				})
				slices.SortFunc(idleReady, func(a, b *corev1.Pod) int {
					return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
				})
				deleted := 0
				for deleted < excess && len(idleStarting) > 0 {
					p := idleStarting[0]
					idleStarting = idleStarting[1:]
					_ = fakeClient.Delete(context.Background(), p)
					deleted++
				}
				for deleted < excess && len(idleReady) > 0 {
					p := idleReady[0]
					idleReady = idleReady[1:]
					_ = fakeClient.Delete(context.Background(), p)
					deleted++
				}
				readyCount = len(idleReady)
			}

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
