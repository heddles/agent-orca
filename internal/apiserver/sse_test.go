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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// shortWriteTimeoutServer starts h behind instrument() (as production does)
// on a server whose WriteTimeout is far shorter than the streams under test.
func shortWriteTimeoutServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	api := httptest.NewUnstartedServer(instrument("test", h))
	api.Config.WriteTimeout = 200 * time.Millisecond
	api.Start()
	t.Cleanup(api.Close)
	return api
}

// readStream GETs url as an SSE client and returns the body and the first
// transport error (request or body read).
func readStream(t *testing.T, api *httptest.Server, path string) (string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, api.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := api.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// tickingSSEHandler writes n events gap apart through a rolling-deadline
// flusher with the given idle bound.
func tickingSSEHandler(n int, gap, idle time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher := newRollingDeadlineFlusher(w, r, w.(http.Flusher), idle)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range n {
			time.Sleep(gap)
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: end\n\n")
		flusher.Flush()
	}
}

// TestRollingDeadlineFlusher_OutlivesWriteTimeout verifies that renewing the
// deadline on every flush lets a stream run well past the server WriteTimeout.
func TestRollingDeadlineFlusher_OutlivesWriteTimeout(t *testing.T) {
	api := shortWriteTimeoutServer(t, tickingSSEHandler(6, 100*time.Millisecond, 300*time.Millisecond))
	body, err := readStream(t, api, "/stream")
	if err != nil {
		t.Fatalf("stream cut off: %v (got %q)", err, body)
	}
	if !strings.Contains(body, "data: end") {
		t.Fatalf("stream ended early:\n%s", body)
	}
}

// TestRollingDeadlineFlusher_StalledWriteStillFails verifies the deadline is a
// per-event bound, not unlimited: a gap longer than idle still tears the
// connection down.
func TestRollingDeadlineFlusher_StalledWriteStillFails(t *testing.T) {
	api := shortWriteTimeoutServer(t, tickingSSEHandler(2, 300*time.Millisecond, 100*time.Millisecond))
	body, err := readStream(t, api, "/stream")
	if err == nil && strings.Contains(body, "data: end") {
		t.Fatalf("stream survived a gap longer than the idle bound:\n%s", body)
	}
}

// TestRollingDeadlineFlusher_UnsupportedWriter verifies a writer without
// deadline support (httptest.ResponseRecorder) still flushes normally.
func TestRollingDeadlineFlusher_UnsupportedWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	f := newRollingDeadlineFlusher(rec, httptest.NewRequest(http.MethodGet, "/stream", nil), rec, time.Minute)
	_, _ = fmt.Fprint(rec, "data: x\n\n")
	f.Flush()
	f.Flush()
	if !rec.Flushed {
		t.Fatal("underlying writer was not flushed")
	}
	if d, ok := f.(*rollingDeadlineFlusher); !ok || !d.failed {
		t.Fatal("expected the unsupported deadline to be recorded (warn once)")
	}
}

// TestStreamTask_OutlivesServerWriteTimeout is the handler-level regression for
// GET /v1/tasks/{id}/stream being cut at the 8084 server's WriteTimeout.
func TestStreamTask_OutlivesServerWriteTimeout(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-1",
			Namespace: "tenant-acme",
			Labels:    map[string]string{"agentorca.io/tenant": "acme"},
		},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
	tokens := []string{"a", "b", "c", "d", "e"}
	s := &ExternalAPIServer{
		auth:      &ExternalAuth{},
		crdClient: fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(run).Build(),
		hasStore:  true,
		store:     &slowTailStore{tokens: tokens, gap: 100 * time.Millisecond},
	}
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme", Namespaces: []string{"tenant-acme"}}
	api := shortWriteTimeoutServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.streamTask(w, r.WithContext(context.WithValue(r.Context(), tenantIdentityKey, tenant)), "task-1")
	}))

	body, err := readStream(t, api, "/v1/tasks/task-1/stream")
	if err != nil {
		t.Fatalf("stream cut off before completion: %v (got %q)", err, body)
	}
	for _, tok := range tokens {
		if !strings.Contains(body, `"content":"`+tok+`"`) {
			t.Fatalf("token %q missing from stream:\n%s", tok, body)
		}
	}
	if !strings.Contains(body, "event: complete") {
		t.Fatalf("stream ended without the complete event:\n%s", body)
	}
}
