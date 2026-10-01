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
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
	// Format: "agentorca/runs/<run-id>/state"
	CheckpointKey string `json:"checkpointKey,omitempty"`

	// MaxTokenStreamLen is the Redis XADD MAXLEN (APPROX) applied to each run's token
	// and trace-event stream ("tokens:<ns>:<run>"). The stream holds one entry per
	// streamed token delta + trace event, so long-running/Reasoning-heavy runs can
	// exceed the historical 10k cap and silently lose the OLDEST tokens on replay.
	// 0 (default) = 100000 (~10x the old floor), enough headroom for very long runs
	// while still bounding per-run memory. Tools/agents reading the stream via
	// TailTokens keep the newest entries; set higher per-deployment if you run
	// extremely long sessions. Not related to output truncation mid-stream.
	MaxTokenStreamLen int `json:"maxTokenStreamLen,omitempty"`
}

// TraceEntry is a single timestamped entry in a run's execution trace, mirroring
// the UI's TraceEntry TypeScript interface. Used to archive the full trace
// (tokens, tool calls, tool results, etc.) from the Redis stream so that
// historical runs can be rendered with the same TraceAccordion component as live
// runs.
type TraceEntry struct {
	// ID is a sequential counter assigned during archival for React key stability.
	ID int `json:"id"`
	// Event is the discriminated trace event JSON (e.g. {"type":"toolCall",...}).
	Event json.RawMessage `json:"event"`
	// TS is an RFC3339 timestamp derived from the Redis stream entry ID.
	TS string `json:"ts"`
	// ChildRunName is set when this entry originated from a child run's stream.
	ChildRunName string `json:"childRunName,omitempty"`
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

	// SaveToken appends a streaming token to a Redis Stream for real-time UI
	// streaming. Completion is signalled by a terminal trace event (see
	// IsTerminalTraceEventJSON and TailTokens), not by an empty token, so every
	// value saved here is real streaming content under the "t" field.
	SaveToken(ctx context.Context, key string, token string) error

	// SaveTraceEvent appends a structured trace event (JSON) to the Redis Stream.
	// These are interleaved with tokens so the UI can display tool calls in order.
	SaveTraceEvent(ctx context.Context, key string, eventJSON string) error

	// TailTokens returns a channel that yields tokens and trace events in order.
	// Regular tokens are plain strings. Trace events are prefixed with "\x00" followed
	// by JSON. Replays all prior entries first (handles page refresh), then blocks for
	// new ones. The channel closes when ctx is cancelled, when a terminal trace event
	// (done/fail/finalOutput) is received, or after a 10-minute absolute deadline.
	TailTokens(ctx context.Context, key string) (<-chan string, error)

	// ReadTraceEvents returns all trace entries from the token-stream Redis key,
	// ordered by stream entry ID. Tokens are converted to
	// {"type":"token","content":"..."} events; structured trace events (ev field)
	// are returned with their original JSON payload. Each entry receives a
	// sequential id and an ISO timestamp parsed from the Redis stream ID.
	// Returns nil if the stream does not exist or is empty. Used at archival
	// time to snapshot a run's full execution trace into PostgreSQL.
	ReadTraceEvents(ctx context.Context, key string) ([]TraceEntry, error)

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

	// ListMessageKeys returns conversation-checkpoint keys matching a glob pattern.
	// The pattern uses Redis SCAN MATCH semantics: '*' matches any run of
	// characters including '/'. Implementations without a native scan should
	// return (nil, nil). Used to enumerate prior-run checkpoints for the
	// warm-pool local cache + history retrieval.
	ListMessageKeys(ctx context.Context, pattern string) ([]string, error)

	// SignalCancel sets a cancellation flag for a run in Redis.
	// The flag has a short TTL (5 minutes) and auto-expires.
	// External systems can also write this key directly: SET cancel:<ns>:<run> 1 EX 300
	SignalCancel(ctx context.Context, ns, runName string) error

	// IsCancelled checks whether the cancel flag is set for a run.
	IsCancelled(ctx context.Context, ns, runName string) (bool, error)

	// Ping verifies connectivity to the backing store (e.g. Redis).
	// Used by /readyz. Implementations must return nil if reachable.
	Ping(ctx context.Context) error

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
func (nopStore) LoadAnswer(_ context.Context, _ string) (string, error)            { return "", nil }
func (nopStore) SaveHTTPOutput(_ context.Context, _ string, _ string) error        { return nil }
func (nopStore) LoadHTTPOutput(_ context.Context, _ string) (string, error)        { return "", nil }
func (nopStore) DeleteKey(_ context.Context, _ string) error                       { return nil }
func (nopStore) SaveToken(_ context.Context, _ string, _ string) error             { return nil }
func (nopStore) SaveTraceEvent(_ context.Context, _ string, _ string) error        { return nil }
func (nopStore) ReadTraceEvents(_ context.Context, _ string) ([]TraceEntry, error) { return nil, nil }
func (nopStore) TailTokens(_ context.Context, _ string) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (nopStore) SaveKV(_ context.Context, _, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (nopStore) LoadKV(_ context.Context, _, _ string) ([]byte, error) { return nil, nil }
func (nopStore) DeleteKV(_ context.Context, _, _ string) error         { return nil }
func (nopStore) ListKV(_ context.Context, _ string) ([]string, error)  { return nil, nil }
func (nopStore) ListMessageKeys(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (nopStore) SignalCancel(_ context.Context, _, _ string) error        { return nil }
func (nopStore) IsCancelled(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (nopStore) Ping(_ context.Context) error                             { return nil }
func (nopStore) Close() error                                             { return nil }

// redisStore implements Store using Redis with zstd compression.
type redisStore struct {
	client            *redis.Client
	encoder           *zstd.Encoder
	decoder           *zstd.Decoder
	tokenStreamMaxLen int64 // XADD MAXLEN(APPROX) for tokens:<ns>:<run> streams
}

func (s *redisStore) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return errors.New("redis store not initialized")
	}
	return s.client.Ping(ctx).Err()
}

// defaultTokenStreamMaxLen bounds each run's token/trace-event Redis stream. Raised
// from the old hard-coded 10000 so very long / Reasoning-heavy sessions don't silently
// shed their oldest streamed tokens when the user refreshes to replay. Each XADD entry is
// small (a token delta or a short trace event), so ~100k entries is negligible memory per
// run and still auto-expires on the 24h sliding TTL.
const defaultTokenStreamMaxLen = 100000

// resolveTokenStreamCap returns the effective MAXLEN for the per-run token stream: an
// explicit Config.MaxTokenStreamLen (if >0) or the package default. Factored out so it can
// be unit-tested without a live Redis connection.
func resolveTokenStreamCap(n int) int64 {
	if n > 0 {
		return int64(n)
	}
	return defaultTokenStreamMaxLen
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

	return &redisStore{
		client:            client,
		encoder:           enc,
		decoder:           dec,
		tokenStreamMaxLen: resolveTokenStreamCap(cfg.MaxTokenStreamLen),
	}, nil
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
	key := "agentorca:runs:" + runName + ":http-output"
	return s.client.Set(ctx, key, output, time.Hour).Err()
}

func (s *redisStore) LoadHTTPOutput(ctx context.Context, runName string) (string, error) {
	key := "agentorca:runs:" + runName + ":http-output"
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
		MaxLen: s.tokenStreamMaxLen,
		Approx: true,
		Values: map[string]any{"ev": eventJSON},
	}).Err(); err != nil {
		return fmt.Errorf("XADD %s (trace): %w", key, err)
	}
	s.client.Expire(ctx, key, expiry)
	return nil
}

