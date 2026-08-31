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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// LocalCacheDirEnv is the env var the model-router reads to activate the warm-pod
// local disk cache. When non-empty, the operator has mounted a disk-backed emptyDir
// at this path into the model-router sidecar and the store is wrapped with a
// localCacheStore.
const LocalCacheDirEnv = "AGENTORC_WARM_CACHE_DIR"

// LocalCacheFileExt is the extension appended to on-disk checkpoint files.
const LocalCacheFileExt = ".msg.zst"

// NewLocalCacheStore wraps a durable backing Store (Redis in production) with a
// local-disk L1 cache rooted at dir. The cache is write-through (every
// SaveMessages persists to disk AND the backing store) and read-preferred
// (LoadMessages returns the local copy when present, falling back to the backing
// store on a local miss). This lets a warm pod resume a conversation without a
// Redis round-trip and keeps serving if Redis is transiently unavailable, while
// Redis remains the durable source of truth (the local cache is wiped when the
// pod is deleted).
//
// On-disk checkpoint keys mirror the storage key as a path (e.g. the key
// "agentorca/runs/<run>/state" is stored at <dir>/agentorca/runs/<run>/state.msg.zst).
// Path traversal is rejected: keys are confined to dir via filepath.Clean + a
// prefix check, and only relative component paths are accepted.
func NewLocalCacheStore(backing Store, dir string) Store {
	if backing == nil || dir == "" {
		return backing // never wrap when there is nothing to cache behind
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	s := &localCacheStore{
		backing: backing,
		dir:     filepath.Clean(dir),
		enc:     enc,
		dec:     dec,
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		slog.Warn("warm local cache: could not create cache dir, falling back to backing store only", "dir", dir, "err", err)
		return backing
	}
	slog.Info("warm local cache activated", "dir", dir)
	return s
}

// localCacheStore is the L1 decorator. It satisfies Store by delegating every
// non-checkpoint method to the backing store and overriding the message and
// message-key-list paths.
type localCacheStore struct {
	backing Store
	dir     string
	enc     *zstd.Encoder
	dec     *zstd.Decoder
}

var _ Store = (*localCacheStore)(nil)

// cachePath maps a storage key to an on-disk file path, rejecting any key that
// would escape the cache directory (path traversal).
func (c *localCacheStore) cachePath(key string) (string, error) {
	rel := strings.TrimPrefix(key, "/")
	p := filepath.Join(c.dir, rel+LocalCacheFileExt)
	cleaned := filepath.Clean(p)
	// Ensure the resolved path stays within the cache dir.
	if cleaned != c.dir && !strings.HasPrefix(cleaned, c.dir+string(filepath.Separator)) {
		return "", fmt.Errorf("cache key escapes cache directory: %s", key)
	}
	return cleaned, nil
}

// SaveMessages persists the messages to the local disk cache (best effort) and
// then to the durable backing store (write-through). A local-write failure is
// logged but never blocks the durable write or the run.
func (c *localCacheStore) SaveMessages(ctx context.Context, key string, messages []json.RawMessage, ttl time.Duration) error {
	c.writeLocal(key, messages)
	return c.backing.SaveMessages(ctx, key, messages, ttl)
}

// LoadMessages returns the locally-cached messages when present; otherwise falls
// back to the backing store (Redis). A corrupt local file is logged, removed,
// and the backing store is consulted instead.
func (c *localCacheStore) LoadMessages(ctx context.Context, key string) ([]json.RawMessage, error) {
	if p, err := c.cachePath(key); err == nil {
		if msgs, ok := c.readLocal(p); ok {
			slog.Debug("warm-local-cache hit", "key", key)
			return msgs, nil
		}
	}
	return c.backing.LoadMessages(ctx, key)
}

// writeLocal best-effort writes a checkpoint to the local cache dir.
func (c *localCacheStore) writeLocal(key string, messages []json.RawMessage) {
	p, err := c.cachePath(key)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		slog.Debug("warm local cache: mkdir failed", "key", key, "err", err)
		return
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		slog.Debug("warm local cache: marshal failed", "key", key, "err", err)
		return
	}
	compressed := c.enc.EncodeAll(raw, make([]byte, 0, len(raw)/4))
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, compressed, 0o640); err != nil {
		slog.Debug("warm local cache: write failed", "key", key, "err", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
	}
}

// readLocal reads + decodes a local checkpoint file. Returns (nil, false) when
// the file is absent; logs and removes a corrupt file.
func (c *localCacheStore) readLocal(p string) ([]json.RawMessage, bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	raw, err := c.dec.DecodeAll(data, nil)
	if err != nil {
		slog.Warn("warm local cache: corrupt checkpoint, falling back to backing store", "path", p, "err", err)
		_ = os.Remove(p)
		return nil, false
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		slog.Warn("warm local cache: unmarshal failed, falling back to backing store", "path", p, "err", err)
		_ = os.Remove(p)
		return nil, false
	}
	return msgs, true
}

