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
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateContextCompactionCmd_Defaults verifies that the command works
// with no flags (uses defaults from ValidateContextCompaction).
func TestValidateContextCompactionCmd_Defaults(t *testing.T) {
	stdout, _, err := runCLI(t, nil, "validate", "context-compaction")
	if err != nil {
		t.Fatalf("validate context-compaction (defaults): %v", err)
	}
	if !strings.Contains(stdout, "ContextCompactionRatio Validation:") {
		t.Fatalf("expected report header, got: %q", stdout)
	}
	if !strings.Contains(stdout, "balanced") {
		t.Fatalf("expected 'balanced' strategy for default ratio, got: %q", stdout)
	}
}

// TestValidateContextCompactionCmd_WithFlags verifies the command applies
// explicitly passed flags.
func TestValidateContextCompactionCmd_WithFlags(t *testing.T) {
	stdout, _, err := runCLI(t, nil,
		"validate", "context-compaction",
		"--ratio", "0.1", "--context-window", "128000")
	if err != nil {
		t.Fatalf("validate context-compaction (flags): %v", err)
	}
	if !strings.Contains(stdout, "aggressive") {
		t.Fatalf("expected 'aggressive' strategy for ratio 0.1, got: %q", stdout)
	}
	if !strings.Contains(stdout, "12800 tokens") {
		t.Fatalf("expected effective target 12800 tokens, got: %q", stdout)
	}
}

// TestValidateContextCompactionCmd_JSON verifies --json output.
func TestValidateContextCompactionCmd_JSON(t *testing.T) {
	stdout, _, err := runCLI(t, nil,
		"validate", "context-compaction",
		"--ratio", "0.5", "--context-window", "200000", "--json")
	if err != nil {
		t.Fatalf("validate context-compaction (json): %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %q", err, stdout)
	}
	if result["ratio"].(float64) != 0.5 {
		t.Errorf("ratio = %v, want 0.5", result["ratio"])
	}
	if result["contextWindow"].(float64) != 200000 {
		t.Errorf("contextWindow = %v, want 200000", result["contextWindow"])
	}
	if result["strategy"].(string) != "balanced" {
		t.Errorf("strategy = %v, want balanced", result["strategy"])
	}
}

// TestValidateContextCompactionCmd_WithSessionLength verifies the command
// accepts --session-length and produces relevant output.
func TestValidateContextCompactionCmd_WithSessionLength(t *testing.T) {
	stdout, _, err := runCLI(t, nil,
		"validate", "context-compaction",
		"--ratio", "0.1", "--context-window", "262144", "--session-length", "50")
	if err != nil {
		t.Fatalf("validate context-compaction (session-length): %v", err)
	}
	if !strings.Contains(stdout, "Session length 50") {
		t.Fatalf("expected session-length-specific recommendation, got: %q", stdout)
	}
}

// TestValidateContextCompactionCmd_AgentType verifies that --agent-type
// adds agent-type-specific recommendations to the output.
func TestValidateContextCompactionCmd_AgentType(t *testing.T) {
	tests := []struct {
		name           string
		agentType      string
		ratio          string
		wantSuggestion string
	}{
		{
			name:           "chat with low ratio",
			agentType:      "chat",
			ratio:          "0.1",
			wantSuggestion: "interactive chat deployments",
		},
		{
			name:           "batch with high ratio",
			agentType:      "batch",
			ratio:          "0.6",
			wantSuggestion: "batch processing",
		},
		{
			name:           "research with moderate ratio",
			agentType:      "research",
			ratio:          "0.3",
			wantSuggestion: "reasoning chains",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := runCLI(t, nil,
				"validate", "context-compaction",
				"--ratio", tc.ratio, "--context-window", "128000",
				"--agent-type", tc.agentType)
			if err != nil {
				t.Fatalf("validate context-compaction (agent-type): %v", err)
			}
			if !strings.Contains(stdout, tc.wantSuggestion) {
				t.Fatalf("expected suggestion %q in output, got: %q", tc.wantSuggestion, stdout)
			}
		})
	}
}

// TestValidateContextCompactionCmd_JSONWithAgentType verifies JSON output
// includes the agent-type-specific recommendations.
func TestValidateContextCompactionCmd_JSONWithAgentType(t *testing.T) {
	stdout, _, err := runCLI(t, nil,
		"validate", "context-compaction",
		"--ratio", "0.1", "--context-window", "128000",
		"--agent-type", "chat", "--json")
	if err != nil {
		t.Fatalf("validate context-compaction (json+agent-type): %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %q", err, stdout)
	}
	recs, ok := result["recommendations"].([]any)
	if !ok {
		t.Fatalf("expected recommendations array, got: %v", result["recommendations"])
	}
	found := false
	for _, r := range recs {
		if s, ok := r.(string); ok && strings.Contains(s, "interactive chat deployments") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'interactive chat deployments' recommendation, got: %v", recs)
	}
}

// TestValidateContextCompactionCmd_UnknownAgentType verifies that an unknown
// agent-type is gracefully handled (falls back to base validation).
func TestValidateContextCompactionCmd_UnknownAgentType(t *testing.T) {
	_, _, err := runCLI(t, nil,
		"validate", "context-compaction",
		"--ratio", "0.5", "--context-window", "128000",
		"--agent-type", "unknown-type")
	if err != nil {
		t.Fatalf("unknown agent-type should not error, got: %v", err)
	}
}
