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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func warmDeployForLifecycle(maxAge time.Duration, recycleOnDrift bool, maxRequests int) *agentorcav1alpha1.AgentDeployment {
	return &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentDeploymentSpec{
			WarmPodMaxAge:        &metav1.Duration{Duration: maxAge},
			RecycleOnConfigDrift: &recycleOnDrift,
			MaxRequestsPerPod:    maxRequests,
		},
	}
}

func TestEffectiveWarmPodMaxAge(t *testing.T) {
	tests := []struct {
		name   string
		deploy *agentorcav1alpha1.AgentDeployment
		want   time.Duration
	}{
		{name: "nil deploy defaults to 0 (indefinite)", deploy: nil, want: 0},
		{name: "nil spec max age defaults to 0 (indefinite)", deploy: &agentorcav1alpha1.AgentDeployment{}, want: 0},
		{name: "explicit 2h", deploy: warmupDeploy(2 * time.Hour), want: 2 * time.Hour},
		{name: "explicit 0 disables", deploy: warmupDeploy(0), want: 0},
		{name: "explicit 10m below floor", deploy: warmupDeploy(10 * time.Minute), want: 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// warmPodMaxAge(0) means "disabled"; only nil/nil-spec gets the default.
			got := effectiveWarmPodMaxAge(tt.deploy)
			if got != tt.want {
				t.Fatalf("effectiveWarmPodMaxAge=%v want %v", got, tt.want)
			}
		})
	}
}

func warmupDeploy(d time.Duration) *agentorcav1alpha1.AgentDeployment {
	return &agentorcav1alpha1.AgentDeployment{
		Spec: agentorcav1alpha1.AgentDeploymentSpec{
			WarmPodMaxAge: &metav1.Duration{Duration: d},
		},
	}
}

func TestEffectiveWarmLocalCache(t *testing.T) {
	tests := []struct {
		name   string
		deploy *agentorcav1alpha1.AgentDeployment
		want   bool
	}{
		{name: "nil deploy", deploy: nil, want: false},
		{name: "no warm pool, no explicit setting -> disabled", deploy: &agentorcav1alpha1.AgentDeployment{}, want: false},
		{name: "warm pool set, no explicit setting -> defaults on", deploy: &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{WarmPoolSize: 2}}, want: true},
		{name: "warm pool set but explicit false -> disabled", deploy: &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{WarmPoolSize: 2, WarmLocalCache: ptr.To(false)}}, want: false},
		{name: "no warm pool but explicit true -> still on", deploy: &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{WarmLocalCache: ptr.To(true)}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveWarmLocalCache(tt.deploy); got != tt.want {
				t.Fatalf("effectiveWarmLocalCache=%v want %v", got, tt.want)
			}
		})
	}
}

func TestWarmLocalCacheSizeMiValue(t *testing.T) {
	tests := []struct {
		name   string
		deploy *agentorcav1alpha1.AgentDeployment
		want   int
	}{
		{name: "nil deploy -> default 256", deploy: nil, want: 256},
		{name: "zero -> default 256", deploy: &agentorcav1alpha1.AgentDeployment{}, want: 256},
		{name: "explicit 512", deploy: &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{WarmLocalCacheSizeMi: 512}}, want: 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warmLocalCacheSizeMiValue(tt.deploy); got != tt.want {
				t.Fatalf("warmLocalCacheSizeMiValue=%d want %d", got, tt.want)
			}
		})
	}
}

func TestRecycleOnConfigDrift(t *testing.T) {
	if !recycleOnConfigDrift(nil) {
		t.Fatal("nil deploy should default to recycling on config drift (true)")
	}
	if !recycleOnConfigDrift(&agentorcav1alpha1.AgentDeployment{}) {
		t.Fatal("nil spec should default to recycling on config drift (true)")
	}
	if !recycleOnConfigDrift(warmupDeployWithDrift(true)) {
		t.Fatal("explicit true should recycle on drift")
	}
	if recycleOnConfigDrift(warmupDeployWithDrift(false)) {
		t.Fatal("explicit false should NOT recycle on drift")
	}
}

func warmupDeployWithDrift(v bool) *agentorcav1alpha1.AgentDeployment {
	return &agentorcav1alpha1.AgentDeployment{
		Spec: agentorcav1alpha1.AgentDeploymentSpec{RecycleOnConfigDrift: ptr.To(v)},
	}
}

func TestWarmPodTokenExpirySeconds(t *testing.T) {
	const hour = int64(3600)
	const year = int64(8760 * 3600)
	tests := []struct {
		name   string
		deploy *agentorcav1alpha1.AgentDeployment
		want   int64
	}{
		{name: "nil defaults to 1-year token", deploy: nil, want: year},
		{name: "nil spec defaults to 1-year token", deploy: &agentorcav1alpha1.AgentDeployment{}, want: year},
		{name: "pod max age 2h -> 2h token", deploy: warmupDeploy(2 * time.Hour), want: int64((2 * time.Hour).Seconds())},
		{name: "pod max age 5m (below 1h floor) -> 1h token", deploy: warmupDeploy(5 * time.Minute), want: hour},
		{name: "disabled (0) -> 1 year token", deploy: warmupDeploy(0), want: year},
		{name: "pod max age 10000h -> clamped to 1 year", deploy: warmupDeploy(10000 * time.Hour), want: year},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warmPodTokenExpirySeconds(tt.deploy)
			if got != tt.want {
				t.Fatalf("warmPodTokenExpirySeconds=%d want %d", got, tt.want)
			}
		})
	}
}

