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
	"testing"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/state"
)

// traceRecorder is a state store that records trace events.
type traceRecorder struct {
	state.Store
	events map[string][]string
}

func (s *traceRecorder) SaveTraceEvent(_ context.Context, key, ev string) error {
	s.events[key] = append(s.events[key], ev)
	return nil
}

func TestRecordSandboxDecisions(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	start := metav1.NewTime(time.Now().Add(-time.Minute))
	run.Status.StartTime = &start
	r, cl := sandboxReconciler(t, gw, run, token)
	nop, _ := state.NewStoreFromConfig(state.Config{})
	store := &traceRecorder{Store: nop, events: map[string][]string{}}
	r.StateStore = store

	t1, t2 := start.Add(10*time.Second), start.Add(20*time.Second)
	gw.decisions = map[string][]openshell.Decision{"sb-1": {
		{Time: t1, Kind: "net", Action: "allowed", Binary: "/usr/bin/curl", Destination: "pypi.org:443", Raw: "NET:OPEN ..."},
		{Time: t2, Kind: "http", Action: "denied", Method: "POST", URL: "https://pypi.org/simple/", Reason: "not permitted", Raw: "HTTP:POST ..."},
	}}
	sb := &osv1.Sandbox{Name: "sb-1"}

	r.recordSandboxDecisions(context.Background(), run, sb)

	events := store.events["tokens:default:run-1"]
	if len(events) != 2 {
		t.Fatalf("trace events = %v, want both decisions", events)
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(events[1]), &second)
	if second["type"] != "egress" || second["action"] != "denied" || second["url"] != "https://pypi.org/simple/" || second["reason"] != "not permitted" {
		t.Errorf("egress event = %v", second)
	}
	got := fetchSandboxRun(t, cl, run)
	if got.Status.SandboxDecisionsSeen == nil || !got.Status.SandboxDecisionsSeen.Time.Equal(t2) {
		t.Fatalf("watermark = %v, want the last decision's time", got.Status.SandboxDecisionsSeen)
	}

	// The next poll continues after the watermark: nothing is recorded twice.
	r.recordSandboxDecisions(context.Background(), got, sb)
	if n := len(store.events["tokens:default:run-1"]); n != 2 {
		t.Errorf("trace events after a second poll = %d, want still 2", n)
	}
}
