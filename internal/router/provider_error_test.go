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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const shapeError = `{"error":"System message must be at the beginning."}`

// countingUpstream answers every request with status/body (200 streams a text
// turn) and counts its hits.
type countingUpstream struct {
	srv  *httptest.Server
	hits atomic.Int32
}

func newCountingUpstream(t *testing.T, status int, body string) *countingUpstream {
	t.Helper()
	u := &countingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		u.hits.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		if strings.Contains(body, "choices") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(textTurnSSE("stop"))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

const nonStreamingOK = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

// newWeightedRouter wires two equally usable weighted providers, "alpha"
// (picked by the injected draw) and "beta", with no fallback chain: the
// configuration of the reported mlx-selector run.
func newWeightedRouter(t *testing.T, alpha, beta *countingUpstream) *Router {
	t.Helper()
	keyFile := writeKeyFile(t)
	cfg := &Config{
		RunName: "test-run", RunNamespace: "default",
		ContextWindowReserve: 0.2, LLMRequestTimeout: time.Hour, SATokenFile: keyFile,
		Providers: []ProviderConfig{
			{Name: "alpha", LiteLLMModel: "openai/a", BaseURL: alpha.srv.URL, APIKeyFile: keyFile, ContextWindow: 100_000, Weight: 50},
			{Name: "beta", LiteLLMModel: "openai/b", BaseURL: beta.srv.URL, APIKeyFile: keyFile, ContextWindow: 100_000, Weight: 50},
		},
		Safeguards: RouterSafeguards{ToolExecutionTimeoutSec: 60},
	}
	r, err := New(cfg, newTestStore(t), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.cancelFunc() })
	r.ruleRouter.randIntn = func(int) int { return 0 } // always the first weighted provider
	return r
}

