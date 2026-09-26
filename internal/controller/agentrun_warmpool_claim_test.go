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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgofake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
)

// testStore is a minimal state.Store stub for controller tests; only HTTP output is
// modelled, everything else is a no-op.
type testStore struct{ httpOutput string }

func (s testStore) SaveMessages(context.Context, string, []json.RawMessage, time.Duration) error {
	return nil
}
func (s testStore) LoadMessages(context.Context, string) ([]json.RawMessage, error) { return nil, nil }
func (s testStore) SaveSpend(context.Context, string, float64, time.Duration) error { return nil }
func (s testStore) LoadSpend(context.Context, string) (float64, error)              { return 0, nil }
func (s testStore) SaveToken(context.Context, string, string) error                 { return nil }
func (s testStore) SaveTraceEvent(context.Context, string, string) error            { return nil }
func (s testStore) TailTokens(context.Context, string) (<-chan string, error) {
	return nil, nil
}
func (s testStore) ReadTraceEvents(context.Context, string) ([]state.TraceEntry, error) {
	return nil, nil
}
func (s testStore) SaveAnswer(context.Context, string, string, time.Duration) error { return nil }
func (s testStore) LoadAnswer(context.Context, string) (string, error)              { return "", nil }
func (s testStore) SaveHTTPOutput(context.Context, string, string) error            { return nil }
func (s testStore) LoadHTTPOutput(context.Context, string) (string, error) {
	return s.httpOutput, nil
}
func (s testStore) DeleteKey(context.Context, string) error { return nil }
func (s testStore) SaveKV(context.Context, string, string, []byte, time.Duration) error {
	return nil
}
func (s testStore) LoadKV(context.Context, string, string) ([]byte, error) { return nil, nil }
func (s testStore) DeleteKV(context.Context, string, string) error         { return nil }
func (s testStore) ListKV(context.Context, string) ([]string, error)       { return nil, nil }
func (s testStore) ListMessageKeys(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s testStore) SignalCancel(context.Context, string, string) error { return nil }
func (s testStore) IsCancelled(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s testStore) Ping(context.Context) error { return nil }
func (s testStore) Close() error               { return nil }

func warmReuseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentorcav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReclaimStaleClaimedWarmPods verifies that a claimed pod whose bound run already
// terminated is returned to idle (and its run label cleared), while a claimed pod whose
// run is still in-flight is left untouched.
func TestReclaimStaleClaimedWarmPods(t *testing.T) {
	scheme := warmReuseScheme(t)

	podDone := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "warm-done", Namespace: "default",
		Labels: map[string]string{
			labelWarmPool:      "dep",
			labelWarmStatus:    warmStatusClaimed,
			labelWarmRequests:  "1",
			"agentorca.io/run": "run-done",
		},
	}}
	podLive := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "warm-live", Namespace: "default",
		Labels: map[string]string{
			labelWarmPool:      "dep",
			labelWarmStatus:    warmStatusClaimed,
			labelWarmRequests:  "2",
			"agentorca.io/run": "run-live",
		},
	}}
	runDone := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-done", Namespace: "default"},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded}}
	runLive := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-live", Namespace: "default"},
		Status: agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning}}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(podDone, podLive, runDone, runLive).
		Build()

	r := &AgentRunReconciler{Client: cl, Scheme: scheme}
	r.reclaimStaleClaimedWarmPods(context.Background(), "default", "dep")

	var gotDone, gotLive corev1.Pod
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "warm-done", Namespace: "default"}, &gotDone)
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "warm-live", Namespace: "default"}, &gotLive)

	if gotDone.Labels[labelWarmStatus] != warmStatusIdle {
		t.Fatalf("terminal-run pod warm-status = %q, want idle", gotDone.Labels[labelWarmStatus])
	}
	if _, ok := gotDone.Labels["agentorca.io/run"]; ok {
		t.Fatalf("terminal-run pod should have agentorca.io/run cleared, got %v", gotDone.Labels)
	}
	if gotLive.Labels[labelWarmStatus] != warmStatusClaimed {
		t.Fatalf("in-flight pod warm-status = %q, want claimed (must not be stolen)", gotLive.Labels[labelWarmStatus])
	}
	if gotLive.Labels["agentorca.io/run"] != "run-live" {
		t.Fatalf("in-flight pod run label should be untouched, got %v", gotLive.Labels)
	}
}

// TestCheckProgress_HTTPPeekReturnsWarmPodToIdle verifies the prompt-release fix: when an
// http/chat warm run produces output, the warm pod is returned to idle in the same
// reconcile that observes completion (rather than waiting for the deferred terminal
// reconcile, which left the pod claimed and caused the next turn to spawn a fresh pod).
func TestCheckProgress_HTTPPeekReturnsWarmPodToIdle(t *testing.T) {
	scheme := warmReuseScheme(t)

	const httpOut = "final review summary: PR #49 is a benign 2-line MCP discoverability change; recommend merge with no risks."
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "warm-1", Namespace: "default",
		Labels: map[string]string{
			labelWarmPool:      "dep",
			labelWarmStatus:    warmStatusClaimed,
			labelWarmRequests:  "1",
			"agentorca.io/run": "run-1",
		},
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"}}
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "run-1", Namespace: "default",
		Labels: map[string]string{"agentorca.io/deployment": "dep"},
	}, Status: agentorcav1alpha1.AgentRunStatus{
		Phase:     agentorcav1alpha1.AgentRunPhaseRunning,
		InputMode: "http",
		PodName:   "warm-1",
	}}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentorcav1alpha1.AgentRun{}).
		WithObjects(pod, run).
		Build()

	r := &AgentRunReconciler{
		Client:     cl,
		Scheme:     scheme,
		StateStore: testStore{httpOutput: httpOut},
		// GetLogs is stubbed behind the kubernetes fake clientset; any error is swallowed
		// by handlePodSuccess (httpOut takes precedence over container logs).
		K8s: clientgofake.NewSimpleClientset(),
	}

	if _, err := r.checkProgress(context.Background(), run, &agentorcav1alpha1.Agent{}); err != nil {
		t.Fatalf("checkProgress: %v", err)
	}

	var got corev1.Pod
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "warm-1", Namespace: "default"}, &got)
	if got.Labels[labelWarmStatus] != warmStatusIdle {
		t.Fatalf("warm pod warm-status = %q, want idle (prompt release failed)", got.Labels[labelWarmStatus])
	}
	if _, ok := got.Labels["agentorca.io/run"]; ok {
		t.Fatalf("warm pod should have agentorca.io/run cleared after completion, got %v", got.Labels)
	}

	var gotRun agentorcav1alpha1.AgentRun
	_ = cl.Get(context.Background(), client.ObjectKeyFromObject(run), &gotRun)
	if gotRun.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded {
		t.Fatalf("run phase = %q, want Succeeded", gotRun.Status.Phase)
	}
	if gotRun.Status.Output != httpOut {
		t.Fatalf("run output = %q, want httpOut", gotRun.Status.Output)
	}
}
