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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/egress"
)

func statusTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func newStatusServer(t *testing.T, objs ...client.Object) *UIServer {
	t.Helper()
	return &UIServer{
		crdClient: fake.NewClientBuilder().WithScheme(statusTestScheme(t)).WithObjects(objs...).Build(),
	}
}

// TestSystemStatusRunHistoryConfigured verifies the pgStore-nil case surfaces a
// degraded postgres-archive subsystem and runHistoryConfigured=false (so the UI
// never shows a silent empty state).
func TestSystemStatusRunHistoryConfigured(t *testing.T) {
	s := newStatusServer(t) // pgStore == nil, k8s == nil, store == nil
	rec := httptest.NewRecorder()
	s.handleSystemStatus(rec, httptest.NewRequest(http.MethodGet, "/api/system/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp SystemStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.RunHistoryConfigured {
		t.Fatal("expected runHistoryConfigured=false when pgStore is nil")
	}
	var foundDegraded bool
	for _, ss := range resp.SubSystems {
		if ss.Name == "postgres-archive" {
			if ss.Status != "degraded" {
				t.Fatalf("postgres-archive status = %q, want degraded", ss.Status)
			}
			foundDegraded = true
		}
	}
	if !foundDegraded {
		t.Fatal("expected a postgres-archive subsystem in degraded state")
	}
}

// TestProbeModelProvidersNamespace verifies the same-named provider in two
// namespaces produces two distinct, namespace-dereferenced entries — the bug
// where duplicates looked like buggy code.
func TestProbeModelProvidersNamespace(t *testing.T) {
	s := newStatusServer(t,
		&v1alpha1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "default"},
			Spec: v1alpha1.ModelProviderSpec{
				LiteLLMModel:   "openai/gpt-4o",
				CredentialsRef: v1alpha1.SecretKeyRef{Name: "k", Key: "v"},
			},
			Status: v1alpha1.ModelProviderStatus{Ready: true},
		},
		&v1alpha1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "tenant-a"},
			Spec: v1alpha1.ModelProviderSpec{
				LiteLLMModel:   "openai/gpt-4o",
				CredentialsRef: v1alpha1.SecretKeyRef{Name: "k", Key: "v"},
			},
			Status: v1alpha1.ModelProviderStatus{Ready: false},
		},
	)
	providers := s.probeModelProviders(context.Background())
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	seen := map[string]bool{}
	for _, p := range providers {
		if p.Name != "openai" {
			t.Fatalf("expected name openai, got %q", p.Name)
		}
		seen[p.Namespace] = true
		if p.Namespace == "" {
			t.Fatal("expected namespace to be set on provider health")
		}
	}
	if !seen["default"] || !seen["tenant-a"] {
		t.Fatalf("expected both namespaces to be represented, got %v", seen)
	}
}

