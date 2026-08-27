package router

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestNewMetricsRegistersAndCounts verifies the model-router Prometheus
// counters actually register into a registry (previously metricsReg was empty,
// so :9091 exposed nothing) and that increments are wired. Also checks that the
// increment methods are nil-safe (the router hot path reads r.metrics without a
// lock, so a nil metrics must never panic).
func TestNewMetricsRegistersAndCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("expected non-nil Metrics")
	}

	m.AddTokens(350)
	m.IncToolCall()
	m.ObserveStreamDuration(0.42)

	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	want := map[string]float64{
		"agentorc_modelrouter_tokens_total":   350,
		"agentorc_modelrouter_tool_calls_total": 1,
	}
	for name, expect := range want {
		var val float64
		var found bool
		for _, mf := range gathered {
			if mf.GetName() != name {
				continue
			}
			found = true
			for _, met := range mf.GetMetric() {
				if c := met.GetCounter(); c != nil {
					val += c.GetValue()
				}
			}
		}
		if !found {
			t.Errorf("registry missing metric %q", name)
		}
		if val != expect {
			t.Errorf("%s = %v, want %v", name, val, expect)
		}
	}

	// stream_duration_seconds histogram should have 1 observation.
	var histSamples uint64
	for _, mf := range gathered {
		if mf.GetName() == "agentorc_modelrouter_stream_duration_seconds" {
			for _, met := range mf.GetMetric() {
				if h := met.GetHistogram(); h != nil {
					histSamples = h.GetSampleCount()
				}
			}
		}
	}
	if histSamples != 1 {
		t.Errorf("stream_duration sample count = %v, want 1", histSamples)
	}
}

// TestMetricsNilSafe verifies nil *Metrics never panics (router hot path calls
// r.metrics.AddTokens without a nil guard at the call site).
func TestMetricsNilSafe(t *testing.T) {
	var nilM *Metrics
	nilM.AddTokens(1)
	nilM.IncToolCall()
	nilM.ObserveStreamDuration(1)
}

// TestNewMetricsNilRegistry verifies a nil registry still yields usable
// (unexposed) counters so the router never crashes when metrics are disabled.
func TestNewMetricsNilRegistry(t *testing.T) {
	m := NewMetrics(nil)
	if m == nil {
		t.Fatal("expected non-nil Metrics even with nil registry")
	}
	m.AddTokens(5) // must not panic
}
