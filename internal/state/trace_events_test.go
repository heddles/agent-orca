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
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// eventTypeToken is the trace-event type for consolidated token bursts.
const eventTypeToken = "token"

func TestStreamIDToISO(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "standard stream id",
			id:   "1740000000000-0",
			want: time.UnixMilli(1740000000000).UTC().Format(time.RFC3339Nano),
		},
		{
			name: "stream id with sequence",
			id:   "1740000000500-3",
			want: time.UnixMilli(1740000000500).UTC().Format(time.RFC3339Nano),
		},
		{
			name: "empty id",
			id:   "",
			want: "",
		},
		{
			name: "no dash separator",
			id:   "abc",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := streamIDToISO(c.id)
			if got != c.want {
				t.Errorf("streamIDToISO(%q) = %q, want %q", c.id, got, c.want)
			}
		})
	}
}

// TestTraceEntryRoundTrip verifies that a TraceEntry marshals to the JSON shape
// the frontend expects (matching the TypeScript TraceEntry interface).
func TestTraceEntryRoundTrip(t *testing.T) {
	type tokenEvent struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	eventJSON, _ := json.Marshal(tokenEvent{Type: eventTypeToken, Content: "hello"})
	entry := TraceEntry{
		ID:    0,
		Event: json.RawMessage(eventJSON),
		TS:    "2026-01-15T12:00:00.000Z",
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"id":0,"event":{"type":"token","content":"hello"},"ts":"2026-01-15T12:00:00.000Z"}`
	if string(raw) != want {
		t.Errorf("marshalled = %s, want %s", raw, want)
	}

	// childRunName is optional and should be omitted when empty.
	entryWithChild := TraceEntry{
		ID:           1,
		Event:        json.RawMessage(eventJSON),
		TS:           "2026-01-15T12:00:01.000Z",
		ChildRunName: "run-child-xyz",
	}
	rawChild, _ := json.Marshal(entryWithChild)
	var m map[string]any
	if err := json.Unmarshal(rawChild, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["childRunName"] != "run-child-xyz" {
		t.Errorf("expected childRunName in output, got %v", m["childRunName"])
	}
}

// addXAdd is a test helper that XAdd's a value to the Redis stream.
func addXAdd(t *testing.T, rdb *redis.Client, stream, field, value string) {
	t.Helper()
	if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: stream,
		Values: map[string]any{field: value},
	}).Err(); err != nil {
		t.Fatalf("XAdd %s: %v", stream, err)
	}
}

// TestRedisReadTraceEventsConsolidatesTokens verifies that consecutive token
// deltas are merged into a single TraceEntry per burst, while structured
// events (toolCall, done, etc.) remain individual entries. This is the key
// optimisation that prevents ~10K per-token rows being archived per turn.
func TestRedisReadTraceEventsConsolidatesTokens(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := &redisStore{client: rdb}

	key := "tokens:default:test-run"
	ctx := context.Background()

	evToolCall := `{"type":"toolCall","name":"search","arguments":"{}"}`
	evDone := `{"type":"done","finish_reason":"stop"}`

	// Interleaved tokens and trace events (mimics real Redis stream content).
	addXAdd(t, rdb, key, "t", "Hello")
	addXAdd(t, rdb, key, "t", " world")
	addXAdd(t, rdb, key, "ev", evToolCall)
	addXAdd(t, rdb, key, "t", "Based")
	addXAdd(t, rdb, key, "t", " on")
	addXAdd(t, rdb, key, "t", " context")
	addXAdd(t, rdb, key, "ev", evDone)

	entries, err := store.ReadTraceEvents(ctx, key)
	if err != nil {
		t.Fatalf("ReadTraceEvents: %v", err)
	}

	// Expect 4 entries: token burst, toolCall, token burst, done.
	if len(entries) != 4 {
		t.Fatalf("expected 4 consolidated entries, got %d", len(entries))
	}

	// Entry 0: consolidated token burst
	var tokEvent map[string]any
	if err := json.Unmarshal(entries[0].Event, &tokEvent); err != nil {
		t.Fatalf("unmarshal entry 0: %v", err)
	}
	if tokEvent["type"] != eventTypeToken {
		t.Errorf("entry 0: expected type 'token', got %v", tokEvent["type"])
	}
	if tokEvent["content"] != "Hello world" {
		t.Errorf("entry 0: expected content 'Hello world', got %v", tokEvent["content"])
	}

	// Entry 1: toolCall (individual)
	var tcEvent map[string]any
	if err := json.Unmarshal(entries[1].Event, &tcEvent); err != nil {
		t.Fatalf("unmarshal entry 1: %v", err)
	}
	if tcEvent["type"] != "toolCall" {
		t.Errorf("entry 1: expected type 'toolCall', got %v", tcEvent["type"])
	}

	// Entry 2: second consolidated token burst
	if err := json.Unmarshal(entries[2].Event, &tokEvent); err != nil {
		t.Fatalf("unmarshal entry 2: %v", err)
	}
	if tokEvent["type"] != eventTypeToken {
		t.Errorf("entry 2: expected type 'token', got %v", tokEvent["type"])
	}
	if tokEvent["content"] != "Based on context" {
		t.Errorf("entry 2: expected content 'Based on context', got %v", tokEvent["content"])
	}

	// Entry 3: done (individual)
	var doneEvent map[string]any
	if err := json.Unmarshal(entries[3].Event, &doneEvent); err != nil {
		t.Fatalf("unmarshal entry 3: %v", err)
	}
	if doneEvent["type"] != "done" {
		t.Errorf("entry 3: expected type 'done', got %v", doneEvent["type"])
	}

	// IDs must be sequential.
	for i, e := range entries {
		if e.ID != i {
			t.Errorf("entry %d: expected ID %d, got %d", i, i, e.ID)
		}
	}
}

// TestRedisReadTraceEventsConsolidatesThoughts verifies that consecutive
// thought deltas are merged into a single expandable thinking entry.
func TestRedisReadTraceEventsConsolidatesThoughts(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := &redisStore{client: rdb}

	key := "tokens:default:thinking-run"
	ctx := context.Background()

	evToolCall := `{"type":"toolCall","name":"x","arguments":"{}"}`

	addXAdd(t, rdb, key, "ev", `{"type":"thought","content":"plan"}`)
	addXAdd(t, rdb, key, "ev", `{"type":"thought","content":"ning"}`)
	addXAdd(t, rdb, key, "ev", evToolCall)
	addXAdd(t, rdb, key, "ev", `{"type":"thought","content":"after"}`)

	entries, err := store.ReadTraceEvents(ctx, key)
	if err != nil {
		t.Fatalf("ReadTraceEvents: %v", err)
	}

	// Expect 3 entries: consolidated thought, toolCall, consolidated thought.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries (2 thought bursts + 1 toolCall), got %d", len(entries))
	}

	var thoughtEvt map[string]any
	if err := json.Unmarshal(entries[0].Event, &thoughtEvt); err != nil {
		t.Fatalf("unmarshal entry 0: %v", err)
	}
	if thoughtEvt["type"] != "thought" {
		t.Errorf("entry 0: expected 'thought', got %v", thoughtEvt["type"])
	}
	if thoughtEvt["content"] != "planning" {
		t.Errorf("entry 0: expected content 'planning', got %v", thoughtEvt["content"])
	}

	var toolEvt map[string]any
	if err := json.Unmarshal(entries[1].Event, &toolEvt); err != nil {
		t.Fatalf("unmarshal entry 1: %v", err)
	}
	if toolEvt["type"] != "toolCall" {
		t.Errorf("entry 1: expected 'toolCall', got %v", toolEvt["type"])
	}

	if err := json.Unmarshal(entries[2].Event, &thoughtEvt); err != nil {
		t.Fatalf("unmarshal entry 2: %v", err)
	}
	if thoughtEvt["content"] != "after" {
		t.Errorf("entry 2: expected content 'after', got %v", thoughtEvt["content"])
	}
}

// TestRedisReadTraceEventsEmptyStream verifies that a non-existent stream
// returns nil (not an error).
func TestRedisReadTraceEventsEmptyStream(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := &redisStore{client: rdb}

	entries, err := store.ReadTraceEvents(context.Background(), "tokens:default:nonexistent")
	if err != nil {
		t.Fatalf("expected nil error for missing stream, got %v", err)
	}
	if entries != nil {
		t.Fatalf("expected nil entries for missing stream, got %d entries", len(entries))
	}
}

// TestRedisReadTraceEventsLargeTokenBurst verifies that 500 individual token
// deltas are consolidated into a single TraceEntry, keeping the archival
// footprint small even for very long outputs.
func TestRedisReadTraceEventsLargeTokenBurst(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := &redisStore{client: rdb}

	key := "tokens:default:long-run"
	ctx := context.Background()

	// Write 500 individual token deltas followed by a toolCall.
	for range 500 {
		addXAdd(t, rdb, key, "t", "x")
	}
	addXAdd(t, rdb, key, "ev", `{"type":"toolCall","name":"finish","arguments":"{}"}`)

	entries, err := store.ReadTraceEvents(ctx, key)
	if err != nil {
		t.Fatalf("ReadTraceEvents: %v", err)
	}

	// Expect exactly 2 entries: one consolidated token burst, one toolCall.
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (1 token burst + 1 toolCall), got %d", len(entries))
	}

	var tokEvent map[string]any
	if err := json.Unmarshal(entries[0].Event, &tokEvent); err != nil {
		t.Fatalf("unmarshal entry 0: %v", err)
	}
	if tokEvent["type"] != eventTypeToken {
		t.Errorf("entry 0: expected 'token', got %v", tokEvent["type"])
	}
	expected := strings.Repeat("x", 500)
	if tokEvent["content"] != expected {
		t.Errorf("entry 0: expected content of 500 'x' chars, got %d chars", len(tokEvent["content"].(string)))
	}
}
