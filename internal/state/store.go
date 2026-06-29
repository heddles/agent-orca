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

// Package state provides conversation checkpoint storage for agent runs.
// Checkpoints are written by the model-router sidecar after each LLM response
// and read on pod restart to enable seamless failure recovery.
//
// Storage is tiered:
//   - Active runs: Redis (fast, TTL-based expiry)
//   - Completed/archived: object storage (S3/GCS/Azure Blob) if configured
//
// All values are zstd-compressed before storage to reduce cost.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/redis/go-redis/v9"
)

// Config holds connection parameters for the state store, sourced from Helm values
// and injected into the model-router config file.
type Config struct {
	// Backend selects the storage backend: "redis" (default).
	// Future backends: "postgres", "s3", "gcs", "azureblob".
	Backend string `json:"backend"`

	// RedisURL is the Redis connection URL.
	// Example: "redis://redis.infra.svc.cluster.local:6379"
	RedisURL string `json:"redisUrl,omitempty"`

	// TTLSeconds is the Redis key TTL beyond the run timeout.
	// Keys auto-expire, so no manual cleanup is needed.
	TTLSeconds int `json:"ttlSeconds,omitempty"`

	// CheckpointKey is the Redis key prefix for this run.
	// Format: "agentorc/runs/<run-id>/state"
	CheckpointKey string `json:"checkpointKey,omitempty"`
}

// Store is the interface for conversation state storage.
type Store interface {
	// SaveMessages persists the full conversation history.
	// The implementation may write incrementally (delta) or as a full snapshot.
	SaveMessages(ctx context.Context, key string, messages []json.RawMessage, ttl time.Duration) error

	// LoadMessages retrieves the full conversation history from a checkpoint key.
	LoadMessages(ctx context.Context, key string) ([]json.RawMessage, error)

	// SaveSpend persists the cumulative USD spend for a run.
	// Called after every LLM response so the value survives pod crashes.
	SaveSpend(ctx context.Context, key string, usd float64, ttl time.Duration) error

	// LoadSpend retrieves the cumulative USD spend for a run.
	// Returns 0 if no spend has been recorded.
	LoadSpend(ctx context.Context, key string) (float64, error)

	// SaveToken appends a token to a Redis Stream for real-time UI streaming.
	// An empty token signals end-of-stream (done sentinel).
	SaveToken(ctx context.Context, key string, token string) error

	// SaveTraceEvent appends a structured trace event (JSON) to the Redis Stream.
	// These are interleaved with tokens so the UI can display tool calls in order.
	SaveTraceEvent(ctx context.Context, key string, eventJSON string) error

	// TailTokens returns a channel that yields tokens and trace events in order.
	// Regular tokens are plain strings. Trace events are prefixed with "\x00" followed
	// by JSON. Replays all prior entries first (handles page refresh), then blocks for
	// new ones. The channel closes when ctx is cancelled or a done sentinel is received.
	TailTokens(ctx context.Context, key string) (<-chan string, error)

	// SaveAnswer stores a human's clarification answer for a run.
	SaveAnswer(ctx context.Context, key string, answer string, ttl time.Duration) error

	// LoadAnswer retrieves a pending clarification answer. Returns "" if none is set.
	LoadAnswer(ctx context.Context, key string) (string, error)

	// SaveHTTPOutput stores the output captured from an http-mode agent run.
	SaveHTTPOutput(ctx context.Context, runName string, output string) error

	// LoadHTTPOutput retrieves http-mode agent run output. Returns "" if not set.
	LoadHTTPOutput(ctx context.Context, runName string) (string, error)

	// DeleteKey removes a key from the store.
	DeleteKey(ctx context.Context, key string) error

	// SaveKV stores an arbitrary key-value pair within a scoped prefix.
	// Values are zstd-compressed. Maximum value size is 1MB (enforced by callers).
	SaveKV(ctx context.Context, scope, key string, value []byte, ttl time.Duration) error

	// LoadKV retrieves a value by scope and key. Returns nil, nil if not found.
	LoadKV(ctx context.Context, scope, key string) ([]byte, error)

	// DeleteKV removes a key within a scope.
	DeleteKV(ctx context.Context, scope, key string) error

	// ListKV returns all keys within a scope.
	ListKV(ctx context.Context, scope string) ([]string, error)

	// SignalCancel sets a cancellation flag for a run in Redis.
	// The flag has a short TTL (5 minutes) and auto-expires.
	// External systems can also write this key directly: SET cancel:<ns>:<run> 1 EX 300
	SignalCancel(ctx context.Context, ns, runName string) error

	// IsCancelled checks whether the cancel flag is set for a run.
	IsCancelled(ctx context.Context, ns, runName string) (bool, error)

	// Close releases any resources held by the store.
	Close() error
}

