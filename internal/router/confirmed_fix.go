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
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
)

const (
	// fixStagingScope is the Redis key scope for staged fixes.
	fixStagingScope = "fix-staging"
	// fixStagingTTL is how long a staged fix lives before expiry.
	fixStagingTTL = 24 * time.Hour
)

// stagedFix is the data stored in Redis for a proposed fix awaiting confirmation.
type stagedFix struct {
	ProposalID    string           `json:"proposalId"`
	KnowledgeBase string           `json:"knowledgeBase"`
	Documents     []stagedDocument `json:"documents"`
	ProposedAt    string           `json:"proposedAt"`
	RunName       string           `json:"runName"`
	AgentRef      string           `json:"agentRef,omitempty"`
}

type stagedDocument struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata"`
}

// executeProposeFix stages a candidate fix in Redis without ingesting it into the KnowledgeBase.
// Returns a proposal ID that the agent must pass to _confirm_fix after user confirmation.
func (r *Router) executeProposeFix(ctx context.Context, args string) string {
	var p struct {
		KnowledgeBase string           `json:"knowledgeBase"`
		Documents     []stagedDocument `json:"documents"`
		Summary       string           `json:"summary"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf(`{"error": "invalid arguments: %v"}`, err)
	}
	if p.KnowledgeBase == "" || len(p.Documents) == 0 {
		return `{"error": "knowledgeBase and documents are required"}`
	}

	// Verify this KB is in the confirm-required list.
	if !slices.Contains(r.cfg.ConfirmRequiredKBs, p.KnowledgeBase) {
		return fmt.Sprintf(`{"error": "knowledgeBase %q is not configured for confirmed-fix ingestion"}`, p.KnowledgeBase)
	}

	// Verify the KB exists in the router config.
	if _, err := r.findKnowledgeBase(p.KnowledgeBase); err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}

	// Generate a proposal ID and store the fix in Redis.
	proposalID := "fix-" + uuid.New().String()[:8]

	fix := stagedFix{
		ProposalID:    proposalID,
		KnowledgeBase: p.KnowledgeBase,
		Documents:     p.Documents,
		ProposedAt:    time.Now().UTC().Format(time.RFC3339),
		RunName:       r.cfg.RunName,
	}

	data, err := json.Marshal(fix)
	if err != nil {
		return fmt.Sprintf(`{"error": "marshaling fix: %v"}`, err)
	}

	redisKey := fmt.Sprintf("%s:%s:%s", r.cfg.RunNamespace, r.cfg.RunName, proposalID)
	if err := r.store.SaveKV(ctx, fixStagingScope, redisKey, data, fixStagingTTL); err != nil {
		return fmt.Sprintf(`{"error": "staging fix: %v"}`, err)
	}

	slog.Info("Fix proposed and staged", "proposalId", proposalID, "kb", p.KnowledgeBase, "run", r.cfg.RunName)

	return fmt.Sprintf(`{"proposalId": %q, "status": "staged", "expiresIn": "24h", "message": "Fix staged. Ask the user to try it and call _confirm_fix with this proposalId once they confirm it worked."}`, proposalID)
}

// executeConfirmFix promotes a staged fix from Redis into the KnowledgeBase.
// Called only after the user explicitly confirms the fix worked.
func (r *Router) executeConfirmFix(ctx context.Context, args string) string {
	var p struct {
		ProposalID string `json:"proposalId"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf(`{"error": "invalid arguments: %v"}`, err)
	}
	if p.ProposalID == "" {
		return `{"error": "proposalId is required"}`
	}

	// Load the staged fix from Redis.
	redisKey := fmt.Sprintf("%s:%s:%s", r.cfg.RunNamespace, r.cfg.RunName, p.ProposalID)
	data, err := r.store.LoadKV(ctx, fixStagingScope, redisKey)
	if err != nil || data == nil {
		return fmt.Sprintf(`{"error": "proposal %q not found or expired"}`, p.ProposalID)
	}

	var fix stagedFix
	if err := json.Unmarshal(data, &fix); err != nil {
		return fmt.Sprintf(`{"error": "corrupted staging data: %v"}`, err)
	}

	// Convert staged documents to the format expected by _rag_ingest.
	ingestArgs := map[string]any{
		"knowledgeBase": fix.KnowledgeBase,
		"documents":     fix.Documents,
	}
	ingestJSON, _ := json.Marshal(ingestArgs)

	// Call the existing _rag_ingest implementation.
	result := r.executeRAGIngest(ctx, string(ingestJSON))

	// Clean up the staging key on success.
	// (On failure, let it expire naturally so the user can retry.)
	if !isErrorResult(result) {
		// Best-effort delete — if this fails, the TTL will clean it up.
		_ = r.store.SaveKV(ctx, fixStagingScope, redisKey, nil, time.Millisecond)
		slog.Info("Confirmed fix ingested into KnowledgeBase", "proposalId", p.ProposalID, "kb", fix.KnowledgeBase)
	}

	return result
}

// isErrorResult checks if a tool result JSON contains an error.
func isErrorResult(result string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		return true
	}
	_, hasError := m["error"]
	return hasError
}

// ProposedFixToolDefinition returns the OpenAI function schema for _propose_fix.
func ProposedFixToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        "_propose_fix",
		Description: "Stage a candidate fix for later ingestion into the knowledge base. The fix will NOT be saved until the user explicitly confirms it worked via _confirm_fix. Always call this BEFORE asking the user to try a fix.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["knowledgeBase", "documents"],
			"properties": {
				"knowledgeBase": {
					"type": "string",
					"description": "The name of the KnowledgeBase to ingest into (after confirmation)."
				},
				"documents": {
					"type": "array",
					"items": {
						"type": "object",
						"required": ["content"],
						"properties": {
							"id": {"type": "string", "description": "Optional document ID."},
							"content": {"type": "string", "description": "The fix description and steps."},
							"metadata": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Optional metadata (e.g. category, severity)."}
						}
					},
					"description": "The fix documents to stage."
				},
				"summary": {
					"type": "string",
					"description": "A brief summary of the proposed fix."
				}
			}
		}`),
		BackendType: "builtin",
	}
}

// ConfirmFixToolDefinition returns the OpenAI function schema for _confirm_fix.
func ConfirmFixToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        "_confirm_fix",
		Description: "Confirm that a previously proposed fix worked and ingest it into the knowledge base. ONLY call this after the user has explicitly confirmed the fix resolved their issue. NEVER assume a fix worked.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"required": ["proposalId"],
			"properties": {
				"proposalId": {
					"type": "string",
					"description": "The proposal ID returned by _propose_fix."
				}
			}
		}`),
		BackendType: "builtin",
	}
}
