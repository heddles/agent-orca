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

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type streamStore struct {
	testStore
	streams map[string][]state.TraceEntry
}

func (s streamStore) ReadTraceEvents(_ context.Context, key string) ([]state.TraceEntry, error) {
	return s.streams[key], nil
}

// A nil trace tells the archive to keep the one it has. Once the run's stream is
// gone (it expires after a day, and Redis can lose it), a trace made only of the
// routing decisions on the run would replace the full one.
func TestTraceForArchiveNeedsTheRunsOwnStream(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}
	run.Status.RoutingDecisions = []agentorcav1alpha1.RoutingDecision{{Model: "m", Provider: "p", Reason: "rule"}}
	run.Status.ChildRunRefs = []string{"child"}
	event := func(kind string) []state.TraceEntry {
		return []state.TraceEntry{{Event: json.RawMessage(`{"type":"` + kind + `"}`), TS: "2026-11-25T10:00:00Z"}}
	}
	for _, tc := range []struct {
		name    string
		streams map[string][]state.TraceEntry
		want    []string // event types expected; nil means no trace
	}{
		{"stream gone", nil, nil},
		{"only a child's stream is left", map[string][]state.TraceEntry{"tokens:ns:child": event("tool_call")}, nil},
		{"stream present", map[string][]state.TraceEntry{
			"tokens:ns:r": event("tool_result"), "tokens:ns:child": event("tool_call"),
		}, []string{"tool_result", "tool_call", "modelSelected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &AgentRunReconciler{StateStore: streamStore{streams: tc.streams}}
			got := r.loadTraceEventsForArchive(context.Background(), run)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("trace = %s, want none so the archived one is kept", got)
				}
				return
			}
			for _, kind := range tc.want {
				if !strings.Contains(string(got), `"type":"`+kind+`"`) {
					t.Errorf("trace %s lacks a %s event", got, kind)
				}
			}
		})
	}
}