// NewStoreFromConfig creates the appropriate Store implementation based on cfg.Backend.
// When Backend is "" (not configured), a no-op store is returned so the
// model-router starts successfully without any external state dependency.
func NewStoreFromConfig(cfg Config) (Store, error) {
	switch cfg.Backend {
	case "":
		return &nopStore{}, nil
	case "redis":
		return newRedisStore(cfg)
	default:
		return nil, fmt.Errorf("unsupported state backend %q; supported: redis", cfg.Backend)
	}
}

// nopStore is a no-op Store used when state persistence is disabled.
type nopStore struct{}

func (nopStore) SaveMessages(_ context.Context, _ string, _ []json.RawMessage, _ time.Duration) error {
	return nil
}
func (nopStore) LoadMessages(_ context.Context, _ string) ([]json.RawMessage, error) {
	return nil, nil
}
func (nopStore) SaveSpend(_ context.Context, _ string, _ float64, _ time.Duration) error {
	return nil
}
func (nopStore) LoadSpend(_ context.Context, _ string) (float64, error) { return 0, nil }
func (nopStore) SaveAnswer(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (nopStore) LoadAnswer(_ context.Context, _ string) (string, error)     { return "", nil }
func (nopStore) SaveHTTPOutput(_ context.Context, _ string, _ string) error { return nil }
func (nopStore) LoadHTTPOutput(_ context.Context, _ string) (string, error) { return "", nil }
func (nopStore) DeleteKey(_ context.Context, _ string) error                { return nil }
func (nopStore) SaveToken(_ context.Context, _ string, _ string) error      { return nil }
func (nopStore) SaveTraceEvent(_ context.Context, _ string, _ string) error { return nil }
func (nopStore) TailTokens(_ context.Context, _ string) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (nopStore) SaveKV(_ context.Context, _, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (nopStore) LoadKV(_ context.Context, _, _ string) ([]byte, error)    { return nil, nil }
func (nopStore) DeleteKV(_ context.Context, _, _ string) error            { return nil }
func (nopStore) ListKV(_ context.Context, _ string) ([]string, error)     { return nil, nil }
func (nopStore) SignalCancel(_ context.Context, _, _ string) error        { return nil }
func (nopStore) IsCancelled(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (nopStore) Close() error                                             { return nil }

// redisStore implements Store using Redis with zstd compression.
type redisStore struct {
	client  *redis.Client
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func newRedisStore(cfg Config) (*redisStore, error) {
	url := cfg.RedisURL
	if url == "" {
		url = "redis://localhost:6379"
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis URL: %w", err)
	}

	client := redis.NewClient(opts)

	// Verify connectivity eagerly so misconfigurations surface at startup
	// instead of silently returning zero spend on every read.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connecting to redis at %s: %w", url, err)
	}

	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)

	return &redisStore{client: client, encoder: enc, decoder: dec}, nil
}

func (s *redisStore) SaveMessages(ctx context.Context, key string, messages []json.RawMessage, ttl time.Duration) error {
	raw, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshalling messages: %w", err)
	}

	compressed := s.encoder.EncodeAll(raw, make([]byte, 0, len(raw)/4))
	return s.client.Set(ctx, key, compressed, ttl).Err()
}

func (s *redisStore) LoadMessages(ctx context.Context, key string) ([]json.RawMessage, error) {
	compressed, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading from redis: %w", err)
	}

	raw, err := s.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing checkpoint: %w", err)
	}

	var messages []json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&messages); err != nil {
		return nil, fmt.Errorf("unmarshalling checkpoint: %w", err)
	}
	return messages, nil
}

func (s *redisStore) SaveSpend(ctx context.Context, key string, usd float64, ttl time.Duration) error {
	return s.client.Set(ctx, key+":spend", fmt.Sprintf("%.6f", usd), ttl).Err()
}

func (s *redisStore) LoadSpend(ctx context.Context, key string) (float64, error) {
	val, err := s.client.Get(ctx, key+":spend").Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

func (s *redisStore) SaveAnswer(ctx context.Context, key string, answer string, ttl time.Duration) error {
	return s.client.Set(ctx, key, answer, ttl).Err()
}

func (s *redisStore) LoadAnswer(ctx context.Context, key string) (string, error) {
	val, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

func (s *redisStore) SaveHTTPOutput(ctx context.Context, runName string, output string) error {
	key := "agentorc:runs:" + runName + ":http-output"
	return s.client.Set(ctx, key, output, time.Hour).Err()
}

func (s *redisStore) LoadHTTPOutput(ctx context.Context, runName string) (string, error) {
	key := "agentorc:runs:" + runName + ":http-output"
	val, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

func (s *redisStore) DeleteKey(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

func (s *redisStore) SaveTraceEvent(ctx context.Context, key string, eventJSON string) error {
	expiry := 24 * time.Hour
	if err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: 10000,
		Approx: true,
		Values: map[string]interface{}{"ev": eventJSON},
	}).Err(); err != nil {
		return fmt.Errorf("XADD %s (trace): %w", key, err)
	}
	s.client.Expire(ctx, key, expiry)
	return nil
}

func (s *redisStore) SaveToken(ctx context.Context, key string, token string) error {
	field := "t"
	value := token
	// Keep the stream long enough for a user to refresh and replay tokens.
	// The done sentinel uses the same TTL — it marks completion but the data
	// should remain available for the full retention window.
	expiry := 24 * time.Hour
	if token == "" {
		field = "done"
		value = "1"
	}
	if err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: 10000,
		Approx: true,
		Values: map[string]interface{}{field: value},
	}).Err(); err != nil {
		return fmt.Errorf("XADD %s: %w", key, err)
	}
	// Sliding expiry: refreshed on every write, auto-cleans on pod crash.
	s.client.Expire(ctx, key, expiry)
	return nil
}

