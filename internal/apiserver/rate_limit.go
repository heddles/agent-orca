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

package apiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/quota"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runTerminalPhases are the AgentRun phases considered "done" for spend/budget
// accounting. A run in one of these phases has finished accruing cost.
var runTerminalPhases = map[agentorcav1alpha1.AgentRunPhase]bool{ //nolint:unused

	agentorcav1alpha1.AgentRunPhaseSucceeded:       true,
	agentorcav1alpha1.AgentRunPhaseFailed:          true,
	agentorcav1alpha1.AgentRunPhaseHandedOff:       true,
	agentorcav1alpha1.AgentRunPhaseWaitingForInput: true,
}

// QuotaError is returned by enforceQuotas and maps directly to an HTTP response
// so callers can translate it without inspecting strings.
type QuotaError struct {
	Code       int
	Message    string
	RetryAfter time.Duration // >0 only for 429
}

func (e *QuotaError) Error() string { return e.Message }

// RateLimiter is a simple, single-process token bucket scoped per tenant.
// It is sufficient for a single-replica operator; in a multi-replica deployment
// each instance enforces independently (documented limitation — a Redis-backed
// limiter can replace the backend without changing call sites).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tenantBucket
}

type tenantBucket struct {
	rate   float64 // tokens per second
	burst  int     // max burst / peak tokens
	tokens float64
	last   time.Time
}

// NewRateLimiter creates a rate limiter with no buckets (lazy population).
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: make(map[string]*tenantBucket)}
}

// Allow returns (retryAfter, ok). ok=false means the request is rejected;
// retryAfter is the duration the caller should wait before retrying.
func (rl *RateLimiter) Allow(tenant string, requestsPerMinute int) (time.Duration, bool) {
	if requestsPerMinute <= 0 || tenant == "" {
		return 0, true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[tenant]
	if !ok {
		rpm := float64(requestsPerMinute)
		b = &tenantBucket{
			rate:   rpm / 60.0,
			burst:  requestsPerMinute,
			tokens: rpm, // start full
			last:   now,
		}
		rl.buckets[tenant] = b
	}
	// Refill based on elapsed time.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * b.rate
	if b.tokens > float64(b.burst) {
		b.tokens = float64(b.burst)
	}
	b.last = now
	if b.tokens < 1 {
		retry := max(time.Duration((1-b.tokens)/b.rate)*time.Second, 1*time.Second)
		return retry, false
	}
	b.tokens -= 1
	return 0, true
}

// quotaKey names a tenant's rate-limit bucket: its ID, so two tenants that
// reuse a name never share a bucket. Tenants without an ID (legacy tokens) fall
// back to the name, kept apart from IDs by the prefix.
func quotaKey(t *TenantIdentity) string {
	if t.TenantID != "" {
		return t.TenantID
	}
	return "name:" + t.TenantName
}

// enforceQuotas checks the three tenant-level admission controls before a task
// is actually created:
//   - requestsPerMinute: per-tenant token-bucket rate limit (429)
//   - concurrentRuns:    count of active external-task runs in the namespace (429)
//   - budgetPerDayUSD:   summed spend across today's runs for the tenant (402)
//
// It is intentionally K8s-backed (no Redis dependency) so it works even when the
// state store is unconfigured.
func enforceQuotas(
	ctx context.Context,
	kube client.Client,
	rl *RateLimiter,
	tenant *TenantIdentity,
) error {
	if tenant == nil || tenant.TenantName == "" {
		return nil
	}

	// 1. Per-tenant request rate limit (token bucket, in memory).
	if tenant.RateLimitRPM > 0 && rl != nil {
		if wait, ok := rl.Allow(quotaKey(tenant), tenant.RateLimitRPM); !ok {
			return &QuotaError{
				Code:       429,
				Message:    fmt.Sprintf("rate limit exceeded for tenant %q (max %d/min)", tenant.TenantName, tenant.RateLimitRPM),
				RetryAfter: wait,
			}
		}
	}

	// 2. Concurrent-run cap and 3. daily budget, shared with the schedule
	// controller so both count the same runs.
	if kube != nil {
		err := quota.Check(ctx, kube, quota.Limits{
			TenantName:      tenant.TenantName,
			TenantID:        tenant.TenantID,
			Namespaces:      tenantNamespaces(tenant),
			ConcurrentRuns:  tenant.ConcurrentRuns,
			BudgetPerDayUSD: tenant.BudgetPerDayUSD,
		})
		var qe *quota.Error
		if errors.As(err, &qe) {
			return &QuotaError{Code: qe.Code, Message: qe.Message}
		}
	}

	return nil
}
