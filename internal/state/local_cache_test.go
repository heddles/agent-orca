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

package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// memStore is a tiny in-memory Store used only to assert the local-cache
// decorator's fallback behaviour. It records every call so tests can verify
// write-through and local-first reads.
type memStore struct {
	messages map[string][]json.RawMessage
	kv       map[string][]byte
	calls    []string // method names invoked on the backing store
	listKeys []string // message keys this store "knows" about
}

func newMemStore(keys ...string) *memStore {
	return &memStore{
		messages: map[string][]json.RawMessage{},
		kv:       map[string][]byte{},
		listKeys: keys,
	}
}

func (m *memStore) record(method string) { m.calls = append(m.calls, method) }

func (m *memStore) SaveMessages(_ context.Context, key string, msgs []json.RawMessage, _ time.Duration) error {
	m.record("SaveMessages")
	m.messages[key] = msgs
	return nil
}
func (m *memStore) LoadMessages(_ context.Context, key string) ([]json.RawMessage, error) {
	m.record("LoadMessages")
	return m.messages[key], nil
}
func (m *memStore) SaveSpend(context.Context, string, float64, time.Duration) error { return nil }
func (m *memStore) LoadSpend(context.Context, string) (float64, error)              { return 0, nil }
func (m *memStore) SaveToken(context.Context, string, string) error                 { return nil }
func (m *memStore) SaveTraceEvent(context.Context, string, string) error            { return nil }
func (m *memStore) TailTokens(context.Context, string) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (m *memStore) SaveAnswer(context.Context, string, string, time.Duration) error { return nil }
func (m *memStore) LoadAnswer(context.Context, string) (string, error)              { return "", nil }
func (m *memStore) SaveHTTPOutput(context.Context, string, string) error            { return nil }
func (m *memStore) LoadHTTPOutput(context.Context, string) (string, error)          { return "", nil }
func (m *memStore) DeleteKey(context.Context, string) error                         { return nil }
func (m *memStore) SaveKV(context.Context, string, string, []byte, time.Duration) error {
	return nil
}
func (m *memStore) LoadKV(context.Context, string, string) ([]byte, error) { return nil, nil }
func (m *memStore) DeleteKV(context.Context, string, string) error         { return nil }
func (m *memStore) ListKV(context.Context, string) ([]string, error)       { return nil, nil }
func (m *memStore) ListMessageKeys(_ context.Context, pattern string) ([]string, error) {
	var out []string
	for _, k := range m.listKeys {
		if redisGlobMatch(pattern, k) {
			out = append(out, k)
		}
	}
	return out, nil
}
func (m *memStore) SignalCancel(context.Context, string, string) error        { return nil }
func (m *memStore) IsCancelled(context.Context, string, string) (bool, error) { return false, nil }
func (m *memStore) Ping(context.Context) error                                { return nil }
func (m *memStore) Close() error                                              { return nil }

var _ Store = (*memStore)(nil)

func TestLocalCacheWriteThroughAndLocalFirstRead(t *testing.T) {
	dir := t.TempDir()
	backing := newMemStore()
	store := NewLocalCacheStore(backing, dir)

	ctx := context.Background()
	key := "agentorc/runs/run-1/state"
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hello"}`),
		json.RawMessage(`{"role":"assistant","content":"hi there"}`),
	}

	if err := store.SaveMessages(ctx, key, msgs, 0); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}
	if len(backing.messages[key]) == 0 {
		t.Fatal("write-through: backing store was not written")
	}

	// Clear the backing store to prove LOAD comes from local disk, not Redis.
	delete(backing.messages, key)
	backing.calls = nil

	got, err := store.LoadMessages(ctx, key)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages from local cache, got %d", len(got))
	}
	if indexOf(backing.calls, "LoadMessages") >= 0 {
		t.Fatalf("LoadMessages should NOT hit the backing store on a local hit; calls=%v", backing.calls)
	}
}

func TestLocalCacheLocalMissFallsBackToBacking(t *testing.T) {
	dir := t.TempDir()
	backing := newMemStore()
	store := NewLocalCacheStore(backing, dir)
	ctx := context.Background()

	key := "agentorc/runs/run-cold/state"
	want := []json.RawMessage{json.RawMessage(`{"role":"user","content":"cold start"}`)}
	backing.messages[key] = want

	got, err := store.LoadMessages(ctx, key)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected fallback to backing store, got %d messages", len(got))
	}

	// A write should populate local so a subsequent read is local-first.
	if err := store.SaveMessages(ctx, key, want, 0); err != nil {
		t.Fatal(err)
	}
	delete(backing.messages, key)
	got2, err := store.LoadMessages(ctx, key)
	if err != nil || len(got2) != 1 {
		t.Fatalf("expected local hit after write, got %v (err %v)", got2, err)
	}
}

func TestLocalCacheRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	store := NewLocalCacheStore(newMemStore(), dir).(*localCacheStore)

	for _, bad := range []string{"../escape", "agentorc/../../etc/passwd", "foo/../../escape"} {
		p, err := store.cachePath(bad)
		if err == nil {
			t.Errorf("expected path-traversal rejection for %q", bad)
			continue
		}
		if p != "" && !isUnderDir(p, dir) {
			t.Errorf("cachePath %q resolved outside %s", p, dir)
		}
	}

	// An absolute-looking key is confined: the leading slash is stripped and the
	// path is joined under the cache dir (no escape).
	if p, err := store.cachePath("/etc/passwd"); err != nil || !isUnderDir(p, dir) {
		t.Errorf("absolute-looking key should be confined under cache dir; got %q err=%v", p, err)
	}
}

func TestLocalCacheListMessageKeysMergesLocalAndRemote(t *testing.T) {
	dir := t.TempDir()
	backing := newMemStore("agentorc/runs/remote-run/state")
	store := NewLocalCacheStore(backing, dir)
	ctx := context.Background()

	localKey := "agentorc/runs/local-run/state"
	if err := store.SaveMessages(ctx, localKey, []json.RawMessage{json.RawMessage(`{"role":"user"}`)}, 0); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListMessageKeys(ctx, "agentorc/runs/*/state")
	if err != nil {
		t.Fatalf("ListMessageKeys: %v", err)
	}
	sort.Strings(got)
	if !contains(got, localKey) {
		t.Errorf("local key missing from merged listing: %v", got)
	}
	if !contains(got, "agentorc/runs/remote-run/state") {
		t.Errorf("remote key missing from merged listing: %v", got)
	}
}

func TestRedisGlobMatch(t *testing.T) {
	tests := []struct {
		pat, name string
		want      bool
	}{
		{"agentorc/runs/*/state", "agentorc/runs/run-1/state", true},
		{"agentorc/runs/*/state", "agentorc/runs/run-1/extra", false},
		{"agentorc/runs/*/state", "agentorc/runs/a/b/state", true}, // * crosses '/'
		{"agentorc/runs/*/state", "agentorc/runs/", false},
		{"*", "anything/at/all", true},
		{"agentorc/runs/r1/state", "agentorc/runs/r1/state", true},
		{"agentorc/runs/r1/state", "agentorc/runs/r2/state", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"", "", true},
	}
	for _, tt := range tests {
		if got := redisGlobMatch(tt.pat, tt.name); got != tt.want {
			t.Errorf("redisGlobMatch(%q,%q)=%v want %v", tt.pat, tt.name, got, tt.want)
		}
	}
}

func TestLocalCacheDisabledWhenDirEmpty(t *testing.T) {
	backing := newMemStore()
	if got := NewLocalCacheStore(backing, ""); got != backing {
		t.Fatalf("expected backing store passthrough when dir empty, got %T", got)
	}
	if got := NewLocalCacheStore(nil, "/tmp/whatever"); got != nil {
		t.Fatalf("expected nil when backing is nil, got %T", got)
	}
}

func TestLocalCacheCorruptFileFallsBack(t *testing.T) {
	dir := t.TempDir()
	backing := newMemStore()
	store := NewLocalCacheStore(backing, dir).(*localCacheStore)
	ctx := context.Background()
	key := "agentorc/runs/run-x/state"

	if err := store.SaveMessages(ctx, key, []json.RawMessage{json.RawMessage(`{"role":"user"}`)}, 0); err != nil {
		t.Fatal(err)
	}
	// Corrupt the on-disk file so the decoder fails.
	p, _ := store.cachePath(key)
	if err := os.WriteFile(p, []byte("not zstd"), 0o640); err != nil {
		t.Fatal(err)
	}
	backing.messages[key] = []json.RawMessage{json.RawMessage(`{"role":"user","content":"from backing"}`)}

	got, err := store.LoadMessages(ctx, key)
	if err != nil {
		t.Fatalf("LoadMessages after corruption: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected fallback to backing store after corruption, got %d", len(got))
	}
}

// --- helpers ---

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func contains(s []string, v string) bool { return indexOf(s, v) >= 0 }

func isUnderDir(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel != "" && !startsDotDot(rel)
}

func startsDotDot(s string) bool {
	return len(s) >= 2 && s[0] == '.' && s[1] == '.' && (len(s) == 2 || s[2] == filepath.Separator)
}
