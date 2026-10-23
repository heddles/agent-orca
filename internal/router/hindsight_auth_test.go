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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// authRecorder is a stand-in hindsight API that records the Authorization
// header of every request, whatever the endpoint.
type authRecorder struct {
	mu      sync.Mutex
	headers []string
}

func (a *authRecorder) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.headers = append(a.headers, r.Header.Get("Authorization"))
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (a *authRecorder) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.headers...)
}

func hindsightTestRouter(t *testing.T, url, apiKey string) *Router {
	t.Helper()
	r := newTracingTestRouter(t, newTestStore(t))
	r.cfg.Hindsight = HindsightConfig{Enabled: true, URL: url, RecallBudget: 5, APIKey: apiKey}
	return r
}

// Regression for #58: with hindsight's ApiKeyTenantExtension on, every call the
// router makes (bank create, recall, retain) must carry the Bearer key.
func TestHindsightCallsSendBearerAPIKey(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	t.Cleanup(srv.Close)
	r := hindsightTestRouter(t, srv.URL, "s3cret")

	// The responses are not valid hindsight payloads, so errors are expected;
	// only the request headers matter here.
	_, _ = r.ensureHindsightBank(context.Background())
	_, _ = r.hindsightRecall(context.Background(), "what did we decide?")
	_ = r.hindsightRetain("run-1", "some content")

	got := rec.seen()
	if len(got) != 3 {
		t.Fatalf("expected 3 requests (bank, recall, retain), got %d: %v", len(got), got)
	}
	for i, h := range got {
		if h != "Bearer s3cret" {
			t.Errorf("request %d Authorization = %q, want %q", i, h, "Bearer s3cret")
		}
	}
}

// Without a key configured nothing is sent, so unauthenticated hindsight
// deployments keep working.
func TestHindsightCallsOmitAuthorizationWithoutKey(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	t.Cleanup(srv.Close)
	r := hindsightTestRouter(t, srv.URL, "")

	_, _ = r.ensureHindsightBank(context.Background())
	_, _ = r.hindsightRecall(context.Background(), "q")
	_ = r.hindsightRetain("run-1", "c")

	got := rec.seen()
	if len(got) == 0 {
		t.Fatal("no requests reached the server")
	}
	for i, h := range got {
		if h != "" {
			t.Errorf("request %d sent Authorization %q with no key configured", i, h)
		}
	}
}

func TestNewHindsightClientHeader(t *testing.T) {
	c := newHindsightClient(HindsightConfig{URL: "http://h:8888", APIKey: "k"})
	if got := c.GetConfig().DefaultHeader["Authorization"]; got != "Bearer k" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer k")
	}
	c = newHindsightClient(HindsightConfig{URL: "http://h:8888"})
	if _, ok := c.GetConfig().DefaultHeader["Authorization"]; ok {
		t.Fatal("Authorization header set without an API key")
	}
}

func writeRouterConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router-config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTORC_ROUTER_CONFIG", path)
}

func TestConfigFromEnv_HindsightAPIKey(t *testing.T) {
	writeRouterConfig(t, `{"runName":"r","hindsight":{"enabled":true}}`)
	t.Setenv("HINDSIGHT_API_KEY", "  from-env\n")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hindsight.APIKey != "from-env" {
		t.Fatalf("APIKey = %q, want %q (trimmed)", cfg.Hindsight.APIKey, "from-env")
	}
}

func TestConfigFromEnv_HindsightAPIKeyUnset(t *testing.T) {
	writeRouterConfig(t, `{"runName":"r","hindsight":{"enabled":true}}`)
	t.Setenv("HINDSIGHT_API_KEY", "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hindsight.APIKey != "" {
		t.Fatalf("APIKey = %q, want empty", cfg.Hindsight.APIKey)
	}
}

// The key must never be serialized into the router config (it is written to a
// ConfigMap by the operator) and must not be accepted from it either.
func TestHindsightAPIKeyNotInConfigJSON(t *testing.T) {
	out, err := json.Marshal(HindsightConfig{Enabled: true, URL: "http://h", APIKey: "topsecret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "topsecret") {
		t.Fatalf("API key leaked into config JSON: %s", out)
	}

	writeRouterConfig(t, `{"runName":"r","hindsight":{"enabled":true,"apiKey":"from-file"}}`)
	t.Setenv("HINDSIGHT_API_KEY", "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hindsight.APIKey != "" {
		t.Fatalf("APIKey = %q; the key must only come from HINDSIGHT_API_KEY", cfg.Hindsight.APIKey)
	}
}

func TestHindsightBankID(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"default is one bank per agent",
			Config{RunNamespace: "team-a", AgentName: "coder", RunName: "run-1"}, "team-a--coder"},
		{"default ignores the deployment",
			Config{RunNamespace: "team-a", AgentName: "senior-programmer", DeploymentName: "senior-programmer-chat"},
			"team-a--senior-programmer"},
		{"template placeholders",
			Config{RunNamespace: "ns", AgentName: "a", RunName: "r", DeploymentName: "d",
				Hindsight: HindsightConfig{BankIDTemplate: "{namespace}/{deployment}/{agent-name}/{run-name}"}},
			"ns/d/a/r"},
		{"camelCase placeholders",
			Config{RunNamespace: "ns", AgentName: "a", RunName: "r",
				Hindsight: HindsightConfig{BankIDTemplate: "{namespace}-{agentName}-{runName}"}},
			"ns-a-r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Router{cfg: &tt.cfg}
			if got := r.hindsightBankID(); got != tt.want {
				t.Errorf("bank = %q, want %q", got, tt.want)
			}
		})
	}
}
