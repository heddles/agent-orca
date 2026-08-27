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

package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

const (
	// sessionScope is the KV scope under which session checkpoints are persisted.
	sessionScope = "agentorca-session"
	// sessionCheckpointTTL is how long a persisted session checkpoint lives.
	// ~30 days mirrors the operator's existing run-checkpoint residency and gives
	// a warm pod plenty of time to resume a chat after a restart.
	sessionCheckpointTTL = 30 * 24 * time.Hour
	// maxSessionHistoryMessages caps the number of conversation messages retained in
	// a persisted session checkpoint. The model-router's per-run Redis checkpoint
	// (agentorca/runs/<run>/state) remains the full-fidelity source of truth for LLM
	// context (it keeps tool-call results etc.); the session store owns the run-chain
	// (LastRunRef -> PriorRunRef) and a bounded UI history. Capping keeps serialized
	// checkpoints comfortably under the state store's 1MB-per-value limit.
	maxSessionHistoryMessages = 100
)

// kvStore is the narrow state-store contract RedisStore depends on. The real
// state.Store (Redis-backed in production) satisfies it structurally; tests can
// supply a tiny stub, keeping this package free of a hard dependency on state.
type kvStore interface {
	SaveKV(ctx context.Context, scope, key string, value []byte, ttl time.Duration) error
	LoadKV(ctx context.Context, scope, key string) ([]byte, error)
	DeleteKV(ctx context.Context, scope, key string) error
	ListKV(ctx context.Context, scope string) ([]string, error)
}

// RedisStore implements Store against a state-store KV backend (Redis in production).
// It makes session checkpoints survive API-server / warm-pod restarts, which is
// required for the LastRunRef conversation chain to persist: when the apiserver
// restarts it must still know each session's last run so the next AgentRun is
// created with PriorRunRef set and the model-router can rehydrate context from
// Redis on the next warm-pod claim.
type RedisStore struct {
	kv kvStore
}

// NewRedisStore creates a Redis-backed (state-store-backed) session checkpoint
// store. kv must be non-nil; in production pass the operator's state.Store.
func NewRedisStore(kv kvStore) *RedisStore { return &RedisStore{kv: kv} }

// Load retrieves a session checkpoint by ID. Returns (nil, nil) when not found.
func (s *RedisStore) Load(ctx context.Context, sessionID string) (*agentorcav1alpha1.Checkpoint, error) {
	if sessionID == "" || s.kv == nil {
		return nil, nil
	}
	b, err := s.kv.LoadKV(ctx, sessionScope, sessionID)
	if err != nil {
		return nil, fmt.Errorf("loading session checkpoint %q: %w", sessionID, err)
	}
	if len(b) == 0 {
		return nil, nil // not found — not an error
	}
	var cp agentorcav1alpha1.Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("unmarshaling session checkpoint %q: %w", sessionID, err)
	}
	return &cp, nil
}

// Save persists (overwrites) a session checkpoint with a bounded TTL.
func (s *RedisStore) Save(ctx context.Context, checkpoint *agentorcav1alpha1.Checkpoint) (string, error) {
	if checkpoint == nil || checkpoint.SessionID == "" {
		return "", fmt.Errorf("checkpoint requires SessionID")
	}
	if s.kv == nil {
		return "", fmt.Errorf("redis checkpoint store is not initialized")
	}

	cp := *checkpoint
	// Keep only the most recent messages; the model-router's run-key checkpoint
	// holds the full transcript for LLM resumption. This bounds the serialized
	// size and keeps the UI history focused on the active conversation.
	if n := len(cp.ConversationHistory); n > maxSessionHistoryMessages {
		slog.Warn("truncating session checkpoint history before persistence",
			"session", cp.SessionID, "had", n, "kept", maxSessionHistoryMessages)
		tail := cp.ConversationHistory[n-maxSessionHistoryMessages:]
		cp.ConversationHistory = append([]agentorcav1alpha1.ConversationMessage(nil), tail...)
	}

	b, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("marshaling session checkpoint %q: %w", cp.SessionID, err)
	}
	if err := s.kv.SaveKV(ctx, sessionScope, cp.SessionID, b, sessionCheckpointTTL); err != nil {
		return "", fmt.Errorf("saving session checkpoint %q: %w", cp.SessionID, err)
	}
	return fmt.Sprintf("session://%s/v%d", cp.SessionID, cp.Version), nil
}

// Delete removes a session checkpoint. No-op for unknown/empty IDs.
func (s *RedisStore) Delete(ctx context.Context, sessionID string) error {
	if sessionID == "" || s.kv == nil {
		return nil
	}
	if err := s.kv.DeleteKV(ctx, sessionScope, sessionID); err != nil {
		return fmt.Errorf("deleting session checkpoint %q: %w", sessionID, err)
	}
	return nil
}

// List returns all known session IDs (for cleanup/metrics).
func (s *RedisStore) List(ctx context.Context) ([]string, error) {
	if s.kv == nil {
		return nil, nil
	}
	return s.kv.ListKV(ctx, sessionScope)
}
