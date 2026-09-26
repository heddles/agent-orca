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
	"sync"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Store defines the interface for persisting and retrieving conversation checkpoints.
// Implementations can be backed by S3, GCS, local filesystem, etcd, or in-memory storage.
type Store interface {
	// Load retrieves a checkpoint by session ID.
	// Returns nil checkpoint and no error if not found (not an error condition).
	Load(ctx context.Context, sessionID string) (*agentorcav1alpha1.Checkpoint, error)

	// Save persists a checkpoint. Overwrites any existing checkpoint for the same sessionID.
	Save(ctx context.Context, checkpoint *agentorcav1alpha1.Checkpoint) (checkpointRef string, error error)

	// Delete removes a checkpoint by session ID.
	Delete(ctx context.Context, sessionID string) error

	// List returns all session IDs (for cleanup, metrics, etc). Optional to implement.
	List(ctx context.Context) ([]string, error)
}

// InMemoryStore is a simple in-memory checkpoint store for development and testing.
// NOT suitable for production (data is lost on pod restart).
type InMemoryStore struct {
	mu          sync.RWMutex
	checkpoints map[string]*agentorcav1alpha1.Checkpoint
}

// NewInMemoryStore creates a new in-memory checkpoint store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		checkpoints: make(map[string]*agentorcav1alpha1.Checkpoint),
	}
}

// Load retrieves a checkpoint from memory.
func (s *InMemoryStore) Load(ctx context.Context, sessionID string) (*agentorcav1alpha1.Checkpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if cp, exists := s.checkpoints[sessionID]; exists {
		// Return a deep copy to prevent external modification
		cpCopy := *cp
		cpCopy.ConversationHistory = make([]agentorcav1alpha1.ConversationMessage, len(cp.ConversationHistory))
		copy(cpCopy.ConversationHistory, cp.ConversationHistory)
		return &cpCopy, nil
	}
	return nil, nil
}

// Save persists a checkpoint to memory.
func (s *InMemoryStore) Save(ctx context.Context, checkpoint *agentorcav1alpha1.Checkpoint) (string, error) {
	if checkpoint == nil || checkpoint.SessionID == "" {
		return "", fmt.Errorf("checkpoint requires SessionID")
	}

	cpCopy := *checkpoint
	cpCopy.ConversationHistory = make([]agentorcav1alpha1.ConversationMessage, len(checkpoint.ConversationHistory))
	copy(cpCopy.ConversationHistory, checkpoint.ConversationHistory)

	s.mu.Lock()
	s.checkpoints[checkpoint.SessionID] = &cpCopy
	s.mu.Unlock()

	// Return a reference string (for in-memory, just the session ID)
	ref := fmt.Sprintf("memory://%s/v%d", checkpoint.SessionID, checkpoint.Version)
	return ref, nil
}

// Delete removes a checkpoint from memory.
func (s *InMemoryStore) Delete(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.checkpoints, sessionID)
	return nil
}

// List returns all session IDs.
func (s *InMemoryStore) List(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.checkpoints))
	for id := range s.checkpoints {
		ids = append(ids, id)
	}
	return ids, nil
}

// SerializeCheckpoint encodes a checkpoint to JSON (for storage).
func SerializeCheckpoint(cp *agentorcav1alpha1.Checkpoint) ([]byte, error) {
	return json.Marshal(cp)
}

// DeserializeCheckpoint decodes a checkpoint from JSON.
func DeserializeCheckpoint(data []byte) (*agentorcav1alpha1.Checkpoint, error) {
	var cp agentorcav1alpha1.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}
