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

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/router"
)

func TestDefaultMaxToolResultTokens(t *testing.T) {
	tests := []struct {
		name  string
		provs []router.ProviderConfig
		want  int
	}{
		{
			name:  "no providers falls back to floor",
			provs: nil,
			want:  8000,
		},
		{
			// 10% of 1M = 100k, no MaxRequestTokens -> capped at ceiling 64000.
			name:  "large context window capped at ceiling",
			provs: []router.ProviderConfig{{Name: "big", ContextWindow: 1_000_000}},
			want:  64000,
		},
		{
			// 10% of 200k = 20000, no MaxRequestTokens, within floor/ceiling -> 20000.
			name:  "medium context window unscaled",
			provs: []router.ProviderConfig{{Name: "med", ContextWindow: 200_000}},
			want:  20000,
		},
		{
			// MaxRequestTokens=128000 -> half = 64000 caps the 100k target -> 64000.
			name:  "maxRequestTokens halves an oversized result",
			provs: []router.ProviderConfig{{Name: "p", ContextWindow: 1_000_000, MaxRequestTokens: 128000}},
			want:  64000,
		},
		{
			// MaxRequestTokens=60000 -> half=30000; 10% of 500k=50000 > 30000 -> 30000.
			name:  "maxRequestTokens binds below the window fraction",
			provs: []router.ProviderConfig{{Name: "p", ContextWindow: 500_000, MaxRequestTokens: 60000}},
			want:  30000,
		},
		{
			// smallest MaxRequestTokens wins (50000 -> half 25000; 10% of 1M=100k -> 25000).
			name: "uses smallest maxRequestTokens across providers",
			provs: []router.ProviderConfig{
				{Name: "a", ContextWindow: 1_000_000, MaxRequestTokens: 128000},
				{Name: "b", ContextWindow: 800_000, MaxRequestTokens: 50000},
			},
			want: 25000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultMaxToolResultTokens(tc.provs); got != tc.want {
				t.Fatalf("defaultMaxToolResultTokens: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestApplySafeguardDefaults(t *testing.T) {
	t.Run("nil overrides still applies defaults", func(t *testing.T) {
		s := &router.RouterSafeguards{}
		applySafeguardDefaults(s, 0, nil)
		assert := func(field string, got, want int) {
			if got != want {
				t.Errorf("%s: got %d, want %d", field, got, want)
			}
		}
		assert("MaxConsecutiveNoopTurns", s.MaxConsecutiveNoopTurns, 15)
		assert("MaxRepeatedToolCalls", s.MaxRepeatedToolCalls, 50)
		assert("MaxConsecutiveNoopTurns", s.MinSubstantiveTokens, 20)
		// ToolFrequencyCap intentionally off by default so legitimate high-volume tool
		// use (e.g. reading many files in a PR review) is not blocked.
		assert("ToolFrequencyCap", s.ToolFrequencyCap, 0)
		assert("ToolExecutionTimeoutSec", s.ToolExecutionTimeoutSec, 60)
	})

	t.Run("deployment tool timeout overrides the default", func(t *testing.T) {
		s := &router.RouterSafeguards{}
		applySafeguardDefaults(s, 1800, nil)
		if s.ToolExecutionTimeoutSec != 1800 {
			t.Fatalf("ToolExecutionTimeoutSec: got %d, want 1800", s.ToolExecutionTimeoutSec)
		}
	})

	t.Run("explicit safeguards override the defaults", func(t *testing.T) {
		s := &router.RouterSafeguards{}
		ov := &agentorcav1alpha1.AgentRunSafeguards{
			MaxRepeatedToolCalls:    200,
			MaxConsecutiveNoopTurns: 40,
			ToolFrequencyCap:        1000,
		}
		applySafeguardDefaults(s, 0, ov)
		if s.MaxRepeatedToolCalls != 200 {
			t.Errorf("MaxRepeatedToolCalls: got %d, want 200", s.MaxRepeatedToolCalls)
		}
		if s.MaxConsecutiveNoopTurns != 40 {
			t.Errorf("MaxConsecutiveNoopTurns: got %d, want 40", s.MaxConsecutiveNoopTurns)
		}
		if s.ToolFrequencyCap != 1000 {
			t.Errorf("ToolFrequencyCap: got %d, want 1000", s.ToolFrequencyCap)
		}
	})
}

// TestBuildDeploymentRouterConfig_LimitsAndSafeguards verifies the operator injects the
// context-window-aware MaxToolResultTokens default and conservative loop guards into the
// router config, and that a deployment-level override wins.
func TestBuildDeploymentRouterConfig_LimitsAndSafeguards(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const ctxWindow = 1_000_000
	const maxReq = 128000
	mp := &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "laguna", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel: "riemann1_turbo4",
			BaseURL:      "https://api.poolsi.de/openai",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{
				Name: "laguna-secret", Key: "api-key",
			},
			Constraints: agentorcav1alpha1.ModelConstraints{
				ContextWindow:    ctxWindow,
				MaxRequestTokens: maxReq,
			},
		},
	}
	selector := &agentorcav1alpha1.ModelSelector{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: agentorcav1alpha1.ModelSelectorSpec{
			Strategy:  "rule-based",
			Providers: []agentorcav1alpha1.ProviderWeight{{Name: "laguna", Weight: 100}},
		},
	}
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default"},
	}

	t.Run("defaults computed from context window", func(t *testing.T) {
		deploy := &agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "default"},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "test-agent"},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(mp, selector, agent, deploy).Build()
		r := &AgentDeploymentReconciler{Client: cl, Scheme: scheme}

		cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
		if err != nil {
			t.Fatalf("buildDeploymentRouterConfig: %v", err)
		}
		// 10% of 1M = 100k; half of MaxRequestTokens(128k) = 64k -> capped to 64000.
		if cfg.MaxToolResultTokens != 64000 {
			t.Fatalf("MaxToolResultTokens: got %d, want 64000", cfg.MaxToolResultTokens)
		}
		sg := cfg.Safeguards
		if sg.MaxRepeatedToolCalls != 50 || sg.MaxConsecutiveNoopTurns != 15 || sg.ToolFrequencyCap != 0 {
			t.Fatalf("default safeguards unexpected: %+v", sg)
		}
	})

	t.Run("deployment overrides win", func(t *testing.T) {
		deploy := &agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "d2", Namespace: "default"},
			Spec: agentorcav1alpha1.AgentDeploymentSpec{
				AgentRef:            "test-agent",
				MaxToolResultTokens: 1000,
				Safeguards: &agentorcav1alpha1.AgentRunSafeguards{
					MaxRepeatedToolCalls:    5,
					MaxConsecutiveNoopTurns: 3,
					ToolFrequencyCap:        50,
				},
			},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(mp, selector, agent, deploy).Build()
		r := &AgentDeploymentReconciler{Client: cl, Scheme: scheme}

		cfg, err := r.buildDeploymentRouterConfig(context.Background(), deploy, agent, "sa", nil)
		if err != nil {
			t.Fatalf("buildDeploymentRouterConfig: %v", err)
		}
		if cfg.MaxToolResultTokens != 1000 {
			t.Fatalf("MaxToolResultTokens override: got %d, want 1000", cfg.MaxToolResultTokens)
		}
		sg := cfg.Safeguards
		if sg.MaxRepeatedToolCalls != 5 || sg.MaxConsecutiveNoopTurns != 3 || sg.ToolFrequencyCap != 50 {
			t.Fatalf("override safeguards unexpected: %+v", sg)
		}
	})
}
