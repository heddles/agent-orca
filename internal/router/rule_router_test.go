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
	"testing"
	"time"
)

// ruleRouterTestProviders returns a three-provider fixture spanning the
// attributes the decision tree branches on: cost, latency, capability
// and context window.
func ruleRouterTestProviders() []ProviderConfig {
	return []ProviderConfig{
		{
			Name:               "cheap",
			LiteLLMModel:       "openai/gpt-4o-mini",
			Weight:             5,
			Capabilities:       []string{"code"},
			ContextWindow:      128_000,
			CostPerInputToken:  0.00000015,
			CostPerOutputToken: 0.00000060,
			LatencyProfile:     "fast",
		},
		{
			Name:               "big",
			LiteLLMModel:       "anthropic/claude-sonnet-4-6",
			Weight:             3,
			Capabilities:       []string{"vision", "reasoning"},
			ContextWindow:      1_000_000,
			CostPerInputToken:  0.000003,
			CostPerOutputToken: 0.000015,
			LatencyProfile:     "medium",
		},
		{
			Name:               "fastonly",
			LiteLLMModel:       "openai/gpt-4o-mini-2024-07-18",
			Weight:             2,
			Capabilities:       []string{"fast"},
			ContextWindow:      128_000,
			CostPerInputToken:  0.00000025,
			CostPerOutputToken: 0.000001,
			LatencyProfile:     "fast",
		},
	}
}

func newTestRuleRouter(t *testing.T, mutators ...func(*Config)) *RuleRouter {
	t.Helper()
	cfg := &Config{
		Providers:         ruleRouterTestProviders(),
		CapabilityRouting: map[string]string{},
	}
	for _, m := range mutators {
		m(cfg)
	}
	return NewRuleRouter(cfg)
}

// userTurn is a plain single-user-turn conversation with no stage signals
// (no tool results, no code fences, no summary phrases).
func userTurn(text string) []Message {
	return []Message{{Role: "user", Content: text}}
}

func TestRuleRouter_NoProviders(t *testing.T) {
	rr := &RuleRouter{cfg: &Config{}}
	res := rr.Route(userTurn("hi"), 0, 0)
	if res.ProviderName != "" {
		t.Errorf("ProviderName = %q, want empty", res.ProviderName)
	}
	if res.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0", res.Confidence)
	}
	if res.Reason != "no providers configured" {
		t.Errorf("Reason = %q, want %q", res.Reason, "no providers configured")
	}
}

func TestRuleRouter_VisionContent(t *testing.T) {
	rr := newTestRuleRouter(t)
	msgs := []Message{{
		Role: "user",
		Content: []any{map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:image/png;base64,iVBORw0KGgo=",
			},
		}},
	}}
	res := rr.Route(msgs, 0, 0)
	if res.ProviderName != "big" {
		t.Errorf("ProviderName = %q, want big (only vision-capable provider)", res.ProviderName)
	}
	if res.Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", res.Confidence)
	}
	if res.Reason != "vision content detected" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_LargeContext(t *testing.T) {
	// 110k tokens exceeds 80% of the 128k windows (102,400) but fits the 1M
	// window comfortably. findLargeContext picks the qualifying provider with
	// the most headroom; cheap and fastonly tie at 128k and the first wins.
	rr := newTestRuleRouter(t)
	res := rr.Route(userTurn("x"), 110_000, 0)
	if res.ProviderName != "cheap" {
		t.Errorf("ProviderName = %q, want cheap (first of the tied 128k windows)", res.ProviderName)
	}
	if res.Confidence != 0.90 {
		t.Errorf("Confidence = %v, want 0.90", res.Confidence)
	}
	if res.Reason != "large context detected" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_ContextFitsAll_NoLargeContextRule(t *testing.T) {
	// 50k tokens fits every provider with headroom → rule 2 must NOT fire;
	// with no stage mapping or hints the result is weighted random.
	rr := newTestRuleRouter(t)
	res := rr.Route(userTurn("x"), 50_000, 0)
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q", res.Reason, "weighted random")
	}
}

func TestRuleRouter_BudgetApproaching(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.BudgetPerRunUSD = 1.00
	})
	rr.spentUSD = 0.85 // 15% remaining, below the 20% rule-3 threshold
	res := rr.Route(userTurn("hi"), 0, 0)
	if res.ProviderName != "cheap" {
		t.Errorf("ProviderName = %q, want cheap (lowest CostPerInputToken)", res.ProviderName)
	}
	if res.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85", res.Confidence)
	}
	if res.Reason != "budget < 20% remaining" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_BudgetNotApproaching(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.BudgetPerRunUSD = 1.00
	})
	rr.spentUSD = 0.50 // 50% remaining — rule 3 must not fire
	res := rr.Route(userTurn("hi"), 0, 0)
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q (no rule should fire)", res.Reason, "weighted random")
	}
}

func TestRuleRouter_TimeoutApproaching(t *testing.T) {
	rr := newTestRuleRouter(t)
	// Simulate 8 minutes elapsed against 2 minutes remaining: 480/600 = 0.80
	// > the 0.70 rule-4 threshold. fastest = first "fast" provider = cheap.
	rr.startedAt = time.Now().Add(-8 * time.Minute)
	res := rr.Route(userTurn("hi"), 0, 120)
	if res.ProviderName != "cheap" {
		t.Errorf("ProviderName = %q, want cheap (first fast-latency provider)", res.ProviderName)
	}
	if res.Confidence != 0.80 {
		t.Errorf("Confidence = %v, want 0.80", res.Confidence)
	}
	if res.Reason != "approaching timeout" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_TimeoutNotApproaching(t *testing.T) {
	rr := newTestRuleRouter(t)
	// 1 minute elapsed of a 9-minute timeout: 60/540 ≈ 0.11 < 0.70.
	rr.startedAt = time.Now().Add(-1 * time.Minute)
	res := rr.Route(userTurn("hi"), 0, 480)
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q (rule 4 must not fire)", res.Reason, "weighted random")
	}
}

