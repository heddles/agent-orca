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
	"fmt"
	"strconv"
	"sync"
	"time"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
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

// activePhases are AgentRun phases that consume a concurrent-run slot.
var activePhases = map[agentorcav1alpha1.AgentRunPhase]bool{
	agentorcav1alpha1.AgentRunPhasePending:         true,
	agentorcav1alpha1.AgentRunPhaseRunning:         true,
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
		if wait, ok := rl.Allow(tenant.TenantName, tenant.RateLimitRPM); !ok {
			return &QuotaError{
				Code:       429,
				Message:    fmt.Sprintf("rate limit exceeded for tenant %q (max %d/min)", tenant.TenantName, tenant.RateLimitRPM),
				RetryAfter: wait,
			}
		}
	}

	// 2. Concurrent-run cap (external-task runs only).
	if tenant.ConcurrentRuns > 0 && kube != nil {
		running, err := countActiveExternalRuns(ctx, kube, tenantNamespaces(tenant))
		if err != nil {
			// Don't block the tenant on a transient K8s error — log via caller; fail closed
			// only matters if we wanted to deny. We deny on uncertainty to avoid budget blowout.
			return &QuotaError{Code: 500, Message: fmt.Sprintf("checking concurrent runs: %s", err)}
		}
		if running >= tenant.ConcurrentRuns {
			return &QuotaError{
				Code:    429,
				Message: fmt.Sprintf("concurrent run limit (%d) reached for tenant %q; retry later", tenant.ConcurrentRuns, tenant.TenantName),
			}
		}
	}

	// 3. Daily budget cap.
	if tenant.BudgetPerDayUSD != "" && tenant.BudgetPerDayUSD != "0" && kube != nil {
		used, err := sumTodayTenantSpend(ctx, kube, tenant.TenantName)
		if err != nil {
			return &QuotaError{Code: 500, Message: fmt.Sprintf("checking budget: %s", err)}
		}
		cap, _ := strconv.ParseFloat(tenant.BudgetPerDayUSD, 64)
		if cap > 0 && used >= cap {
			return &QuotaError{
				Code:    402,
				Message: fmt.Sprintf("daily budget ($%s) exceeded for tenant %q (used $%s)", tenant.BudgetPerDayUSD, tenant.TenantName, strconv.FormatFloat(used, 'f', 6, 64)),
			}
		}
	}

	return nil
}

// countActiveExternalRuns counts AgentRuns in any of the tenant's authorized
// namespaces that are active (holding a concurrent-run slot), selected by the
// external-task label.
func countActiveExternalRuns(ctx context.Context, kube client.Client, namespaces []string) (int, error) {
	if len(namespaces) == 0 {
		return 0, fmt.Errorf("tenant has no authorized namespaces")
	}
	var list agentorcav1alpha1.AgentRunList
	for _, ns := range namespaces {
		var l agentorcav1alpha1.AgentRunList
		if err := kube.List(ctx, &l,
			client.InNamespace(ns),
			client.MatchingLabels{"agentorca.io/external-task": "true"},
		); err != nil {
			return 0, err
		}
		list.Items = append(list.Items, l.Items...)
	}
	n := 0
	for i := range list.Items {
		if activePhases[list.Items[i].Status.Phase] {
			n++
		}
	}
	return n, nil
}

// sumTodayTenantSpend sums status.spendUSD across all AgentRuns belonging to a
// tenant (by label) that were created today. Runs still in-flight contribute
// their last-reported status.spendUSD; runs with no spend field contribute 0.
func sumTodayTenantSpend(ctx context.Context, kube client.Client, tenantName string) (float64, error) {
	var list agentorcav1alpha1.AgentRunList
	if err := kube.List(ctx, &list,
		client.MatchingLabels{"agentorca.io/tenant": tenantName},
	); err != nil {
		return 0, err
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var total float64
	for i := range list.Items {
		r := &list.Items[i]
		if !r.CreationTimestamp.IsZero() && r.CreationTimestamp.UTC().Before(today) {
			continue
		}
		if r.Status.SpendUSD == "" {
			continue
		}
		if v, err := strconv.ParseFloat(r.Status.SpendUSD, 64); err == nil {
			total += v
		}
	}
	return total, nil
}
