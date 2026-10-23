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

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	spendInputRate  = 0.001
	spendOutputRate = 0.002
	// spendMetric is the exported per-run spend counter's name.
	spendMetric = "agentorca_modelrouter_spend_usd_total"
)

// spendTestRouter is a streaming test router with priced tokens and a metrics
// set attached, plus the registry it exports to.
func spendTestRouter(t *testing.T, providerURL string, store *testStore) (*Router, *prometheus.Registry) {
	t.Helper()
	r := newStreamingTestRouter(t, providerURL, writeKeyFile(t), store)
	r.cfg.CheckpointKey = "agentorca/runs/test-run/state"
	r.cfg.Providers[0].CostPerInputToken = spendInputRate
	r.cfg.Providers[0].CostPerOutputToken = spendOutputRate
	reg := prometheus.NewRegistry()
	r.SetMetrics(NewMetrics(reg))
	return r, reg
}

// spendCounter reads the exported value of the test run's spend counter for one
// outcome by scraping the registry, as Prometheus would.
func spendCounter(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != spendMetric {
			continue
		}
		for _, metric := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["run"] == "test-run" && labels["provider"] == "test" && labels["model"] == "openai/gpt-4o" &&
				labels["namespace"] == "default" && labels["outcome"] == outcome {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no spend series for outcome %q", outcome)
	return 0
}

func spendSeriesCount(t *testing.T, reg *prometheus.Registry) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == spendMetric {
			return len(mf.GetMetric())
		}
	}
	return 0
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func streamBody() []byte {
	b, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "write me a long story"}},
	})
	return b
}

func contentChunk(text string) string {
	return sseFrame(streamingChatCompletionChunk{
		ID: "c", Object: "chat.completion.chunk",
		Choices: []streamChoice{{Index: 0, Delta: streamDelta{Content: text}}},
	})
}

func usageChunk(prompt, completion int) string {
	return sseFrame(streamingChatCompletionChunk{
		ID: "c", Object: "chat.completion.chunk",
		Usage: &TokenUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
	})
}

func runStream(t *testing.T, r *Router) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(streamBody()))
	req.Header.Set("Content-Type", "application/json")
	r.HandleChatCompletions(httptest.NewRecorder(), req)
}

// Baseline: a completed call is counted under success and persisted.
func TestSpend_SuccessIsCountedAndPersisted(t *testing.T) {
	body := contentChunk("done.") + usageChunk(100, 50) + sseDone()
	srv, _ := fakeProvider(t, []byte(body))
	store := newTestStore(t)
	r, reg := spendTestRouter(t, srv.URL, store)

	runStream(t, r)

	want := 100*spendInputRate + 50*spendOutputRate
	if got := spendCounter(t, reg, spendOutcomeSuccess); !approx(got, want) {
		t.Fatalf("success spend = %v, want %v", got, want)
	}
	if saved, ok := store.lastSavedSpend(); !ok || !approx(saved, want) {
		t.Fatalf("persisted spend = %v (called=%v), want %v", saved, ok, want)
	}
}

// Regression for #75: a stream cut off before the provider's final usage chunk
// used to record $0. The partial output was billed, so it must be approximated,
// persisted, and attributed to stream_interrupted.
func TestSpend_StreamInterruptedPersistsPartialSpend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Promise far more than is sent, then hang up: the client sees an
		// unexpected EOF mid-body.
		w.Header().Set("Content-Length", "100000")
		_, _ = fmt.Fprint(w, contentChunk("Once upon a time, "))
		_, _ = fmt.Fprint(w, contentChunk("there was a very long story that never finished"))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t)
	r, reg := spendTestRouter(t, srv.URL, store)

	runStream(t, r)

	got := spendCounter(t, reg, spendOutcomeStreamInterrupted)
	if got <= 0 {
		t.Fatalf("stream_interrupted spend = %v, want > 0 (partial usage must not be dropped)", got)
	}
	if spendCounter(t, reg, spendOutcomeSuccess) != 0 {
		t.Fatal("an interrupted call must not be counted as success")
	}
	if saved, ok := store.lastSavedSpend(); !ok || !approx(saved, got) {
		t.Fatalf("persisted spend = %v (called=%v), want %v", saved, ok, got)
	}
	if !approx(r.SpendUSD(), got) {
		t.Fatalf("run spend = %v, want %v", r.SpendUSD(), got)
	}
}

