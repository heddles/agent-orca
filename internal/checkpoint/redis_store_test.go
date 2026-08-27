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
	"testing"
	"time"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// memKV is a tiny in-memory kvStore for testing RedisStore without Redis.
type memKV struct {
	data map[string][]byte
	ttl  map[string]time.Duration
}

func newMemKV() *memKV {
	return &memKV{data: make(map[string][]byte), ttl: make(map[string]time.Duration)}
}

func (m *memKV) SaveKV(_ context.Context, scope, key string, value []byte, ttl time.Duration) error {
	m.data[scope+"/"+key] = value
	m.ttl[scope+"/"+key] = ttl
	return nil
}
func (m *memKV) LoadKV(_ context.Context, scope, key string) ([]byte, error) {
	return m.data[scope+"/"+key], nil
}
func (m *memKV) DeleteKV(_ context.Context, scope, key string) error {
	delete(m.data, scope+"/"+key)
	delete(m.ttl, scope+"/"+key)
	return nil
}
func (m *memKV) ListKV(_ context.Context, scope string) ([]string, error) {
	var keys []string //nolint:prealloc

	for k := range m.data {
		keys = append(keys, k)
	}
	return keys, nil
}

func TestRedisStoreRoundTrip(t *testing.T) {
	s := NewRedisStore(newMemKV())
	cp := &agentorcav1alpha1.Checkpoint{
		SessionID:  "sess-1",
		Version:    3,
		LastRunRef: "run-2",
		ConversationHistory: []agentorcav1alpha1.ConversationMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "world"},
		},
	}
	if _, err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil { //nolint:staticcheck

		t.Fatal("expected checkpoint, got nil")
	}
	if got.SessionID != "sess-1" || got.LastRunRef != "run-2" || got.Version != 3 { //nolint:staticcheck

		t.Fatalf("unexpected checkpoint: %+v", got)
	}
	if len(got.ConversationHistory) != 2 || got.ConversationHistory[0].Content != "hello" {
		t.Fatalf("unexpected history: %+v", got.ConversationHistory)
	}
}

func TestRedisStoreNotFoundReturnsNil(t *testing.T) {
	s := NewRedisStore(newMemKV())
	got, err := s.Load(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Load unknown: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for unknown session, got %+v", got)
	}
}

func TestRedisStoreEmptySessionID(t *testing.T) {
	s := NewRedisStore(newMemKV())
	if got, err := s.Load(context.Background(), ""); err != nil || got != nil {
		t.Fatalf("Load(\"\") = %v, %v; want nil,nil", got, err)
	}
	if _, err := s.Save(context.Background(), &agentorcav1alpha1.Checkpoint{}); err == nil {
		t.Fatal("Save without SessionID should error")
	}
	if err := s.Delete(context.Background(), ""); err != nil {
		t.Fatalf("Delete(\"\") should be no-op, got %v", err)
	}
}

func TestRedisStoreHistoryTruncated(t *testing.T) {
	s := NewRedisStore(newMemKV())
	cp := &agentorcav1alpha1.Checkpoint{SessionID: "sess"}
	// More than maxSessionHistoryMessages: only the tail should persist.
	got, want := 250, maxSessionHistoryMessages
	history := make([]agentorcav1alpha1.ConversationMessage, got)
	for i := range history {
		history[i] = agentorcav1alpha1.ConversationMessage{Role: "user", Content: "msg"}
	}
	cp.ConversationHistory = history
	if _, err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := s.Load(context.Background(), "sess")
	if err != nil || loaded == nil {
		t.Fatalf("Load: %v %+v", err, loaded)
	}
	if len(loaded.ConversationHistory) != want {
		t.Fatalf("history truncated to %d, want %d", len(loaded.ConversationHistory), want)
	}
	// Keep the MOST RECENT messages: the last 'want' of the input.
	if loaded.ConversationHistory[0].Content != history[got-want].Content {
		t.Fatalf("expected tail of history to be retained, got %q", loaded.ConversationHistory[0].Content)
	}
}

func TestRedisStoreTTLPropagated(t *testing.T) {
	kv := newMemKV()
	s := NewRedisStore(kv)
	cp := &agentorcav1alpha1.Checkpoint{SessionID: "sess2"}
	if _, err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if ttl, ok := kv.ttl[sessionScope+"/sess2"]; !ok || ttl <= 0 {
		t.Fatalf("expected positive TTL forwarded to KV, got %v (ok=%v)", ttl, ok)
	}
	if ttl := kv.ttl[sessionScope+"/sess2"]; ttl != sessionCheckpointTTL {
		t.Fatalf("TTL=%v want %v", ttl, sessionCheckpointTTL)
	}
}

func TestRedisStoreDeleteAndList(t *testing.T) {
	s := NewRedisStore(newMemKV())
	for _, sid := range []string{"a", "b", "c"} {
		if _, err := s.Save(context.Background(), &agentorcav1alpha1.Checkpoint{SessionID: sid}); err != nil {
			t.Fatalf("Save %s: %v", sid, err)
		}
	}
	if err := s.Delete(context.Background(), "b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ := s.Load(context.Background(), "b"); got != nil {
		t.Fatalf("expected b deleted, got %+v", got)
	}
	ids, _ := s.List(context.Background())
	if len(ids) != 2 {
		t.Fatalf("expected 2 sessions after delete, got %d (%v)", len(ids), ids)
	}
}
