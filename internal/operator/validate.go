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
	"fmt"
	"strings"
)

// CompactionValidation holds the result of validating a ContextCompactionRatio
// configuration against an expected session profile.
type CompactionValidation struct {
	// Ratio is the configured compaction ratio.
	Ratio float64 `json:"ratio"`
	// ContextWindow is the provider's context window in tokens.
	ContextWindow int `json:"contextWindow"`
	// EffectiveTarget is the computed compaction target in tokens.
	EffectiveTarget int `json:"effectiveTarget"`
	// CheckpointBudget is the 80% safety ceiling in tokens.
	CheckpointBudget int `json:"checkpointBudget"`
	// Warnings are human-readable warnings about the configuration.
	Warnings []string `json:"warnings,omitempty"`
	// Recommendations are actionable suggestions for the operator.
	Recommendations []string `json:"recommendations,omitempty"`
	// Strategy is a human-readable label for the assessed compaction strategy.
	Strategy string `json:"strategy"`
}

const (
	// MinRecommendedRatio is the lowest ratio we recommend in production.
	MinRecommendedRatio = 0.1
	// MaxRecommendedRatio is the highest ratio we recommend in production.
	MaxRecommendedRatio = 0.9
	// DefaultRatio is the default compaction ratio used by the model-router.
	DefaultRatio = 0.5
	// DefaultContextWindow is used when no provider context window is specified.
	DefaultContextWindow = 200000
	// MinCompactionTarget is the floor for the compaction target (2k tokens).
	MinCompactionTarget = 2000
)

// ValidateContextCompaction validates a ContextCompactionRatio configuration
// and returns recommendations for the operator.
//
// Parameters:
//   - ratio: the configured ContextCompactionRatio (0 = use default 0.5)
//   - contextWindow: the provider's context window in tokens (0 = use default 200000)
//   - expectedSessionLength: estimated number of turns in a typical session (0 = unknown)
func ValidateContextCompaction(ratio float64, contextWindow, expectedSessionLength int) *CompactionValidation {
	// Apply defaults
	if ratio <= 0 || ratio >= 1.0 {
		ratio = DefaultRatio
	}
	if contextWindow <= 0 {
		contextWindow = DefaultContextWindow
	}

	// Compute derived values
	checkpointBudget := checkpointBudget(contextWindow)
	target := int(float64(contextWindow) * ratio)

	// Apply floor and cap
	if target < MinCompactionTarget {
		target = MinCompactionTarget
	}
	if target > checkpointBudget {
		target = checkpointBudget
	}

	result := &CompactionValidation{
		Ratio:            ratio,
		ContextWindow:    contextWindow,
		EffectiveTarget:  target,
		CheckpointBudget: checkpointBudget,
	}

	// Assess strategy label
	result.Strategy = classifyStrategy(ratio)

	// Generate warnings based on ratio value
	result.Warnings = generateWarnings(ratio, contextWindow, expectedSessionLength)

	// Generate recommendations
	result.Recommendations = generateRecommendations(ratio, contextWindow, expectedSessionLength, target, checkpointBudget)

	return result
}

// validateContextCompactionForAgent validates the compaction ratio specifically
// for a given agent type and use case.
func validateContextCompactionForAgent(agentType string, ratio float64, contextWindow int, expectedSessionLength int) *CompactionValidation {
	base := ValidateContextCompaction(ratio, contextWindow, expectedSessionLength)

	switch agentType {
	case "chat":
		if ratio < 0.3 {
			base.Recommendations = append([]string{
				"For interactive chat deployments, consider a higher ratio (0.3-0.5) to preserve " +
					"context across long sessions with user relabeling.",
			}, base.Recommendations...)
		}
	case "batch":
		if ratio > 0.3 {
			base.Recommendations = append([]string{
				"For batch processing, you may safely use a more aggressive ratio (0.1-0.3) " +
					"to reduce memory footprint and checkpoint size.",
			}, base.Recommendations...)
		}
	case "research":
		if ratio < 0.5 {
			base.Recommendations = append([]string{
				"For research agents performing complex reasoning chains, consider a higher " +
					"ratio (0.5-0.7) to preserve intermediate conclusions.",
			}, base.Recommendations...)
		}
	}

	return base
}

// classifyStrategy returns a human-readable label based on the ratio.
func classifyStrategy(ratio float64) string {
	switch {
	case ratio <= 0.15:
		return "aggressive"
	case ratio <= 0.3:
		return "moderate"
	case ratio <= 0.6:
		return "balanced"
	case ratio <= 0.9:
		return "high-fidelity"
	default:
		return "conservative"
	}
}