// TestHandleGetArchivedRunNotConfigured verifies the archived-run detail
// endpoint returns 503 when the archival store is not wired (mirrors the
// history list behaviour and is what drives the UI not-configured banner).
func TestHandleGetArchivedRunNotConfigured(t *testing.T) {
	s := newStatusServer(t) // pgStore == nil
	rec := httptest.NewRecorder()
	s.handleGetArchivedRun(rec, httptest.NewRequest(http.MethodGet, "/api/runs/history/default/foo", nil), "default", "foo")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleRunOrStreamRoutesArchivedDetail verifies the /api/runs/history/{ns}/
// {name} path is dispatched to the archived handler (not the live CRD path),
// so an archived-only run is reachable.
func TestHandleRunOrStreamRoutesArchivedDetail(t *testing.T) {
	s := newStatusServer(t) // pgStore == nil -> 503 is the archived-detail path
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, httptest.NewRequest(http.MethodGet, "/api/runs/history/default/my-run", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected archived-detail 503 (not a live-run 404), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestScrapeMetricsReadsEgressRegistry verifies that scrapeMetrics reads egress
// counters from prometheus.DefaultGatherer (where the egress package's promauto
// counters live), not from externalReg where they never existed. Before the fix
// these metrics were always 0.
func TestScrapeMetricsReadsEgressRegistry(t *testing.T) {
	s := &UIServer{} // k8s nil -> token throughput stays 0 (documented stub)

	before, err := s.scrapeMetrics(context.Background())
	if err != nil {
		t.Fatalf("scrapeMetrics error: %v", err)
	}
	if before == nil {
		t.Fatal("expected non-nil metrics")
	}

	// Publish a result through the egress package; this increments the egress
	// counter that lives on the DEFAULT registry (promauto).
	egress.PublishAndRecord(context.Background(), mockEgressPublisher{}, v1alpha1.EgressResult{
		RunID: "metrics-test", Phase: "Succeeded", Output: "ok", Tenant: "t",
	})

	after, err := s.scrapeMetrics(context.Background())
	if err != nil {
		t.Fatalf("scrapeMetrics error: %v", err)
	}
	if after.EgressPublished < before.EgressPublished+1 {
		t.Fatalf("EgressPublished = %d, want >= %d (registry wiring not reading egress counters)",
			after.EgressPublished, before.EgressPublished+1)
	}
}

// mockEgressPublisher is a no-op Publisher satisfying egress.Publisher for tests.
type mockEgressPublisher struct{}

func (mockEgressPublisher) Publish(context.Context, v1alpha1.EgressResult) error { return nil }
func (mockEgressPublisher) Close() error                                         { return nil }

// TestUIServerInstrumentationCountsRequests verifies the UI server (8083) is
// wrapped with instrument() so browser-driven status/UI traffic populates the
// shared externalReg request counter and request-duration histogram that the
// status page reads. Previously only the external Task API (8084) was
// instrumented, so the status page always showed requestCount=0 / p95=0.
func TestUIServerInstrumentationCountsRequests(t *testing.T) {
	// Wrap a trivial handler the same way Handler() does: instrument("ui", ...).
	h := instrument("ui", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	before := getCounterValue(externalReg, "agentorca_external_requests_total")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/system/status", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	after := getCounterValue(externalReg, "agentorca_external_requests_total")
	if after <= before {
		t.Fatalf("instrument(\"ui\") did not increment external_requests_total: before=%v after=%v", before, after)
	}
}

// TestInstrumentPreservesFlusher verifies the recordingResponseWriter (used by
// instrument()) still satisfies http.Flusher so SSE stream handlers like
// handleStream — which assert `w.(http.Flusher)` — keep working behind the
// instrumentation wrapper. Regression for "stream connection lost".
func TestInstrumentPreservesFlusher(t *testing.T) {
	streamed := false
	h := instrument("ui", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("inner handler could not assert http.Flusher through instrument wrapper")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: hi\n\n"))
		flusher.Flush()
		streamed = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/default/r/stream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !streamed {
		t.Fatal("Flush path was not exercised")
	}
}

// TestParseMetricRange verifies time-range parsing for the ?range= query param.
func TestParseMetricRange(t *testing.T) {
	cases := map[string]time.Duration{
		"1h":   time.Hour,
		"6h":   6 * time.Hour,
		"24h":  24 * time.Hour,
		"7d":   7 * 24 * time.Hour,
		"":     24 * time.Hour, // default
		"junk": 24 * time.Hour, // unknown -> default
	}
	for in, want := range cases {
		if got := parseMetricRange(in); got != want {
			t.Errorf("parseMetricRange(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestMetricSampleRingBuffer verifies samplesForRange filters by window,
// downsamples, and never returns nil (which would serialize as JSON null and
// crash the UI's spread of the samples array).
func TestMetricSampleRingBuffer(t *testing.T) {
	s := &UIServer{}
	now := time.Now().Unix()
	// Seed samples at known offsets from now.
	s.metricHistory = []MetricSample{
		{Time: now - 7200, RequestCount: 0, EgressPublished: 0}, // 2h ago
		{Time: now - 5400, RequestCount: 5, EgressPublished: 5}, // 1.5h ago
		{Time: now - 300, RequestCount: 8, EgressPublished: 8},  // 5m ago
		{Time: now, RequestCount: 10, EgressPublished: 10},      // now
	}

	if got := s.samplesForRange(context.Background(), "24h"); len(got) != 4 {
		t.Fatalf("samplesForRange(24h) = %d, want 4", len(got))
	}
	// 1h window excludes the two older samples (>1h ago), keeps the 5m + now.
	if got := s.samplesForRange(context.Background(), "1h"); len(got) != 2 {
		t.Fatalf("samplesForRange(1h) = %d, want 2", len(got))
	}
	// A window excluding all samples returns a non-nil empty slice.
	s.metricHistory = []MetricSample{{Time: now - 7200, RequestCount: 0}}
	empty := s.samplesForRange(context.Background(), "1h")
	if empty == nil {
		t.Fatal("samplesForRange returned nil — would serialize as null and crash the UI")
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty slice, got %d", len(empty))
	}
}

// TestRecordMetricSampleThrottled verifies the sample-throttle so the ring
// buffer doesn't grow unbounded on a sub-30s poll.
func TestRecordMetricSampleThrottled(t *testing.T) {
	s := &UIServer{}
	m := &SystemMetrics{RequestCount24h: 1, EgressPublished: 1}
	s.recordMetricSample(m)
	s.recordMetricSample(m) // same second -> throttled away
	if len(s.metricHistory) != 1 {
		t.Fatalf("expected 1 sample recorded (2nd throttled), got %d", len(s.metricHistory))
	}
}

// TestRoutingDecisionsToJSON verifies the archived-run detail converter that
// feeds the Execution Trace timeline: it formats the CRD timestamp to RFC3339,
// maps strategy/provider/reason/confidence, is nil-timestamp safe, and returns a
// non-nil empty slice for nil input (so JSON serializes to "[]" not "null").
func TestRoutingDecisionsToJSON(t *testing.T) {
	ts := metav1.NewTime(time.Date(2026, 1, 1, 12, 0, 5, 0, time.UTC))
	rds := []v1alpha1.RoutingDecision{
		{Model: "openai/gpt-4o", Provider: "openai", Strategy: "rule-based", Reason: "cheapest capable", Confidence: "0.9", Timestamp: &ts},
	}
	out := routingDecisionsToJSON(rds)
	if len(out) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(out))
	}
	d := out[0]
	if d.Model != "openai/gpt-4o" || d.Provider != "openai" || d.Strategy != "rule-based" || d.Reason != "cheapest capable" || d.Confidence != "0.9" {
		t.Fatalf("unexpected mapping: %+v", d)
	}
	if d.Timestamp != "2026-01-01T12:00:05Z" {
		t.Fatalf("timestamp not RFC3339-formatted: %q", d.Timestamp)
	}
	// Nil timestamp must not panic and yields "".
	out2 := routingDecisionsToJSON([]v1alpha1.RoutingDecision{{Model: "x"}})
	if out2[0].Timestamp != "" {
		t.Fatalf("expected empty timestamp for nil, got %q", out2[0].Timestamp)
	}
	// Nil input -> non-nil empty slice (so the JSON is [] not null, which would
	// crash the UI's routingDecisions.map).
	if r := routingDecisionsToJSON(nil); r == nil || len(r) != 0 {
		t.Fatal("expected non-nil empty slice for nil input")
	}
}

// TestScrapePromCounter verifies the line-based Prometheus text parser sums
// labelled counter series correctly and returns ok=false for a missing counter.
func TestScrapePromCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		body := strings.Join([]string{
			"# HELP agentorca_modelrouter_tokens_total Output tokens streamed.",
			"# TYPE agentorca_modelrouter_tokens_total counter",
			`agentorca_modelrouter_tokens_total{run="r1"} 100`,
			`agentorca_modelrouter_tokens_total{run="r2"} 250`,
		}, "\n") + "\n"
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	v, ok := scrapePromCounter(context.Background(), srv.URL, "agentorca_modelrouter_tokens_total")
	if !ok {
		t.Fatal("expected ok for an existing counter")
	}
	if v != 350 {
		t.Fatalf("summed tokens = %v, want 350", v)
	}
	if _, ok := scrapePromCounter(context.Background(), srv.URL, "nope_not_registered"); ok {
		t.Fatal("expected not-ok for a missing counter")
	}
}

// TestUIOpenAPIConvertsAndReflectsStatusFields verifies the embedded UI OpenAPI
// YAML still converts to JSON at runtime (no syntax regressions from the schema
// edits) and that the new status fields are documented.
func TestUIOpenAPIConvertsAndReflectsStatusFields(t *testing.T) {
	b, err := uiOpenAPIJSONBytes()
	if err != nil {
		t.Fatalf("converting embedded UI OpenAI to JSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("UI spec is not valid JSON: %v", err)
	}
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	status, _ := schemas["SystemStatus"].(map[string]any)
	props, _ := status["properties"].(map[string]any)
	if _, ok := props["runHistoryConfigured"]; !ok {
		t.Fatal("SystemStatus schema missing runHistoryConfigured")
	}
	ph, _ := schemas["ProviderHealth"].(map[string]any)
	phProps, _ := ph["properties"].(map[string]any)
	if _, ok := phProps["namespace"]; !ok {
		t.Fatal("ProviderHealth schema missing namespace")
	}
	// Sanity: the doc is non-trivial and the new archived-detail route + schema exist.
	if !strings.Contains(string(b), "modelProviders") {
		t.Fatal("UI spec missing modelProviders")
	}
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/api/runs/history/{namespace}/{name}"]; !ok {
		t.Fatal("UI spec missing /api/runs/history/{namespace}/{name} path")
	}
	if _, ok := schemas["RunHistoryDetail"]; !ok {
		t.Fatal("UI spec missing RunHistoryDetail schema")
	}
	// The top-level security requirement must survive the restructure.
	if _, ok := doc["security"]; !ok {
		t.Fatal("UI spec missing top-level security requirement")
	}
}
