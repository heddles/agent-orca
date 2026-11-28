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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// retainRecorder is a stand-in hindsight API that counts retain calls (POST to
// .../banks/<bank>/memories). Recall and the bank upsert hit other paths, so the
// count isolates the retain step under test.
type retainRecorder struct {
	mu      sync.Mutex
	retains int
}

func (a *retainRecorder) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories") {
		a.mu.Lock()
		a.retains++
		a.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (a *retainRecorder) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.retains
}

// TestPerTurnRetainHonoursRetainOnEveryTurn is the regression for the inert
// RetainOnEveryTurn flag: the streaming retain step ignored it, so setting
// retainOnEveryTurn: false still retained every turn. The default (true) keeps
// today's behaviour.
func TestPerTurnRetainHonoursRetainOnEveryTurn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retain    bool
		wantCalls int
	}{
		{name: "default keeps per-turn retain", retain: true, wantCalls: 1},
		{name: "disabled skips retain", retain: false, wantCalls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &retainRecorder{}
			hindsightSrv := httptest.NewServer(http.HandlerFunc(rec.handler))
			t.Cleanup(hindsightSrv.Close)

			// The provider stub is the LLM; the hindsight stub is memory.
			srv, _ := fakeProvider(t, textTurnSSE(finishReasonStop))
			keyFile := writeKeyFile(t)
			store := newTestStore(t)
			r := newStreamingTestRouter(t, srv.URL, keyFile, store)
			r.cfg.Hindsight = HindsightConfig{
				Enabled:           true,
				URL:               hindsightSrv.URL,
				RetainOnEveryTurn: tc.retain,
			}

			body, _ := json.Marshal(map[string]any{
				"model": "default", "stream": true,
				"messages": []map[string]any{{"role": "user", "content": "hello"}},
			})
			consumeClientBody(t, r, body)

			if got := rec.count(); got != tc.wantCalls {
				t.Errorf("retain calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}
