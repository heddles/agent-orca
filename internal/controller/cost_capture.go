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
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	// conditionCostCapture records whether Status.SpendUSD can be trusted. Without
	// it an empty SpendUSD is ambiguous: a run that cost nothing and a run whose
	// spend could not be read look the same, and the second is billed work that
	// silently disappears from every cost view.
	conditionCostCapture = "CostCapture"
	// reasonCostComplete: the run's spend was read from the state store.
	reasonCostComplete = "Complete"
	// reasonCostIncomplete: the spend could not be read; Status.SpendUSD (if set)
	// is only a lower bound from an earlier read.
	reasonCostIncomplete = "Incomplete"

	// costCaptureRetryWindow bounds how long a terminal run keeps retrying a
	// failed spend read, so a permanently unreachable store cannot keep every
	// finished run in a requeue loop.
	costCaptureRetryWindow = 10 * time.Minute
	// costCaptureRetryInterval is the delay between those retries.
	costCaptureRetryInterval = 30 * time.Second
)

// errSpendStoreUnavailable is returned when there is no state store to read the
// run's spend from (e.g. the dev profile).
var errSpendStoreUnavailable = errors.New("no state store configured: run spend is not tracked")

// loadSpend reads the run's cumulative spend from the state store. It returns
// "" with a nil error for a run that genuinely recorded no spend, and a non-nil
// error whenever the answer is unknown, so callers can tell "free" from
// "untracked".
func (r *AgentRunReconciler) loadSpend(ctx context.Context, runName string) (string, error) {
	if r.StateStore == nil {
		return "", errSpendStoreUnavailable
	}
	key := fmt.Sprintf("agentorca/runs/%s/state", runName)
	usd, err := r.StateStore.LoadSpend(ctx, key)
	if err != nil {
		return "", fmt.Errorf("loading spend from %s: %w", key, err)
	}
	if usd == 0 {
		return "", nil
	}
	return fmt.Sprintf("%.6f", usd), nil
}

// stampSpend writes the run's spend and the CostCapture condition into
// run.Status (the caller patches it). A failed read never blanks spend that an
// earlier stamp already recorded: that value is a lower bound, and the
// condition says it is incomplete.
func (r *AgentRunReconciler) stampSpend(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	spend, err := r.loadSpend(ctx, run.Name)
	if err != nil {
		log.FromContext(ctx).Error(err, "run spend could not be captured", "run", run.Name)
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               conditionCostCapture,
			Status:             metav1.ConditionFalse,
			Reason:             reasonCostIncomplete,
			Message:            err.Error(),
			ObservedGeneration: run.Generation,
		})
		return
	}
	// A successful read of zero must not erase a partial value stamped earlier:
	// the store key can expire, which reads back as zero.
	if spend != "" || run.Status.SpendUSD == "" {
		run.Status.SpendUSD = spend
	}
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               conditionCostCapture,
		Status:             metav1.ConditionTrue,
		Reason:             reasonCostComplete,
		Message:            "run spend read from the state store",
		ObservedGeneration: run.Generation,
	})
}

// costCaptureIncomplete reports whether run's spend is flagged as not captured.
func costCaptureIncomplete(run *agentorcav1alpha1.AgentRun) bool {
	c := meta.FindStatusCondition(run.Status.Conditions, conditionCostCapture)
	return c != nil && c.Status == metav1.ConditionFalse
}

// spendStamped reports whether run already carries a definitive spend: a value,
// or a successful read that found none.
func spendStamped(run *agentorcav1alpha1.AgentRun) bool {
	if costCaptureIncomplete(run) {
		return false
	}
	return run.Status.SpendUSD != "" || meta.IsStatusConditionTrue(run.Status.Conditions, conditionCostCapture)
}

// ensureSpendStamped makes sure a terminal run carries its spend or a
// CostCapture=Incomplete condition. It covers runs the model-router ended itself
// through the internal API (_done, _fail), which never pass through the pod
// success or failure handlers that stamp spend. It is idempotent: a stamped run
// costs one condition lookup. While a read keeps failing, it asks to be retried
// for costCaptureRetryWindow after completion.
func (r *AgentRunReconciler) ensureSpendStamped(ctx context.Context, run *agentorcav1alpha1.AgentRun) (time.Duration, error) {
	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded && run.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		return 0, nil
	}
	if spendStamped(run) {
		return 0, nil
	}

	before := run.DeepCopy()
	r.stampSpend(ctx, run)
	if !equality.Semantic.DeepEqual(before.Status, run.Status) {
		if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
			return 0, fmt.Errorf("patching spend: %w", err)
		}
		// The archive row was written before the spend was known; refresh it.
		r.maybeArchiveRun(run)
	}

	if costCaptureIncomplete(run) && run.Status.CompletionTime != nil &&
		time.Since(run.Status.CompletionTime.Time) < costCaptureRetryWindow {
		return costCaptureRetryInterval, nil
	}
	return 0, nil
}
