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
	"math/rand"
	"slices"
	"strings"
	"time"
)

// RouteResult is the output of the rule-based router.
type RouteResult struct {
	// ProviderName is the selected ModelProvider CRD name.
	ProviderName string
	// Confidence is a 0.0–1.0 score indicating how certain the rule-based router is.
	// Low confidence triggers the LLM meta-router in hybrid mode.
	Confidence float64
	// Reason is a human-readable explanation for the routing decision.
	Reason string
}

// RuleRouter selects a model provider using deterministic rules.
// It is the fast path in both "rule-based" and "hybrid" strategies.
type RuleRouter struct {
	cfg       *Config
	startedAt time.Time
	spentUSD  float64
	turnCount int
}

// NewRuleRouter creates a RuleRouter for a given configuration.
func NewRuleRouter(cfg *Config) *RuleRouter {
	return &RuleRouter{cfg: cfg, startedAt: time.Now()}
}

// Route selects a provider for the given chat completion request.
// Decision tree (evaluated in priority order):
//  0. Token limit filter: exclude providers whose MaxRequestTokens < estimated tokens
//  1. Vision capability: request contains image attachments → vision-capable model
//  2. Context length: estimated tokens > 80% of model context window → long-context model
//  3. Budget: remaining budget < 20% → cheapest model
//  4. Timeout proximity: elapsed > 70% of run timeout → fastest model
//  5. Capability routing map: check configured tag hints
//  6. Default: weighted random selection
func (r *RuleRouter) Route(messages []Message, estimatedTokens int, remainingTimeoutSec float64) RouteResult {
	providers := r.cfg.Providers
	if len(providers) == 0 {
		return RouteResult{Confidence: 0, Reason: "no providers configured"}
	}

	// Rule 0: Filter out providers whose MaxRequestTokens is exceeded.
	// This prevents routing to providers with account-level TPM limits lower
	// than the model's context window (e.g., 30K TPM on a 128K context model).
	if estimatedTokens > 0 {
		filtered := r.filterByRequestTokens(estimatedTokens)
		if len(filtered) > 0 {
			// Use a scoped config with only eligible providers for this request.
			scoped := *r.cfg
			scoped.Providers = filtered
			scopedRouter := &RuleRouter{cfg: &scoped, startedAt: r.startedAt, spentUSD: r.spentUSD, turnCount: r.turnCount}
			result := scopedRouter.routeInner(messages, estimatedTokens, remainingTimeoutSec)
			return result
		}
		// All providers filtered out — fall through with full list and let
		// the provider return a retryable error (history truncation should kick in).
	}

	return r.routeInner(messages, estimatedTokens, remainingTimeoutSec)
}

// routeInner runs the core decision tree (Rules 1–6) on r.cfg.Providers.
func (r *RuleRouter) routeInner(messages []Message, estimatedTokens int, remainingTimeoutSec float64) RouteResult {
	// Rule 1: Vision — request contains image content.
	if hasImageContent(messages) {
		if p := r.findCapable("vision"); p != nil {
			return RouteResult{ProviderName: p.Name, Confidence: 0.95, Reason: "vision content detected"}
		}
	}

	// Rule 2: Context length — estimated tokens > 80% of provider's context window.
	if estimatedTokens > 0 {
		if p := r.findLargeContext(estimatedTokens); p != nil {
			return RouteResult{ProviderName: p.Name, Confidence: 0.90, Reason: "large context detected"}
		}
	}

	// Rule 3: Budget — remaining budget < 20%.
	if r.cfg.BudgetPerRunUSD > 0 {
		remaining := r.cfg.BudgetPerRunUSD - r.spentUSD
		if remaining < r.cfg.BudgetPerRunUSD*0.20 {
			if p := r.findCheapest(); p != nil {
				return RouteResult{ProviderName: p.Name, Confidence: 0.85, Reason: "budget < 20% remaining"}
			}
		}
	}

	// Rule 4: Timeout proximity — elapsed > 70% of timeout.
	if remainingTimeoutSec > 0 {
		elapsed := time.Since(r.startedAt).Seconds()
		total := elapsed + remainingTimeoutSec
		if total > 0 && elapsed/total > 0.70 {
			if p := r.findFastest(); p != nil {
				return RouteResult{ProviderName: p.Name, Confidence: 0.80, Reason: "approaching timeout"}
			}
		}
	}

	// Rule 5: Capability routing map — check message content hints.
	for capability, providerName := range r.cfg.CapabilityRouting {
		if messagesMentionCapability(messages, capability) {
			return RouteResult{ProviderName: providerName, Confidence: 0.75,
				Reason: "capability routing: " + capability}
		}
	}

	// Rule 6: Weighted random default.
	p := r.weightedRandom()
	return RouteResult{ProviderName: p.Name, Confidence: 0.50, Reason: "weighted random"}
}

