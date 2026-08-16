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
	"os"
)

// corsMiddleware wraps an http.Handler with CORS headers.
// The allowed origin is read from the CORS_ALLOWED_ORIGIN env var. When empty,
// no CORS headers are emitted (same-origin policy applies).
func corsMiddleware(next http.Handler) http.Handler {
	allowedOrigin := os.Getenv("CORS_ALLOWED_ORIGIN")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowedOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeAuthFailureJSON writes a simple JSON 401 denial response.
func writeAuthFailureJSON(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// writeOperatorAuthFailure writes JSON deny for operator internal API (8082) auth failures.
func writeOperatorAuthFailure(w http.ResponseWriter, missingToken bool, validateErr error) {
	if missingToken {
		writeAuthFailureJSON(w, "missing Authorization header")
		return
	}
	msg := "unauthorized"
	if validateErr != nil {
		msg = validateErr.Error()
	}
	writeAuthFailureJSON(w, msg)
}

// writeUIAuthFailure writes JSON deny for UI API auth (TokenReview audience agentorc/ui).
func writeUIAuthFailure(w http.ResponseWriter, missingToken bool, validateErr error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="agentorc-ui"`)
	if missingToken {
		writeAuthFailureJSON(w, "authentication required")
		return
	}
	msg := "invalid or expired token"
	if validateErr != nil {
		msg = validateErr.Error()
	}
	writeAuthFailureJSON(w, msg)
}
