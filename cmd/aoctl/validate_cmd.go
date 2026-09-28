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

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/heddles/agent-orca/internal/operator"
)

// validateCmd is the parent command for configuration validation utilities.
var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate agent-orca configuration settings",
	Long: `Validate agent-orca configuration settings to ensure optimal performance.

These utilities help operators tune configuration values by analyzing them
against known best practices and the expected usage patterns of their agents.
`,
}

// validateContextCompactionCmd validates ContextCompactionRatio settings.
var validateContextCompactionCmd = &cobra.Command{
	Use:   "context-compaction",
	Short: "Validate ContextCompactionRatio configuration",
	Long: `Validate the ContextCompactionRatio configuration for your model-router.

This command helps operators choose an appropriate compaction ratio based on
their provider's context window and expected session characteristics.

The ContextCompactionRatio controls how aggressively the model-router compacts
conversation history when the checkpoint budget (80% of context window) is
reached. Lower ratios retain less context in memory but reduce checkpoint
size and token-counting overhead.

Examples:
  # Validate with defaults (ratio=0.5, CW=200000)
  aoctl validate context-compaction

  # Validate with custom values
  aoctl validate context-compaction --ratio 0.1 --context-window 128000

  # Include expected session length for session-specific advice
  aoctl validate context-compaction --ratio 0.3 --context-window 262144 --session-length 50

  # Validate for a specific agent type
  aoctl validate context-compaction --ratio 0.1 --context-window 128000 --agent-type chat

  # Machine-readable output
  aoctl validate context-compaction --ratio 0.1 --context-window 128000 --json
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		flags := cmd.Flags()

		ratio, _ := flags.GetFloat64("ratio")
		cw, _ := flags.GetInt("context-window")
		sessionLen, _ := flags.GetInt("session-length")
		agentType, _ := flags.GetString("agent-type")
		jsonOut, _ := flags.GetBool("json")

		var result *operator.CompactionValidation
		if agentType != "" {
			result = validateForAgentType(agentType, ratio, cw, sessionLen)
		} else {
			result = operator.ValidateContextCompaction(ratio, cw, sessionLen)
		}

		if jsonOut {
			return printJSON(cmd.OutOrStdout(), result)
		}

		_, err := fmt.Fprintln(cmd.OutOrStdout(), result.FormatReport())
		return err
	},
}

// validateForAgentType dispatches to the appropriate validation function
// based on the agent type, adding agent-type-specific recommendations.
func validateForAgentType(agentType string, ratio float64, cw, sessionLen int) *operator.CompactionValidation {
	result := operator.ValidateContextCompaction(ratio, cw, sessionLen)

	switch agentType {
	case "chat":
		if ratio < 0.3 {
			result.Recommendations = append([]string{
				"ℹ For interactive chat deployments, consider a higher ratio (0.3-0.5) to preserve " +
					"context across long sessions with user relabeling.",
			}, result.Recommendations...)
		}
	case "batch":
		if ratio > 0.3 {
			result.Recommendations = append([]string{
				"ℹ For batch processing, you may safely use a more aggressive ratio (0.1-0.3) " +
					"to reduce memory footprint and checkpoint size.",
			}, result.Recommendations...)
		}
	case "research":
		if ratio < 0.5 {
			result.Recommendations = append([]string{
				"ℹ For research agents performing complex reasoning chains, consider a higher " +
					"ratio (0.5-0.7) to preserve intermediate conclusions.",
			}, result.Recommendations...)
		}
	}

	return result
}

func init() {
	validateContextCompactionCmd.Flags().Float64P("ratio", "r", 0, "context compaction ratio (0=use default 0.5)")
	validateContextCompactionCmd.Flags().IntP("context-window", "w", 0, "provider context window in tokens (0=use default 200000)")
	validateContextCompactionCmd.Flags().IntP("session-length", "s", 0, "expected number of turns per session (0=unknown)")
	validateContextCompactionCmd.Flags().StringP("agent-type", "t", "", "agent type: chat, batch, or research (for targeted recommendations)")

	validateCmd.AddCommand(validateContextCompactionCmd)
}
