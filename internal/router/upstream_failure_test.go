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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// failingProvider answers every call with a 500.
func failingProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func streamRequest(t *testing.T) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// All providers failing before any content is streamed is an HTTP 502 and the
// body carries no [DONE].
func TestStreamingAllProvidersFailReturns502WithoutDone(t *testing.T) {
	srv := failingProvider(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), newTestStore(t))
	rec := httptest.NewRecorder()
	r.HandleChatCompletions(rec, streamRequest(t))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("body must not contain [DONE]: %s", rec.Body.String())
	}
}

// When the SSE response has already begun, the failure is an SSE error event,
// not a second WriteHeader and not a [DONE].
func TestStreamingAllProvidersFailAfterSSEStartedEmitsErrorEvent(t *testing.T) {
	srv := failingProvider(t)
	r := newStreamingTestRouter(t, srv.URL, writeKeyFile(t), newTestStore(t))
	// A tool-call continuation writes to a stream the first turn already began.
	req := streamRequest(t)
	req = req.WithContext(context.WithValue(req.Context(), continuationKey{}, true))
	rec := httptest.NewRecorder()
	r.HandleChatCompletions(rec, req)
	out := rec.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Errorf("body must not contain [DONE]: %s", out)
	}
	if !strings.Contains(out, `data: {"error":{"message":"upstream error","type":"upstream_error"}}`) {
		t.Errorf("want a well-formed SSE error event, got: %s", out)
	}
}

// kvOutputStore also records SaveKV, which the nop store drops.
type kvOutputStore struct {
	outputStore
	kv map[string]string
}

func (s *kvOutputStore) SaveKV(_ context.Context, scope, key string, v []byte, _ time.Duration) error {
	if s.kv == nil {
		s.kv = map[string]string{}
	}
	s.kv[scope+"/"+key] = string(v)
	return nil
}

func (s *kvOutputStore) SaveHTTPOutput(ctx context.Context, name, out string) error {
	return s.outputStore.SaveHTTPOutput(ctx, name, out)
}

// RunHTTPInput must not store an agent's error response as the run's answer.
func TestRunHTTPInputRecordsAgentFailure(t *testing.T) {
	bodies := map[string]struct {
		status int
		body   string
	}{
		"502":         {http.StatusBadGateway, `{"error":{"message":"upstream error"}}`},
		"legacy body": {http.StatusOK, "data: [DONE]\n{\"error\":\"upstream error\"}\n"},
		"sse error":   {http.StatusOK, "data: {\"error\":{\"message\":\"upstream error\"}}\n\n"},
	}
	for name, tc := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			port := srv.Listener.Addr().(*net.TCPAddr).Port
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store := &kvOutputStore{outputStore: outputStore{Store: newTestStore(t)}}
			if err := RunHTTPInput(ctx, HTTPInputConfig{Port: port, Path: "/invoke", RunName: "r", Input: "hi"}, store); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(store.kv[HTTPFailureScope+"/r"], "upstream error") {
				t.Fatalf("failure reason = %q", store.kv[HTTPFailureScope+"/r"])
			}
			if !strings.Contains(store.output, "upstream error") || strings.Contains(store.output, "[DONE]") {
				t.Errorf("output = %q", store.output)
			}
		})
	}
}