// generateWarnings produces warnings based on the configuration.
func generateWarnings(ratio float64, contextWindow, expectedSessionLength int) []string {
	var warnings []string

	// Ratio range warnings
	if ratio < MinRecommendedRatio {
		warnings = append(warnings, fmt.Sprintf(
			"Ratio %.2f is below the recommended minimum (%.1f). This may truncate important context.",
			ratio, MinRecommendedRatio))
	}
	if ratio > MaxRecommendedRatio {
		warnings = append(warnings, fmt.Sprintf(
			"Ratio %.2f is above the recommended maximum (%.1f). Compaction may be ineffective.",
			ratio, MaxRecommendedRatio))
	}

	// Context window warnings
	if contextWindow > 1000000 {
		warnings = append(warnings, fmt.Sprintf(
			"Very large context window (%d tokens) — ensure your model provider supports full utilization.",
			contextWindow))
	}

	// Session length warnings
	if expectedSessionLength > 0 && ratio < 0.3 {
		warnings = append(warnings, fmt.Sprintf(
			"For sessions of ~%d turns, ratio %.1f may truncate important historical context.",
			expectedSessionLength, ratio))
	}

	return warnings
}

// generateRecommendations produces actionable suggestions for the operator.
func generateRecommendations(ratio float64, contextWindow, expectedSessionLength, target, budget int) []string {
	var recommendations []string

	// Base recommendation based on strategy
	switch {
	case ratio <= 0.15:
		recommendations = append(recommendations,
			"AGGRESSIVE compaction (≤15%) is suitable for:")
		recommendations = append(recommendations,
			"  - Short, single-purpose agent runs (<10 turns)")
		recommendations = append(recommendations,
			"  - High-throughput batch processing")
		recommendations = append(recommendations, "Consider 0.2-0.3 for interactive chats with long sessions.")
	case ratio <= 0.3:
		recommendations = append(recommendations,
			"MODERATE compaction (15-30%) balances context retention with memory efficiency.")
		recommendations = append(recommendations,
			"Suitable for most agent deployments and medium-length sessions.")
		recommendations = append(recommendations, "Consider 0.1 for very high-throughput scenarios.")
	case ratio <= 0.6:
		recommendations = append(recommendations,
			"BALANCED compaction (30-60%) is the default — preserves substantial context.")
		recommendations = append(recommendations,
			"Suitable for interactive chat deployments and long-running agents.")
		recommendations = append(recommendations, "Consider 0.1-0.3 for batch processing to reduce checkpoint size.")
	default:
		recommendations = append(recommendations,
			"HIGH-FIDELITY compaction (>60%) maximizes context retention at")
		recommendations = append(recommendations,
			"the cost of larger memory usage and checkpoint size.")
		recommendations = append(recommendations, "Typically used for reasoning-heavy agents.")
	}

	// Session-length-specific advice
	if expectedSessionLength > 0 {
		if expectedSessionLength > 30 && ratio < 0.3 {
			recommendations = append(recommendations,
				fmt.Sprintf("Session length %d turns is long — consider increasing ratio to 0.4-0.5.", expectedSessionLength))
		}
		if expectedSessionLength < 5 && ratio > 0.6 {
			recommendations = append(recommendations,
				fmt.Sprintf("Session length %d turns is short — aggressive compaction (0.1-0.3) would save resources.", expectedSessionLength))
		}
	}

	// Effective target info
	recommendations = append(recommendations,
		fmt.Sprintf("Effective compaction target: %d tokens (%.1f%% of %d-token window, capped at %d-token budget).",
			target, ratio*100, contextWindow, budget))

	return recommendations
}

// checkpointBudget returns the 80% safety ceiling for the context window.
// This matches the implementation in internal/router/router.go.
func checkpointBudget(contextWindow int) int {
	return contextWindow * 8 / 10
}

// FormatReport renders a CompactionValidation as a human-readable report.
func (v *CompactionValidation) FormatReport() string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "ContextCompactionRatio Validation:\n")
	fmt.Fprintf(&sb, "  Provider context window:  %d tokens\n", v.ContextWindow)
	fmt.Fprintf(&sb, "  Configured ratio:         %.2f (%s)\n", v.Ratio, v.Strategy)
	fmt.Fprintf(&sb, "  Checkpoint budget (80%%):  %d tokens\n", v.CheckpointBudget)
	fmt.Fprintf(&sb, "  Effective target:         %d tokens (%.1f%% of CW)\n",
		v.EffectiveTarget, float64(v.EffectiveTarget)/float64(v.ContextWindow)*100)

	if len(v.Warnings) > 0 {
		fmt.Fprintf(&sb, "\nWarnings:\n")
		for _, w := range v.Warnings {
			fmt.Fprintf(&sb, "  ⚠ %s\n", w)
		}
	}

	if len(v.Recommendations) > 0 {
		fmt.Fprintf(&sb, "\nRecommendations:\n")
		for _, r := range v.Recommendations {
			// Indent continuation lines
			if strings.HasPrefix(r, "  -") {
				fmt.Fprintf(&sb, "%s\n", r)
			} else {
				fmt.Fprintf(&sb, "  ℹ %s\n", r)
			}
		}
	}

	return sb.String()
}