func streamChatBody() []byte {
	return []byte(`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

func geminiCall(r *Router) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent?key=tok",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	rec := httptest.NewRecorder()
	r.HandleGemini(rec, req)
	return rec
}

func TestWeightedCandidatesAreFallbacksWithoutChain(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "zero", Weight: 0}, {Name: "a", Weight: 1}, {Name: "b", Weight: 5}, {Name: "c", Weight: 3},
		},
		FallbackChain: []string{"c", "zero"},
	}
	got := newLegacySelector(cfg, nil, nil).candidates("a")
	want := []string{"a", "b", "c", "zero"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestStreamingShapeErrorFallsBackToWeightedCandidate(t *testing.T) {
	alpha := newCountingUpstream(t, http.StatusNotFound, shapeError)
	beta := newCountingUpstream(t, http.StatusOK, "")
	r := newWeightedRouter(t, alpha, beta)

	rec := serve(r, streamChatBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 via the second candidate, got %d: %s", rec.Code, rec.Body.String())
	}
	if alpha.hits.Load() != 1 || beta.hits.Load() != 1 {
		t.Fatalf("want one hit each, got alpha=%d beta=%d", alpha.hits.Load(), beta.hits.Load())
	}
}

func TestNonStreamingShapeErrorFallsBackToWeightedCandidate(t *testing.T) {
	alpha := newCountingUpstream(t, http.StatusNotFound, shapeError)
	beta := newCountingUpstream(t, http.StatusOK, nonStreamingOK)
	r := newWeightedRouter(t, alpha, beta)

	rec := geminiCall(r)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 via the second candidate, got %d: %s", rec.Code, rec.Body.String())
	}
	if alpha.hits.Load() != 1 || beta.hits.Load() != 1 {
		t.Fatalf("want one hit each, got alpha=%d beta=%d", alpha.hits.Load(), beta.hits.Load())
	}
}

func TestRejectedByEveryCandidateFailsWithoutRetryingAny(t *testing.T) {
	for _, stream := range []bool{true, false} {
		alpha := newCountingUpstream(t, http.StatusNotFound, shapeError)
		beta := newCountingUpstream(t, http.StatusNotFound, shapeError)
		r := newWeightedRouter(t, alpha, beta)

		var rec *httptest.ResponseRecorder
		if stream {
			rec = serve(r, streamChatBody())
		} else {
			rec = geminiCall(r)
		}
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("stream=%v: want 502, got %d: %s", stream, rec.Code, rec.Body.String())
		}
		if alpha.hits.Load() != 1 || beta.hits.Load() != 1 {
			t.Fatalf("stream=%v: each provider must be hit once, got alpha=%d beta=%d",
				stream, alpha.hits.Load(), beta.hits.Load())
		}
	}
}

func TestProviderHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
		rejected  bool
		class     string
	}{
		{http.StatusRequestTimeout, true, false, errClassProvider4xx},
		{http.StatusTooManyRequests, true, false, errClassRateLimited},
		{http.StatusInternalServerError, true, false, errClassProvider5xx},
		{http.StatusServiceUnavailable, true, false, errClassProvider5xx},
		{http.StatusNotFound, false, true, errClassProvider4xx},
		{http.StatusBadRequest, false, true, errClassProvider4xx},
		{http.StatusRequestEntityTooLarge, false, true, errClassProvider4xx},
	}
	for _, c := range cases {
		err := fmt.Errorf("wrapped: %w", &ProviderHTTPError{Provider: "p", Status: c.status, Body: "x"})
		if got := isRetryableProviderError(err); got != c.retryable {
			t.Errorf("%d retryable = %v, want %v", c.status, got, c.retryable)
		}
		if got := isRejectedProviderError(err); got != c.rejected {
			t.Errorf("%d rejected = %v, want %v", c.status, got, c.rejected)
		}
		if got := classifyProviderError(err); got != c.class {
			t.Errorf("%d class = %q, want %q", c.status, got, c.class)
		}
	}
	if !isRetryableProviderError(context.DeadlineExceeded) {
		t.Error("a timeout must be retryable")
	}
	if isRetryableProviderError(errors.New("boom")) {
		t.Error("an unknown error is not retryable")
	}
	pe := &ProviderHTTPError{Provider: "p", Status: 404, Body: "nope"}
	if pe.Error() != "provider p returned 404: nope" {
		t.Errorf("Error() text changed: %q", pe.Error())
	}
}

// A 503 is not a rejection: it falls back, and a retryable 429 keeps its class.
func TestServerErrorFallsBackAndIsRetryable(t *testing.T) {
	alpha := newCountingUpstream(t, http.StatusServiceUnavailable, "overloaded")
	beta := newCountingUpstream(t, http.StatusOK, "")
	r := newWeightedRouter(t, alpha, beta)
	if rec := serve(r, streamChatBody()); rec.Code != http.StatusOK {
		t.Fatalf("want 200 via fallback, got %d", rec.Code)
	}
}

func TestRoleSequence(t *testing.T) {
	msgs := []Message{{Role: "system"}, {Role: "user"}, {Role: "assistant"}, {Role: "tool"}}
	if got := roleSequence(msgs); got != "system,user,assistant,tool" {
		t.Errorf("roleSequence = %q", got)
	}
	long := make([]Message, 1, 11)
	long[0] = Message{Role: "system"}
	for range 10 {
		long = append(long, Message{Role: "tool"})
	}
	if got := roleSequence(long); got != "system,tool x10" {
		t.Errorf("run not compressed: %q", got)
	}
	alternating := make([]Message, 0, 200)
	for i := range 200 {
		alternating = append(alternating, Message{Role: []string{"user", "assistant"}[i%2]})
	}
	if got := roleSequence(alternating); len(got) > maxRoleSequenceLen+3 {
		t.Errorf("not capped: %d chars", len(got))
	}
	if roleSequence(nil) != "" {
		t.Error("empty input must give empty string")
	}
}

type recordingHandler struct {
	records *[]slog.Record
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestRejectedRequestLogsRolesNotContent(t *testing.T) {
	var records []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(recordingHandler{records: &records}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "TOP-SECRET-CONTENT"
	msgs := []Message{{Role: "system", Content: secret}, {Role: "user", Content: secret}}
	logProviderFailure("alpha", &ProviderHTTPError{Provider: "alpha", Status: 404, Body: shapeError}, msgs, false)

	var sawNoFallback, sawRoles bool
	for _, rec := range records {
		var line strings.Builder
		line.WriteString(rec.Message)
		rec.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&line, " %s=%v", a.Key, a.Value)
			return true
		})
		s := line.String()
		if strings.Contains(s, secret) {
			t.Fatalf("log leaked message content: %s", s)
		}
		if rec.Message == "provider failed, no fallback configured" {
			sawNoFallback = true
		}
		if rec.Level == slog.LevelWarn && strings.Contains(s, "roles=system,user") && strings.Contains(s, "messages=2") {
			sawRoles = true
		}
		if strings.Contains(rec.Message, "trying fallback") {
			t.Fatalf("must not claim a fallback when none exists: %s", s)
		}
	}
	if !sawNoFallback || !sawRoles {
		t.Fatalf("missing log lines (noFallback=%v roles=%v): %v", sawNoFallback, sawRoles, records)
	}
}
