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

package router

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEmitRoutingTraceEvent verifies the per-request routing decision lands
// in the run's trace stream — ACP/editor clients never poll the AgentRun CRD,
// so the trace stream is their only source of model-selection visibility.
func TestEmitRoutingTraceEvent(t *testing.T) {
	store := newTestStore(t)
	r := newTracingTestRouter(t, store)

	r.emitRoutingTraceEvent(&ProviderConfig{
		Name:         "openrouter-free",
		LiteLLMModel: "z-ai/glm-5.3-flash",
	}, RouteResult{Reason: "weighted random", Confidence: 0.5})

	var routing map[string]any
	found := false
	for _, ev := range store.traceEvents {
		var m map[string]any
		if json.Unmarshal([]byte(ev), &m) != nil || m["type"] != traceEventTypeRouting {
			continue
		}
		if found {
			t.Fatalf("duplicate routing event: %v", ev)
		}
		routing = m
		found = true
	}
	if !found {
		t.Fatalf("no routing trace event in %v", store.traceEvents)
	}
	if routing["model"] != "z-ai/glm-5.3-flash" {
		t.Errorf("model = %v", routing["model"])
	}
	if routing["provider"] != "openrouter-free" {
		t.Errorf("provider = %v", routing["provider"])
	}
	if routing["strategy"] != "rule-based" {
		t.Errorf("strategy = %v", routing["strategy"])
	}
	if routing["reason"] != "weighted random" {
		t.Errorf("reason = %v", routing["reason"])
	}
	if conf, ok := routing["confidence"].(float64); !ok || conf != 0.5 {
		t.Errorf("confidence = %v, want 0.5", routing["confidence"])
	}
}

// TestAnnounceHindsightBankOnce verifies the bank-ready trace event is emitted
// at most once per run even though ensureHindsightBank runs on every request.
func TestAnnounceHindsightBankOnce(t *testing.T) {
	store := newTestStore(t)
	r := newTracingTestRouter(t, store)

	r.announceHindsightBank()
	r.announceHindsightBank()

	var n int
	for _, ev := range store.traceEvents {
		var m map[string]any
		if json.Unmarshal([]byte(ev), &m) == nil && m["type"] == traceEventTypeHindsight {
			if m["op"] != "bank" {
				t.Fatalf("unexpected hindsight op %v", m["op"])
			}
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 hindsight bank event, got %d", n)
	}
}

// TestEmitHindsightTraceEventShape verifies recall/retain events carry the op
// and bankID, and failures add failed+reason — the fields the ACP bridge
// renders for humans watching silent background memory work.
func TestEmitHindsightTraceEventShape(t *testing.T) {
	store := newTestStore(t)
	r := newTracingTestRouter(t, store)

	r.emitHindsightTraceEvent("recall", false, "")
	r.emitHindsightTraceEvent("retain", true, "connect: refused")

	type shaped struct {
		Type   string `json:"type"`
		Op     string `json:"op"`
		BankID string `json:"bankID"`
		Failed bool   `json:"failed"`
		Reason string `json:"reason"`
	}
	var events []shaped
	for _, ev := range store.traceEvents {
		var m shaped
		if json.Unmarshal([]byte(ev), &m) == nil && m.Type == traceEventTypeHindsight {
			events = append(events, m)
		}
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 hindsight events, got %d in %v", len(events), store.traceEvents)
	}
	if events[0].Op != "recall" || events[0].Failed {
		t.Errorf("event 0 = %+v, want recall success", events[0])
	}
	if !strings.Contains(events[0].BankID, "test-deployment") {
		t.Errorf("bankID = %q, want derived from deployment name", events[0].BankID)
	}
	if events[1].Op != "retain" || !events[1].Failed || events[1].Reason != "connect: refused" {
		t.Errorf("event 1 = %+v, want retain failure with reason", events[1])
	}
}

// newTracingTestRouter builds a minimal router wired to the test trace store
// (no operator URL, no real LLM providers traffic — trace emission only needs
// the run name/namespace and the store).
func newTracingTestRouter(t *testing.T, store *testStore) *Router {
	t.Helper()
	cfg := &Config{
		RunName:        "test-run-routing",
		RunNamespace:   "default",
		DeploymentName: "test-deployment",
		Providers: []ProviderConfig{{
			Name:         "openai",
			LiteLLMModel: "openai/gpt-4o",
		}},
		Strategy: "rule-based",
	}
	r, err := New(cfg, store, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}
