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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrUnauthenticated is returned when a request carries an invalid or missing token.
var ErrUnauthenticated = errors.New("unauthenticated")

// tokenCache caches the result of a TokenReview for the token's remaining validity
// (minus a safety margin). This avoids a round-trip to the API server on every LLM call.
type tokenCache struct {
	mu      sync.Mutex
	entries map[string]tokenCacheEntry
}

type tokenCacheEntry struct {
	saName    string
	expiresAt time.Time
}

var globalTokenCache = &tokenCache{entries: make(map[string]tokenCacheEntry)}

// Authenticator validates Kubernetes ServiceAccount JWT tokens via the TokenReview API.
// The model-router sidecar uses this to authenticate every incoming request.
//
// Token flow:
//  1. Agent pod has a projected SA token at /var/run/secrets/agentorc/token
//  2. Agent framework sends it as "Authorization: Bearer <token>" (via OPENAI_API_KEY env var)
//  3. Authenticator calls k8s TokenReview API to validate the token
//  4. Validates the SA name matches the expected Agent SA
type Authenticator struct {
	kubeAPIURL  string
	saTokenFile string // path to the model-router's own SA token for API calls
	expectedSA  string // "system:serviceaccount:<ns>:<sa-name>"
	httpClient  *http.Client
}

const (
	saTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// NewAuthenticator creates an Authenticator configured for the given run.
// saName is "agentorc-agent-<agent-name>" in the run's namespace.
func NewAuthenticator(kubeAPIURL, namespace, saName string) *Authenticator {
	a := &Authenticator{
		kubeAPIURL:  kubeAPIURL,
		saTokenFile: saTokenFile,
		expectedSA:  fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
		httpClient:  http.DefaultClient,
	}
	// Build an HTTP client that trusts the cluster's CA bundle.
	if caBytes, err := os.ReadFile(saCAFile); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caBytes) {
			a.httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{RootCAs: pool},
				},
			}
		}
	}
	return a
}

// Authenticate validates the bearer token and returns an error if invalid.
// Results are cached to avoid a Kubernetes API round-trip on every LLM call.
func (a *Authenticator) Authenticate(ctx context.Context, bearerToken string) error {
	if bearerToken == "" {
		return fmt.Errorf("%w: missing bearer token", ErrUnauthenticated)
	}

	// Check cache first.
	if entry, ok := globalTokenCache.get(bearerToken); ok {
		if entry.saName != a.expectedSA {
			return fmt.Errorf("%w: SA mismatch (got %s, want %s)", ErrUnauthenticated, entry.saName, a.expectedSA)
		}
		return nil
	}

	saName, err := a.reviewToken(ctx, bearerToken)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	// Cache for 10 minutes (well within the 15-minute token expiry).
	globalTokenCache.set(bearerToken, saName, 10*time.Minute)

	if saName != a.expectedSA {
		return fmt.Errorf("%w: SA mismatch (got %s, want %s)", ErrUnauthenticated, saName, a.expectedSA)
	}
	return nil
}

// reviewToken performs a Kubernetes TokenReview API call and returns the authenticated SA username.
func (a *Authenticator) reviewToken(ctx context.Context, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec": map[string]any{
			"token":     token,
			"audiences": []string{"agentorc/model-router"},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.kubeAPIURL+"/apis/authentication.k8s.io/v1/tokenreviews",
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Use the model-router's own SA token (different audience) to call the k8s API.
	if selfToken, readErr := os.ReadFile(a.saTokenFile); readErr == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(selfToken)))
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("TokenReview request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Status struct {
			Authenticated bool `json:"authenticated"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
			Error string `json:"error"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding TokenReview response: %w", err)
	}
	if !result.Status.Authenticated {
		return "", fmt.Errorf("token not authenticated: %s", result.Status.Error)
	}
	return result.Status.User.Username, nil
}

// ExtractBearerToken returns the token from an "Authorization: Bearer <token>" header.
func ExtractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return after
	}
	return ""
}

func (c *tokenCache) get(token string) (tokenCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[token]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(c.entries, token)
		return tokenCacheEntry{}, false
	}
	return entry, true
}

func (c *tokenCache) set(token, saName string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[token] = tokenCacheEntry{saName: saName, expiresAt: time.Now().Add(ttl)}
}
