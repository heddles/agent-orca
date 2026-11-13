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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
)

// traceEventTypeHarness is the trace event for something a coding-agent CLI did.
const traceEventTypeHarness = "harness"

var (
	harnessToolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_harness_tool_calls_total",
		Help: "Tool calls made by coding-agent CLI harness runs, by harness and tool.",
	}, []string{"namespace", "harness", "tool"})
	harnessTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_harness_tokens_total",
		Help: "Tokens used by coding-agent CLI harness runs, by harness and type (input, output, cache_read, cache_creation).",
	}, []string{"namespace", "harness", "type"})
	harnessUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agentorca_harness_subscription_utilization",
		Help: "Share (0 to 1) of a subscription window a coding-agent CLI reported as used, by harness and window.",
	}, []string{"namespace", "harness", "window"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(harnessToolCalls, harnessTokens, harnessUtilization)
}

// recordHarnessEvents copies what a coding-agent CLI did into the run's trace.
// The sandbox supervisor reports only network connections, so the CLI's own
// output stream is where tool calls, token usage and subscription use show up.
// It reads the workload's logs, not the run's stored output, which is capped.
// A failure is logged and retried on the next poll from the same line.
func (r *AgentRunReconciler) recordHarnessEvents(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent, sb *osv1.Sandbox) {
	if r.StateStore == nil || sb == nil || agent.Spec.Runtime.Harness != openshell.HarnessClaudeCode {
		return
	}
	logger := log.FromContext(ctx)
	logs, err := r.Sandboxes.MainProcessLogs(ctx, sb)
	if err != nil {
		logger.V(1).Info("reading harness output", "sandbox", sb.Name, "err", err.Error())
		return
	}
	runKey := run.Namespace + "/" + run.Name + "/" + string(run.UID) // a recreated run has a new UID
	skip := run.Status.HarnessLinesSeen
	if n, ok := r.harnessRecorded.Load(runKey); ok && n.(int) > skip {
		skip = n.(int) // the cached run is behind a patch made moments ago
	}
	events, consumed := openshell.ParseClaudeStream(logs, skip)
	if consumed <= skip {
		return
	}
	key := "tokens:" + run.Namespace + ":" + run.Name
	ns, harness := run.Namespace, agent.Spec.Runtime.Harness
	model := ""
	spent := 0.0
	for _, e := range events {
		if e.Kind == openshell.HarnessInit && e.Model != "" {
			model = e.Model
		}
		if e.Kind == openshell.HarnessResult && e.CostUSD > 0 {
			spent += e.CostUSD
		}
		ev, err := json.Marshal(struct {
			Type string `json:"type"`
			openshell.HarnessEvent
		}{traceEventTypeHarness, e})
		if err != nil {
			continue
		}
		if err := r.StateStore.SaveTraceEvent(ctx, key, string(ev)); err != nil {
			logger.Error(err, "recording a harness event in the trace", "sandbox", sb.Name)
			return // retry from the same line next poll
		}
		observeHarnessEvent(ns, harness, e)
	}
	r.harnessRecorded.Store(runKey, consumed)
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.HarnessLinesSeen = consumed
	if spent > 0 {
		// The model-router is not in the path, so nothing else records this run's spend.
		// The CLI reports a list-price equivalent, which on a subscription is a measure of
		// how much of the usage window the run took, not a bill. Each line is read once
		// (the watermark above), so adding is safe.
		prev, _ := strconv.ParseFloat(run.Status.SpendUSD, 64)
		run.Status.SpendUSD = fmt.Sprintf("%.6f", prev+spent)
	}
	if model != "" && !slices.ContainsFunc(run.Status.RoutingDecisions, func(d agentorcav1alpha1.RoutingDecision) bool { return d.Provider == harness }) {
		// The model-router is not in the path, so the model that answered is the one
		// the CLI reports. Recording it as the run's last decision makes it the run's
		// resolved model in the UI, ahead of the selector's unused candidates.
		now := metav1.Now()
		run.Status.RoutingDecisions = append(run.Status.RoutingDecisions, agentorcav1alpha1.RoutingDecision{
			Model: model, Provider: harness, Strategy: "harness", Reason: "model reported by the " + harness + " CLI", Timestamp: &now,
		})
	}
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		logger.Error(err, "recording the harness output watermark")
	}
}

func observeHarnessEvent(ns, harness string, e openshell.HarnessEvent) {
	switch e.Kind {
	case openshell.HarnessToolUse:
		harnessToolCalls.WithLabelValues(ns, harness, e.Tool).Inc()
	case openshell.HarnessResult:
		if u := e.Usage; u != nil {
			harnessTokens.WithLabelValues(ns, harness, "input").Add(float64(u.InputTokens))
			harnessTokens.WithLabelValues(ns, harness, "output").Add(float64(u.OutputTokens))
			harnessTokens.WithLabelValues(ns, harness, "cache_read").Add(float64(u.CacheReadTokens))
			harnessTokens.WithLabelValues(ns, harness, "cache_creation").Add(float64(u.CacheCreationTokens))
		}
	case openshell.HarnessRateLimit:
		for window, used := range e.Utilization {
			harnessUtilization.WithLabelValues(ns, harness, window).Set(used)
		}
	}
}
