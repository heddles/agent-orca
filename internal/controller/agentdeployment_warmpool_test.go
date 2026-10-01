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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	p.Finalizers = []string{"test.agentorca.io/warm-pool"}
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

// TestScaleDownWarmPool exercises the real scale-down decision path of reconcileWarmPool:
// pods are partitioned with classifyWarmPod and the excess set is chosen by
// warmScaleDownPlan. Only the List/Delete plumbing around them is omitted.
func TestScaleDownWarmPool(t *testing.T) {
	now := time.Now()
	const configHash = "hash1"
	// No age recycling; config-drift recycling on (all pods carry the current hash).
	deploy := warmDeployForLifecycle(0, true, 0)

	tests := []struct {
		name            string
		warmPoolSize    int
		pods            []*corev1.Pod
		wantReadyCount  int // idle-ready pods remaining after scale-down
		wantDeletedPods []string
	}{
		{
			name:         "no excess pods",
			warmPoolSize: 3,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", configHash, now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", configHash, now.Add(-5*time.Minute), true),
			},
			wantReadyCount: 2,
		},
		{
			name:         "exact match",
			warmPoolSize: 3,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", configHash, now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", configHash, now.Add(-5*time.Minute), true),
				makeWarmPod("ready-3", "deploy-a", configHash, now.Add(-1*time.Minute), true),
			},
			wantReadyCount: 3,
		},
		{
			name:         "excess ready pods - oldest deleted first",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("oldest", "deploy-a", configHash, now.Add(-30*time.Minute), true),
				makeWarmPod("middle", "deploy-a", configHash, now.Add(-15*time.Minute), true),
				makeWarmPod("newest-1", "deploy-a", configHash, now.Add(-5*time.Minute), true),
				makeWarmPod("newest-2", "deploy-a", configHash, now.Add(-1*time.Minute), true),
			},
			wantReadyCount:  2,
			wantDeletedPods: []string{"oldest", "middle"},
		},
		{
			name:         "excess mixed - not-yet-ready starting pods deleted first",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("ready-old", "deploy-a", configHash, now.Add(-20*time.Minute), true),
				makeWarmPod("ready-new", "deploy-a", configHash, now.Add(-5*time.Minute), true),
				makeWarmPod("starting-1", "deploy-a", configHash, now.Add(-2*time.Minute), false),
				makeWarmPod("starting-2", "deploy-a", configHash, now.Add(-1*time.Minute), false),
			},
			wantReadyCount:  2, // both ready pods kept; starting ones pruned
			wantDeletedPods: []string{"starting-1", "starting-2"},
		},
		{
			name:         "all starting pods - excess deleted",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("starting-1", "deploy-a", configHash, now.Add(-10*time.Minute), false),
				makeWarmPod("starting-2", "deploy-a", configHash, now.Add(-5*time.Minute), false),
				makeWarmPod("starting-3", "deploy-a", configHash, now.Add(-2*time.Minute), false),
				makeWarmPod("starting-4", "deploy-a", configHash, now.Add(-1*time.Minute), false),
			},
			wantReadyCount:  0,
			wantDeletedPods: []string{"starting-1", "starting-2"},
		},
		{
			name:         "scale to zero deletes all idle pods",
			warmPoolSize: 0,
			pods: []*corev1.Pod{
				makeWarmPod("ready-1", "deploy-a", configHash, now.Add(-10*time.Minute), true),
				makeWarmPod("ready-2", "deploy-a", configHash, now.Add(-5*time.Minute), true),
			},
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
				makeWarmPod("idle-1", "deploy-a", configHash, now.Add(-10*time.Minute), true),
				makeWarmPod("idle-2", "deploy-a", configHash, now.Add(-5*time.Minute), true),
				makeClaimedWarmPod("claimed", "deploy-a", configHash, now.Add(-2*time.Minute), true),
			},
			wantReadyCount:  1,
			wantDeletedPods: []string{"idle-1"},
		},
		{
			// Regression: a pod already being terminated must be ignored by the
			// count (not recounted as excess and re-deleted on every reconcile).
			name:         "terminating pod is ignored, not recounted",
			warmPoolSize: 2,
			pods: []*corev1.Pod{
				makeWarmPod("idle-1", "deploy-a", configHash, now.Add(-10*time.Minute), true),
				makeTerminatingWarmPod("terminating", "deploy-a", configHash, now.Add(-5*time.Minute), true),
			},
			wantReadyCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Partition pods exactly like reconcileWarmPool: skip terminating pods,
			// classify the rest, split idle by model-router readiness.
			var idleReady, idleStarting, claimed []*corev1.Pod
			for _, p := range tt.pods {
				if p.DeletionTimestamp != nil {
					continue // terminating pods are ignored
				}
				disp, _ := classifyWarmPod(p, deploy, configHash, now)
				switch disp {
				case warmDisposeClaimed:
					claimed = append(claimed, p)
				case warmDisposeIdle:
					if warmPodModelRouterReady(p) {
						idleReady = append(idleReady, p)
					} else {
						idleStarting = append(idleStarting, p)
					}
				}
			}

			// warmPoolSize is the TOTAL pool (idle + claimed), mirroring reconcileWarmPool.
			excess := len(idleReady) + len(idleStarting) + len(claimed) - tt.warmPoolSize

			var deleted []string
			readyRemaining := len(idleReady)
			if excess > 0 {
				toDelete, keepReady, _ := warmScaleDownPlan(idleReady, idleStarting, excess)
				for _, p := range toDelete {
					deleted = append(deleted, p.Name)
				}
				readyRemaining = len(keepReady)
			}

			if readyRemaining != tt.wantReadyCount {
				t.Errorf("ready pods remaining: got %d, want %d", readyRemaining, tt.wantReadyCount)
			}
			slices.Sort(deleted)
			wantSorted := slices.Clone(tt.wantDeletedPods)
			slices.Sort(wantSorted)
			if !slices.Equal(deleted, wantSorted) {
				t.Errorf("deleted pods: got %v, want %v", deleted, wantSorted)
			}
		})
	}
}
