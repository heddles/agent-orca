package runoutput

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/heddles/agent-orca/internal/state"
)

// kvStore keeps KV values in memory; everything else is the nop store.
type kvStore struct {
	state.Store
	kv      map[string][]byte
	saveErr error
}

func newKV(t *testing.T) *kvStore {
	t.Helper()
	nop, err := state.NewStoreFromConfig(state.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &kvStore{Store: nop, kv: map[string][]byte{}}
}

func (s *kvStore) SaveKV(_ context.Context, scope, key string, v []byte, _ time.Duration) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.kv[scope+"/"+key] = v
	return nil
}

func (s *kvStore) LoadKV(_ context.Context, scope, key string) ([]byte, error) {
	return s.kv[scope+"/"+key], nil
}

func TestFinishKeepsTheAnswerOutOfTheStatus(t *testing.T) {
	st := newKV(t)
	long := strings.Repeat("a", 50_000) // far over the old 10 KB status cap
	if got := Finish(context.Background(), st, "ns", "r1", long); got != "" {
		t.Fatalf("status.output = %d bytes, want none: the answer is stored", len(got))
	}
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "r1"}}
	if got := Of(context.Background(), st, run); got != "" {
		t.Fatalf("a run that has not succeeded read the store: %d bytes", len(got))
	}
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	if got := Of(context.Background(), st, run); got != long {
		t.Fatalf("stored answer = %d bytes, want %d", len(got), len(long))
	}
}

func TestFinishFallsBackToAClippedStatusCopy(t *testing.T) {
	long := strings.Repeat("a", 50_000)
	for name, st := range map[string]state.Store{"no store": nil, "store fails": func() state.Store {
		s := newKV(t)
		s.saveErr = errors.New("redis is down")
		return s
	}()} {
		t.Run(name, func(t *testing.T) {
			got := Finish(context.Background(), st, "ns", "r1", long)
			if got == "" || len(got) > StatusMaxBytes || !strings.HasSuffix(got, truncationMarker) {
				t.Fatalf("got %d bytes, want a clipped copy of at most %d", len(got), StatusMaxBytes)
			}
		})
	}
}

func TestOfReadsAnOlderRunsStatus(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "old"}}
	run.Status.Output = "kept in status before this change"
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	if got := Of(context.Background(), newKV(t), run); got != run.Status.Output {
		t.Fatalf("got %q", got)
	}
	if got := Of(context.Background(), nil, run); got != run.Status.Output {
		t.Fatalf("nil store: got %q", got)
	}
}

func TestSaveClipsToTheSanityLimit(t *testing.T) {
	st := newKV(t)
	if err := Save(context.Background(), st, "ns", "big", strings.Repeat("a", MaxBytes+10)); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(context.Background(), st, "ns", "big"); len(got) > MaxBytes {
		t.Fatalf("stored %d bytes, limit %d", len(got), MaxBytes)
	}
}
