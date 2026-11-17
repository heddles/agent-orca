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

package runner

import (
	"context"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// tenantLimits are the parts of a TenantConfig that bound an experiment.
type tenantLimits struct {
	Name string
	// RateLimitRPM is spec.rateLimit.requestsPerMinute, 0 for none.
	RateLimitRPM int
	// ConcurrentRuns is spec.rateLimit.concurrentRuns, 0 for none.
	ConcurrentRuns int
	// DailyUSD is spec.budgetPerDayUSD, 0 for none.
	DailyUSD float64
}

func (r *Runner) tenantConfigs(ctx context.Context) ([]agentorcav1alpha1.TenantConfig, error) {
	var list agentorcav1alpha1.TenantConfigList
	if err := r.kube.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// limitsOf finds the tenant's limits among the TenantConfigs. A tenant with no
// TenantConfig has none.
func limitsOf(tcs []agentorcav1alpha1.TenantConfig, tenantID string) tenantLimits {
	for i := range tcs {
		tc := &tcs[i]
		if tenancy.TenantIDOf(tc) != tenantID {
			continue
		}
		l := tenantLimits{Name: tc.Name}
		if rl := tc.Spec.RateLimit; rl != nil {
			l.RateLimitRPM, l.ConcurrentRuns = rl.RequestsPerMinute, rl.ConcurrentRuns
		}
		if v, err := strconv.ParseFloat(tc.Spec.BudgetPerDayUSD, 64); err == nil && v > 0 {
			l.DailyUSD = v
		}
		return l
	}
	return tenantLimits{}
}

// runLimit is the most eval runs the tenant may have active at once: the
// experiment's concurrency, capped at 16 and at the tenant's ConcurrentRuns.
func runLimit(concurrency int, lim tenantLimits) (perExperiment, perTenant int) {
	perTenant = MaxTenantEvalRuns
	if lim.ConcurrentRuns > 0 && lim.ConcurrentRuns < perTenant {
		perTenant = lim.ConcurrentRuns
	}
	perExperiment = min(concurrency, MaxTenantEvalRuns, perTenant)
	return perExperiment, perTenant
}

// activeTenantEvalRuns counts the tenant's eval runs that hold a slot, in every
// experiment and namespace.
func (r *Runner) activeTenantEvalRuns(ctx context.Context, tenantID string) (int, error) {
	var list agentorcav1alpha1.AgentRunList
	if err := r.kube.List(ctx, &list, client.MatchingLabels{tenancy.LabelTenantID: tenantID, tenancy.LabelEval: tenancy.LabelEvalValue}); err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		if !isTerminal(&list.Items[i]) {
			n++
		}
	}
	return n, nil
}

// todaySpend sums the status.spendUSD of the tenant's runs created today (UTC),
// the same figure the external API's daily budget check uses. Eval runs carry
// the tenant's ID label, so they are in it: an experiment's spend counts
// towards the tenant's daily budget.
func (r *Runner) todaySpend(ctx context.Context, tenantID string) (float64, error) {
	var list agentorcav1alpha1.AgentRunList
	if err := r.kube.List(ctx, &list, client.MatchingLabels{tenancy.LabelTenantID: tenantID}); err != nil {
		return 0, err
	}
	today := r.cfg.Now().UTC().Truncate(24 * time.Hour)
	var total float64
	// Judge calls are not on an AgentRun: they are on the grades and, for the
	// canary check, the experiment.
	judged, err := r.store.EvalJudgeSpendSince(ctx, r.cfg.Now().UTC().Truncate(24*time.Hour))
	if err != nil {
		return 0, err
	}
	total += judged
	for i := range list.Items {
		run := &list.Items[i]
		if !run.CreationTimestamp.IsZero() && run.CreationTimestamp.UTC().Before(today) {
			continue
		}
		if v, err := strconv.ParseFloat(run.Status.SpendUSD, 64); err == nil {
			total += v
		}
	}
	return total, nil
}

// runCap is the spend cap of the next run: the per-run cap, an even share of
// what the experiment has left among the runs in flight and this one, and what
// is left once the caps already given to runs in flight are set aside. A cap
// under minRunBudgetUSD means no run can start now.
func runCap(runCapUSD, budgetUSD, spentUSD, committedUSD float64, inflight int) float64 {
	remaining := budgetUSD - spentUSD
	c := runCapUSD
	if share := remaining / float64(inflight+1); share < c {
		c = share
	}
	if free := remaining - committedUSD; free < c {
		c = free
	}
	if c < 0 {
		return 0
	}
	return c
}
