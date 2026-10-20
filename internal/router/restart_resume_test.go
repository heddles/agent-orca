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
	"testing"

	"github.com/heddles/agent-orca/internal/state"
)

// keyedCheckpointStore serves conversation checkpoints and spend per key so
// tests can tell which checkpoint ClaimRun resumed from (#37).
type keyedCheckpointStore struct {
	state.Store
	msgs  map[string][]json.RawMessage
	spend map[string]float64
}

func newKeyedCheckpointStore(t *testing.T) *keyedCheckpointStore {
	t.Helper()
	nop, err := state.NewStoreFromConfig(state.Config{})
	if err != nil {
		t.Fatalf("nop store: %v", err)
	}
	return &keyedCheckpointStore{Store: nop, msgs: map[string][]json.RawMessage{}, spend: map[string]float64{}}
}

func (s *keyedCheckpointStore) LoadMessages(_ context.Context, key string) ([]json.RawMessage, error) {
	return s.msgs[key], nil
}

func (s *keyedCheckpointStore) LoadSpend(_ context.Context, key string) (float64, error) {
	return s.spend[key], nil
}

func (s *keyedCheckpointStore) put(t *testing.T, key string, spend float64, msgs ...Message) {
	t.Helper()
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		s.msgs[key] = append(s.msgs[key], raw)
	}
	s.spend[key] = spend
}

func newClaimTestRouter(store state.Store) *Router {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &Config{Providers: []ProviderConfig{{Name: "p", ContextWindow: 200000}}}
	return &Router{
		cfg:        cfg,
		store:      store,
		ruleRouter: NewRuleRouter(cfg),
		cancelCtx:  ctx,
		cancelFunc: cancel,
	}
}

// Regression for #37: a restarted run re-claiming a warm pod must resume its
// OWN checkpoint (progress + spend), not the prior chat turn's.
func TestClaimRun_RestartResumesOwnCheckpoint(t *testing.T) {
	store := newKeyedCheckpointStore(t)
	store.put(t, checkpointKeyForRun("prior"), 0.10,
		Message{Role: "user", Content: "previous turn"})
	store.put(t, checkpointKeyForRun("run-1"), 0.75,
		Message{Role: "user", Content: "previous turn"},
		Message{Role: "user", Content: "refactor the parser"},
		Message{Role: "assistant", Content: "step 1 done"})

	r := newClaimTestRouter(store)
	defer r.cancelFunc()
	r.ClaimRun(WarmRunInput{
		RunName: "run-1", Input: "refactor the parser", PriorRunRef: "prior",
		ResumeCheckpointKey: checkpointKeyForRun("run-1"),
	})

	if r.cfg.ResumeCheckpointKey != checkpointKeyForRun("run-1") {
		t.Fatalf("resume key: got %q, want the run's own checkpoint", r.cfg.ResumeCheckpointKey)
	}
	if len(r.priorMessages) != 3 || r.priorMessages[2].Content != "step 1 done" {
		t.Fatalf("want the run's own 3-message checkpoint, got %+v", r.priorMessages)
	}
	if r.spendUSD != 0.75 {
		t.Fatalf("spend must continue from the run's own checkpoint, got %v", r.spendUSD)
	}
	if !r.restartResume {
		t.Fatal("restartResume must be set when resuming the run's own checkpoint")
	}
}

// Older operators only send PriorRunRef: chat-turn continuation keeps working.
func TestClaimRun_PriorRunRefFallback(t *testing.T) {
	store := newKeyedCheckpointStore(t)
	store.put(t, checkpointKeyForRun("prior"), 0.10, Message{Role: "user", Content: "previous turn"})

	r := newClaimTestRouter(store)
	defer r.cancelFunc()
	r.ClaimRun(WarmRunInput{RunName: "run-2", Input: "next", PriorRunRef: "prior"})

	if r.cfg.ResumeCheckpointKey != checkpointKeyForRun("prior") {
		t.Fatalf("resume key: got %q, want the prior run's checkpoint", r.cfg.ResumeCheckpointKey)
	}
	if len(r.priorMessages) != 1 || r.spendUSD != 0.10 {
		t.Fatalf("want prior history + spend, got %+v / %v", r.priorMessages, r.spendUSD)
	}
	if r.restartResume {
		t.Fatal("a chat-turn continuation is not a restart resume")
	}
}

