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
)

// TestIngestionBackoffExponential verifies generic failures back off
// exponentially with the consecutive-failure count, capped at 1h.
func TestIngestionBackoffExponential(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 1 * time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{5, 32 * time.Minute},
		{6, time.Hour},  // 64m capped at maxIngestionBackoff
		{7, time.Hour},  // shift capped, then maxIngestionBackoff
		{50, time.Hour}, // capped at maxIngestionBackoff
	}
	for _, tc := range tests {
		backoff, reason := ingestionBackoff(tc.failures, false)
		if backoff != tc.want {
			t.Errorf("ingestionBackoff(%d) = %v, want %v", tc.failures, backoff, tc.want)
		}
		if reason == "" {
			t.Errorf("ingestionBackoff(%d) returned empty reason", tc.failures)
		}
	}
}

// TestIngestionBackoffDailyQuota verifies daily-quota exhaustion waits until
// the provider's quota reset (next midnight Pacific) rather than a short
// exponential backoff — retrying sooner is guaranteed to fail.
func TestIngestionBackoffDailyQuota(t *testing.T) {
	backoff, reason := ingestionBackoff(0, true)
	if backoff <= time.Hour {
		t.Errorf("daily-quota backoff = %v, want > 1h (until quota reset)", backoff)
	}
	if reason != "embedding provider daily quota exhausted; waiting for quota reset" {
		t.Errorf("unexpected reason: %q", reason)
	}

	// The reset must land at midnight Pacific.
	reset := time.Now().Add(backoff)
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	local := reset.In(loc)
	if local.Hour() != 0 || local.Minute() != 0 || local.Second() > 5 {
		t.Errorf("quota reset = %v, want midnight Pacific", local)
	}
}

// TestPodsReportDailyQuota verifies the termination-message marker detection.
func TestPodsReportDailyQuota(t *testing.T) {
	markerPod := corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `2026-10-15T17:04:33Z ERROR QUOTA_EXHAUSTED_DAILY embedding provider daily quota exhausted; aborting ingestion until quota reset`,
						},
					},
				},
			},
		},
	}
	otherPod := corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `ingesting documents: document "x": embedding chunks: embeddings API returned 503: overloaded`,
						},
					},
				},
			},
		},
	}

	if !podsReportDailyQuota([]corev1.Pod{otherPod, markerPod}) {
		t.Error("marker pod not detected")
	}
	if podsReportDailyQuota([]corev1.Pod{otherPod}) {
		t.Error("non-quota failure misclassified as daily quota exhaustion")
	}
	if podsReportDailyQuota(nil) {
		t.Error("no pods should not report daily quota")
	}
}
