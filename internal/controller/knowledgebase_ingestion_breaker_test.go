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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func newBreakerTestScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func failedIngestJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Status: batchv1.JobStatus{
			Failed:    1,
			Active:    0,
			Succeeded: 0,
		},
	}
}

func succeededIngestJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Status: batchv1.JobStatus{
			Succeeded: 1,
			Failed:    0,
		},
	}
}

// TestHandleExistingJobFailureRecordsPerSourceBreaker pins the core
// runaway-loop protection: a failed Job records a breaker entry keyed by that
// Job's name, with a future NextAttempt.
func TestHandleExistingJobFailureRecordsPerSourceBreaker(t *testing.T) {
	scheme := newBreakerTestScheme(t)
	kb := &agentorcav1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "test"},
	}
	job := failedIngestJob("kb-src-demo")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kb, job).Build()
	r := &KnowledgeBaseReconciler{Client: cl, Scheme: scheme}

	// A failed Job reports done=false and a logged error — expected here.
	done, wait, _ := r.handleExistingJob(context.Background(), kb, job)
	if done {
		t.Error("failed Job must not report done")
	}
	if wait <= 0 || wait > time.Hour {
		t.Errorf("backoff wait = %v, want (0, 1h]", wait)
	}

	breaker, ok := kb.Status.IngestionBreakers["kb-src-demo"]
	if !ok {
		t.Fatalf("no breaker recorded for kb-src-demo")
	}
	if breaker.Failures != 1 {
		t.Errorf("Failures = %d, want 1", breaker.Failures)
	}
	if breaker.NextAttempt == nil || !breaker.NextAttempt.Time.After(time.Now()) {
		t.Error("NextAttempt must be in the future")
	}
	if breaker.Reason == "" {
		t.Error("Reason must explain the pause")
	}
}

// TestHandleExistingJobSiblingSuccessDoesNotClearBreaker pins the exact
// runaway-loop scenario: a static source's success must NOT cancel a failed
// MCP source's backoff pause.
func TestHandleExistingJobSiblingSuccessDoesNotClearBreaker(t *testing.T) {
	scheme := newBreakerTestScheme(t)
	future := metav1.NewTime(time.Now().Add(time.Hour))
	kb := &agentorcav1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "test"},
		Status: agentorcav1alpha1.KnowledgeBaseStatus{
			IngestionBreakers: map[string]agentorcav1alpha1.IngestionBreakerState{
				"kb-mcp-demo-0": {
					Failures:    2,
					NextAttempt: future.DeepCopy(),
					Reason:      "ingestion job failed",
				},
			},
		},
	}
	staticJob := succeededIngestJob("kb-src-demo")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kb, staticJob).Build()
	r := &KnowledgeBaseReconciler{Client: cl, Scheme: scheme}

	done, wait, err := r.handleExistingJob(context.Background(), kb, staticJob)
	if err != nil {
		t.Fatalf("handleExistingJob: %v", err)
	}
	if !done || wait != 0 {
		t.Errorf("succeeded Job: done=%v wait=%v, want true/0", done, wait)
	}

	// The failed MCP source's pause must survive the sibling's success.
	mcpBreaker, ok := kb.Status.IngestionBreakers["kb-mcp-demo-0"]
	if !ok {
		t.Fatal("sibling success cleared the MCP source's breaker — runaway loop would restart")
	}
	if mcpBreaker.Failures != 2 || !breakersNextAttempt(mcpBreaker, time.Now()) {
		t.Errorf("MCP breaker mutated: %+v", mcpBreaker)
	}
}

// TestHandleExistingJobConcurrentFailuresCountIndependently verifies two
// sources failing in the same reconcile each record one failure — no
// cross-source amplification of the backoff escalation.
func TestHandleExistingJobConcurrentFailuresCountIndependently(t *testing.T) {
	scheme := newBreakerTestScheme(t)
	kb := &agentorcav1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "test"},
	}
	jobA := failedIngestJob("kb-mcp-demo-0")
	jobB := failedIngestJob("kb-src-demo")
	objects := []client.Object{kb, jobA, jobB}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &KnowledgeBaseReconciler{Client: cl, Scheme: scheme}
	ctx := context.Background()

	// Failed Jobs return errors for the caller to log — expected for both.
	_, _, _ = r.handleExistingJob(ctx, kb, jobA)
	_, _, _ = r.handleExistingJob(ctx, kb, jobB)

	if got := kb.Status.IngestionBreakers["kb-mcp-demo-0"].Failures; got != 1 {
		t.Errorf("kb-mcp-demo-0 Failures = %d, want 1", got)
	}
	if got := kb.Status.IngestionBreakers["kb-src-demo"].Failures; got != 1 {
		t.Errorf("kb-src-demo Failures = %d, want 1", got)
	}
}

