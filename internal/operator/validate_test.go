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

package operator

import (
	"strings"
	"testing"
)

func TestValidateContextCompaction_Defaults(t *testing.T) {
	// When ratio is 0, should default to 0.5
	v := ValidateContextCompaction(0, 0, 0)

	if v.Ratio != 0.5 {
		t.Errorf("default ratio = %v, want 0.5", v.Ratio)
	}
	if v.ContextWindow != DefaultContextWindow {
		t.Errorf("default context window = %d, want %d", v.ContextWindow, DefaultContextWindow)
	}
	if v.Strategy != "balanced" {
		t.Errorf("strategy for ratio 0.5 = %s, want balanced", v.Strategy)
	}
}

func TestValidateContextCompaction_AggressiveRatio(t *testing.T) {
	// Aggressive ratio (0.1)
	v := ValidateContextCompaction(0.1, 128000, 0)

	if v.Ratio != 0.1 {
		t.Errorf("ratio = %v, want 0.1", v.Ratio)
	}
	expectedTarget := int(float64(128000) * 0.1) // 12800
	if v.EffectiveTarget != expectedTarget {
		t.Errorf("effective target = %d, want %d", v.EffectiveTarget, expectedTarget)
	}
	if v.Strategy != "aggressive" {
		t.Errorf("strategy = %s, want aggressive", v.Strategy)
	}
}

func TestValidateContextCompaction_HighFidelity(t *testing.T) {
	// High-fidelity ratio (0.7)
	v := ValidateContextCompaction(0.7, 262144, 0)

	if v.Strategy != "high-fidelity" {
		t.Errorf("strategy = %s, want high-fidelity", v.Strategy)
	}
}

func TestValidateContextCompaction_FloorAndCap(t *testing.T) {
	// Very small context window with low ratio - should hit floor
	v := ValidateContextCompaction(0.1, 10000, 0) // target would be 1000, capped to 2000 floor

	if v.EffectiveTarget != MinCompactionTarget {
		t.Errorf("effective target with floor = %d, want %d", v.EffectiveTarget, MinCompactionTarget)
	}

	// Very high ratio - should be capped at budget
	v2 := ValidateContextCompaction(0.95, 262144, 0) // target would be 249036, capped to 209715

	expectedBudget := 262144 * 8 / 10 // 209715
	if v2.EffectiveTarget != expectedBudget {
		t.Errorf("effective target with cap = %d, want %d", v2.EffectiveTarget, expectedBudget)
	}
	if len(v2.Warnings) == 0 {
		t.Error("expected warnings for ratio above recommended max")
	}
}

func TestValidateContextCompaction_Warnings(t *testing.T) {
	tests := []struct {
		name      string
		ratio     float64
		cw        int
		session   int
		wantWarn  int
		wantMatch string
	}{
		{
			name:      "ratio below minimum",
			ratio:     0.05,
			cw:        128000,
			session:   0,
			wantWarn:  1,
			wantMatch: "below the recommended minimum",
		},
		{
			name:      "ratio above maximum",
			ratio:     0.95,
			cw:        128000,
			session:   0,
			wantWarn:  1,
			wantMatch: "above the recommended maximum",
		},
		{
			name:      "long session with low ratio",
			ratio:     0.1,
			cw:        262144,
			session:   50,
			wantWarn:  1,
			wantMatch: "may truncate important historical context",
		},
		{
			name:      "very large context window",
			ratio:     0.5,
			cw:        2000000,
			session:   0,
			wantWarn:  1,
			wantMatch: "Very large context window",
		},
		{
			name:      "no warnings for healthy config",
			ratio:     0.5,
			cw:        128000,
			session:   10,
			wantWarn:  0,
			wantMatch: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ValidateContextCompaction(tt.ratio, tt.cw, tt.session)
			if len(v.Warnings) != tt.wantWarn {
				t.Errorf("warnings count = %d, want %d", len(v.Warnings), tt.wantWarn)
			}
			if tt.wantMatch != "" {
				found := false
				for _, w := range v.Warnings {
					if strings.Contains(w, tt.wantMatch) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected warning containing %q, got %v", tt.wantMatch, v.Warnings)
				}
			}
		})
	}
}

