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
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// RunArchiveStore is the PostgreSQL archive of finished runs.
type RunArchiveStore interface {
	ArchiveRun(ctx context.Context, run *agentorcav1alpha1.AgentRun, traceEventsJSON []byte) error
	GetRun(ctx context.Context, namespace, name string) (*postgresql.RunArchive, error)
}

// Run archival (docs/run-archival.md).
//
// A finished run's answer and trace live in the state store (Redis) until the
// run is archived to PostgreSQL. The terminal reconcile is the one archiver:
// it archives synchronously, retries with backoff while PostgreSQL is
// unavailable, and records the result as the Archived condition. A run is not
// deleted before it is archived, within a grace period.
const (
	// conditionArchived is True once the run is in PostgreSQL.
	conditionArchived = "Archived"
	reasonArchived    = "Archived"

	// AnnotationSkipArchive, set to "true", keeps a run out of the archive and
	// out of the deletion wait.
	AnnotationSkipArchive = "agentorca.io/skip-archive"

	// DefaultArchiveDeleteGrace is how long a deleted run waits for the archive
	// before it is deleted without one.
	DefaultArchiveDeleteGrace = 24 * time.Hour

	// archiveTimeout bounds one archive attempt.
	archiveTimeout = 30 * time.Second
	// archiveDeleteRetry is the wait between attempts while a deletion waits.
	archiveDeleteRetry = time.Minute
	// pendingAfter is how long a finished run may go unarchived before it counts
	// as pending (and the alert fires).
	pendingAfter = 10 * time.Minute

	// outputGone is the archived answer of a run whose stored answer expired
	// before the run was archived.
	outputGone = "[No answer is stored for this run: it was not in the state store when the run was archived.]"
)

var (
	archiveResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_run_archive_total",
		Help: "Archive attempts of finished runs, by result: archived, failed (will retry), abandoned (deleted unarchived after the grace period), skipped (opted out).",
	}, []string{"result"})
	archiveSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agentorca_run_archive_seconds",
		Help:    "Time to archive one run to PostgreSQL.",
		Buckets: prometheus.DefBuckets,
	})
	archiveOutputGone = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agentorca_run_archive_output_missing_total",
		Help: "Succeeded runs archived with no stored answer (it had expired from the state store).",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(archiveResults, archiveSeconds, archiveOutputGone)
}

// registerArchiveGauges adds the gauges that count runs waiting for the archive.
// They read the manager's cache, so a scrape costs no API call.
func (r *AgentRunReconciler) registerArchiveGauges() {
	pending := func() (n float64, oldest float64) {
		var list agentorcav1alpha1.AgentRunList
		if err := r.List(context.Background(), &list); err != nil {
			return 0, 0
		}
		now := time.Now()
		for i := range list.Items {
			run := &list.Items[i]
			if !r.archivable(run) || archived(run) || run.Status.CompletionTime == nil {
				continue
			}
			if age := now.Sub(run.Status.CompletionTime.Time); age > pendingAfter {
				n++
				oldest = max(oldest, age.Seconds())
			}
		}
		return n, oldest
	}
	for _, g := range []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "agentorca_run_archive_pending",
			Help: "Finished runs not archived to PostgreSQL ten minutes after they finished.",
		}, func() float64 { n, _ := pending(); return n }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "agentorca_run_archive_oldest_pending_seconds",
			Help: "Age of the oldest finished run not yet archived (counted once past ten minutes).",
		}, func() float64 { _, o := pending(); return o }),
	} {
		_ = ctrlmetrics.Registry.Register(g)
	}
}

func archived(run *agentorcav1alpha1.AgentRun) bool {
	return meta.IsStatusConditionTrue(run.Status.Conditions, conditionArchived)
}

// archivable reports whether run belongs in the archive: archiving is on, the
// run is finished, and it did not opt out.
func (r *AgentRunReconciler) archivable(run *agentorcav1alpha1.AgentRun) bool {
	return r.PostgresStore != nil && postgresql.IsTerminalPhase(run.Status.Phase) &&
		run.Annotations[AnnotationSkipArchive] != annotationTrue
}

func (r *AgentRunReconciler) archiveGrace() time.Duration {
	if r.ArchiveDeleteGrace > 0 {
		return r.ArchiveDeleteGrace
	}
	return DefaultArchiveDeleteGrace
}

// ensureArchived archives a finished run once and records it. It returns an
// error while the archive keeps failing, so controller-runtime retries it with
// exponential backoff; nothing is written to the run until it succeeds.
func (r *AgentRunReconciler) ensureArchived(ctx context.Context, run *agentorcav1alpha1.AgentRun) (ctrl.Result, error) {
	if !r.archivable(run) || archived(run) {
		return ctrl.Result{}, nil
	}
	// One archive row carries the final spend, so wait for it to settle (bounded
	// by the cost-capture retry window) instead of writing the row twice.
	if !r.spendSettled(run) {
		return ctrl.Result{RequeueAfter: costCaptureRetryInterval}, nil
	}
	if err := r.archiveNow(ctx, run); err != nil {
		archiveResults.WithLabelValues("failed").Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "ArchiveFailed",
				"archiving the run to PostgreSQL failed, retrying: %v", err)
		}
		return ctrl.Result{}, fmt.Errorf("archiving run %s/%s: %w", run.Namespace, run.Name, err)
	}
	archiveResults.WithLabelValues("archived").Inc()
	return ctrl.Result{}, r.markArchived(ctx, run)
}