func TestRuleRouter_StageRoutingToolExec(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting["tool-exec"] = "cheap"
	})
	msgs := []Message{
		{Role: "user", Content: "run the tests"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1", Content: "PASS"},
	}
	res := rr.Route(msgs, 0, 0)
	if res.ProviderName != "cheap" {
		t.Errorf("ProviderName = %q, want cheap (tool-exec stage mapping)", res.ProviderName)
	}
	if res.Reason != "stage routing: tool-exec" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_StageRoutingCode(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting["code"] = "big"
	})
	msgs := []Message{
		{Role: "user", Content: "write a function"},
		{Role: "assistant", Content: "here: ```go\nfunc f() {}\n```"},
	}
	res := rr.Route(msgs, 0, 0)
	if res.ProviderName != "big" {
		t.Errorf("ProviderName = %q, want big (code stage mapping)", res.ProviderName)
	}
	if res.Reason != "stage routing: code" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_StageRoutingBelowGate_ignored(t *testing.T) {
	// "chat" classifies at 0.40, below the 0.8 stageProvider gate — even a
	// configured "chat" mapping must not be used for routing.
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting["chat"] = "big"
	})
	res := rr.Route(userTurn("what is kubernetes?"), 0, 0)
	if res.ProviderName == "big" && res.Reason != "" && res.Reason != "weighted random" {
		t.Errorf("below-gate stage routed: ProviderName=%q Reason=%q", res.ProviderName, res.Reason)
	}
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q (chat is below the 0.8 gate)", res.Reason, "weighted random")
	}
}

func TestRuleRouter_StageRoutingUnmappedFallsThrough(t *testing.T) {
	rr := newTestRuleRouter(t) // no CapabilityRouting entries
	msgs := []Message{
		{Role: "user", Content: "run the tests"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1", Content: "PASS"},
	}
	res := rr.Route(msgs, 0, 0)
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q (tool-exec stage unmapped → fall through)",
			res.Reason, "weighted random")
	}
}

func TestRuleRouter_StageRoutingDisabled(t *testing.T) {
	t.Setenv("ROUTER_STAGE_ROUTING", "false")
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting["tool-exec"] = "cheap"
	})
	msgs := []Message{
		{Role: "user", Content: "run the tests"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1", Content: "PASS"},
	}
	res := rr.Route(msgs, 0, 0)
	if res.Reason == "stage routing: tool-exec" {
		t.Errorf("stage routing fired although ROUTER_STAGE_ROUTING=false")
	}
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q", res.Reason, "weighted random")
	}
}

func TestRuleRouter_CapabilityHint(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting["code"] = "big"
	})
	// Plain text with a "code" keyword and no code fence → classifyStage
	// returns chat (0.40, below gate), so rule 5 keyword hints fire.
	res := rr.Route(userTurn("please implement a function to do X"), 0, 0)
	if res.ProviderName != "big" {
		t.Errorf("ProviderName = %q, want big (capability hint: code keywords)", res.ProviderName)
	}
	if res.Confidence != 0.75 {
		t.Errorf("Confidence = %v, want 0.75", res.Confidence)
	}
	if res.Reason != "capability routing: code" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_WeightedRandomRespectsWeightZero(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		// fastonly gets weight 0 → reserved for meta-router escalation only.
		c.Providers[2].Weight = 0
	})
	for range 50 {
		res := rr.Route(userTurn("hi"), 0, 0)
		if res.ProviderName == "fastonly" {
			t.Fatalf("fastonly selected by weighted random with Weight=0")
		}
	}
}

func TestRuleRouter_MaxRequestTokensFilter(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		// Only "big" (1M window) can carry a 150k-token request.
		c.Providers[0].MaxRequestTokens = 100_000
		c.Providers[2].MaxRequestTokens = 100_000
	})
	// 150k tokens: cheap and fastonly are filtered by MaxRequestTokens, so
	// rule 2 sees only "big" — 150k < 80% of 1M, so no large-context rule;
	// weighted random over a single provider still selects "big".
	res := rr.Route(userTurn("x"), 150_000, 0)
	if res.ProviderName != "big" {
		t.Errorf("ProviderName = %q, want big (only unfiltered provider)", res.ProviderName)
	}
	if res.Reason != "weighted random" {
		t.Errorf("Reason = %q, want %q", res.Reason, "weighted random")
	}
	// The scoped-config copy must not have mutated the original providers.
	if len(rr.cfg.Providers) != 3 {
		t.Errorf("cfg.Providers length = %d, want 3 (original config untouched)", len(rr.cfg.Providers))
	}
}

func TestRuleRouter_UpdateSpendAndTurnCount(t *testing.T) {
	rr := newTestRuleRouter(t)
	rr.UpdateSpend(0.10)
	rr.UpdateSpend(0.05)
	if rr.spentUSD != 0.15 {
		t.Errorf("spentUSD = %v, want 0.15", rr.spentUSD)
	}
	rr.IncrementTurn()
	rr.IncrementTurn()
	if rr.TurnCount() != 2 {
		t.Errorf("TurnCount() = %d, want 2", rr.TurnCount())
	}
}
