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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
)

// TestACPRequestTimeoutMiddleware_Selection verifies the per-request timeout
// choice
func TestACPRequestTimeoutMiddleware_Selection(t *testing.T) {
	const (
		defaultTimeout = time.Second
		streamTimeout  = 2 * time.Minute
	)

	// The probe records the deadline the handler received.
	var probe http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := ctxDeadline(r.Context())
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("no deadline"))
			return
		}
		remaining := time.Until(deadline)
		w.WriteHeader(http.StatusOK)
		switch {
		case remaining > defaultTimeout+time.Minute:
			_, _ = w.Write([]byte("stream"))
		default:
			_, _ = w.Write([]byte("bounded"))
		}
	}
	h := acpRequestTimeoutMiddleware(probe, defaultTimeout)

	cases := []struct {
		name   string
		method string
		path   string
		accept string
		want   string
	}{
		{"live run stream", http.MethodGet, "/runs/acp-run-1", "text/event-stream", "stream"},
		{"accept wildcard alongside event-stream", http.MethodGet, "/runs/acp-run-1", "text/event-stream, */*", "stream"},
		{"run read without SSE accept", http.MethodGet, "/runs/acp-run-1", "application/json", "bounded"},
		{"events list", http.MethodGet, "/runs/acp-run-1/events", "text/event-stream", "bounded"},
		{"fs pending poll", http.MethodGet, "/runs/acp-run-1/fs/pending", "text/event-stream", "bounded"},
		{"resume post", http.MethodPost, "/runs/acp-run-1", "text/event-stream", "bounded"},
		{"other endpoints", http.MethodGet, "/agents/bot", "text/event-stream", "bounded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Accept", tc.accept)
			req = req.WithContext(context.Background())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Body.String(); got != tc.want {
				t.Fatalf("timeout selection = %q, want %q", got, tc.want)
			}
		})
	}
}

// ctxDeadline is context.Deadline for test readability.
func ctxDeadline(ctx context.Context) (time.Time, bool) {
	return ctx.Deadline()
}

// slowTailStore yields tokens with a delay between each, modelling a run that
// keeps streaming longer than the http.Server's WriteTimeout.
type slowTailStore struct {
	state.Store
	tokens []string
	gap    time.Duration
}

func (s *slowTailStore) TailTokens(ctx context.Context, _ string) (<-chan string, error) {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for _, tok := range s.tokens {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.gap):
			}
			select {
			case <-ctx.Done():
				return
			case ch <- tok:
			}
		}
	}()
	return ch, nil
}

// TestStreamACPRun_OutlivesServerWriteTimeout is the regression for SSE
// streams being cut at the server's WriteTimeout (120s in newHTTPServer)
// instead of acpStreamWindow: the stream must extend its write deadline so a
// long run is delivered in one connection. The chain mirrors production
// (timeout middleware → instrument wrapper → handler) so the
// recordingResponseWriter must expose Unwrap for ResponseController.
func TestStreamACPRun_OutlivesServerWriteTimeout(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "acp-run-1", Namespace: "tenant-acme"},
	}
	s := newACPServer(t, run)
	tokens := []string{"a", "b", "c", "d", "e"}
	s.store = &slowTailStore{tokens: tokens, gap: 100 * time.Millisecond}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.streamACPRun(w, r, run); err != nil {
			t.Errorf("streamACPRun: %v", err)
		}
	})
	api := httptest.NewUnstartedServer(acpRequestTimeoutMiddleware(instrument("acp", handler), time.Second))
	// Far shorter than the ~500ms stream: without the deadline extension the
	// connection is torn down mid-stream.
	api.Config.WriteTimeout = 200 * time.Millisecond
	api.Start()
	t.Cleanup(api.Close)

	req, err := http.NewRequest(http.MethodGet, api.URL+"/runs/acp-run-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := api.Client().Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut off before completion: %v (got %q)", err, body)
	}
	got := string(body)
	for _, tok := range tokens {
		if !strings.Contains(got, `"content":"`+tok+`"`) {
			t.Fatalf("token %q missing from stream:\n%s", tok, got)
		}
	}
	if !strings.Contains(got, "event: run.completed") {
		t.Fatalf("stream ended without run.completed:\n%s", got)
	}
}

// TestRecordingResponseWriterUnwrap verifies ResponseController can reach the
// underlying writer through the instrument wrapper.
func TestRecordingResponseWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &recordingResponseWriter{ResponseWriter: rec}
	if rw.Unwrap() != rec {
		t.Fatal("Unwrap did not return the wrapped ResponseWriter")
	}
}
