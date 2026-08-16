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
