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
	// The wait depends on the time of day, so only bound it here; the reset
	// instant itself is pinned in TestNextQuotaReset.
	backoff, reason := ingestionBackoff(0, true)
	if backoff <= 0 || backoff > 25*time.Hour {
		t.Errorf("daily-quota backoff = %v, want within (0, 25h] (until quota reset)", backoff)
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

// TestNextQuotaReset pins the clock to check the reset lands on the next
// midnight Pacific, including just before midnight and across DST changes.
func TestNextQuotaReset(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"just after midnight", time.Date(2026, 10, 20, 0, 0, 1, 0, loc), time.Date(2026, 10, 21, 0, 0, 0, 0, loc)},
		{"midday", time.Date(2026, 10, 20, 12, 0, 0, 0, loc), time.Date(2026, 10, 21, 0, 0, 0, 0, loc)},
		{"last hour before midnight", time.Date(2026, 10, 20, 23, 25, 0, 0, loc), time.Date(2026, 10, 21, 0, 0, 0, 0, loc)},
		{"exactly midnight", time.Date(2026, 10, 21, 0, 0, 0, 0, loc), time.Date(2026, 10, 22, 0, 0, 0, 0, loc)},
		{"UTC input", time.Date(2026, 10, 21, 6, 25, 0, 0, time.UTC), time.Date(2026, 10, 21, 0, 0, 0, 0, loc)},
		{"end of month", time.Date(2026, 10, 31, 18, 0, 0, 0, loc), time.Date(2026, 11, 1, 0, 0, 0, 0, loc)},
		{"spring forward", time.Date(2026, 3, 7, 22, 0, 0, 0, loc), time.Date(2026, 3, 8, 0, 0, 0, 0, loc)},
		{"fall back", time.Date(2026, 11, 1, 22, 0, 0, 0, loc), time.Date(2026, 11, 2, 0, 0, 0, 0, loc)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextQuotaReset(tc.now); !got.Equal(tc.want) {
				t.Errorf("nextQuotaReset(%v) = %v, want %v", tc.now, got.In(loc), tc.want)
			}
		})
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
