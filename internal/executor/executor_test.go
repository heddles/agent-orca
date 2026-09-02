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

package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExecutor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Executor Suite")
}

// ── fakeStore ────────────────────────────────────────────────────────────────
//
// fakeStore implements state.Store for unit tests.
// TailTokens returns a pre-configured channel so tests can inject child tokens.
// SaveToken records calls so tests can assert what was forwarded to the parent.

type fakeStore struct {
	mu sync.Mutex

	// savedTokens is a log of all SaveToken(key, token) calls.
	savedTokens []savedCall

	// tailChans maps a token-stream key to the channel TailTokens returns.
	// If no entry exists for a key, TailTokens returns an immediately-closed channel.
	tailChans map[string]chan string
}

type savedCall struct {
	key   string
	token string
}

func newFakeStore() *fakeStore {
	return &fakeStore{tailChans: make(map[string]chan string)}
}

// setChildChan registers the channel that TailTokens returns for a given key prefix.
// Tests call this before triggering the executor to inject tokens into the child stream.
func (f *fakeStore) setChildChan(key string, ch chan string) { //nolint:unused

	f.mu.Lock()
	defer f.mu.Unlock()
	f.tailChans[key] = ch
}

// savedFor returns all token values saved for the given key (parent stream).
func (f *fakeStore) savedFor(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.savedTokens {
		if c.key == key {
			out = append(out, c.token)
		}
	}
	return out
}

func (f *fakeStore) SaveToken(_ context.Context, key, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedTokens = append(f.savedTokens, savedCall{key, token})
	return nil
}