func TestClassifyWarmPod(t *testing.T) {
	now := time.Now()
	hash := "hash1"

	tests := []struct {
		name       string
		deploy     *agentorcav1alpha1.AgentDeployment
		pod        *corev1.Pod
		wantDisp   warmPodDisposition
		wantReason string
	}{
		{
			name:     "terminal phase is recycled",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 0),
			pod:      withPhase(makeWarmPod("p", "dep", hash, now.Add(-1*time.Minute), true), corev1.PodSucceeded),
			wantDisp: warmDisposeRecycle, wantReason: "terminal-phase",
		},
		{
			name:     "claimed pod is never recycled (counts toward total)",
			deploy:   warmDeployForLifecycle(1*time.Minute, true, 0),
			pod:      makeClaimedWarmPod("p", "dep", hash, now.Add(-5*time.Minute), true),
			wantDisp: warmDisposeClaimed,
		},
		{
			name:     "idle pod under age is kept",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 0),
			pod:      makeWarmPod("p", "dep", hash, now.Add(-1*time.Minute), true),
			wantDisp: warmDisposeIdle,
		},
		{
			name:     "idle pod over age is recycled",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 0),
			pod:      makeWarmPod("p", "dep", hash, now.Add(-55*time.Minute), true),
			wantDisp: warmDisposeRecycle, wantReason: "token-age-expired",
		},
		{
			name:     "age recycling disabled (0) keeps old pod",
			deploy:   warmDeployForLifecycle(0, true, 0),
			pod:      makeWarmPod("p", "dep", hash, now.Add(-24*time.Hour), true),
			wantDisp: warmDisposeIdle,
		},
		{
			// Regression: the DEFAULT (nil WarmPodMaxAge) must keep an old idle
			// pod indefinitely — age recycling is opt-in, not opt-out.
			name:     "default nil max age keeps old pod indefinitely",
			deploy:   &agentorcav1alpha1.AgentDeployment{Spec: agentorcav1alpha1.AgentDeploymentSpec{RecycleOnConfigDrift: ptr.To(true), MaxRequestsPerPod: 0}},
			pod:      makeWarmPod("p", "dep", hash, now.Add(-24*time.Hour), true),
			wantDisp: warmDisposeIdle,
		},
		{
			name:     "stale config recycled when RecycleOnConfigDrift=true",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 0),
			pod:      makeWarmPod("p", "dep", "old-hash", now.Add(-1*time.Minute), true),
			wantDisp: warmDisposeRecycle, wantReason: "stale-config",
		},
		{
			name:     "stale config kept when RecycleOnConfigDrift=false",
			deploy:   warmDeployForLifecycle(50*time.Minute, false, 0),
			pod:      makeWarmPod("p", "dep", "old-hash", now.Add(-1*time.Minute), true),
			wantDisp: warmDisposeIdle,
		},
		{
			name:     "matching config not recycled",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 0),
			pod:      makeWarmPod("p", "dep", hash, now.Add(-1*time.Minute), true),
			wantDisp: warmDisposeIdle,
		},
		{
			name:     "request cap reached is recycled",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 3),
			pod:      withLabel(makeWarmPod("p", "dep", hash, now.Add(-1*time.Minute), true), labelWarmRequests, "3"),
			wantDisp: warmDisposeRecycle, wantReason: "request-cap-3",
		},
		{
			name:     "request cap not reached is kept",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 3),
			pod:      withLabel(makeWarmPod("p", "dep", hash, now.Add(-1*time.Minute), true), labelWarmRequests, "2"),
			wantDisp: warmDisposeIdle,
		},
		{
			name:     "claimed pod short-circuits request-cap check",
			deploy:   warmDeployForLifecycle(1*time.Minute, true, 1),
			pod:      withLabel(makeClaimedWarmPod("p", "dep", hash, now.Add(-5*time.Minute), true), labelWarmRequests, "5"),
			wantDisp: warmDisposeClaimed,
		},
		{
			name:     "failed pod is recycled even if over age would also recycle",
			deploy:   warmDeployForLifecycle(50*time.Minute, true, 0),
			pod:      withPhase(makeWarmPod("p", "dep", hash, now.Add(-55*time.Minute), true), corev1.PodFailed),
			wantDisp: warmDisposeRecycle, wantReason: "terminal-phase",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disp, reason := classifyWarmPod(tt.pod, tt.deploy, hash, now)
			if disp != tt.wantDisp {
				t.Fatalf("disp=%q want %q (reason=%q)", disp, tt.wantDisp, reason)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason=%q want %q", reason, tt.wantReason)
			}
		})
	}
}

func withPhase(p *corev1.Pod, ph corev1.PodPhase) *corev1.Pod {
	p.Status.Phase = ph
	return p
}

func withLabel(p *corev1.Pod, k, v string) *corev1.Pod {
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	p.Labels[k] = v
	return p
}
