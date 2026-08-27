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

package apiserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// recordingStore embeds the existing fakeReadyStore (full state.Store impl) and
// overrides only the message read/write path so we can assert whether the
// safety-net wrote a fallback checkpoint.
type recordingStore struct {
	fakeReadyStore
	existing map[string][]json.RawMessage
	written  map[string][]json.RawMessage
}

func newRecordingStore(existing map[string][]json.RawMessage) *recordingStore {
	if existing == nil {
		existing = map[string][]json.RawMessage{}
	}
	return &recordingStore{existing: existing, written: map[string][]json.RawMessage{}}
}

func (s *recordingStore) LoadMessages(_ context.Context, key string) ([]json.RawMessage, error) {
	return s.existing[key], nil
}
func (s *recordingStore) SaveMessages(_ context.Context, key string, msgs []json.RawMessage, _ time.Duration) error {
	s.written[key] = msgs
	return nil
}

func TestEnsureRunCheckpoint_PreservesExisting(t *testing.T) {
	// The model-router already checkpointed the prior run with full-fidelity
	// messages (incl. tool calls). The safety-net must NOT overwrite it.
	full := []json.RawMessage{json.RawMessage(`{"role":"assistant","tool_calls":[...]}`)}
	store := newRecordingStore(map[string][]json.RawMessage{
		"agentorca/runs/run-1/state": full,
	})
	srv := &UIServer{store: store}
	history := []agentorcav1alpha1.ConversationMessage{{Role: "user", Content: "fallback only"}}
	if err := srv.ensureRunCheckpoint(context.Background(), "run-1", history); err != nil {
		t.Fatalf("ensureRunCheckpoint: %v", err)
	}
	if len(store.written) != 0 {
		t.Fatalf("safety-net should NOT have written (model-router checkpoint exists), but wrote: %v", store.written)
	}
}

func TestEnsureRunCheckpoint_FallbackWhenEmpty(t *testing.T) {
	// No model-router checkpoint yet — safety-net must write the session transcript
	// so a restarted pod can resume from it.
	store := newRecordingStore(nil)
	srv := &UIServer{store: store}
	history := []agentorcav1alpha1.ConversationMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	if err := srv.ensureRunCheckpoint(context.Background(), "run-2", history); err != nil {
		t.Fatalf("ensureRunCheckpoint: %v", err)
	}
	got, ok := store.written["agentorca/runs/run-2/state"]
	if !ok {
		t.Fatalf("safety-net should have written fallback checkpoint, wrote: %v", store.written)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 fallback messages, got %d", len(got))
	}
}
