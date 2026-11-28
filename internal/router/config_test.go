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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConfigFromEnv_LLMRequestTimeoutDefault verifies an unset/empty
// llmRequestTimeout defaults to the new 1h cap (was a 120s hard-coded const).
func TestConfigFromEnv_LLMRequestTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTORC_ROUTER_CONFIG", path)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.LLMRequestTimeout != time.Hour {
		t.Fatalf("expected default LLMRequestTimeout = 1h, got %v", cfg.LLMRequestTimeout)
	}
}

// TestConfigFromEnv_LLMRequestTimeoutOverride verifies a config-supplied
// duration is honored (configurable per deployment).
func TestConfigFromEnv_LLMRequestTimeoutOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	contents := fmt.Sprintf(`{"llmRequestTimeout": %d}`, (30 * time.Minute).Nanoseconds())
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTORC_ROUTER_CONFIG", path)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.LLMRequestTimeout != 30*time.Minute {
		t.Fatalf("expected LLMRequestTimeout = 30m, got %v", cfg.LLMRequestTimeout)
	}
}

// TestHTTPInputTimeoutDefault verifies the agent-HTTP-respon se wait is no longer
// the old 60s hard-cap (which killed long-trajectory sessions mid-task) and instead
// defaults to 30m so long chats can complete.
func TestHTTPInputTimeoutDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  HTTPInputConfig
		want time.Duration
	}{
		{name: "unset (0) defaults to 30m", cfg: HTTPInputConfig{}, want: 30 * time.Minute},
		{name: "explicit 120s honored", cfg: HTTPInputConfig{TimeoutSeconds: 120}, want: 120 * time.Second},
		{name: "explicit 1h honored", cfg: HTTPInputConfig{TimeoutSeconds: 3600}, want: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpInputTimeout(tt.cfg); got != tt.want {
				t.Fatalf("httpInputTimeout=%v want %v", got, tt.want)
			}
		})
	}
}

// --- context compaction target tests ---

// TestConfigFromEnv_CompactionRatioDefault verifies ConfigFromEnv seeds the default.
func TestConfigFromEnv_CompactionRatioDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router-config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTORC_ROUTER_CONFIG", path)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.ContextCompactionRatio != 0.5 {
		t.Errorf("default ContextCompactionRatio = %v, want 0.5", cfg.ContextCompactionRatio)
	}
}

// TestConfigFromEnv_RetainOnEveryTurnIsHonoured is the regression for
// ConfigFromEnv coercing retainOnEveryTurn: false back to true, which made the
// flag — and the streaming retain gate that reads it — unreachable through
// normal configuration. An explicit false must survive verbatim; an omitted
// key defaults to true (the CRD default).
func TestConfigFromEnv_RetainOnEveryTurnIsHonoured(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{name: "explicit false stays false", body: `{"runName":"r","hindsight":{"enabled":true,"retainOnEveryTurn":false}}`, want: false},
		{name: "explicit true stays true", body: `{"runName":"r","hindsight":{"enabled":true,"retainOnEveryTurn":true}}`, want: true},
		{name: "omitted defaults to true", body: `{"runName":"r","hindsight":{"enabled":true}}`, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeRouterConfig(t, tt.body)
			t.Setenv("HINDSIGHT_API_KEY", "")
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Hindsight.RetainOnEveryTurn != tt.want {
				t.Errorf("RetainOnEveryTurn = %v, want %v", cfg.Hindsight.RetainOnEveryTurn, tt.want)
			}
		})
	}
}

// TestHindsightRetainOnEveryTurnIsMarshaled is the regression for the operator
// path: the controllers serialize router.Config into the router ConfigMap with
// json.Marshal. With omitempty on RetainOnEveryTurn, an Agent that set
// retainOnEveryTurn: false produced a config with the key absent, the router's
// default turned it back on, and the per-turn retain gate never closed. The key
// must always be present so an explicit false survives the round trip.
func TestHindsightRetainOnEveryTurnIsMarshaled(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run(fmt.Sprintf("retain=%v", want), func(t *testing.T) {
			// This is the operator's path: marshal the struct, write it, and let
			// the router read it back.
			cfg := Config{RunName: "r", Hindsight: HindsightConfig{Enabled: true, RetainOnEveryTurn: want}}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			var h map[string]json.RawMessage
			if err := json.Unmarshal(raw["hindsight"], &h); err != nil {
				t.Fatal(err)
			}
			if _, ok := h["retainOnEveryTurn"]; !ok {
				t.Fatalf("hindsight.retainOnEveryTurn omitted from marshaled config: %s", data)
			}

			writeRouterConfig(t, string(data))
			t.Setenv("HINDSIGHT_API_KEY", "")
			got, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if got.Hindsight.RetainOnEveryTurn != want {
				t.Errorf("round-tripped RetainOnEveryTurn = %v, want %v", got.Hindsight.RetainOnEveryTurn, want)
			}
		})
	}
}
