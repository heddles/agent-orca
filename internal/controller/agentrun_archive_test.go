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
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/runoutput"
)

// fakeArchive is the PostgreSQL archive, with a switch for an outage.
type fakeArchive struct {
	down     bool
	archived []*agentorcav1alpha1.AgentRun
	existing *postgresql.RunArchive
}

func (f *fakeArchive) ArchiveRun(_ context.Context, run *agentorcav1alpha1.AgentRun, _ []byte) error {
	if f.down {
		return errors.New("connection refused")
	}
	f.archived = append(f.archived, run)
	return nil
}

func (f *fakeArchive) GetRun(context.Context, string, string) (*postgresql.RunArchive, error) {
	return f.existing, nil
}

func archiveFixture(t *testing.T, mutate func(*agentorcav1alpha1.AgentRun)) (*AgentRunReconciler, *agentorcav1alpha1.AgentRun, *fakeArchive, map[string][]byte) {
	t.Helper()
	scheme := warmReuseScheme(t)
	done := metav1.NewTime(time.Now().Add(-time.Hour))
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default", Finalizers: []string{agentRunFinalizer}},
		Status: agentorcav1alpha1.AgentRunStatus{
			Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, CompletionTime: &done, SpendUSD: "0.01",
		},
	}
	if mutate != nil {
		mutate(run)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&agentorcav1alpha1.AgentRun{}).WithObjects(run).Build()
	kv := map[string][]byte{}
	fa := &fakeArchive{}
	r := &AgentRunReconciler{Client: cl, Scheme: scheme, StateStore: testStore{kv: kv}, PostgresStore: fa}
	return r, run, fa, kv
}

