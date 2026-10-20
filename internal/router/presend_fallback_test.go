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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// approxCharsPerToken is the estimator's rough ratio for these ASCII fixtures
// (countTokens ≈ len/4), used to turn a token budget into a char ceiling.
const approxCharsPerToken = 4

// testRoleUser avoids repeating the role literal across this file's fixtures.
const testRoleUser = "user"

// capturingProvider records request bodies; failing providers answer 500.
type capturingProvider struct {
	mu       sync.Mutex
	received [][]byte
	srv      *httptest.Server
}

func newCapturingProvider(t *testing.T, fail bool) *capturingProvider {
	t.Helper()
	p := &capturingProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.received = append(p.received, body)
		p.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(textTurnSSE("stop"))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *capturingProvider) requests() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.received...)
}

// sentContentChars sums the message content the provider actually received.
func sentContentChars(t *testing.T, body []byte) int {
	t.Helper()
	var sent struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decoding provider-received body: %v", err)
	}
	n := 0
	for _, m := range sent.Messages {
		n += len(m.Content)
	}
	return n
}

// newFallbackTestRouter wires a weighted primary and a weight-0 fallback
// (reserved for the fallback chain) with the given context windows.
func newFallbackTestRouter(t *testing.T, primary, fallback *capturingProvider, primaryCW, fallbackCW int) *Router {
	t.Helper()
	keyFile := writeKeyFile(t)
	cfg := &Config{
		RunName:              "test-run",
		RunNamespace:         "default",
		ContextWindowReserve: 0.2,
		LLMRequestTimeout:    time.Hour,
		SATokenFile:          keyFile,
		FallbackChain:        []string{"primary", "fallback"},
		Providers: []ProviderConfig{
			{Name: "primary", LiteLLMModel: "openai/gpt-4o", BaseURL: primary.srv.URL, APIKeyFile: keyFile,
				ContextWindow: primaryCW, Weight: 1},
			{Name: "fallback", LiteLLMModel: "openai/gpt-4o", BaseURL: fallback.srv.URL, APIKeyFile: keyFile,
				ContextWindow: fallbackCW, Weight: 0},
		},
		Safeguards: RouterSafeguards{ToolExecutionTimeoutSec: 60},
	}
	r, err := New(cfg, newTestStore(t), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.cancelFunc() })
	return r
}

