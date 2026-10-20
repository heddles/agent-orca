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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// oversizedTokenTruncationTest is the regression for issue #42: the pre-send
// truncation estimate was hardcoded to zero (`if est := 0; est > budget`), so
// an over-window conversation was forwarded verbatim, the provider answered
// 400 context-length-exceeded, and every retry replayed the same oversized
// request against every fallback provider. The estimate is now real, so an
// oversized request must be truncated before it reaches the provider and a
// `truncation` trace event must land in the trace stream.
func TestPreSendTruncationCapsOversizedRequests(t *testing.T) {
	// Capturing provider: records every request body it receives and answers
	// with one canned terminal text turn.
	var mu sync.Mutex
	var received [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(textTurnSSE("stop"))
	}))
	t.Cleanup(srv.Close)

	keyFile := writeKeyFile(t)
	store := newTestStore(t)
	r := newStreamingTestRouter(t, srv.URL, keyFile, store)

	// newStreamingTestRouter configures ContextWindow=100000; with the default
	// compaction fallback ratio (0.5) the message budget is ~49k tokens. Three
	// 200k-char user messages (~50k tokens each by the whole-word len/4 proxy
	// estimator) must overflow it and trigger pre-send truncation.
	const bigLen = 200_000
	big := strings.Repeat("a1b2 c3d4 ", bigLen/10) // ASCII, ~countTokens(body)/4≈len/4
	messages := []map[string]any{
		{"role": "system", "content": "You are helpful."},
		{"role": "user", "content": big},
		{"role": "assistant", "content": big},
		{"role": "user", "content": big},
	}
	sentTotal := 3*bigLen + len("You are helpful.")
	body, err := json.Marshal(map[string]any{
		"model":    "default",
		"stream":   true,
		"messages": messages,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	_, _, _, _ = consumeClientBody(t, r, body)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("provider received %d requests, want exactly 1", len(received))
	}
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(received[0], &sent); err != nil {
		t.Fatalf("decoding provider-received body: %v", err)
	}
	gotTotal := 0
	for _, m := range sent.Messages {
		gotTotal += len(m.Content)
	}
	if gotTotal >= sentTotal {
		t.Fatalf("provider received %d content chars un-truncated (sent %d) — pre-send truncation did not fire", gotTotal, sentTotal)
	}
	// Budget ≈ 48,976 tokens ⇒ at ~4 chars/token the truncated payload must
	// fit comfortably under 5× the char budget (generous margin over the
	// per-message +4-token overhead).
	if gotTotal > 5*48_976*4 {
		t.Fatalf("truncated request still too large: %d content chars (budget 48,976 tokens)", gotTotal)
	}

	var truncation []string
	for _, ev := range store.traceEvents {
		if strings.Contains(ev, `"type":"truncation"`) {
			truncation = append(truncation, ev)
		}
	}
	if len(truncation) != 1 {
		t.Fatalf("expected exactly 1 truncation trace event, got %d in %v", len(truncation), store.traceEvents)
	}
	var tev map[string]any
	if err := json.Unmarshal([]byte(truncation[0]), &tev); err != nil {
		t.Fatalf("decoding truncation event: %v", err)
	}
	before, _ := tev["before"].(float64)
	after, _ := tev["after"].(float64)
	budget, _ := tev["budget"].(float64)
	if before < float64(gotTotal) || after <= 0 || after > before || budget != 48_976 {
		t.Fatalf("unexpected truncation event fields: %v (gotTotal=%d chars)", tev, gotTotal)
	}
}
