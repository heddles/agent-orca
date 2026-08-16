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
	"net/http"
	"sync"

	_ "embed"

	"sigs.k8s.io/yaml"
)

// Embedded OpenAPI specs (canonical source of truth for the external APIs).
//
//go:embed schemas/openapi-external.yaml
var openapiExternalYAML []byte

//go:embed schemas/openapi-acp.yaml
var openapiACPYAML []byte

var (
	externalOpenAPIOnce sync.Once
	externalOpenAPIJSON []byte
	externalOpenAPIErr  error

	acpOpenAPIOnce sync.Once
	acpOpenAPIJSON []byte
	acpOpenAPIErr  error
)

// externalOpenAPIJSONBytes converts the embedded External Task API spec to JSON
// once (lazily) and returns it. Parsing happens at first request, not at
// startup, so a malformed spec can never crash the operator.
func externalOpenAPIJSONBytes() ([]byte, error) {
	externalOpenAPIOnce.Do(func() {
		externalOpenAPIJSON, externalOpenAPIErr = yaml.YAMLToJSON(openapiExternalYAML)
	})
	return externalOpenAPIJSON, externalOpenAPIErr
}

// acpOpenAPIJSONBytes converts the embedded ACP spec to JSON once.
func acpOpenAPIJSONBytes() ([]byte, error) {
	acpOpenAPIOnce.Do(func() {
		acpOpenAPIJSON, acpOpenAPIErr = yaml.YAMLToJSON(openapiACPYAML)
	})
	return acpOpenAPIJSON, acpOpenAPIErr
}

// handleExternalOpenAPI serves the External Task API OpenAPI document at
// GET /openapi.json. This endpoint is intentionally unauthenticated so that
// SDKs, IDE tooling, and integrators can discover the contract without
// credentials.
func (s *ExternalAPIServer) handleExternalOpenAPI(w http.ResponseWriter, r *http.Request) {
	b, err := externalOpenAPIJSONBytes()
	if err != nil {
		http.Error(w, `{"error":"openapi spec unavailable"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}

// handleACPOpenAPI serves the ACP API OpenAPI document at GET /openapi.json.
func (s *ACPServer) handleACPOpenAPI(w http.ResponseWriter, r *http.Request) {
	b, err := acpOpenAPIJSONBytes()
	if err != nil {
		http.Error(w, `{"error":"openapi spec unavailable"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}

// publicPaths are endpoints on the external servers that must remain
// unauthenticated: the token exchange, the OpenAPI contract, the health probes,
// and the metrics endpoint. Everything else is gated by the
// tenant/TokenReview middleware. Resolving the set from a table (rather than an
// else-if chain) keeps it consistent for every server that shares this middleware.
var publicPaths = map[string]bool{
	"/oauth/token":  true,
	"/openapi.json": true,
	"/ping":         true,
	"/healthz":      true,
	"/readyz":       true,
	"/version":      true,
	"/metrics":      true,
}
