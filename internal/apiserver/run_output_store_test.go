package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
)

// memKV keeps KV values in memory; everything else is the nop store.
type memKV struct {
	state.Store
	kv map[string][]byte
}

func newMemKV(t *testing.T) *memKV {
	t.Helper()
	nop, err := state.NewStoreFromConfig(state.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &memKV{Store: nop, kv: map[string][]byte{}}
}

func (s *memKV) SaveKV(_ context.Context, scope, key string, v []byte, _ time.Duration) error {
	s.kv[scope+"/"+key] = v
	return nil
}

func (s *memKV) LoadKV(_ context.Context, scope, key string) ([]byte, error) {
	return s.kv[scope+"/"+key], nil
}

// _done keeps a long answer out of the AgentRun status, and the status poll a
// parent (or the executor) makes gets it back.
func TestDoneStoresTheAnswerOutsideTheStatus(t *testing.T) {
	srv, cl := budgetServer(t, budgetRun(nil))
	st := newMemKV(t)
	srv.WithStateStore(st)
	answer := strings.Repeat("all good. ", 5000) // 50 KB, over the old status cap

	body, _ := json.Marshal(map[string]string{"output": answer})
	rec := httptest.NewRecorder()
	srv.doneAgentRun(rec, httptest.NewRequest(http.MethodPost, "/done", strings.NewReader(string(body))), "team-a", "run-1")
	if rec.Code >= 300 {
		t.Fatalf("done: %d %s", rec.Code, rec.Body.String())
	}

	var run agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "run-1"}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Status.Output != "" {
		t.Fatalf("status.output = %d bytes, want none: it belongs in the state store, not etcd", len(run.Status.Output))
	}

	rec = httptest.NewRecorder()
	srv.getAgentRun(rec, httptest.NewRequest(http.MethodGet, "/status", nil), "team-a", "run-1")
	var got agentorcav1alpha1.AgentRun
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Output != answer {
		t.Fatalf("status poll returned %d bytes, want the full %d", len(got.Status.Output), len(answer))
	}
}
