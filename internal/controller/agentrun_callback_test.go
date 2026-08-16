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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

// capturedHeaders returns an httptest.Server that records the headers of the
// last received request into the returned *http.Header.
func capturedHeaders(t *testing.T) (*httptest.Server, *http.Header) {
	t.Helper()
	var mu sync.Mutex
	rec := http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		for k, v := range r.Header {
			rec[k] = append(rec[k][:0:0], v...)
		}
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	return srv, &rec
}

// TestDeliverCallback_Unsigned verifies that without a signing key the request
// is delivered with only the standard Content-Type header and no signature.
func TestDeliverCallback_Unsigned(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	body := []byte(`{"hello":"world"}`)
	r := &AgentRunReconciler{}
	resp, err := r.deliverCallback(context.Background(), srv.URL, body, nil)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if got := (*h).Get("X-Agentorc-Signature"); got != "" {
		t.Fatalf("expected no signature header when unsigned, got %q", got)
	}
	if ct := (*h).Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}
}

// TestDeliverCallback_Signed verifies the HMAC-SHA256 signature and timestamp
// headers are applied and verifiable with the shared key.
func TestDeliverCallback_Signed(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	body := []byte(`{"hello":"world"}`)
	key := []byte("shared-secret")
	r := &AgentRunReconciler{}
	resp, err := r.deliverCallback(context.Background(), srv.URL, body, key)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	sig := (*h).Get("X-Agentorc-Signature")
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("expected sha256= signature, got %q", sig)
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil {
		t.Fatalf("decoding signature hex: %v", err)
	}

	// Recompute the HMAC and compare with the delivered header.
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	want := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		t.Fatal("HMAC signature does not match recomputed value")
	}

	ts := (*h).Get("X-Agentorc-Timestamp")
	if ts == "" {
		t.Fatal("missing X-Agentorc-Timestamp")
	}
	unixTs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		t.Fatalf("timestamp not an integer: %v", err)
	}
	if unixTs <= 0 {
		t.Fatalf("expected positive unix timestamp, got %d", unixTs)
	}
}

// TestResolveCallbackKey covers reading the shared hmac-key from a K8s Secret
// via the fake clientset, including missing-secret and missing-key cases.
func TestResolveCallbackKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cb-secret", Namespace: "tenant-ns"},
		Data:       map[string][]byte{"hmac-key": []byte("top-secret")},
	}
	suite := &AgentRunReconciler{K8s: fake.NewSimpleClientset(secret)} //nolint:staticcheck

	t.Run("returns key when secret and key exist", func(t *testing.T) {
		got := suite.resolveCallbackKey(context.Background(), "tenant-ns", "cb-secret", "run-1")
		if string(got) != "top-secret" {
			t.Fatalf("expected top-secret, got %q", got)
		}
	})

	t.Run("nil when secret missing", func(t *testing.T) {
		got := suite.resolveCallbackKey(context.Background(), "tenant-ns", "does-not-exist", "run-1")
		if got != nil {
			t.Fatalf("expected nil for missing secret, got %q", got)
		}
	})

	t.Run("nil when hmac-key missing in secret", func(t *testing.T) {
		noKey := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cb-nokey", Namespace: "tenant-ns"},
			Data:       map[string][]byte{"other": []byte("x")},
		}
		s := &AgentRunReconciler{K8s: fake.NewSimpleClientset(noKey)} //nolint:staticcheck

		got := s.resolveCallbackKey(context.Background(), "tenant-ns", "cb-nokey", "run-2")
		if got != nil {
			t.Fatalf("expected nil for missing hmac-key, got %q", got)
		}
	})
}

