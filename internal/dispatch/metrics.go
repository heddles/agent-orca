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

package dispatch

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	decisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_dispatch_decisions_total",
		Help: "Requests that named no agent, by how the agent was picked (method), why a decider's pick was " +
			"not simply taken (reason) and what decided (backend).",
	}, []string{"method", "reason", "backend", "escalated"})
	duration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agentorca_dispatch_duration_seconds",
		Help:    "Time to pick an agent.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20},
	})
	confidence = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agentorca_dispatch_confidence",
		Help:    "How sure a decider was of its pick.",
		Buckets: []float64{.2, .4, .5, .6, .7, .8, .9, .95, .99, 1},
	}, []string{"backend"})
	recordFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agentorca_dispatch_record_failures_total",
		Help: "Dispatch decisions that could not be stored; their cost is missing from the budget check.",
	})
)

func init() { ctrlmetrics.Registry.MustRegister(decisions, duration, confidence, recordFailures) }

func observe(r Result, took time.Duration) {
	escalated := "false"
	if r.Escalated {
		escalated = "true"
	}
	decisions.WithLabelValues(string(r.Method), r.Reason, r.Provider, escalated).Inc()
	duration.Observe(took.Seconds())
	if r.Provider != "" && r.Confidence > 0 {
		confidence.WithLabelValues(r.Provider).Observe(r.Confidence)
	}
}