// ListMessageKeys merges the local cache and the backing store. Local keys are
// returned first (they are the freshest this pod has served), then any backing
// keys not already present locally. The pattern uses Redis SCAN MATCH semantics.
func (c *localCacheStore) ListMessageKeys(ctx context.Context, pattern string) ([]string, error) {
	seen := make(map[string]bool)
	var keys []string

	for _, k := range c.listLocal(pattern) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if remote, err := c.backing.ListMessageKeys(ctx, pattern); err != nil {
		slog.Warn("warm local cache: backing store ListMessageKeys failed", "err", err)
	} else {
		for _, k := range remote {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys, nil
}

// listLocal walks the cache dir and returns storage keys whose path matches the
// glob pattern. Only files ending in LocalCacheFileExt are considered; the
// matched key is the on-disk path relative to dir with the extension stripped.
func (c *localCacheStore) listLocal(pattern string) []string {
	var out []string
	_ = filepath.WalkDir(c.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, LocalCacheFileExt) {
			return nil
		}
		rel, err := filepath.Rel(c.dir, path)
		if err != nil {
			return nil
		}
		key := strings.TrimSuffix(rel, LocalCacheFileExt)
		if redisGlobMatch(pattern, key) {
			out = append(out, key)
		}
		return nil
	})
	return out
}

// Close releases the zstd encoder/decoder. The backing store is closed by its
// owner (the model-router main), not here, to avoid double-close when the local
// cache is the top-level store handed to New().
func (c *localCacheStore) Close() error {
	_ = c.enc.Close()
	c.dec.Close()
	return nil
}

// --- Delegated methods: every Store operation other than the locally-accelerated
//     checkpoint paths simply forwards to the durable backing store. ---

func (c *localCacheStore) SaveSpend(ctx context.Context, key string, usd float64, ttl time.Duration) error {
	return c.backing.SaveSpend(ctx, key, usd, ttl)
}
func (c *localCacheStore) LoadSpend(ctx context.Context, key string) (float64, error) {
	return c.backing.LoadSpend(ctx, key)
}
func (c *localCacheStore) SaveToken(ctx context.Context, key string, token string) error {
	return c.backing.SaveToken(ctx, key, token)
}
func (c *localCacheStore) SaveTraceEvent(ctx context.Context, key string, eventJSON string) error {
	return c.backing.SaveTraceEvent(ctx, key, eventJSON)
}
func (c *localCacheStore) TailTokens(ctx context.Context, key string) (<-chan string, error) {
	return c.backing.TailTokens(ctx, key)
}
func (c *localCacheStore) ReadTraceEvents(ctx context.Context, key string) ([]TraceEntry, error) {
	return c.backing.ReadTraceEvents(ctx, key)
}
func (c *localCacheStore) SaveAnswer(ctx context.Context, key string, answer string, ttl time.Duration) error {
	return c.backing.SaveAnswer(ctx, key, answer, ttl)
}
func (c *localCacheStore) LoadAnswer(ctx context.Context, key string) (string, error) {
	return c.backing.LoadAnswer(ctx, key)
}
func (c *localCacheStore) SaveHTTPOutput(ctx context.Context, runName string, output string) error {
	return c.backing.SaveHTTPOutput(ctx, runName, output)
}
func (c *localCacheStore) LoadHTTPOutput(ctx context.Context, runName string) (string, error) {
	return c.backing.LoadHTTPOutput(ctx, runName)
}
func (c *localCacheStore) DeleteKey(ctx context.Context, key string) error {
	// Best-effort local invalidation, then durable delete.
	if p, err := c.cachePath(key); err == nil {
		_ = os.Remove(p)
	}
	return c.backing.DeleteKey(ctx, key)
}
func (c *localCacheStore) SaveKV(ctx context.Context, scope, key string, value []byte, ttl time.Duration) error {
	return c.backing.SaveKV(ctx, scope, key, value, ttl)
}
func (c *localCacheStore) LoadKV(ctx context.Context, scope, key string) ([]byte, error) {
	return c.backing.LoadKV(ctx, scope, key)
}
func (c *localCacheStore) DeleteKV(ctx context.Context, scope, key string) error {
	return c.backing.DeleteKV(ctx, scope, key)
}
func (c *localCacheStore) ListKV(ctx context.Context, scope string) ([]string, error) {
	return c.backing.ListKV(ctx, scope)
}
func (c *localCacheStore) SignalCancel(ctx context.Context, ns, runName string) error {
	return c.backing.SignalCancel(ctx, ns, runName)
}
func (c *localCacheStore) IsCancelled(ctx context.Context, ns, runName string) (bool, error) {
	return c.backing.IsCancelled(ctx, ns, runName)
}
func (c *localCacheStore) Ping(ctx context.Context) error {
	return c.backing.Ping(ctx)
}

// redisGlobMatch implements the subset of Redis SCAN MATCH glob semantics we use:
// '*' matches any run of characters (including '/'), '?' matches a single
// character, and all other bytes are literal. This makes local key enumeration
// consistent with the Redis SCAN MATCH used by redisStore.ListMessageKeys.
func redisGlobMatch(pattern, name string) bool {
	for {
		// Consume a run of literal characters (no '*' or '?').
		i := 0
		for i < len(pattern) && pattern[i] != '*' && pattern[i] != '?' {
			i++
		}
		if i > 0 {
			if !strings.HasPrefix(name, pattern[:i]) {
				return false
			}
			name = name[i:]
			pattern = pattern[i:]
		}
		if len(pattern) == 0 {
			return len(name) == 0
		}
		switch pattern[0] {
		case '*':
			pattern = pattern[1:]
			// Greedy backtracking: try to anchor the remainder at every position.
			for i := 0; i <= len(name); i++ {
				if redisGlobMatch(pattern, name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		}
	}
}
