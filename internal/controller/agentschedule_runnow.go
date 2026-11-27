package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// A Run now run is not owned by its schedule, so that it stays out of the
// failure streak, the spend caps and the history limits. It is still reported
// like a timed run: the API marks it with schedule.AnnotationReport.

// requestedReports claims the finished runs that asked to be reported and
// returns the messages to send. Each is claimed before anything is sent, and
// marked as handled even when it is not announced, so a later change to the
// schedule never reports an old run.
func (r *AgentScheduleReconciler) requestedReports(ctx context.Context, s *agentorcav1alpha1.AgentSchedule,
	dest *agentorcav1alpha1.ScheduleNotify, destErr error) ([]notifyJob, error) {
	var runs agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &runs, client.InNamespace(s.Namespace), client.MatchingLabels{schedule.LabelSchedule: s.Name}); err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	var jobs []notifyJob
	for i := range runs.Items {
		run := &runs.Items[i]
		if !wantsReport(s, run) {
			continue
		}
		// The cache can lag behind the claim of the reconcile before this one.
		if r.APIReader != nil {
			var fresh agentorcav1alpha1.AgentRun
			if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), &fresh); err != nil {
				continue // gone, or unreadable: a later reconcile tries again
			}
			if run = &fresh; !wantsReport(s, run) {
				continue
			}
		}
		job, send := r.runNotice(ctx, s, scheduleChild{obj: run, state: runChildState(run)})
		send = send && (destErr != nil || r.canDeliver(dest))
		patch := client.MergeFrom(run.DeepCopy())
		run.Annotations[schedule.AnnotationReport] = schedule.ReportHandled
		if send {
			run.Annotations[schedule.AnnotationNotified] = claimOf(dest, destErr)
		}
		if err := r.Patch(ctx, run, patch); err != nil {
			logf.FromContext(ctx).Error(err, "claiming a run's report", "run", run.Name)
			continue
		}
		if send {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

// wantsReport reports whether run is finished, asks to be reported, and is a
// run of this schedule's own agent and tenant that the schedule does not own
// (an owned run is reported when it is counted).
func wantsReport(s *agentorcav1alpha1.AgentSchedule, run *agentorcav1alpha1.AgentRun) bool {
	return run.Annotations[schedule.AnnotationReport] == schedule.ReportRequested &&
		run.Labels[schedule.LabelSchedule] == s.Name &&
		run.Labels[tenancy.LabelTenantID] == s.Labels[tenancy.LabelTenantID] &&
		run.Spec.AgentRef == s.Spec.AgentRef &&
		!metav1.IsControlledBy(run, s) &&
		runChildState(run) != childActive
}

// claimOf is the value that records where a run's notification was claimed for.
func claimOf(dest *agentorcav1alpha1.ScheduleNotify, destErr error) string {
	if destErr != nil {
		return "unknown"
	}
	return dest.Destination()
}

// scheduleOfRequestedReport wakes the schedule of a run that asks to be
// reported: no owner reference does it.
func scheduleOfRequestedReport(_ context.Context, o client.Object) []reconcile.Request {
	name := o.GetLabels()[schedule.LabelSchedule]
	if name == "" || o.GetAnnotations()[schedule.AnnotationReport] != schedule.ReportRequested {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
}
