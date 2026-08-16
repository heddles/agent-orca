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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEmbeddedSpecsAreValidJSON ensures the embedded OpenAPI YAML documents
// parse to valid JSON (the conversion we serve at runtime) and look like an
// OpenAPI document.
func TestEmbeddedSpecsAreValidJSON(t *testing.T) {
	for name, conv := range map[string]func() ([]byte, error){
		"external": externalOpenAPIJSONBytes,
		"acp":      acpOpenAPIJSONBytes,
	} {
		t.Run(name, func(t *testing.T) {
			b, err := conv()
			if err != nil {
				t.Fatalf("converting %s spec to JSON: %v", name, err)
			}
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s spec is not valid JSON: %v", name, err)
			}
			if doc["openapi"] == "" {
				t.Fatalf("%s spec missing openapi version", name)
			}
			if doc["paths"] == nil {
				t.Fatalf("%s spec missing paths", name)
			}
			// Ensure paths is a non-empty object.
			paths, ok := doc["paths"].(map[string]any)
			if !ok || len(paths) == 0 {
				t.Fatalf("%s spec has no paths", name)
			}
		})
	}
}

// TestExternalOpenAPIEndpointServesSpec verifies GET /openapi.json on the
// External Task API returns the spec as JSON and is not gated by auth.
func TestExternalOpenAPIEndpointServesSpec(t *testing.T) {
	// Auth is not needed for this path (it short-circuits before touching
	// auth internals), so a zero-value ExternalAuth is sufficient here.
	srv := &ExternalAPIServer{auth: &ExternalAuth{}}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected application/json content-type, got %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/v1/tasks"]; !ok {
		t.Fatalf("spec missing /v1/tasks path; paths=%v", paths)
	}
	if _, ok := paths["/oauth/token"]; !ok {
		t.Fatalf("spec missing /oauth/token path")
	}
}

// TestACPOOpenAPIEndpointServesSpec verifies GET /openapi.json on the ACP API
// returns the ACP spec and is reachable without auth.
func TestACPOOpenAPIEndpointServesSpec(t *testing.T) {
	srv := &ACPServer{auth: &ExternalAuth{}}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected application/json content-type, got %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	for _, expect := range []string{"/ping", "/agents", "/runs", "/session/{session_id}"} {
		if _, ok := paths[expect]; !ok {
			t.Fatalf("ACP spec missing %q path; paths=%v", expect, paths)
		}
	}
}

// TestOpenAPIIsPublicButTasksRequireAuth verifies the public-path table: the
// OpenAPI contract is reachable with no credentials, while a protected task
// endpoint rejects an unauthenticated request with 401.
func TestOpenAPIIsPublicButTasksRequireAuth(t *testing.T) {
	srv := &ExternalAPIServer{auth: &ExternalAuth{}}
	h := srv.Handler()

	// /openapi.json: no Authorization header -> must NOT 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("openapi endpoint should be public, got 401")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi: expected 200, got %d", rec.Code)
	}

	// /v1/tasks: no Authorization header -> 401 "missing Authorization header".
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("tasks endpoint should require auth (401), got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "Authorization") {
		t.Fatalf("expected 401 to mention Authorization, got: %s", rec2.Body.String())
	}
}

// TestPublicPathsSet enumerates the intended unauthenticated surface so a
// future refactor can't silently lock out OpenAPI / ping / token exchange.
func TestPublicPathsSet(t *testing.T) {
	want := map[string]bool{
		"/oauth/token":  true,
		"/openapi.json": true,
		"/ping":         true,
		"/healthz":      true,
		"/readyz":       true,
		"/version":      true,
		"/metrics":      true,
	}
	for p := range want {
		if !publicPaths[p] {
			t.Errorf("publicPaths missing %q", p)
		}
	}
	for p := range publicPaths {
		if !want[p] {
			t.Errorf("publicPaths has unexpected entry %q", p)
		}
	}
}