// filterByRequestTokens returns providers that can handle the given token count.
// A provider is excluded when its MaxRequestTokens is set and is less than estimatedTokens.
// Providers with MaxRequestTokens == 0 (unset) are always included.
func (r *RuleRouter) filterByRequestTokens(estimatedTokens int) []ProviderConfig {
	var eligible []ProviderConfig
	for _, p := range r.cfg.Providers {
		if p.MaxRequestTokens > 0 && estimatedTokens > p.MaxRequestTokens {
			continue
		}
		eligible = append(eligible, p)
	}
	return eligible
}

// UpdateSpend records additional spend so budget rules stay accurate.
func (r *RuleRouter) UpdateSpend(usd float64) { r.spentUSD += usd }

// IncrementTurn increments the turn counter.
func (r *RuleRouter) IncrementTurn() { r.turnCount++ }

// TurnCount returns the number of LLM turns completed.
func (r *RuleRouter) TurnCount() int { return r.turnCount }

func (r *RuleRouter) findCapable(capability string) *ProviderConfig {
	for i := range r.cfg.Providers {
		if slices.Contains(r.cfg.Providers[i].Capabilities, capability) {
			return &r.cfg.Providers[i]
		}
	}
	return nil
}

func (r *RuleRouter) findLargeContext(estimatedTokens int) *ProviderConfig {
	// Only trigger large-context routing when tokens exceed 80% of a provider's window.
	// Returns nil (no special routing) if all providers have sufficient headroom.
	var best *ProviderConfig
	for i := range r.cfg.Providers {
		p := &r.cfg.Providers[i]
		if p.ContextWindow <= 0 {
			continue
		}
		if float64(estimatedTokens) < float64(p.ContextWindow)*0.80 {
			continue // fits comfortably — no large-context routing needed
		}
		// Tokens are large relative to this provider; pick the one with most headroom.
		if best == nil || p.ContextWindow > best.ContextWindow {
			best = p
		}
	}
	return best
}

func (r *RuleRouter) findCheapest() *ProviderConfig {
	var best *ProviderConfig
	for i := range r.cfg.Providers {
		p := &r.cfg.Providers[i]
		if best == nil || p.CostPerInputToken < best.CostPerInputToken {
			best = p
		}
	}
	return best
}

func (r *RuleRouter) findFastest() *ProviderConfig {
	// Prefer "fast" latency profile, then "medium", then "slow".
	for _, profile := range []string{"fast", "medium", "slow"} {
		for i := range r.cfg.Providers {
			if r.cfg.Providers[i].LatencyProfile == profile {
				return &r.cfg.Providers[i]
			}
		}
	}
	return &r.cfg.Providers[0]
}

func (r *RuleRouter) weightedRandom() *ProviderConfig {
	// Providers with weight=0 are reserved for meta-router escalation and error
	// fallback only — they are never selected by random weighted routing.
	total := 0
	for _, p := range r.cfg.Providers {
		if p.Weight > 0 {
			total += p.Weight
		}
	}
	if total == 0 {
		return &r.cfg.Providers[0]
	}
	pick := rand.Intn(total) //nolint:gosec // non-cryptographic routing weight
	cumulative := 0
	for i := range r.cfg.Providers {
		if r.cfg.Providers[i].Weight <= 0 {
			continue
		}
		cumulative += r.cfg.Providers[i].Weight
		if pick < cumulative {
			return &r.cfg.Providers[i]
		}
	}
	return &r.cfg.Providers[0]
}

// hasImageContent returns true if any message contains an image URL or base64 image.
func hasImageContent(messages []Message) bool {
	for _, m := range messages {
		switch v := m.Content.(type) {
		case string:
			// Plain string content — no images.
		case []interface{}:
			for _, part := range v {
				if partMap, ok := part.(map[string]interface{}); ok {
					if partMap["type"] == "image_url" {
						return true
					}
				}
			}
		case []map[string]interface{}:
			for _, part := range v {
				if part["type"] == "image_url" {
					return true
				}
			}
		default:
			_ = v
		}
	}
	return false
}

// messagesMentionCapability checks if a capability keyword appears in the conversation.
// This is a lightweight heuristic; the meta-router handles nuanced cases.
func messagesMentionCapability(messages []Message, capability string) bool {
	keywords := capabilityKeywords(capability)
	for _, m := range messages {
		if text, ok := m.Content.(string); ok {
			lower := strings.ToLower(text)
			for _, kw := range keywords {
				if strings.Contains(lower, kw) {
					return true
				}
			}
		}
	}
	return false
}

// capabilityKeywords maps capability tags to hint phrases found in user messages.
func capabilityKeywords(capability string) []string {
	switch capability {
	case "code":
		return []string{"write code", "debug", "function", "class", "implement", "program"}
	case "reasoning":
		return []string{"reason", "think step by step", "analyze", "explain why", "proof"}
	case "fast":
		return []string{"quick", "brief", "short answer", "one sentence"}
	default:
		return []string{capability}
	}
}