func (s *redisStore) SaveToken(ctx context.Context, key string, token string) error {
	// Keep the stream long enough for a user to refresh and replay tokens.
	// Completion is signalled by a terminal trace event (see IsTerminalTraceEventJSON
	// and TailTokens), not by an empty token — so every value saved here is real
	// streaming content under the "t" field.
	expiry := 24 * time.Hour
	if err := s.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: s.tokenStreamMaxLen,
		Approx: true,
		Values: map[string]any{"t": token},
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

		// Absolute deadline: if no terminal trace event arrives (e.g. the sidecar
		// crashed after streaming content but before the final `done` event), don't
		// block the UI handler forever. The normal close signal is a terminal trace
		// event (done/fail/finalOutput) emitted at the OpenAI-schema terminal turn,
		// not a magic empty token.
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
			if s.sendStreamEntry(ctx, ch, entry) {
				return
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
					if s.sendStreamEntry(ctx, ch, entry) {
						return
					}
					lastID = entry.ID
				}
			}
		}
	}()
	return ch, nil
}

// ReadTraceEvents returns all trace entries from the Redis stream for the given
// token-stream key, in stream order. Tokens and thinking deltas are grouped
// into a single entry per consecutive burst (rather than one entry per
// individual delta) — this keeps the archived trace compact (~100x smaller)
// while preserving the same information the UI renders for live runs.
// Structured trace events (toolCall, toolResult, guardrail, etc.) are returned
// individually with their original JSON payload. The ts field is derived from
// the Redis stream entry ID (millisecond timestamp). Returns nil if the stream
// does not exist or is empty.
func (s *redisStore) ReadTraceEvents(ctx context.Context, key string) ([]TraceEntry, error) {
	entries, err := s.client.XRange(ctx, key, "-", "+").Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("XRANGE %s: %w", key, err)
	}

	result := make([]TraceEntry, 0, len(entries))
	id := 0

	// Accumulators for grouping consecutive same-type delta events. Tokens and
	// thinking deltas are emitted by the model-router one fragment at a time;
	// merging them into a single entry per burst avoids storing O(10K) entries
	// per turn in PostgreSQL (which was ~15MB per run at scale).
	var tokenBuf strings.Builder
	var thoughtBuf strings.Builder
	var pendingTS string
	var pendingToken bool
	var pendingThought bool

	flushPending := func() {
		if pendingToken {
			eventJSON, _ := json.Marshal(map[string]string{
				"type":    "token",
				"content": tokenBuf.String(),
			})
			result = append(result, TraceEntry{ID: id, Event: json.RawMessage(eventJSON), TS: pendingTS})
			id++
			tokenBuf.Reset()
			pendingToken = false
		}
		if pendingThought {
			eventJSON, _ := json.Marshal(map[string]string{
				"type":    "thought",
				"content": thoughtBuf.String(),
			})
			result = append(result, TraceEntry{ID: id, Event: json.RawMessage(eventJSON), TS: pendingTS})
			id++
			thoughtBuf.Reset()
			pendingThought = false
		}
	}

	for _, entry := range entries {
		// Skip legacy empty-token "done" sentinels from older producers.
		if _, ok := entry.Values["done"]; ok {
			continue
		}
		ts := streamIDToISO(entry.ID)

		if t, ok := entry.Values["t"]; ok {
			// Token delta: accumulate into the current burst.
			if !pendingToken {
				pendingTS = ts
			}
			tokenBuf.WriteString(fmt.Sprint(t))
			pendingToken = true
			continue
		}

		if ev, ok := entry.Values["ev"]; ok {
			evStr := fmt.Sprint(ev)

			// Peek at the event type to decide whether to group or flush.
			var meta struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			}
			if json.Unmarshal([]byte(evStr), &meta) == nil {
				if meta.Type == "thought" {
					// Thinking delta: accumulate into the current burst.
					if !pendingThought {
						if pendingToken {
							flushPending()
						}
						pendingTS = ts
					}
					thoughtBuf.WriteString(meta.Content)
					pendingThought = true
					continue
				}
				if meta.Type == "token" {
					// A token wrapped in the ev field (unusual, but handle it).
					if !pendingToken {
						pendingTS = ts
					}
					tokenBuf.WriteString(meta.Content)
					pendingToken = true
					continue
				}
			}

			// Any other event type: flush pending accumulators, then store as-is.
			flushPending()
			result = append(result, TraceEntry{
				ID:    id,
				Event: json.RawMessage(evStr),
				TS:    ts,
			})
			id++
		}
	}
	flushPending()

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// streamIDToISO parses a Redis stream entry ID ("<milliseconds>-<sequence>")
// into an RFC3339 timestamp string. Returns "" if the ID cannot be parsed.
func streamIDToISO(id string) string {
	before, _, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	ms, err := strconv.ParseInt(before, 10, 64)
	if err != nil {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// sendStreamEntry forwards one Redis stream entry to ch. Regular tokens are yielded
// as plain strings; trace events are prefixed with "\x00" (see the uiapi SSE handler).
// It returns true when the channel should close: on a terminal trace event
// (done/fail/finalOutput — the OpenAI turn loop has completed) or on context
// cancellation. This replaces the old empty-token "done sentinel", which was written
// at per-turn boundaries and could close the stream mid-loop ("done sentinel too
// soon" / "exit early"); a terminal trace event is only emitted at the genuine
// schema terminal turn. A legacy `done` field entry (older producers) is still
// treated as terminal for backward compatibility.
func (s *redisStore) sendStreamEntry(ctx context.Context, ch chan<- string, entry redis.XMessage) bool {
	if _, ok := entry.Values["done"]; ok {
		// Legacy empty-token "done sentinel" from older producers.
		return true
	}
	if t, ok := entry.Values["t"]; ok {
		select {
		case ch <- fmt.Sprint(t):
		case <-ctx.Done():
			return true
		}
	}
	if ev, ok := entry.Values["ev"]; ok {
		evStr := fmt.Sprint(ev)
		select {
		case ch <- "\x00" + evStr:
		case <-ctx.Done():
			return true
		}
		if IsTerminalTraceEventJSON(evStr) {
			return true
		}
	}
	return false
}

// terminalTraceEventTypes are trace-event types that mark the end of an OpenAI
// streaming turn loop. They mirror the UI's isTerminalTraceEvent (ui/src/api/traceStream.ts).
// `clarify` is intentionally excluded: a clarified run pauses for human input and
// later resumes on the same token-stream key, so it is not a hard close — that path
// is closed via the CRD WaitingForInput phase (uiapi terminal-state poller) instead.
var terminalTraceEventTypes = map[string]struct{}{
	"fail":        {},
	"finalOutput": {},
}

// IsTerminalTraceEventJSON reports whether a trace-event JSON payload (the value of
// a Redis stream "ev" entry) represents a terminal event — i.e. the OpenAI turn loop
// has completed and TailTokens should close the UI SSE channel. Exported so the
// apiserver's external HTTP client and tests reuse the same rule.
func IsTerminalTraceEventJSON(evJSON string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(evJSON), &m); err != nil {
		return false
	}
	t, _ := m["type"].(string)
	_, ok := terminalTraceEventTypes[t]
	return ok
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

// ListMessageKeys enumerates conversation-checkpoint keys matching the Redis SCAN
// MATCH glob. The model-router uses this to discover prior-run checkpoints for the
// warm-pool history retrieval (the checkpoint keys use slash-delimited paths like
// "agentorca/runs/<run>/state", so '*' must match across slashes — SCAN semantics).
func (s *redisStore) ListMessageKeys(ctx context.Context, pattern string) ([]string, error) {
	var keys []string
	iter := s.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning message keys with pattern %s: %w", pattern, err)
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