// TestFireCallback_SignedEndToEnd exercises the full fireCallback path against
// the fake clientset (secret read) + a real local HTTP server (delivery),
// verifying the annotation -> HMAC -> X-Agentorc-Signature wiring without
// needing a cluster. fireCallback is async, so we poll for the signature.
func TestFireCallback_SignedEndToEnd(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck

		ObjectMeta: metav1.ObjectMeta{Name: "cb-secret", Namespace: "tenant-ns"},
		Data:       map[string][]byte{"hmac-key": []byte("top-secret")},
	})

	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-1",
			Namespace: "tenant-ns",
			Annotations: map[string]string{
				"agentorc.io/callback-secret": "cb-secret",
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			Callbacks: &agentorcv1alpha1.CallbackConfig{OnComplete: srv.URL},
		},
		Status: agentorcv1alpha1.AgentRunStatus{
			Phase:    agentorcv1alpha1.AgentRunPhaseSucceeded,
			SpendUSD: "0.042000",
		},
	}

	r := &AgentRunReconciler{K8s: clientset}
	r.fireCallback(context.Background(), run)

	// The body the controller signs is the same payload makeCallbackPayload builds.
	bodyWant := makeCallbackPayload(run)
	mac := hmac.New(sha256.New, []byte("top-secret"))
	mac.Write(bodyWant)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sig := (*h).Get("X-Agentorc-Signature"); sig != "" {
			if sig != want {
				t.Fatalf("signature mismatch\ngot:  %s\nwant: %s", sig, want)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for signed callback; headers=%v", http.Header(*h)) //nolint:unconvert

}

// TestFireCallback_UnsignedEndToEnd verifies that without the callback-secret
// annotation the delivery proceeds unsigned (backward compatible).
func TestFireCallback_UnsignedEndToEnd(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-2", Namespace: "tenant-ns"},
		Spec: agentorcv1alpha1.AgentRunSpec{
			Callbacks: &agentorcv1alpha1.CallbackConfig{OnComplete: srv.URL},
		},
		Status: agentorcv1alpha1.AgentRunStatus{
			Phase: agentorcv1alpha1.AgentRunPhaseSucceeded,
		},
	}

	r := &AgentRunReconciler{K8s: fake.NewSimpleClientset()} //nolint:staticcheck

	r.fireCallback(context.Background(), run)

	// Poll briefly for delivery to complete.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ct := (*h).Get("Content-Type"); ct != "" {
			if sig := (*h).Get("X-Agentorc-Signature"); sig != "" {
				t.Fatalf("expected unsigned delivery, got signature %q", sig)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for unsigned callback delivery; headers=%v", http.Header(*h)) //nolint:unconvert

}

// TestFireCallback_NonTerminalPhase verifies fireCallback is a no-op for runs
// that have not reached a terminal phase (covers the default switch branch).
func TestFireCallback_NonTerminalPhase(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck

		ObjectMeta: metav1.ObjectMeta{Name: "cb-secret", Namespace: "tenant-ns"},
		Data:       map[string][]byte{"hmac-key": []byte("top-secret")},
	})
	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-4",
			Namespace: "tenant-ns",
			Annotations: map[string]string{
				"agentorc.io/callback-secret": "cb-secret",
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			Callbacks: &agentorcv1alpha1.CallbackConfig{OnComplete: srv.URL, OnFailed: srv.URL},
		},
		Status: agentorcv1alpha1.AgentRunStatus{Phase: agentorcv1alpha1.AgentRunPhaseRunning},
	}
	r := &AgentRunReconciler{K8s: clientset}
	r.fireCallback(context.Background(), run)

	// Give the (should-not-run) goroutine a moment; assert nothing delivered.
	time.Sleep(200 * time.Millisecond)
	if sig := (*h).Get("X-Agentorc-Signature"); sig != "" {
		t.Fatalf("non-terminal run should not trigger a callback, got signature %q", sig)
	}
}

// TestFireCallback_NoCallbacksAndEmptyURL covers the remaining early-return
// branches (nil Callbacks, and a terminal phase with an empty callback URL).
func TestFireCallback_NoCallbacksAndEmptyURL(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	r := &AgentRunReconciler{K8s: fake.NewSimpleClientset()} //nolint:staticcheck

	// nil Callbacks -> immediate return, no delivery.
	r.fireCallback(context.Background(), &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-nocb", Namespace: "tenant-ns"},
		Status:     agentorcv1alpha1.AgentRunStatus{Phase: agentorcv1alpha1.AgentRunPhaseSucceeded},
	})

	// Succeeded but OnComplete empty -> return before delivery.
	r.fireCallback(context.Background(), &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-emptyurl", Namespace: "tenant-ns"},
		Spec:       agentorcv1alpha1.AgentRunSpec{Callbacks: &agentorcv1alpha1.CallbackConfig{}},
		Status:     agentorcv1alpha1.AgentRunStatus{Phase: agentorcv1alpha1.AgentRunPhaseSucceeded},
	})

	time.Sleep(200 * time.Millisecond)
	if (*h).Get("Content-Type") != "" {
		t.Fatalf("expected no delivery for nil/empty callback config; got headers=%v", http.Header(*h)) //nolint:unconvert

	}
}

// TestFireCallback_FailedPhase uses OnFailed (the Failure branch of the phase
// switch) to confirm failure callbacks are delivered.
func TestFireCallback_FailedPhase(t *testing.T) {
	srv, h := capturedHeaders(t)
	defer srv.Close()

	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-3",
			Namespace: "tenant-ns",
			Annotations: map[string]string{
				"agentorc.io/callback-secret": "cb-secret",
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			Callbacks: &agentorcv1alpha1.CallbackConfig{OnFailed: srv.URL},
		},
		Status: agentorcv1alpha1.AgentRunStatus{
			Phase:         agentorcv1alpha1.AgentRunPhaseFailed,
			FailureReason: "boom",
		},
	}
	clientset := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck

		ObjectMeta: metav1.ObjectMeta{Name: "cb-secret", Namespace: "tenant-ns"},
		Data:       map[string][]byte{"hmac-key": []byte("top-secret")},
	})
	r := &AgentRunReconciler{K8s: clientset}
	r.fireCallback(context.Background(), run)

	bodyWant := makeCallbackPayload(run)
	mac := hmac.New(sha256.New, []byte("top-secret"))
	mac.Write(bodyWant)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sig := (*h).Get("X-Agentorc-Signature"); sig != "" {
			if sig != want {
				t.Fatalf("signature mismatch\ngot:  %s\nwant: %s", sig, want)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for failed-phase callback; headers=%v", http.Header(*h)) //nolint:unconvert

}

// makeCallbackPayload reconstructs the exact JSON body fireCallback sends, so a
// test can verify the HMAC over that body without inspecting the server.
func makeCallbackPayload(run *agentorcv1alpha1.AgentRun) []byte {
	payload := map[string]any{
		"taskId":    run.Name,
		"agent":     run.Spec.AgentRef,
		"phase":     string(run.Status.Phase),
		"output":    run.Status.Output,
		"spendUSD":  run.Status.SpendUSD,
		"namespace": run.Namespace,
	}
	if run.Status.FailureReason != "" {
		payload["failureReason"] = run.Status.FailureReason
	}
	if run.Status.CompletionTime != nil {
		payload["completedAt"] = run.Status.CompletionTime.Format(time.RFC3339)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return b
}
