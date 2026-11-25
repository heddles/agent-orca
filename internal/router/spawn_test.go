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
	"testing"
	"time"
)

// operatorSpawnAPI serves only the routes the operator registers for a child
// AgentRun: POST /agentrun/{ns} and GET /agentrun/{ns}/{name}. Any other path
// is a 404, as in the operator, so a poll against a wrong route fails.
func operatorSpawnAPI(t *testing.T, phase string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(strings.Trim(strings.TrimPrefix(req.URL.Path, "/agentrun/"), "/"), "/")
		switch {
		case req.Method == http.MethodPost && len(parts) == 1:
			w.WriteHeader(http.StatusCreated)
		case req.Method == http.MethodGet && len(parts) == 2:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"phase": phase, "output": "child answer", "failureReason": "boom"},
			})
		default:
			http.Error(w, "404 page not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func spawnRouter(t *testing.T, operatorURL string) *Router {
	t.Helper()
	return &Router{cfg: &Config{
		RunName:        "parent",
		RunNamespace:   "ns-1",
		OperatorAPIURL: operatorURL,
		SATokenFile:    fsSATokenFile(t),
	}}
}

func TestExecuteSpawn_ReturnsSucceededChildOutput(t *testing.T) {
	srv := operatorSpawnAPI(t, "Succeeded")
	r := spawnRouter(t, srv.URL)

	start := time.Now()
	out := r.executeSpawn(t.Context(), `{"agentRef":"child","input":"go","timeoutSeconds":600}`)
	if time.Since(start) > 30*time.Second {
		t.Fatalf("spawn took %s; it should return once the child succeeds", time.Since(start))
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %q", out)
	}
	if got["status"] != "succeeded" || got["output"] != "child answer" {
		t.Fatalf("got %v, want succeeded with the child's output", got)
	}
}

func TestExecuteSpawn_ReturnsFailedChildReason(t *testing.T) {
	srv := operatorSpawnAPI(t, "Failed")
	r := spawnRouter(t, srv.URL)

	out := r.executeSpawn(t.Context(), `{"agentRef":"child","input":"go","timeoutSeconds":600}`)
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %q", out)
	}
	if got["status"] != "failed" || got["reason"] != "boom" {
		t.Fatalf("got %v, want failed with the child's reason", got)
	}
}

func TestExecuteSpawn_TimeoutReportsPollError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	r := spawnRouter(t, srv.URL)

	out := r.executeSpawn(t.Context(), `{"agentRef":"child","input":"go","timeoutSeconds":3}`)
	if !strings.Contains(out, "timed out") || !strings.Contains(out, "HTTP 404") {
		t.Fatalf("got %s, want a timeout that names the HTTP 404 poll error", out)
	}
}