func TestValidateContextCompaction_Recommendations(t *testing.T) {
	tests := []struct {
		name         string
		ratio        float64
		cw           int
		session      int
		wantStrategy string
		wantRecip    string
	}{
		{
			name:         "aggressive recommendation",
			ratio:        0.1,
			cw:           128000,
			session:      5,
			wantStrategy: "aggressive",
			wantRecip:    "AGGRESSIVE compaction",
		},
		{
			name:         "moderate recommendation",
			ratio:        0.25,
			cw:           128000,
			session:      15,
			wantStrategy: "moderate",
			wantRecip:    "MODERATE compaction",
		},
		{
			name:         "balanced recommendation",
			ratio:        0.5,
			cw:           128000,
			session:      20,
			wantStrategy: "balanced",
			wantRecip:    "BALANCED compaction",
		},
		{
			name:         "high-fidelity recommendation",
			ratio:        0.7,
			cw:           128000,
			session:      10,
			wantStrategy: "high-fidelity",
			wantRecip:    "HIGH-FIDELITY compaction",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ValidateContextCompaction(tt.ratio, tt.cw, tt.session)
			if v.Strategy != tt.wantStrategy {
				t.Errorf("strategy = %s, want %s", v.Strategy, tt.wantStrategy)
			}
			if len(v.Recommendations) == 0 {
				t.Error("expected at least one recommendation")
			}
			found := false
			for _, r := range v.Recommendations {
				if strings.Contains(r, tt.wantRecip) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected recommendation containing %q, got %v", tt.wantRecip, v.Recommendations)
			}
		})
	}
}

func TestValidateContextCompaction_SessionLengthAdvice(t *testing.T) {
	// Long session with low ratio should get session-specific advice
	v := ValidateContextCompaction(0.1, 262144, 50)
	found := false
	for _, r := range v.Recommendations {
		if strings.Contains(r, "Session length 50 turns is long") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected session-length-specific recommendation for long session with low ratio")
	}

	// Short session with high ratio should get advice about aggressiveness
	v2 := ValidateContextCompaction(0.7, 262144, 3)
	found = false
	for _, r := range v2.Recommendations {
		if strings.Contains(r, "Session length 3 turns is short") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected session-length-specific recommendation for short session with high ratio")
	}
}

func TestCompactionValidation_FormatReport(t *testing.T) {
	v := ValidateContextCompaction(0.3, 128000, 25)
	report := v.FormatReport()

	if !strings.Contains(report, "ContextCompactionRatio Validation:") {
		t.Error("report should contain header")
	}
	if !strings.Contains(report, "128000 tokens") {
		t.Error("report should contain context window")
	}
	if !strings.Contains(report, "38400 tokens") {
		t.Error("report should contain effective target")
	}
	if !strings.Contains(report, "moderate") {
		t.Error("report should contain strategy label")
	}
}

func TestValidateContextCompaction_ModerateStrategy(t *testing.T) {
	v := ValidateContextCompaction(0.25, 128000, 0)
	if v.Strategy != "moderate" {
		t.Errorf("strategy for ratio 0.25 = %s, want moderate", v.Strategy)
	}
}

func TestValidateContextCompaction_ConservativeStrategy(t *testing.T) {
	v := ValidateContextCompaction(0.85, 128000, 0)
	if v.Strategy != "high-fidelity" {
		t.Errorf("strategy for ratio 0.85 = %s, want high-fidelity", v.Strategy)
	}
}

// TestValidateContextCompactionForAgent tests the agent-type-specific validation.
// This function is unexported and tested within the operator package.
func TestValidateContextCompactionForAgent(t *testing.T) {
	tests := []struct {
		name      string
		agent     string
		ratio     float64
		cw        int
		session   int
		wantRecip string
	}{
		{
			name:      "chat agent with low ratio",
			agent:     "chat",
			ratio:     0.1,
			cw:        128000,
			session:   30,
			wantRecip: "interactive chat deployments",
		},
		{
			name:      "batch agent with high ratio",
			agent:     "batch",
			ratio:     0.6,
			cw:        128000,
			session:   10,
			wantRecip: "batch processing",
		},
		{
			name:      "research agent with moderate ratio",
			agent:     "research",
			ratio:     0.3,
			cw:        128000,
			session:   20,
			wantRecip: "reasoning chains",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := validateContextCompactionForAgent(tt.agent, tt.ratio, tt.cw, tt.session)
			found := false
			for _, r := range v.Recommendations {
				if strings.Contains(r, tt.wantRecip) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected recommendation containing %q, got %v", tt.wantRecip, v.Recommendations)
			}
		})
	}
}