func TestIsRestartResume(t *testing.T) {
	own := checkpointKeyForRun("r")
	cases := []struct {
		resume, ckpt string
		want         bool
	}{
		{"", own, false},
		{checkpointKeyForRun("prior"), own, false},
		{own, own, true},
	}
	for _, c := range cases {
		if got := isRestartResume(&Config{ResumeCheckpointKey: c.resume, CheckpointKey: c.ckpt}); got != c.want {
			t.Errorf("isRestartResume(%q, %q) = %v, want %v", c.resume, c.ckpt, got, c.want)
		}
	}
}

func TestDropResentUserInput(t *testing.T) {
	prior := []Message{
		{Role: "user", Content: "refactor the parser"},
		{Role: "assistant", Content: "step 1 done"},
	}
	sys := Message{Role: "system", Content: "sys"}

	// The runtime re-sends the original input after a restart: drop it.
	got := dropResentUserInput([]Message{sys, {Role: "user", Content: "refactor the parser"}}, prior)
	if len(got) != 1 || got[0].Content != sys.Content {
		t.Fatalf("re-sent input must be dropped, got %+v", got)
	}
	// A genuinely new user message must reach the model.
	got = dropResentUserInput([]Message{{Role: "user", Content: "also add tests"}}, prior)
	if len(got) != 1 {
		t.Fatalf("new user message must be kept, got %+v", got)
	}
	// Non-user trailing messages are untouched.
	in := []Message{{Role: "user", Content: "refactor the parser"}, {Role: "tool", Content: "x"}}
	if got = dropResentUserInput(in, prior); len(got) != 2 {
		t.Fatalf("trailing tool message must be untouched, got %+v", got)
	}
}

// A chat turn that restarts before writing its first checkpoint must fall back
// to the prior turn's history (and spend) rather than lose the conversation.
func TestClaimRun_RestartBeforeFirstCheckpointFallsBackToPrior(t *testing.T) {
	store := newKeyedCheckpointStore(t)
	store.put(t, checkpointKeyForRun("prior"), 0.10, Message{Role: "user", Content: "previous turn"})

	r := newClaimTestRouter(store)
	defer r.cancelFunc()
	r.ClaimRun(WarmRunInput{
		RunName: "run-1", Input: "next question", PriorRunRef: "prior",
		ResumeCheckpointKey: checkpointKeyForRun("run-1"), // own checkpoint: empty
	})

	if len(r.priorMessages) != 1 || r.priorMessages[0].Content != "previous turn" {
		t.Fatalf("want the prior turn's history, got %+v", r.priorMessages)
	}
	if r.spendUSD != 0.10 {
		t.Fatalf("spend must come from the checkpoint actually loaded, got %v", r.spendUSD)
	}
}

func TestLoadResumeMessages(t *testing.T) {
	store := newKeyedCheckpointStore(t)
	store.put(t, "own", 0, Message{Role: "user", Content: "own"})
	store.put(t, "prior", 0, Message{Role: "user", Content: "prior"})
	ctx := context.Background()

	if msgs, key, _ := LoadResumeMessages(ctx, store, "own", "prior"); key != "own" || len(msgs) != 1 {
		t.Fatalf("non-empty primary must win, got key %q (%d msgs)", key, len(msgs))
	}
	if msgs, key, _ := LoadResumeMessages(ctx, store, "missing", "prior"); key != "prior" || len(msgs) != 1 {
		t.Fatalf("empty primary must fall back, got key %q (%d msgs)", key, len(msgs))
	}
	if msgs, key, _ := LoadResumeMessages(ctx, store, "missing", ""); key != "missing" || len(msgs) != 0 {
		t.Fatalf("no fallback: want empty from primary, got key %q (%d msgs)", key, len(msgs))
	}
}
