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

// TestCompactionTarget_DefaultsAndCaps verifies the compaction target defaults
// to 50% of the context window, is capped at the checkpoint budget (80%), and
// has a 2k token floor.
func TestCompactionTarget_DefaultsAndCaps(t *testing.T) {
	cw := 262144
	budget := cw * 8 / 10 // 209715

	// Default ratio (0) → 0.5 → 50% of CW = 131072. Below budget, above floor.
	r := &Router{cfg: &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: cw}}}}
	if got := r.compactionTarget(); got != cw/2 {
		t.Errorf("default target = %d, want %d", got, cw/2)
	}

	// Ratio above budget → capped to checkpointBudget.
	r.cfg.ContextCompactionRatio = 0.99
	if got := r.compactionTarget(); got != budget {
		t.Errorf("ratio 0.99 should cap at budget %d, got %d", budget, got)
	}
}

// TestCompactionTarget_AggressiveRatio verifies a 0.1 ratio targets ~10% of CW.
func TestCompactionTarget_AggressiveRatio(t *testing.T) {
	cw := 262144
	r := &Router{cfg: &Config{
		Providers:              []ProviderConfig{{Name: "p", ContextWindow: cw}},
		ContextCompactionRatio: 0.1,
	}}
	if got := r.compactionTarget(); got != cw/10 {
		t.Errorf("aggressive target = %d, want %d (10%% of CW)", got, cw/10)
	}
	if got := r.compactionTarget(); got > r.checkpointBudget() {
		t.Errorf("target %d should not exceed budget %d", got, r.checkpointBudget())
	}
}

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