// Providers that already sent usage before the cut-off are billed exactly, not
// estimated.
func TestSpend_InterruptedWithReportedUsageUsesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		_, _ = fmt.Fprint(w, contentChunk("partial"))
		_, _ = fmt.Fprint(w, usageChunk(200, 10))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	r, reg := spendTestRouter(t, srv.URL, newTestStore(t))

	runStream(t, r)

	want := 200*spendInputRate + 10*spendOutputRate
	if got := spendCounter(t, reg, spendOutcomeStreamInterrupted); !approx(got, want) {
		t.Fatalf("stream_interrupted spend = %v, want the reported %v", got, want)
	}
}

// Regression for #75: the cancel path returned before any spend accounting, so
// every call in flight at cancellation was lost.
func TestSpend_CancelledCallIsRecorded(t *testing.T) {
	var r *Router
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, contentChunk("working on it, "))
		_, _ = fmt.Fprint(w, contentChunk("still working"))
		w.(http.Flusher).Flush()
		// Let the client receive the headers and chunks, then cancel the run
		// while the call is still in flight.
		time.Sleep(100 * time.Millisecond)
		r.cancelFunc()
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t)
	var reg *prometheus.Registry
	r, reg = spendTestRouter(t, srv.URL, store)

	runStream(t, r)

	got := spendCounter(t, reg, spendOutcomeCancelled)
	if got <= 0 {
		t.Fatalf("cancelled spend = %v, want > 0", got)
	}
	if saved, ok := store.lastSavedSpend(); !ok || !approx(saved, got) {
		t.Fatalf("persisted spend = %v (called=%v), want %v", saved, ok, got)
	}
}

// A provider that rejects the request bills nothing, but the failure must be
// visible per provider so alerting can see it.
func TestSpend_ProviderErrorIsVisibleAndFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r, reg := spendTestRouter(t, srv.URL, newTestStore(t))

	runStream(t, r)

	if got := spendCounter(t, reg, spendOutcomeProviderError); got != 0 {
		t.Fatalf("provider_error spend = %v, want 0", got)
	}
	if r.SpendUSD() != 0 {
		t.Fatalf("run spend = %v, want 0 for a rejected request", r.SpendUSD())
	}
	if !hasOutcomeSeries(t, reg, spendOutcomeProviderError) {
		t.Fatal("provider_error series missing after a provider failure")
	}
}

func hasOutcomeSeries(t *testing.T, reg *prometheus.Registry, outcome string) bool {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != spendMetric {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == outcome {
					return true
				}
			}
		}
	}
	return false
}

// All four outcome labels must be on the first scrape, before any failure has
// happened; otherwise rate() and alerts have nothing to compare against.
func TestSpend_AllOutcomeSeriesExportedUpFront(t *testing.T) {
	srv, _ := fakeProvider(t)
	_, reg := spendTestRouter(t, srv.URL, newTestStore(t))

	for _, outcome := range []string{"success", "provider_error", "cancelled", "stream_interrupted"} {
		if !hasOutcomeSeries(t, reg, outcome) {
			t.Errorf("outcome %q not exported at startup", outcome)
		}
	}
}

// A warm pod serves many runs: each claim must export its own run's series.
func TestSpend_ClaimRunInitializesSeriesForNewRun(t *testing.T) {
	srv, _ := fakeProvider(t)
	r, reg := spendTestRouter(t, srv.URL, newTestStore(t))
	r.cfg.WarmMode = true

	r.ClaimRun(WarmRunInput{RunName: "run-two"})

	if n := spendSeriesCount(t, reg); n != 8 {
		t.Fatalf("expected 4 outcomes x 2 runs = 8 series, got %d", n)
	}
}

