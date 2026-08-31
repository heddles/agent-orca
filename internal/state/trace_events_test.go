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
	"encoding/json"
	"testing"
	"time"
)

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
	eventJSON, _ := json.Marshal(tokenEvent{Type: "token", Content: "hello"})
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
