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

// Package quota holds the tenant admission checks that read the cluster: how
// many runs a tenant has going and what it has spent today. The External API
// calls them before it creates a task, and the AgentSchedule controller calls
// them before it starts a scheduled run, so both count the same runs.
//
// The request rate limit is not here: it is a per-process token bucket that
// belongs to the API server. A schedule's minimum interval takes its place.
package quota

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Labels that put a run in the concurrency count: an External API task, or a
// run an AgentSchedule started.
const labelExternalTask = "agentorca.io/external-task"

// Error is a refusal. Code is the HTTP status the External API returns for it
// (429 over the concurrent-run limit, 402 over the daily budget, 500 when the
// cluster could not be read).
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Transient reports whether the check failed because the cluster could not be
// read, not because the tenant is over a limit.
func (e *Error) Transient() bool { return e.Code >= 500 }

// Limits is what a tenant may do and where its objects live.
type Limits struct {
	TenantName string
	TenantID   string
	// Namespaces are the tenant's authorized namespaces.
	Namespaces []string
	// ConcurrentRuns is the cap on active runs; zero means none.
	ConcurrentRuns int
	// BudgetPerDayUSD is the daily spend cap as a decimal string; empty or "0" means none.
	BudgetPerDayUSD string
}

// FromTenantConfig builds the limits of a TenantConfig.
func FromTenantConfig(tc *agentorcav1alpha1.TenantConfig) Limits {
	l := Limits{
		TenantName:      tc.Name,
		TenantID:        tenancy.TenantIDOf(tc),
		Namespaces:      tc.Spec.AllowedNamespaces,
		BudgetPerDayUSD: tc.Spec.BudgetPerDayUSD,
	}
	if tc.Spec.RateLimit != nil {
		l.ConcurrentRuns = tc.Spec.RateLimit.ConcurrentRuns
	}
	return l
}

// activePhases are the run phases that hold a concurrent-run slot.
var activePhases = map[agentorcav1alpha1.AgentRunPhase]bool{
	"":                                     true, // not yet picked up
	agentorcav1alpha1.AgentRunPhasePending: true,
	agentorcav1alpha1.AgentRunPhaseRunning: true,
	agentorcav1alpha1.AgentRunPhaseWaitingForInput: true,
}

// Check applies the concurrent-run cap and the daily budget. It returns nil
// when the tenant may start another run.
func Check(ctx context.Context, kube client.Client, l Limits) error {
	if l.TenantName == "" || kube == nil {
		return nil
	}
	if l.ConcurrentRuns > 0 {
		running, err := CountActiveRuns(ctx, kube, l.Namespaces, l.TenantID)
		if err != nil {
			// Deny on uncertainty: guessing could blow the budget.
			return &Error{Code: 500, Message: fmt.Sprintf("checking concurrent runs: %s", err)}
		}
		if running >= l.ConcurrentRuns {
			return &Error{Code: 429, Message: fmt.Sprintf("concurrent run limit (%d) reached for tenant %q; retry later", l.ConcurrentRuns, l.TenantName)}
		}
	}
	if l.BudgetPerDayUSD != "" && l.BudgetPerDayUSD != "0" {
		used, err := SumTodaySpend(ctx, kube, l.TenantName, l.TenantID, time.Now())
		if err != nil {
			return &Error{Code: 500, Message: fmt.Sprintf("checking budget: %s", err)}
		}
		if limit, _ := strconv.ParseFloat(l.BudgetPerDayUSD, 64); limit > 0 && used >= limit {
			return &Error{Code: 402, Message: fmt.Sprintf("daily budget ($%s) exceeded for tenant %q (used $%s)", l.BudgetPerDayUSD, l.TenantName, strconv.FormatFloat(used, 'f', 6, 64))}
		}
	}
	return nil
}

// CountActiveRuns counts the runs in the tenant's namespaces that hold a
// concurrent-run slot: External API tasks and runs started by a schedule.
func CountActiveRuns(ctx context.Context, kube client.Client, namespaces []string, tenantID string) (int, error) {
	if len(namespaces) == 0 {
		return 0, fmt.Errorf("tenant has no authorized namespaces")
	}
	n := 0
	for _, ns := range namespaces {
		seen := map[string]bool{}
		for _, sel := range []client.ListOption{
			client.MatchingLabels{labelExternalTask: "true"},
			client.HasLabels{schedule.LabelSchedule},
		} {
			var l agentorcav1alpha1.AgentRunList
			if err := kube.List(ctx, &l, client.InNamespace(ns), sel); err != nil {
				return 0, err
			}
			for i := range l.Items {
				r := &l.Items[i]
				if seen[r.Name] {
					continue
				}
				seen[r.Name] = true
				// Two tenants can share a namespace: a run labelled for another
				// tenant does not hold this tenant's slot. Unlabelled runs still count.
				if owner := r.Labels[tenancy.LabelTenantID]; tenantID != "" && owner != "" && owner != tenantID {
					continue
				}
				if activePhases[r.Status.Phase] {
					n++
				}
			}
		}
	}
	return n, nil
}

// SumTodaySpend sums status.spendUSD over the tenant's runs created today (UTC).
// Runs in flight contribute what they last reported.
func SumTodaySpend(ctx context.Context, kube client.Client, tenantName, tenantID string, now time.Time) (float64, error) {
	// Select by the stable ID when there is one: a same-name tenant recreated
	// later must not inherit this one's spend.
	selector := client.MatchingLabels{"agentorca.io/tenant": tenantName}
	if tenantID != "" {
		selector = client.MatchingLabels{tenancy.LabelTenantID: tenantID}
	}
	var list agentorcav1alpha1.AgentRunList
	if err := kube.List(ctx, &list, selector); err != nil {
		return 0, err
	}
	today := now.UTC().Truncate(24 * time.Hour)
	var total float64
	for i := range list.Items {
		r := &list.Items[i]
		if !r.CreationTimestamp.IsZero() && r.CreationTimestamp.UTC().Before(today) {
			continue
		}
		if v, err := strconv.ParseFloat(r.Status.SpendUSD, 64); err == nil {
			total += v
		}
	}
	return total, nil
}
