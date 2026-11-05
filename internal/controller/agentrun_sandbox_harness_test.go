package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"

	"github.com/heddles/agent-orca/internal/openshell"
	"github.com/heddles/agent-orca/internal/state"
)

func TestRecordHarnessEvents(t *testing.T) {
	raw, err := os.ReadFile("../openshell/testdata/claude-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")

	gw := newStubGateway()
	run, token := gitRun()
	agent := gitAgent()
	agent.Spec.Runtime.Harness = openshell.HarnessClaudeCode
	r, cl := sandboxReconciler(t, gw, run, token)
	nop, _ := state.NewStoreFromConfig(state.Config{})
	store := &traceRecorder{Store: nop, events: map[string][]string{}}
	r.StateStore = store
	sb := &osv1.Sandbox{Name: "sb-1"}
	key := "tokens:default:run-1"

	// Half way through the run: the first three lines are out.
	gw.logs = strings.Join(lines[:3], "")
	r.recordHarnessEvents(context.Background(), run, agent, sb)
	if n := len(store.events[key]); n != 3 { // init, tool_use, rate_limit
		t.Fatalf("trace events = %d, want 3: %v", n, store.events[key])
	}
	got := fetchSandboxRun(t, cl, run)
	if got.Status.HarnessLinesSeen != 3 {
		t.Fatalf("watermark = %d, want 3", got.Status.HarnessLinesSeen)
	}

	// The run ends: only the new lines are recorded, none twice.
	gw.logs = string(raw)
	r.recordHarnessEvents(context.Background(), got, agent, sb)
	events := store.events[key]
	if len(events) != 6 {
		t.Fatalf("trace events = %d, want 6", len(events))
	}
	var use, last map[string]any
	_ = json.Unmarshal([]byte(events[1]), &use)
	_ = json.Unmarshal([]byte(events[5]), &last)
	if use["type"] != "harness" || use["kind"] != "tool_use" || use["tool"] != "Bash" {
		t.Errorf("tool_use event = %v", use)
	}
	if last["kind"] != "result" || last["turns"] != float64(2) {
		t.Errorf("result event = %v", last)
	}
	if got := fetchSandboxRun(t, cl, run); got.Status.HarnessLinesSeen != 6 {
		t.Errorf("watermark = %d, want 6", got.Status.HarnessLinesSeen)
	}
}

func TestRecordHarnessEvents_NoHarness(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, _ := sandboxReconciler(t, gw, run, token)
	nop, _ := state.NewStoreFromConfig(state.Config{})
	store := &traceRecorder{Store: nop, events: map[string][]string{}}
	r.StateStore = store
	gw.logs = `{"type":"result","num_turns":1}` + "\n"

	r.recordHarnessEvents(context.Background(), run, gitAgent(), &osv1.Sandbox{Name: "sb-1"})
	if len(store.events) != 0 {
		t.Fatalf("an agent without a harness got trace events: %v", store.events)
	}
}

// Two reconciles can run back to back with the same cached run, before the status
// patch of the first is visible. The second must not record the first's lines again.
func TestRecordHarnessEvents_StaleWatermark(t *testing.T) {
	raw, err := os.ReadFile("../openshell/testdata/claude-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	gw := newStubGateway()
	run, token := gitRun()
	agent := gitAgent()
	agent.Spec.Runtime.Harness = openshell.HarnessClaudeCode
	r, _ := sandboxReconciler(t, gw, run, token)
	nop, _ := state.NewStoreFromConfig(state.Config{})
	store := &traceRecorder{Store: nop, events: map[string][]string{}}
	r.StateStore = store
	gw.logs = strings.SplitAfter(string(raw), "\n")[0] // just the init line
	sb := &osv1.Sandbox{Name: "sb-1"}

	stale := run.DeepCopy() // what the cache still holds: HarnessLinesSeen == 0
	r.recordHarnessEvents(context.Background(), run, agent, sb)
	r.recordHarnessEvents(context.Background(), stale, agent, sb)

	if n := len(store.events["tokens:default:run-1"]); n != 1 {
		t.Fatalf("trace events = %d, want 1: the init line was recorded twice", n)
	}
}