func (s *redisStore) TailTokens(ctx context.Context, key string) (<-chan string, error) {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)

		// Absolute deadline: if the done sentinel never arrives (e.g. sidecar crashed
		// before writing it), don't block the UI handler forever.
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		// Phase 1: Replay all existing entries.
		entries, err := s.client.XRange(ctx, key, "-", "+").Result()
		if err != nil && err != redis.Nil {
			slog.Warn("XRANGE failed for token stream", "key", key, "err", err)
			return
		}
		lastID := "0-0"
		for _, entry := range entries {
			if _, ok := entry.Values["done"]; ok {
				return
			}
			if t, ok := entry.Values["t"]; ok {
				select {
				case ch <- fmt.Sprint(t):
				case <-ctx.Done():
					return
				}
			}
			if ev, ok := entry.Values["ev"]; ok {
				select {
				case ch <- "\x00" + fmt.Sprint(ev):
				case <-ctx.Done():
					return
				}
			}
			lastID = entry.ID
		}

		// Phase 2: Tail new entries via blocking XREAD.
		for {
			streams, err := s.client.XRead(ctx, &redis.XReadArgs{
				Streams: []string{key, lastID},
				Count:   100,
				Block:   5 * time.Second,
			}).Result()
			if err != nil {
				if err == redis.Nil || ctx.Err() != nil {
					// Timeout or context cancelled.
					if ctx.Err() != nil {
						return
					}
					continue
				}
				slog.Warn("XREAD failed for token stream", "key", key, "err", err)
				return
			}
			for _, stream := range streams {
				for _, entry := range stream.Messages {
					if _, ok := entry.Values["done"]; ok {
						return
					}
					if t, ok := entry.Values["t"]; ok {
						select {
						case ch <- fmt.Sprint(t):
						case <-ctx.Done():
							return
						}
					}
					if ev, ok := entry.Values["ev"]; ok {
						select {
						case ch <- "\x00" + fmt.Sprint(ev):
						case <-ctx.Done():
							return
						}
					}
					lastID = entry.ID
				}
			}
		}
	}()
	return ch, nil
}

// kvKey builds a namespaced Redis key for the KV store.
func kvKey(scope, key string) string {
	return scope + ":" + key
}

func (s *redisStore) SaveKV(ctx context.Context, scope, key string, value []byte, ttl time.Duration) error {
	compressed := s.encoder.EncodeAll(value, make([]byte, 0, len(value)/4))
	return s.client.Set(ctx, kvKey(scope, key), compressed, ttl).Err()
}

func (s *redisStore) LoadKV(ctx context.Context, scope, key string) ([]byte, error) {
	compressed, err := s.client.Get(ctx, kvKey(scope, key)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading kv %s: %w", kvKey(scope, key), err)
	}
	raw, err := s.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing kv %s: %w", kvKey(scope, key), err)
	}
	return raw, nil
}

func (s *redisStore) DeleteKV(ctx context.Context, scope, key string) error {
	return s.client.Del(ctx, kvKey(scope, key)).Err()
}

func (s *redisStore) ListKV(ctx context.Context, scope string) ([]string, error) {
	pattern := scope + ":*"
	prefix := scope + ":"
	var keys []string
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		// Strip the scope prefix so callers see just the key name.
		k := iter.Val()
		if len(k) > len(prefix) {
			keys = append(keys, k[len(prefix):])
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning kv keys with prefix %s: %w", pattern, err)
	}
	return keys, nil
}

// cancelKey builds the Redis key for a run's cancellation flag.
func cancelKey(ns, runName string) string {
	return "cancel:" + ns + ":" + runName
}

func (s *redisStore) SignalCancel(ctx context.Context, ns, runName string) error {
	return s.client.Set(ctx, cancelKey(ns, runName), "1", 5*time.Minute).Err()
}

func (s *redisStore) IsCancelled(ctx context.Context, ns, runName string) (bool, error) {
	n, err := s.client.Exists(ctx, cancelKey(ns, runName)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *redisStore) Close() error {
	s.decoder.Close()
	return s.client.Close()
}
