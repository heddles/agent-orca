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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
