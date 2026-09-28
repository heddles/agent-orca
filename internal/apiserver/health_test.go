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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/state"
)

// fakeReadyStore is a minimal state.Store used only to drive /readyz's Ping path.
type fakeReadyStore struct{ pingErr error }

func (f fakeReadyStore) SaveMessages(context.Context, string, []json.RawMessage, time.Duration) error {
	return nil
}
func (f fakeReadyStore) LoadMessages(context.Context, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f fakeReadyStore) SaveSpend(context.Context, string, float64, time.Duration) error { return nil }
func (f fakeReadyStore) LoadSpend(context.Context, string) (float64, error)              { return 0, nil }
func (f fakeReadyStore) SaveToken(context.Context, string, string) error                 { return nil }
func (f fakeReadyStore) SaveTraceEvent(context.Context, string, string) error            { return nil }
func (f fakeReadyStore) ReadTraceEvents(context.Context, string) ([]state.TraceEntry, error) {
	return nil, nil
}
func (f fakeReadyStore) TailTokens(context.Context, string) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (f fakeReadyStore) SaveAnswer(context.Context, string, string, time.Duration) error { return nil }
func (f fakeReadyStore) LoadAnswer(context.Context, string) (string, error)              { return "", nil }
func (f fakeReadyStore) SaveHTTPOutput(context.Context, string, string) error            { return nil }
func (f fakeReadyStore) LoadHTTPOutput(context.Context, string) (string, error)          { return "", nil }
func (f fakeReadyStore) DeleteKey(context.Context, string) error                         { return nil }
func (f fakeReadyStore) SaveKV(context.Context, string, string, []byte, time.Duration) error {
	return nil
}
func (f fakeReadyStore) LoadKV(context.Context, string, string) ([]byte, error) { return nil, nil }
func (f fakeReadyStore) DeleteKV(context.Context, string, string) error         { return nil }
func (f fakeReadyStore) ListKV(context.Context, string) ([]string, error)       { return nil, nil }
func (f fakeReadyStore) ListMessageKeys(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f fakeReadyStore) SignalCancel(context.Context, string, string) error { return nil }
func (f fakeReadyStore) IsCancelled(context.Context, string, string) (bool, error) {
	return false, nil
}
func (f fakeReadyStore) Ping(context.Context) error { return f.pingErr }
func (f fakeReadyStore) Close() error               { return nil }

var _ state.Store = fakeReadyStore{}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	healthzHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("expected ok body, got %q", rec.Body.String())
	}
}

func TestVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	versionHandler(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var v map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if v["version"] != Version {
		t.Fatalf("expected %q, got %q", Version, v["version"])
	}
}

func TestReadyz_NoStore(t *testing.T) {
	h := readyzHandlerBuilder(nil, false, nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReadyz_StorePingFails(t *testing.T) {
	store := fakeReadyStore{pingErr: errors.New("redis down")}
	h := readyzHandlerBuilder(nil, true, store)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestReadyz_K8sUnreachable(t *testing.T) {
	// A k8sReady func that fails simulates an unreachable API server.
	h := readyzHandlerBuilder(func() error { return errors.New("api server unreachable") }, false, nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when K8s unreachable, got %d", rec.Code)
	}
}

// TestInstrument_RecordsMetrics verifies instrument increments counters and
// surfaces them via the /metrics endpoint, plus records an audit log entry on a 401.
func TestInstrument_RecordsMetrics(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler())
	mux.Handle("/", instrument("external", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	m, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(m.Body)
	_ = m.Body.Close()
	if !strings.Contains(string(body), "agentorca_external_requests_total") {
		t.Fatalf("metrics missing requests counter:\n%s", body)
	}
	if !strings.Contains(string(body), "agentorca_external_auth_failures_total") {
		t.Fatalf("metrics missing auth-failures counter:\n%s", body)
	}
}

// TestExternalAPI_PublicHealth verifies the External API serves probe endpoints
// unauthenticated while still gating the protected API surface. (/ping is an ACP
// endpoint, not exposed on the External Task API.)
func TestExternalAPI_PublicHealth(t *testing.T) {
	srv := &ExternalAPIServer{auth: &ExternalAuth{}}
	h := srv.Handler()

	for _, path := range []string{"/healthz", "/version", "/metrics", "/openapi.json"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code >= 400 {
			t.Fatalf("%s: expected <400, got %d", path, rec.Code)
		}
	}

	// Protected endpoint without a token → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /v1/tasks, got %d", rec.Code)
	}
}