// spendSettled is true once the run's spend is stamped, or the window to stamp
// it has passed, or the run has no spend to stamp (a handed-off run).
func (r *AgentRunReconciler) spendSettled(run *agentorcav1alpha1.AgentRun) bool {
	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded && run.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		return true
	}
	if spendStamped(run) {
		return true
	}
	c := run.Status.CompletionTime
	return c == nil || time.Since(c.Time) >= costCaptureRetryWindow
}

// archiveNow writes the run, its answer and its trace to PostgreSQL.
func (r *AgentRunReconciler) archiveNow(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	traceJSON := r.loadTraceEventsForArchive(ctx, run)
	if err := r.PostgresStore.ArchiveRun(ctx, r.archiveCopy(ctx, run), traceJSON); err != nil {
		return err
	}
	archiveSeconds.Observe(time.Since(start).Seconds())
	return nil
}

// archiveCopy is a copy of run carrying its answer in status.output, which is
// what the archive reads. The live object is not touched, so the answer never
// reaches etcd through it. A succeeded run with no stored answer anywhere is
// archived with a note saying so, unless the archive already has one.
func (r *AgentRunReconciler) archiveCopy(ctx context.Context, run *agentorcav1alpha1.AgentRun) *agentorcav1alpha1.AgentRun {
	out := r.outputOf(ctx, run)
	if out == "" && run.Status.Phase == agentorcav1alpha1.AgentRunPhaseSucceeded && !r.archiveHasOutput(ctx, run) {
		out = outputGone
		archiveOutputGone.Inc()
		log.FromContext(ctx).Info("archiving a run whose answer is no longer stored", "run", run.Name, "ns", run.Namespace)
	}
	if out == run.Status.Output {
		return run
	}
	cp := run.DeepCopy()
	cp.Status.Output = out
	return cp
}

// archiveHasOutput reports whether the archive already holds this run's answer.
// An error counts as yes: the archive keeps what it has, and a note must not
// overwrite a real answer.
func (r *AgentRunReconciler) archiveHasOutput(ctx context.Context, run *agentorcav1alpha1.AgentRun) bool {
	scope := postgresql.WithPlatform(ctx)
	if tid := run.Labels[tenancy.LabelTenantID]; tid != "" {
		scope = postgresql.WithTenant(ctx, tid)
	}
	row, err := r.PostgresStore.GetRun(scope, run.Namespace, run.Name)
	if err != nil {
		return true
	}
	return row != nil && row.Output != ""
}

// markArchived records the archive on the run. It uses an optimistic lock so a
// concurrent status write is never overwritten with a stale conditions list.
func (r *AgentRunReconciler) markArchived(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	base := run.DeepCopy()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: conditionArchived, Status: metav1.ConditionTrue, Reason: reasonArchived,
		Message: "the run, its answer and its trace are in PostgreSQL", ObservedGeneration: run.Generation,
	})
	if err := r.Status().Patch(ctx, run, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("recording the archive: %w", err)
	}
	return nil
}

// archiveBeforeDelete holds a finished run's deletion until it is archived. It
// returns true when the deletion may go on: the run is archived, needs no
// archive, or has waited out the grace period. Otherwise it returns the delay
// before the next attempt.
func (r *AgentRunReconciler) archiveBeforeDelete(ctx context.Context, run *agentorcav1alpha1.AgentRun) (bool, time.Duration) {
	if !r.archivable(run) {
		if r.PostgresStore != nil && postgresql.IsTerminalPhase(run.Status.Phase) {
			archiveResults.WithLabelValues("skipped").Inc()
		}
		return true, 0
	}
	if archived(run) {
		return true, 0
	}
	err := r.archiveNow(ctx, run)
	if err == nil {
		archiveResults.WithLabelValues("archived").Inc()
		return true, 0
	}
	archiveResults.WithLabelValues("failed").Inc()
	waited := time.Duration(0)
	if run.DeletionTimestamp != nil {
		waited = time.Since(run.DeletionTimestamp.Time)
	}
	if waited >= r.archiveGrace() {
		archiveResults.WithLabelValues("abandoned").Inc()
		log.FromContext(ctx).Error(err, "deleting a run that could not be archived", "run", run.Name, "ns", run.Namespace, "waited", waited.String())
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "ArchiveAbandoned",
				"deleted without an archive after waiting %s: %v", r.archiveGrace(), err)
		}
		return true, 0
	}
	log.FromContext(ctx).Info("holding a run's deletion until it is archived", "run", run.Name, "ns", run.Namespace, "err", err.Error(), "waited", waited.String())
	if r.Recorder != nil && waited < archiveDeleteRetry {
		r.Recorder.Eventf(run, corev1.EventTypeWarning, "ArchiveBlocksDeletion",
			"waiting up to %s for the run to be archived: %v", r.archiveGrace(), err)
	}
	return false, archiveDeleteRetry
}