func (f *fakeStore) TailTokens(ctx context.Context, key string) (<-chan string, error) {
	f.mu.Lock()
	ch, ok := f.tailChans[key]
	if !ok {
		// Wildcard: used in tests where the child run name is not predictable.
		ch, ok = f.tailChans["*"]
	}
	f.mu.Unlock()
	if !ok {
		closed := make(chan string)
		close(closed)
		return closed, nil
	}
	// Wrap in a goroutine that closes the channel when ctx is cancelled,
	// so the forwarding goroutine in executeAgentTool exits cleanly.
	out := make(chan string, cap(ch))
	go func() {
		defer close(out)
		for {
			select {
			case t, open := <-ch:
				if !open {
					return
				}
				out <- t
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// No-op implementations for the rest of the state.Store interface.
func (f *fakeStore) SaveMessages(_ context.Context, _ string, _ []json.RawMessage, _ time.Duration) error {
	return nil
}
func (f *fakeStore) LoadMessages(_ context.Context, _ string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeStore) SaveSpend(_ context.Context, _ string, _ float64, _ time.Duration) error {
	return nil
}
func (f *fakeStore) LoadSpend(_ context.Context, _ string) (float64, error) { return 0, nil }
func (f *fakeStore) SaveTraceEvent(_ context.Context, _ string, _ string) error {
	return nil
}
func (f *fakeStore) ReadTraceEvents(_ context.Context, _ string) ([]state.TraceEntry, error) {
	return nil, nil
}
func (f *fakeStore) SaveAnswer(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (f *fakeStore) LoadAnswer(_ context.Context, _ string) (string, error)     { return "", nil }
func (f *fakeStore) SaveHTTPOutput(_ context.Context, _ string, _ string) error { return nil }
func (f *fakeStore) LoadHTTPOutput(_ context.Context, _ string) (string, error) { return "", nil }
func (f *fakeStore) DeleteKey(_ context.Context, _ string) error                { return nil }
func (f *fakeStore) SaveKV(_ context.Context, _, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (f *fakeStore) LoadKV(_ context.Context, _, _ string) ([]byte, error) { return nil, nil }
func (f *fakeStore) DeleteKV(_ context.Context, _, _ string) error         { return nil }
func (f *fakeStore) ListKV(_ context.Context, _ string) ([]string, error)  { return nil, nil }
func (f *fakeStore) ListMessageKeys(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeStore) SignalCancel(_ context.Context, _, _ string) error { return nil }
func (f *fakeStore) IsCancelled(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeStore) Ping(_ context.Context) error { return nil }
func (f *fakeStore) Close() error                 { return nil }

var _ state.Store = (*fakeStore)(nil) // compile-time interface check

// ── Helpers ───────────────────────────────────────────────────────────────────

// fakeOperatorServer builds an httptest.Server that mimics the operator's internal API.
// POST /agentrun/{ns} → 201 Created (decodes + re-encodes the AgentRun).
// GET  /agentrun/{ns}/{name} → returns successPhase after successAfter calls; Running before that.
func fakeOperatorServer(successAfter int, output string) (*httptest.Server, *atomic.Int32) {
	var getCount atomic.Int32
	mux := http.NewServeMux()

	mux.HandleFunc("/agentrun/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			var run agentorcav1alpha1.AgentRun
			_ = json.NewDecoder(r.Body).Decode(&run)
			_ = json.NewEncoder(w).Encode(&run)

		case http.MethodGet:
			n := int(getCount.Add(1))
			phase := agentorcav1alpha1.AgentRunPhaseRunning
			if n >= successAfter {
				phase = agentorcav1alpha1.AgentRunPhaseSucceeded
			}
			run := agentorcav1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "child"},
				Status: agentorcav1alpha1.AgentRunStatus{
					Phase:  phase,
					Output: output,
				},
			}
			_ = json.NewEncoder(w).Encode(&run)
		}
	})

	return httptest.NewServer(mux), &getCount
}

// tempSAToken writes a fake token to a temp file and returns its path.
func tempSAToken(t GinkgoTInterface, token string) string {
	t.Helper()
	f, err := os.CreateTemp("", "sa-token-*")
	Expect(err).NotTo(HaveOccurred())
	_, err = f.WriteString(token)
	Expect(err).NotTo(HaveOccurred())
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

// ── Specs ─────────────────────────────────────────────────────────────────────

var _ = Describe("Executor", func() {

	Describe("WithStore", func() {
		It("sets parentTokenKey to tokens:{namespace}:{runName}", func() {
			e := &Executor{namespace: "prod", runName: "my-run"}
			store := newFakeStore()
			e.WithStore(store)
			Expect(e.store).To(Equal(store))
			Expect(e.parentTokenKey).To(Equal("tokens:prod:my-run"))
		})

		It("overwrites a previously set store", func() {
			e := &Executor{namespace: "ns", runName: "run"}
			s1 := newFakeStore()
			s2 := newFakeStore()
			e.WithStore(s1)
			e.WithStore(s2)
			Expect(e.store).To(BeIdenticalTo(s2))
		})
	})

	Describe("executeAgentTool — token forwarding", func() {
		const (
			ns        = "default"
			parentRun = "parent-run"
			childOut  = "analysis complete"
		)

		var (
			srv       *httptest.Server
			store     *fakeStore
			exec      *Executor
			saFile    string
			parentKey string
		)

		BeforeEach(func() {
			store = newFakeStore()
			parentKey = fmt.Sprintf("tokens:%s:%s", ns, parentRun)

			// Server: first GET returns Running, second returns Succeeded.
			srv, _ = fakeOperatorServer(2, childOut)

			saFile = tempSAToken(GinkgoT(), "test-sa-token")
			exec = &Executor{
				namespace:      ns,
				runName:        parentRun,
				operatorAPIURL: srv.URL,
				saTokenFile:    saFile,
			}
			exec.WithStore(store)
		})

		AfterEach(func() { srv.Close() })

		It("forwards child tokens to the parent Redis stream", func() {
			// We don't know the child run name in advance (it embeds nanoseconds),
			// so we register a catch-all: fakeStore returns our channel for any key
			// that isn't already registered. Inject tokens + done sentinel.
			childCh := make(chan string, 5)
			childCh <- "hello "
			childCh <- "world"
			childCh <- "" // done sentinel — must NOT appear in parent stream

			// Register channel for every key (child key is determined at runtime).
			// Intercept TailTokens by overriding tailChans after the Executor is set up.
			// We use a wildcard approach: set a default in TailTokens for unknown keys.
			store.mu.Lock()
			store.tailChans["*"] = childCh
			store.mu.Unlock()

			req := ToolExecuteRequest{
				Tool:        "ehr-meds-agent",
				BackendType: "agent",
				Arguments:   `{"task":"check drug interactions"}`,
				RunName:     parentRun,
				Namespace:   ns,
			}

			resp, err := exec.Dispatch(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Result).To(Equal(childOut))

			// Allow a brief window for the forwarding goroutine to flush any
			// remaining tokens (normally it exits via done sentinel before this).
			Eventually(func() []string {
				return store.savedFor(parentKey)
			}, 500*time.Millisecond, 10*time.Millisecond).Should(ConsistOf("hello ", "world"))
		})

		It("does not forward the done sentinel to the parent stream", func() {
			childCh := make(chan string, 3)
			childCh <- "token"
			childCh <- "" // sentinel

			store.mu.Lock()
			store.tailChans["*"] = childCh
			store.mu.Unlock()

			req := ToolExecuteRequest{
				Tool:        "ehr-meds-agent",
				BackendType: "agent",
				Arguments:   `{"task":"check"}`,
				RunName:     parentRun,
				Namespace:   ns,
			}
			_, err := exec.Dispatch(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() []string {
				return store.savedFor(parentKey)
			}, 500*time.Millisecond, 10*time.Millisecond).Should(ConsistOf("token"))

			// Give a brief window to ensure "" is never written.
			Consistently(func() []string {
				return store.savedFor(parentKey)
			}, 100*time.Millisecond, 20*time.Millisecond).ShouldNot(ContainElement(""))
		})

		It("is a no-op when no store is configured", func() {
			exec.store = nil
			exec.parentTokenKey = ""

			req := ToolExecuteRequest{
				Tool:        "ehr-meds-agent",
				BackendType: "agent",
				Arguments:   `{"task":"check"}`,
				RunName:     parentRun,
				Namespace:   ns,
			}
			Expect(func() {
				_, _ = exec.Dispatch(context.Background(), req)
			}).NotTo(Panic())
		})

		It("returns the child AgentRun output as the tool result", func() {
			req := ToolExecuteRequest{
				Tool:        "ehr-meds-agent",
				BackendType: "agent",
				Arguments:   `{"task":"check"}`,
				RunName:     parentRun,
				Namespace:   ns,
			}
			resp, err := exec.Dispatch(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Result).To(Equal(childOut))
			Expect(resp.Error).To(BeEmpty())
		})

		It("surfaces a child failure as a non-nil error in the response", func() {
			// Override server: first GET Running, second GET Failed.
			srv.Close()
			var getN atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/agentrun/", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(&agentorcav1alpha1.AgentRun{})
					return
				}
				n := int(getN.Add(1))
				phase := agentorcav1alpha1.AgentRunPhaseRunning
				reason := ""
				if n >= 2 {
					phase = agentorcav1alpha1.AgentRunPhaseFailed
					reason = "OOM killed"
				}
				_ = json.NewEncoder(w).Encode(&agentorcav1alpha1.AgentRun{
					Status: agentorcav1alpha1.AgentRunStatus{Phase: phase, FailureReason: reason},
				})
			})
			failSrv := httptest.NewServer(mux)
			defer failSrv.Close()

			exec.operatorAPIURL = failSrv.URL
			req := ToolExecuteRequest{
				Tool: "bad-agent", BackendType: "agent",
				Arguments: `{"task":"will fail"}`, RunName: parentRun, Namespace: ns,
			}
			resp, err := exec.Dispatch(context.Background(), req)
			Expect(err).NotTo(HaveOccurred()) // Dispatch itself doesn't error
			Expect(resp.Error).To(ContainSubstring("OOM killed"))
		})
	})
})
