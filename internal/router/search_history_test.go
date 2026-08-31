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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/floppyfish14/agent-orca/internal/state"
)

// searchFakeStore is a minimal state.Store for exercising _search_history.
// It holds a deployment run index (scope "agentorca/deployments/<dep>/runs"
// → keys) plus message checkpoints keyed by "agentorca/runs/<run>/state".
type searchFakeStore struct {
	index    map[string][]string          // scope -> keys  (only the runs scope is meaningful)
	messages map[string][]json.RawMessage // checkpoint key -> messages
}

func newSearchStore() *searchFakeStore {
	return &searchFakeStore{index: map[string][]string{}, messages: map[string][]json.RawMessage{}}
}

func (s *searchFakeStore) SaveMessages(_ context.Context, key string, msgs []json.RawMessage, _ time.Duration) error {
	s.messages[key] = msgs
	return nil
}
func (s *searchFakeStore) LoadMessages(_ context.Context, key string) ([]json.RawMessage, error) {
	return s.messages[key], nil
}
func (s *searchFakeStore) SaveSpend(context.Context, string, float64, time.Duration) error {
	return nil
}
func (s *searchFakeStore) LoadSpend(context.Context, string) (float64, error)   { return 0, nil }
func (s *searchFakeStore) SaveToken(context.Context, string, string) error      { return nil }
func (s *searchFakeStore) SaveTraceEvent(context.Context, string, string) error { return nil }
func (s *searchFakeStore) ReadTraceEvents(context.Context, string) ([]state.TraceEntry, error) {
	return nil, nil
}
func (s *searchFakeStore) TailTokens(context.Context, string) (<-chan string, error) {
	ch := make(chan string)
	close(ch)
	return ch, nil
}
func (s *searchFakeStore) SaveAnswer(context.Context, string, string, time.Duration) error {
	return nil
}
func (s *searchFakeStore) LoadAnswer(context.Context, string) (string, error)     { return "", nil }
func (s *searchFakeStore) SaveHTTPOutput(context.Context, string, string) error   { return nil }
func (s *searchFakeStore) LoadHTTPOutput(context.Context, string) (string, error) { return "", nil }
func (s *searchFakeStore) DeleteKey(context.Context, string) error                { return nil }
func (s *searchFakeStore) SaveKV(context.Context, string, string, []byte, time.Duration) error {
	return nil
}
func (s *searchFakeStore) LoadKV(context.Context, string, string) ([]byte, error) { return nil, nil }
func (s *searchFakeStore) DeleteKV(context.Context, string, string) error         { return nil }
func (s *searchFakeStore) ListKV(_ context.Context, scope string) ([]string, error) {
	return s.index[scope], nil
}
func (s *searchFakeStore) ListMessageKeys(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s *searchFakeStore) SignalCancel(context.Context, string, string) error { return nil }
func (s *searchFakeStore) IsCancelled(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *searchFakeStore) Ping(context.Context) error { return nil }
func (s *searchFakeStore) Close() error               { return nil }

var _ state.Store = (*searchFakeStore)(nil)

func msg(raw string) json.RawMessage { return json.RawMessage(raw) }

func TestSearchHistoryFindsRelevantPriorTurns(t *testing.T) {
	store := newSearchStore()
	// Deployment run index advertises two prior runs.
	store.index["agentorca/deployments/soc-runs/runs"] = []string{"run-1", "run-2"}
	// run-1 mentions the sought term "CVE-2024-9999".
	store.messages["agentorca/runs/run-1/state"] = []json.RawMessage{
		msg(`{"role":"user","content":"investigate CVE-2024-9999 on the edge host"}`),
		msg(`{"role":"assistant","content":"I could not find that CVE in any source."}`),
	}
	// run-2 is irrelevant.
	store.messages["agentorca/runs/run-2/state"] = []json.RawMessage{
		msg(`{"role":"user","content":"what is the weather today"}`),
	}

	r := &Router{cfg: &Config{DeploymentName: "soc-runs", RunName: "run-3"}, store: store}

	out := r.executeSearchHistory(context.Background(), `{"query":"CVE-2024-9999","topK":3}`)
	var result searchOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("executeSearchHistory returned non-JSON %q: %v", out, err)
	}
	if !result.Found {
		t.Fatalf("expected found=true for relevant prior turn; got %+v", result)
	}
	if len(result.Results) == 0 {
		t.Fatalf("expected at least one matching snippet; got %+v", result)
	}
	hit := result.Results[0].Snippet
	if !strings.Contains(hit, "CVE-2024-9999") {
		t.Fatalf("expected the matching turn snippet to contain the query term; got %q", hit)
	}
	if result.Results[0].Run != "run-1" {
		t.Fatalf("expected match from run-1 (relevance), got %q", result.Results[0].Run)
	}
	// The current run must never be returned as "prior" context.
	for _, res := range result.Results {
		if res.Run == "run-3" {
			t.Errorf("current run must be excluded from search results")
		}
	}
}

func TestSearchHistoryNotFoundReturnsFoundFalse(t *testing.T) {
	store := newSearchStore()
	store.index["agentorca/deployments/soc-runs/runs"] = []string{"run-1"}
	store.messages["agentorca/runs/run-1/state"] = []json.RawMessage{
		msg(`{"role":"user","content":"talk about the weather"}`),
	}
	r := &Router{cfg: &Config{DeploymentName: "soc-runs", RunName: "run-2"}, store: store}

	out := r.executeSearchHistory(context.Background(), `{"query":"CVE-2024-9999"}`)
	var result searchOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("non-JSON result %q: %v", out, err)
	}
	if result.Found {
		t.Fatalf("expected found=false when no prior turn matches; got %+v", result)
	}
}

func TestSearchHistoryEmptyQuery(t *testing.T) {
	r := &Router{cfg: &Config{DeploymentName: "soc-runs"}, store: newSearchStore()}
	out := r.executeSearchHistory(context.Background(), `{}`)
	var result searchOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("non-JSON result %q: %v", out, err)
	}
	if result.Found {
		t.Fatalf("expected found=false for empty query, got %+v", result)
	}
}
