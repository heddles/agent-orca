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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// metaRouterCacheEntry caches a meta-router decision for (task-hash, available-models).
type metaRouterCacheEntry struct {
	providerName string
	expiresAt    time.Time
}

// MetaRouter uses a cheap/fast LLM to decide which model to use for ambiguous requests.
// It is only invoked when the rule-router's confidence falls below the configured threshold.
type MetaRouter struct {
	cfg      *Config
	cache    map[string]metaRouterCacheEntry
	cacheMu  sync.Mutex
	cacheTTL time.Duration
}

// NewMetaRouter creates a MetaRouter from the given configuration.
func NewMetaRouter(cfg *Config) *MetaRouter {
	return &MetaRouter{
		cfg:      cfg,
		cache:    make(map[string]metaRouterCacheEntry),
		cacheTTL: 60 * time.Second,
	}
}

// Route asks the meta-router LLM to select the best provider for the given messages.
// Returns the provider name or an error; callers fall back to rule-based routing on error.
func (m *MetaRouter) Route(ctx context.Context, messages []Message) (string, TokenUsage, error) {
	// Find the meta-router provider.
	var metaProvider *ProviderConfig
	for i := range m.cfg.Providers {
		if m.cfg.Providers[i].Name == m.cfg.MetaRouterProviderName {
			metaProvider = &m.cfg.Providers[i]
			break
		}
	}
	if metaProvider == nil {
		return "", TokenUsage{}, fmt.Errorf("meta-router provider %q not found in provider list", m.cfg.MetaRouterProviderName)
	}

	// Build a cache key from the task content hash + available model names.
	cacheKey := m.cacheKey(messages)
	if entry := m.getCached(cacheKey); entry != "" {
		return entry, TokenUsage{}, nil
	}

	// Build the routing prompt.
	routingPrompt := m.buildRoutingPrompt(messages)
	providerName, usage, err := m.callLLM(ctx, metaProvider, routingPrompt)
	if err != nil {
		return "", usage, err
	}

	// Validate the returned provider name.
	providerName = strings.TrimSpace(providerName)
	slog.Info("meta-router LLM response", "raw", providerName)

	// Exact match first.
	for _, p := range m.cfg.Providers {
		if p.Name == providerName {
			m.setCached(cacheKey, providerName)
			return providerName, usage, nil
		}
	}

	// Fuzzy fallback: LLM may have added surrounding text — find the first provider
	// name that appears as a substring in the response.
	for _, p := range m.cfg.Providers {
		if strings.Contains(providerName, p.Name) {
			slog.Info("meta-router fuzzy match", "provider", p.Name, "raw", providerName)
			m.setCached(cacheKey, p.Name)
			return p.Name, usage, nil
		}
	}

	return "", usage, fmt.Errorf("meta-router returned unknown provider %q", providerName)
}

// buildRoutingPrompt constructs a routing decision prompt.
// The routing hint for each provider is the primary decision criterion.
func (m *MetaRouter) buildRoutingPrompt(messages []Message) string {
	// Extract the last user message as the task summary.
	taskSummary := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if text, ok := messages[i].Content.(string); ok {
				if len(text) > 500 {
					text = text[:500] + "..."
				}
				taskSummary = text
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("Match the task below to the correct provider using the routing guidelines.\n\n")
	sb.WriteString("TASK:\n" + taskSummary + "\n\n")
	sb.WriteString("PROVIDERS:\n")
	for _, p := range m.cfg.Providers {
		if p.RoutingHint != "" {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", p.Name, p.RoutingHint))
		} else {
			// No routing hint — fall back to capability/latency metadata.
			sb.WriteString(fmt.Sprintf("- %s: capabilities=%v latency=%s\n", p.Name, p.Capabilities, p.LatencyProfile))
		}
	}
	sb.WriteString("\nProvider name:")
	return sb.String()
}

// callLLM sends a single-turn request to the meta-router provider and returns the response text
// along with token usage for cost tracking.
func (m *MetaRouter) callLLM(ctx context.Context, provider *ProviderConfig, prompt string) (string, TokenUsage, error) {
	apiKey, err := readAPIKey(provider.APIKeyFile)
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("reading API key for meta-router provider: %w", err)
	}

	// Use LiteLLM proxy format — the provider's LiteLLM model string is passed directly.
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model": provider.LiteLLMModel,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens":  50,
		"temperature": 0,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		liteLLMEndpoint(provider), bytes.NewReader(reqBody))
	if err != nil {
		return "", TokenUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("meta-router LLM call: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", TokenUsage{}, fmt.Errorf("meta-router LLM returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage TokenUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", TokenUsage{}, fmt.Errorf("parsing meta-router response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", TokenUsage{}, fmt.Errorf("meta-router returned no choices")
	}
	return result.Choices[0].Message.Content, result.Usage, nil
}

func (m *MetaRouter) cacheKey(messages []Message) string {
	h := sha256.New()
	for _, msg := range messages {
		if text, ok := msg.Content.(string); ok {
			h.Write([]byte(text))
		}
	}
	for _, p := range m.cfg.Providers {
		h.Write([]byte(p.Name))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (m *MetaRouter) getCached(key string) string {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	entry, ok := m.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return ""
	}
	return entry.providerName
}

func (m *MetaRouter) setCached(key, providerName string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.cache[key] = metaRouterCacheEntry{
		providerName: providerName,
		expiresAt:    time.Now().Add(m.cacheTTL),
	}
}

// liteLLMEndpoint returns the LiteLLM proxy endpoint for a provider.
// We use a local LiteLLM instance that the operator deploys as a separate pod,
// or we call the provider directly using litellm library conventions via the OpenAI SDK.
// For simplicity, we translate to the provider's native endpoint here.
func liteLLMEndpoint(provider *ProviderConfig) string {
	// If the provider has an explicit BaseURL, use it directly.
	if provider.BaseURL != "" {
		return strings.TrimRight(provider.BaseURL, "/") + "/v1/chat/completions"
	}

	// Otherwise, map from the LiteLLM model prefix to the default endpoint.
	model := provider.LiteLLMModel
	switch {
	case strings.HasPrefix(model, "anthropic/"):
		return "https://api.anthropic.com/v1/messages"
	case strings.HasPrefix(model, "openai/"):
		return "https://api.openai.com/v1/chat/completions"
	case strings.HasPrefix(model, "gemini/"):
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	case strings.HasPrefix(model, "ollama/"):
		return "http://localhost:11434/v1/chat/completions"
	default:
		slog.Warn("unknown provider prefix, using OpenAI endpoint", "model", model)
		return "https://api.openai.com/v1/chat/completions"
	}
}

// readAPIKey reads an API key from a file path.
func readAPIKey(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no API key file configured")
	}
	data, err := io.ReadAll(bytes.NewReader([]byte(path))) // placeholder
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