// TestHandleExistingJobEscalatesOnRepeatFailures verifies the same source
// failing twice escalates its own counter to 2 (→ 4m backoff).
func TestHandleExistingJobEscalatesOnRepeatFailures(t *testing.T) {
	scheme := newBreakerTestScheme(t)
	kb := &agentorcav1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "test"},
	}
	job := failedIngestJob("kb-src-demo")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kb, job).Build()
	r := &KnowledgeBaseReconciler{Client: cl, Scheme: scheme}
	ctx := context.Background()

	// A failed Job returns an error for the caller to log — expected here.
	if _, first, _ := r.handleExistingJob(ctx, kb, job); first != time.Minute {
		t.Errorf("first backoff = %v, want 1m", first)
	}
	if _, second, _ := r.handleExistingJob(ctx, kb, job); second != 2*time.Minute {
		t.Errorf("second backoff = %v, want 2m", second)
	}
	if got := kb.Status.IngestionBreakers["kb-src-demo"].Failures; got != 2 {
		t.Errorf("Failures = %d, want 2", got)
	}
}

// TestRunIngestionHoldsRecreateUntilBackoffElapses is the end-to-end guard:
// while a source's breaker pause is active, runIngestion must NOT create its
// Job and must surface the remaining wait for requeueing.
func TestRunIngestionHoldsRecreateUntilBackoffElapses(t *testing.T) {
	scheme := newBreakerTestScheme(t)
	pausedJob := sourcesIngestJobName("demo") // 63+ chars? short enough here
	future := metav1.NewTime(time.Now().Add(10 * time.Minute))
	kb := &agentorcav1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "test"},
		Spec: agentorcav1alpha1.KnowledgeBaseSpec{
			Ingestion: &agentorcav1alpha1.IngestionConfig{
				// Static source so the static-source Job is the one gated.
				URLs: []string{"http://example.com/Be On Time.pdf"},
			},
		},
		Status: agentorcav1alpha1.KnowledgeBaseStatus{
			EmbeddingModelProvider: "google-embedding",
			LastSyncTime:           nil,
			IngestionBreakers: map[string]agentorcav1alpha1.IngestionBreakerState{
				pausedJob: {
					Failures:    1,
					NextAttempt: future.DeepCopy(),
					Reason:      "ingestion job failed",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kb).Build()
	r := &KnowledgeBaseReconciler{Client: cl, Scheme: scheme}

	var jobs batchv1.JobList
	if err := cl.List(context.Background(), &jobs, client.InNamespace("test")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("precondition: no Jobs should exist, got %d", len(jobs.Items))
	}

	wait, err := r.runIngestion(context.Background(), kb)
	if err != nil {
		t.Fatalf("runIngestion: %v", err)
	}
	if wait <= 0 || wait > 10*time.Minute {
		t.Errorf("wait = %v, want in (0, 10m]", wait)
	}

	if err := cl.List(context.Background(), &jobs, client.InNamespace("test")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("recreate not held: %d Jobs created while breaker pause active", len(jobs.Items))
	}
	// A paused run must not masquerade as a completed sync.
	if kb.Status.LastSyncTime != nil {
		t.Error("LastSyncTime stamped for a paused run")
	}
}

// breakersNextAttempt reports whether the breaker's NextAttempt is after now.
func breakersNextAttempt(b agentorcav1alpha1.IngestionBreakerState, now time.Time) bool {
	return b.NextAttempt != nil && b.NextAttempt.Time.After(now)
}

// keep corev1 imported for the Job fixture helpers above (statuses).
var _ = corev1.Pod{}
