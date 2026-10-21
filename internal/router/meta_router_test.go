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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMetaProviderConfig builds a Config with two providers (whose names
// deliberately collide on substring: gpt-4o ⊂ gpt-4o-mini) pointed at the
// given LiteLLM-compatible test server.
func fakeMetaProviderConfig(baseURL, apiKeyFile string, order int) *Config {
	a, b := "gpt-4o", "gpt-4o-mini"
	if order == 1 {
		a, b = "gpt-4o-mini", "gpt-4o"
	}
	return &Config{
		Providers: []ProviderConfig{
			{Name: a, LiteLLMModel: a, BaseURL: baseURL, APIKeyFile: apiKeyFile},
			{Name: b, LiteLLMModel: b, BaseURL: baseURL, APIKeyFile: apiKeyFile},
		},
		MetaRouterProviderName: a,
	}
}

func writeTestAPIKeyFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeMetaLLMServer returns an httptest server that responds to any
// chat/completions request with the configured provider name text.
func fakeMetaLLMServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": response}},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
}

func TestResolveProviderName(t *testing.T) {
	tests := []struct {
		name      string
		providers []string
		raw       string
		want      string
		wantOK    bool
	}{
		{
			name:      "exact match",
			providers: []string{"gpt-4o", "gpt-4o-mini"},
			raw:       "gpt-4o",
			want:      "gpt-4o",
			wantOK:    true,
		},
		{
			name:      "mini response resolves to mini even when gpt-4o is first",
			providers: []string{"gpt-4o", "gpt-4o-mini"},
			raw:       "gpt-4o-mini",
			want:      "gpt-4o-mini",
			wantOK:    true,
		},
		{
			name:      "mini response resolves to mini when mini is first",
			providers: []string{"gpt-4o-mini", "gpt-4o"},
			raw:       "gpt-4o-mini",
			want:      "gpt-4o-mini",
			wantOK:    true,
		},
		{
			name:      "case-insensitive exact",
			providers: []string{"GPT-4o-Mini"},
			raw:       "gpt-4o-mini",
			want:      "GPT-4o-Mini",
			wantOK:    true,
		},
		{
			name:      "fuzzy with surrounding text",
			providers: []string{"claude-sonnet-4-6"},
			raw:       "The best provider is claude-sonnet-4-6.",
			want:      "claude-sonnet-4-6",
			wantOK:    true,
		},
		{
			name:      "blank response rejected",
			providers: []string{"gpt-4o"},
			raw:       "   ",
			want:      "",
			wantOK:    false,
		},
		{
			name:      "empty response rejected",
			providers: []string{"gpt-4o"},
			raw:       "",
			want:      "",
			wantOK:    false,
		},
		{
			name:      "unknown provider rejected",
			providers: []string{"gpt-4o"},
			raw:       "gpt-5",
			want:      "",
			wantOK:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Providers: make([]ProviderConfig, 0, len(tt.providers))}
			for _, n := range tt.providers {
				cfg.Providers = append(cfg.Providers, ProviderConfig{Name: n})
			}
			m := NewMetaRouter(cfg)
			got, ok := m.resolveProviderName(tt.raw)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("resolveProviderName(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRouteResolvesMiniRegardlessOfProviderOrder(t *testing.T) {
	for order := 0; order < 2; order++ {
		t.Run(fmt.Sprintf("order%d", order), func(t *testing.T) {
			apiKeyFile := writeTestAPIKeyFile(t)
			srv := fakeMetaLLMServer(t, "gpt-4o-mini")
			defer srv.Close()
			cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, order)
			m := NewMetaRouter(cfg)
			name, _, err := m.Route(context.Background(), []Message{{Role: "user", Content: "hi"}})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if name != "gpt-4o-mini" {
				t.Errorf("got %q, want gpt-4o-mini (order %d)", name, order)
			}
		})
	}
}

func TestRouteCacheKeyRespectsStateBuckets(t *testing.T) {
	apiKeyFile := writeTestAPIKeyFile(t)
	srv := fakeMetaLLMServer(t, "gpt-4o-mini")
	defer srv.Close()
	cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, 0)
	m := NewMetaRouter(cfg)
	msgs := []Message{{Role: "user", Content: "same task"}}

	// First call caches under the high-budget bucket.
	first := RoutingState{BudgetUSD: 10, SpentUSD: 1, TurnCount: 1}
	if _, _, err := m.Route(context.Background(), msgs, first); err != nil {
		t.Fatalf("first Route: %v", err)
	}

	// Same bucket (different raw values, same buckets) → same cache key.
	sameBucket := RoutingState{BudgetUSD: 10, SpentUSD: 5, TurnCount: 5}
	m.cacheMu.Lock()
	_, cachedSame := m.cache[m.cacheKey(msgs, sameBucket)]
	m.cacheMu.Unlock()
	if !cachedSame {
		t.Errorf("expected cache hit within the same state bucket")
	}

	// Budget crossing the 20% boundary → cache MISS (different key).
	lowBudget := RoutingState{BudgetUSD: 10, SpentUSD: 8.5, TurnCount: 5}
	m.cacheMu.Lock()
	_, cachedLow := m.cache[m.cacheKey(msgs, lowBudget)]
	m.cacheMu.Unlock()
	if cachedLow {
		t.Errorf("expected cache miss when budget crosses the 20%% boundary")
	}

	// Timeout crossing the 70% boundary → cache MISS.
	urgent := RoutingState{BudgetUSD: 10, SpentUSD: 5, TimeoutTotalSec: 100, TimeoutElapsedSec: 80, TurnCount: 5}
	m.cacheMu.Lock()
	_, cachedUrgent := m.cache[m.cacheKey(msgs, urgent)]
	m.cacheMu.Unlock()
	if cachedUrgent {
		t.Errorf("expected cache miss when timeout goes urgent")
	}

	// Turn bucket change → cache MISS.
	deep := RoutingState{BudgetUSD: 10, SpentUSD: 5, TurnCount: 50}
	m.cacheMu.Lock()
	_, cachedDeep := m.cache[m.cacheKey(msgs, deep)]
	m.cacheMu.Unlock()
	if cachedDeep {
		t.Errorf("expected cache miss when the turn bucket changes")
	}
}

func TestRouteCacheHitReturnsCachedProvider(t *testing.T) {
	apiKeyFile := writeTestAPIKeyFile(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "gpt-4o-mini"}},
			},
		})
	}))
	defer srv.Close()
	cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, 0)
	m := NewMetaRouter(cfg)
	msgs := []Message{{Role: "user", Content: "cache me"}}
	state := RoutingState{BudgetUSD: 5, SpentUSD: 1}

	if _, _, err := m.Route(context.Background(), msgs, state); err != nil {
		t.Fatalf("first Route: %v", err)
	}
	if _, _, err := m.Route(context.Background(), msgs, state); err != nil {
		t.Fatalf("second Route: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 LLM call (cache hit on second), got %d", calls)
	}
}

func TestRouteBlankResponseErrors(t *testing.T) {
	apiKeyFile := writeTestAPIKeyFile(t)
	srv := fakeMetaLLMServer(t, "   ")
	defer srv.Close()
	cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, 0)
	m := NewMetaRouter(cfg)
	_, _, err := m.Route(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error for blank meta-router response")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("expected unknown-provider error, got: %v", err)
	}
}