func reload(t *testing.T, r *AgentRunReconciler, run *agentorcav1alpha1.AgentRun) *agentorcav1alpha1.AgentRun {
	t.Helper()
	var got agentorcav1alpha1.AgentRun
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestEnsureArchivedRetriesUntilTheDatabaseIsBack(t *testing.T) {
	r, run, fa, kv := archiveFixture(t, nil)
	kv[runoutput.Scope+"/default/r1"] = []byte("the answer")
	ctx := context.Background()

	fa.down = true
	if _, err := r.ensureArchived(ctx, run); err == nil {
		t.Fatal("an archive that failed must return an error so the reconcile is retried")
	}
	if archived(reload(t, r, run)) {
		t.Fatal("a failed archive must not mark the run archived")
	}

	fa.down = false
	if _, err := r.ensureArchived(ctx, reload(t, r, run)); err != nil {
		t.Fatal(err)
	}
	got := reload(t, r, run)
	if !archived(got) {
		t.Fatal("the run was not marked archived after a successful archive")
	}
	if len(fa.archived) != 1 || fa.archived[0].Status.Output != "the answer" {
		t.Fatalf("archived %d times; output %q, want the stored answer", len(fa.archived), fa.archived[0].Status.Output)
	}
	if got.Status.Output != "" {
		t.Fatalf("the answer reached etcd through status.output: %q", got.Status.Output)
	}

	// Archived once is archived: later reconciles do not write again.
	if _, err := r.ensureArchived(ctx, got); err != nil || len(fa.archived) != 1 {
		t.Fatalf("re-archived a run already marked archived: %d writes, %v", len(fa.archived), err)
	}
}

func TestEnsureArchivedWaitsForTheSpendToSettle(t *testing.T) {
	r, run, fa, _ := archiveFixture(t, func(run *agentorcav1alpha1.AgentRun) {
		now := metav1.Now()
		run.Status.CompletionTime = &now
		run.Status.SpendUSD = ""
	})
	res, err := r.ensureArchived(context.Background(), run)
	if err != nil || res.RequeueAfter == 0 || len(fa.archived) != 0 {
		t.Fatalf("res=%+v err=%v writes=%d: want a requeue and no archive until spend is stamped", res, err, len(fa.archived))
	}

	// Past the cost-capture window the run is archived whatever the spend.
	old := metav1.NewTime(time.Now().Add(-2 * costCaptureRetryWindow))
	run.Status.CompletionTime = &old
	if _, err := r.ensureArchived(context.Background(), run); err != nil || len(fa.archived) != 1 {
		t.Fatalf("err=%v writes=%d: want the run archived once the window passed", err, len(fa.archived))
	}
}

func TestEnsureArchivedIgnoresRunsThatNeedNoArchive(t *testing.T) {
	r, run, fa, _ := archiveFixture(t, func(run *agentorcav1alpha1.AgentRun) {
		run.Annotations = map[string]string{AnnotationSkipArchive: "true"}
	})
	if _, err := r.ensureArchived(context.Background(), run); err != nil || len(fa.archived) != 0 {
		t.Fatalf("opted-out run archived: %d, %v", len(fa.archived), err)
	}
	r.PostgresStore = nil
	run.Annotations = nil
	if _, err := r.ensureArchived(context.Background(), run); err != nil {
		t.Fatalf("no database: %v", err)
	}
}

func TestArchiveBeforeDeleteHoldsThenReleases(t *testing.T) {
	ctx := context.Background()
	r, run, fa, _ := archiveFixture(t, nil)
	r.ArchiveDeleteGrace = 24 * time.Hour
	fa.down = true

	just := metav1.NewTime(time.Now().Add(-time.Minute))
	run.DeletionTimestamp = &just
	if ok, retry := r.archiveBeforeDelete(ctx, run); ok || retry == 0 {
		t.Fatalf("deletion went ahead (%v) while the archive was failing inside the grace period", ok)
	}

	// The database comes back inside the grace period: the run is archived, then released.
	fa.down = false
	if ok, _ := r.archiveBeforeDelete(ctx, run); !ok || len(fa.archived) != 1 {
		t.Fatalf("ok=%v writes=%d: want the run archived and released", ok, len(fa.archived))
	}

	// The database never comes back: after the grace period the deletion goes ahead.
	r2, run2, fa2, _ := archiveFixture(t, nil)
	r2.ArchiveDeleteGrace = 24 * time.Hour
	fa2.down = true
	late := metav1.NewTime(time.Now().Add(-25 * time.Hour))
	run2.DeletionTimestamp = &late
	if ok, _ := r2.archiveBeforeDelete(ctx, run2); !ok {
		t.Fatal("deletion still held after the grace period")
	}
}

func TestArchiveBeforeDeleteSkipsWhatNeedsNoArchive(t *testing.T) {
	ctx := context.Background()
	r, run, fa, _ := archiveFixture(t, func(run *agentorcav1alpha1.AgentRun) {
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{Type: conditionArchived, Status: metav1.ConditionTrue, Reason: reasonArchived})
	})
	fa.down = true
	if ok, _ := r.archiveBeforeDelete(ctx, run); !ok {
		t.Fatal("an archived run's deletion was held")
	}
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseRunning
	run.Status.Conditions = nil
	if ok, _ := r.archiveBeforeDelete(ctx, run); !ok {
		t.Fatal("a run that never finished was held")
	}
}

func TestArchiveCopyNotesAnAnswerThatIsGone(t *testing.T) {
	ctx := context.Background()
	r, run, fa, _ := archiveFixture(t, nil)

	got := r.archiveCopy(ctx, run)
	if !strings.Contains(got.Status.Output, "No answer is stored") {
		t.Fatalf("output %q: want a note that no answer is stored", got.Status.Output)
	}
	if run.Status.Output != "" {
		t.Fatal("the note was written to the live run")
	}

	// The archive already has the real answer: a note must not replace it.
	fa.existing = &postgresql.RunArchive{Output: "the real answer"}
	if got := r.archiveCopy(ctx, run); got.Status.Output != "" {
		t.Fatalf("output %q: want nothing, so the archive keeps its answer", got.Status.Output)
	}

	// A run that failed has no answer to miss.
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	fa.existing = nil
	if got := r.archiveCopy(ctx, run); got.Status.Output != "" {
		t.Fatalf("a failed run got a note: %q", got.Status.Output)
	}
}
