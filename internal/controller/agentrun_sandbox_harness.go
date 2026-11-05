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

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/prometheus/client_golang/prometheus"
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
	events, consumed := openshell.ParseClaudeStream(logs, run.Status.HarnessLinesSeen)
	if consumed <= run.Status.HarnessLinesSeen {
		return
	}
	key := "tokens:" + run.Namespace + ":" + run.Name
	ns, harness := run.Namespace, agent.Spec.Runtime.Harness
	for _, e := range events {
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
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.HarnessLinesSeen = consumed
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
