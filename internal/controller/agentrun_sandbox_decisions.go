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
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
)

// traceEventTypeEgress is the trace event for a sandbox network decision.
const traceEventTypeEgress = "egress"

// openshellDecision is embedded in egress trace events, flattening its fields.
type openshellDecision = openshell.Decision

// sandboxDecisions counts the network decisions sandbox supervisors made, by
// kind (net, http) and action (allowed, denied).
var sandboxDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "agentorca_sandbox_egress_decisions_total",
	Help: "Network decisions made by OpenShell sandbox supervisors, by kind and action.",
}, []string{"namespace", "kind", "action"})

func init() {
	ctrlmetrics.Registry.MustRegister(sandboxDecisions)
}

// recordSandboxDecisions copies the sandbox's new network decisions into the
// run's trace as egress events, so the run timeline shows every connection
// and request the agent made and whether the policy allowed it. A failure is
// logged and retried on the next poll from the same point.
func (r *AgentRunReconciler) recordSandboxDecisions(ctx context.Context, run *agentorcav1alpha1.AgentRun, sb *osv1.Sandbox) {
	if r.StateStore == nil || sb == nil || sb.Name == "" {
		return
	}
	logger := log.FromContext(ctx)
	// MicroTime: a watermark rounded to the second would re-read, and
	// duplicate, that second's decisions.
	var since time.Time
	if run.Status.SandboxDecisionsSeen != nil {
		since = run.Status.SandboxDecisionsSeen.Time
	} else if run.Status.StartTime != nil {
		since = run.Status.StartTime.Time
	}
	decisions, err := r.Sandboxes.Decisions(ctx, sb.Name, since)
	if err != nil {
		logger.V(1).Info("reading sandbox decisions", "sandbox", sb.Name, "err", err.Error())
		return
	}
	if len(decisions) == 0 {
		return
	}
	key := "tokens:" + run.Namespace + ":" + run.Name
	last := since
	for _, d := range decisions {
		ev, err := json.Marshal(struct {
			Type string `json:"type"`
			openshellDecision
		}{traceEventTypeEgress, d})
		if err != nil {
			continue
		}
		if err := r.StateStore.SaveTraceEvent(ctx, key, string(ev)); err != nil {
			logger.Error(err, "recording a sandbox decision in the trace", "sandbox", sb.Name)
			break
		}
		sandboxDecisions.WithLabelValues(run.Namespace, d.Kind, d.Action).Inc()
		last = d.Time
	}
	if !last.After(since) {
		return
	}
	patch := client.MergeFrom(run.DeepCopy())
	seen := metav1.NewMicroTime(last)
	run.Status.SandboxDecisionsSeen = &seen
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		logger.Error(err, "recording the sandbox decision watermark")
	}
}
