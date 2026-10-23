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

package router

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the Prometheus counters/gauges/histograms that the model-router
// exposes on its :9091/metrics endpoint. They are registered into the
// router's own registry (created in cmd/model-router/main.go), NOT the process
// default registry, so they are scoped to this model-router pod.
type Metrics struct {
	// tokens is the total output tokens streamed by this model-router.
	tokens prometheus.Counter
	// toolCalls is the total tool calls dispatched by this model-router.
	toolCalls prometheus.Counter
	// streamDuration observes the latency of a streaming chat response.
	streamDuration prometheus.Histogram
	// spendUSD is the USD cost this model-router has incurred, split by how the
	// LLM call ended (see the spendOutcome* constants). Summing every outcome
	// gives the run's full cost; the failed-call share is the part a
	// success-only counter would hide.
	spendUSD *prometheus.CounterVec
}

// Outcomes of an LLM call for the spend metric.
const (
	// spendOutcomeSuccess is a call that completed and reported its usage.
	spendOutcomeSuccess = "success"
	// spendOutcomeProviderError is a call the provider rejected or failed.
	spendOutcomeProviderError = "provider_error"
	// spendOutcomeCancelled is a call cut short because the run was cancelled.
	spendOutcomeCancelled = "cancelled"
	// spendOutcomeStreamInterrupted is a streamed call whose connection broke
	// before the provider sent its final usage chunk.
	spendOutcomeStreamInterrupted = "stream_interrupted"
)

// spendOutcomes lists every outcome label value, so each is exported from the
// first scrape (a series that only appears after its first failure breaks
// rate() and alerting).
var spendOutcomes = []string{
	spendOutcomeSuccess,
	spendOutcomeProviderError,
	spendOutcomeCancelled,
	spendOutcomeStreamInterrupted,
}

// NewMetrics builds a Metrics set and registers it into reg. If reg is nil the
// counters are still allocated (no-ops at the registry level) so call sites can
// never nil-deref; they simply aren't exposed.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		tokens: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentorca_modelrouter_tokens_total",
			Help: "Output tokens streamed by this model-router.",
		}),
		toolCalls: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentorca_modelrouter_tool_calls_total",
			Help: "Tool calls dispatched by this model-router.",
		}),
		streamDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "agentorca_modelrouter_stream_duration_seconds",
			Help:    "Duration of a model-router streaming chat response.",
			Buckets: []float64{0.1, 0.3, 0.5, 1, 2, 5, 10, 20, 60},
		}),
		spendUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentorca_modelrouter_spend_usd_total",
			Help: "USD spent on LLM calls by this model-router, by run, provider, model and call outcome.",
		}, []string{"namespace", "run", "provider", "model", "outcome"}),
	}
	if reg != nil {
		reg.MustRegister(m.tokens, m.toolCalls, m.streamDuration, m.spendUSD)
	}
	return m
}

// AddTokens records n streamed output tokens. Nil-safe.
func (m *Metrics) AddTokens(n int) {
	if m == nil {
		return
	}
	m.tokens.Add(float64(n))
}

// IncToolCall records one dispatched tool call. Nil-safe.
func (m *Metrics) IncToolCall() {
	if m == nil {
		return
	}
	m.toolCalls.Inc()
}

// ObserveStreamDuration records a streaming response duration (seconds). Nil-safe.
func (m *Metrics) ObserveStreamDuration(seconds float64) {
	if m == nil {
		return
	}
	m.streamDuration.Observe(seconds)
}

// AddSpend records usd spent on one LLM call that ended with outcome. A zero
// amount still creates the series, so every outcome is exported even before
// its first non-zero sample. Nil-safe.
func (m *Metrics) AddSpend(namespace, run, provider, model, outcome string, usd float64) {
	if m == nil {
		return
	}
	m.spendUSD.WithLabelValues(namespace, run, provider, model, outcome).Add(usd)
}

// InitSpendSeries exports a zero sample for every outcome of one provider/model
// so all four outcome labels are present from the first scrape. Nil-safe.
func (m *Metrics) InitSpendSeries(namespace, run, provider, model string) {
	if m == nil {
		return
	}
	for _, outcome := range spendOutcomes {
		m.spendUSD.WithLabelValues(namespace, run, provider, model, outcome).Add(0)
	}
}