// conversation builds a multi-turn request of n messages (~5k tokens each)
// so truncateHistory has many safe cut points and each window keeps a
// different amount of history.
func conversation(t *testing.T, n int) []byte {
	t.Helper()
	big := strings.Repeat("a1b2 c3d4 ", 2_000)
	msgs := make([]map[string]any, 0, 1+n)
	msgs = append(msgs, map[string]any{"role": "system", "content": "You are helpful."})
	roles := [2]string{testRoleUser, "assistant"}
	for i := range n {
		msgs = append(msgs, map[string]any{"role": roles[i%2], "content": big})
	}
	body, err := json.Marshal(map[string]any{"model": "default", "stream": true, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func serve(r *Router, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.HandleChatCompletions(rec, req)
	return rec
}

// Regression for #42: when the primary fails, a smaller-window fallback must
// receive a request fitted to ITS window, not the primary's fitted request.
func TestFallbackRequestFittedToFallbackWindow(t *testing.T) {
	primary := newCapturingProvider(t, true)
	fallback := newCapturingProvider(t, false)
	// primary budget 0.5*100k-1024 = 48,976 tokens; fallback 0.5*20k-1024 = 8,976.
	r := newFallbackTestRouter(t, primary, fallback, 100_000, 20_000)

	rec := serve(r, conversation(t, 20))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 via fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	pReqs, fReqs := primary.requests(), fallback.requests()
	if len(pReqs) == 0 || len(fReqs) != 1 {
		t.Fatalf("want primary tried then one fallback request, got %d / %d", len(pReqs), len(fReqs))
	}
	primaryChars, fallbackChars := sentContentChars(t, pReqs[0]), sentContentChars(t, fReqs[0])
	if fallbackChars >= primaryChars {
		t.Fatalf("fallback got the primary's request (%d chars) instead of one fitted to its smaller window (%d chars)",
			primaryChars, fallbackChars)
	}
	if fallbackChars > 5*8_976*approxCharsPerToken {
		t.Fatalf("fallback request still too large for its window: %d chars (budget 8,976 tokens)", fallbackChars)
	}
}

// The fallback is fitted from the ORIGINAL request: a larger-window fallback
// keeps more context than the primary's compacted request.
func TestFallbackRefitsFromOriginalRequest(t *testing.T) {
	primary := newCapturingProvider(t, true)
	fallback := newCapturingProvider(t, false)
	r := newFallbackTestRouter(t, primary, fallback, 20_000, 400_000)

	rec := serve(r, conversation(t, 20))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 via fallback, got %d", rec.Code)
	}
	pReqs, fReqs := primary.requests(), fallback.requests()
	if len(pReqs) == 0 || len(fReqs) != 1 {
		t.Fatalf("want primary tried then one fallback request, got %d / %d", len(pReqs), len(fReqs))
	}
	if sentContentChars(t, fReqs[0]) <= sentContentChars(t, pReqs[0]) {
		t.Fatal("a larger-window fallback must be re-fitted from the original request, not the primary's compacted one")
	}
}

// oversizedToolsRequest has a short conversation but tool definitions that
// alone exceed the configured windows (~25k tokens): message compaction
// cannot make it fit, so it is unfittable for 20k/24k-token windows.
func oversizedToolsRequest(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": testRoleUser, "content": "hi"}},
		"tools": []map[string]any{{"type": "function", "function": map[string]any{
			"name": "toolbelt", "description": strings.Repeat("a1b2 c3d4 ", 10_000),
			"parameters": map[string]any{"type": "object"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A request no provider can hold fails fast with 413 and never reaches a
// provider — previously it was sent to every provider in turn, each answering
// 400 context-length-exceeded.
func TestOversizedRequestFailsFastWithoutCallingProviders(t *testing.T) {
	primary := newCapturingProvider(t, false)
	fallback := newCapturingProvider(t, false)
	r := newFallbackTestRouter(t, primary, fallback, 20_000, 24_000)

	rec := serve(r, oversizedToolsRequest(t))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context window") {
		t.Fatalf("error must name the context window, got %q", rec.Body.String())
	}
	if n := len(primary.requests()) + len(fallback.requests()); n != 0 {
		t.Fatalf("an unfittable request must never be sent, providers received %d", n)
	}
}

// Same, with no fallback configured: still a clear 413, not a generic 502.
func TestOversizedRequestWithoutFallbackIs413(t *testing.T) {
	primary := newCapturingProvider(t, false)
	fallback := newCapturingProvider(t, false)
	r := newFallbackTestRouter(t, primary, fallback, 20_000, 20_000)
	r.cfg.FallbackChain = nil

	if rec := serve(r, oversizedToolsRequest(t)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 without fallbacks, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(primary.requests()); n != 0 {
		t.Fatalf("primary must not be called, got %d", n)
	}
}

// A large-window fallback still serves a request too big for the primary.
func TestOversizedForPrimaryServedByLargerFallback(t *testing.T) {
	primary := newCapturingProvider(t, false)
	fallback := newCapturingProvider(t, false)
	r := newFallbackTestRouter(t, primary, fallback, 20_000, 400_000)

	if rec := serve(r, oversizedToolsRequest(t)); rec.Code != http.StatusOK {
		t.Fatalf("want 200 from the larger fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(primary.requests()) != 0 || len(fallback.requests()) != 1 {
		t.Fatalf("want primary skipped and one fallback call, got %d / %d",
			len(primary.requests()), len(fallback.requests()))
	}
}

// PR #46 review: when the request FITS the primary but the primary fails
// transiently and every fallback is too small, the client must get a
// retriable 502 — not a 413 claiming the request exceeds the window.
func TestTransientPrimaryFailureWithTooSmallFallbacksIs502(t *testing.T) {
	primary := newCapturingProvider(t, true) // fits, but answers 500
	fallback := newCapturingProvider(t, false)
	r := newFallbackTestRouter(t, primary, fallback, 400_000, 20_000)

	rec := serve(r, oversizedToolsRequest(t)) // ~25k tool tokens: fits 400k, not 20k
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 for a transient primary failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(primary.requests()) != 1 || len(fallback.requests()) != 0 {
		t.Fatalf("want one primary call and the too-small fallback skipped, got %d / %d",
			len(primary.requests()), len(fallback.requests()))
	}
}