// Regression for #75: the non-streaming Gemini-format path never counted usage,
// so those calls were free for the run total and the budget cap.
func TestSpend_GeminiNonStreamingCallIsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`))
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t)
	r, reg := spendTestRouter(t, srv.URL, store)

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent?key=tok",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	rec := httptest.NewRecorder()
	r.HandleGemini(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HandleGemini status = %d: %s", rec.Code, rec.Body.String())
	}

	want := 120*spendInputRate + 30*spendOutputRate
	if got := spendCounter(t, reg, spendOutcomeSuccess); !approx(got, want) {
		t.Fatalf("gemini spend = %v, want %v", got, want)
	}
	if saved, ok := store.lastSavedSpend(); !ok || !approx(saved, want) {
		t.Fatalf("persisted spend = %v (called=%v), want %v", saved, ok, want)
	}
}

func TestSpend_GeminiProviderErrorIsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r, reg := spendTestRouter(t, srv.URL, newTestStore(t))

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent?key=tok",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	rec := httptest.NewRecorder()
	r.HandleGemini(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !hasOutcomeSeries(t, reg, spendOutcomeProviderError) {
		t.Fatal("provider_error series missing")
	}
}

// Regression for #75: _fail must persist spend itself rather than rely on an
// earlier SaveSpend having succeeded.
func TestSpend_FailFlushesSpend(t *testing.T) {
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(operator.Close)
	srv, _ := fakeProvider(t)
	store := newTestStore(t)
	r, _ := spendTestRouter(t, srv.URL, store)
	r.cfg.OperatorAPIURL = operator.URL
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg.SATokenFile = tokenFile
	r.spendUSD = 1.25 // accrued earlier; its SaveSpend is assumed lost

	r.executeFail(context.Background(), `{"reason":"unrecoverable"}`)

	if saved, ok := store.lastSavedSpend(); !ok || !approx(saved, 1.25) {
		t.Fatalf("spend persisted by _fail = %v (called=%v), want 1.25", saved, ok)
	}
}

func TestEstimateInterruptedUsage(t *testing.T) {
	sent := []Message{{Role: "user", Content: "hello there, please write something"}}
	partial := Message{Role: "assistant", Content: strings.Repeat("word ", 40)}
	u := estimateInterruptedUsage(sent, partial)
	if u.PromptTokens <= 0 || u.CompletionTokens <= 0 {
		t.Fatalf("estimate must be positive for non-empty input and output: %+v", u)
	}
	if u.TotalTokens != u.PromptTokens+u.CompletionTokens {
		t.Fatalf("total %d != prompt %d + completion %d", u.TotalTokens, u.PromptTokens, u.CompletionTokens)
	}
	if u.CompletionTokens < 30 {
		t.Fatalf("completion estimate %d is too low for 40 words", u.CompletionTokens)
	}
}

func TestMetricsAddSpendNilSafe(t *testing.T) {
	var m *Metrics
	m.AddSpend("ns", "run", "p", "m", spendOutcomeSuccess, 1)
	m.InitSpendSeries("ns", "run", "p", "m")
}

// A cancel that lands before the response headers arrive fails the request
// itself; that is a cancellation, not a provider error.
func TestSpend_CancelBeforeResponseIsNotProviderError(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	r, reg := spendTestRouter(t, srv.URL, newTestStore(t))
	go func() {
		time.Sleep(100 * time.Millisecond)
		r.cancelFunc()
	}()

	runStream(t, r)

	if spendCounter(t, reg, spendOutcomeProviderError) != 0 {
		t.Fatal("provider_error must stay 0 for a cancelled request")
	}
	if !hasOutcomeSeries(t, reg, spendOutcomeCancelled) {
		t.Fatal("cancelled series missing")
	}
}
