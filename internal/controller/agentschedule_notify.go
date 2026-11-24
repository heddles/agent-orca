package controller

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/heddles/agent-orca/internal/runoutput"
	"github.com/heddles/agent-orca/internal/state"
	"github.com/heddles/agent-orca/internal/tenancy"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/notify"
)

// Notifier delivers a notification. *notify.Sender is one.
type Notifier interface {
	Send(ctx context.Context, ch *notify.Channel, msg notify.Message) error
}

// notifyJob is a notification to send once the schedule's status is stored.
type notifyJob struct {
	event string
	msg   notify.Message
}

// notifyOn is the events of spec.result.notify; Failed when none is listed.
func notifyOn(n *agentorcav1alpha1.ScheduleNotify) []string {
	if len(n.On) == 0 {
		return []string{agentorcav1alpha1.ScheduleNotifyFailed}
	}
	return n.On
}

func scheduleNotify(s *agentorcav1alpha1.AgentSchedule) *agentorcav1alpha1.ScheduleNotify {
	if s.Spec.Result == nil {
		return nil
	}
	return s.Spec.Result.Notify
}

// resolveChannel finds the schedule's channel in the tenant's TenantConfig and
// reads its Secret. The error is safe to show: it names keys, never values.
func (r *AgentScheduleReconciler) resolveChannel(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig) (*notify.Channel, string, error) {
	n := scheduleNotify(s)
	if tenant == nil {
		return nil, "NoTenant", fmt.Errorf("no TenantConfig owns this schedule, so there are no notification channels")
	}
	var def *agentorcav1alpha1.NotificationChannel
	if tenant.Spec.Notifications != nil {
		for i := range tenant.Spec.Notifications.Channels {
			if tenant.Spec.Notifications.Channels[i].Name == n.Channel {
				def = &tenant.Spec.Notifications.Channels[i]
			}
		}
	}
	if def == nil {
		return nil, "ChannelNotFound", fmt.Errorf("the tenant has no notification channel %q: an administrator adds it to the TenantConfig", n.Channel)
	}
	ns := def.SecretRef.Namespace
	if ns == "" {
		ns = tenant.Namespace
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: def.SecretRef.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "SecretNotFound", fmt.Errorf("the Secret of channel %q was not found", n.Channel)
		}
		return nil, "SecretUnreadable", fmt.Errorf("the Secret of channel %q could not be read", n.Channel)
	}
	ch, err := notify.FromSecret(def.Name, def.Type, def.Insecure, def.AllowPrivateAddresses, sec.Data)
	if err != nil {
		return nil, "SecretInvalid", fmt.Errorf("channel %q: %w", n.Channel, err)
	}
	return ch, "", nil
}

// setNotifyCondition keeps the NotifyReady condition: present while
// spec.result.notify is set, true when the channel resolves. It never blocks a
// run, and the message never holds a Secret value.
func (r *AgentScheduleReconciler) setNotifyCondition(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig) {
	if scheduleNotify(s) == nil {
		meta.RemoveStatusCondition(&s.Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
		return
	}
	_, reason, err := r.resolveChannel(ctx, s, tenant)
	c := metav1.Condition{
		Type: agentorcav1alpha1.ScheduleConditionNotifyReady, Status: metav1.ConditionTrue, Reason: "ChannelResolved", ObservedGeneration: s.Generation,
	}
	if err != nil {
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, reason, err.Error()
	}
	prev := meta.FindStatusCondition(s.Status.Conditions, c.Type)
	meta.SetStatusCondition(&s.Status.Conditions, c)
	if err != nil && (prev == nil || prev.Status != metav1.ConditionFalse) && r.Recorder != nil {
		r.Recorder.Event(s, corev1.EventTypeWarning, "NotifyChannelUnavailable", err.Error())
	}
}

// runNotice builds the message for a finished run, or false when the run is not
// announced: a run the Replace policy cancelled, an outcome the schedule did not
// ask for, or a run that is not a plain AgentRun.
func (r *AgentScheduleReconciler) runNotice(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, c scheduleChild) (notifyJob, bool) {
	n := scheduleNotify(s)
	run, ok := c.obj.(*agentorcav1alpha1.AgentRun)
	if n == nil || !ok || c.replaced {
		return notifyJob{}, false
	}
	event := agentorcav1alpha1.ScheduleNotifyFailed
	if c.state == childSucceeded {
		event = agentorcav1alpha1.ScheduleNotifySucceeded
	}
	if !slices.Contains(notifyOn(n), event) {
		return notifyJob{}, false
	}
	msg := notify.Message{
		Schedule: s.Name, Namespace: s.Namespace, Agent: run.Spec.AgentRef, Outcome: event, Run: run.Name,
		CostUSD: run.Status.SpendUSD, Link: r.runLink(run.Namespace, run.Name), Time: r.now(),
	}
	if run.Status.StartTime != nil && run.Status.CompletionTime != nil {
		msg.Duration = run.Status.CompletionTime.Sub(run.Status.StartTime.Time)
	}
	if event == agentorcav1alpha1.ScheduleNotifyFailed {
		msg.Reason = run.Status.FailureReason
	}
	if n.IncludeOutput {
		msg.Output = run.Status.Output
		if r.StateStore != nil {
			msg.Output = runoutput.Of(ctx, state.ForTenantIfEnabled(r.StateStore, run.Labels[tenancy.LabelTenantID]), run)
		}
	}
	return notifyJob{event: event, msg: msg}, true
}

// scheduleNotices are the schedule-level events a reconcile caused: the spend
// cap stopping ticks, and the failure breaker opening. orig is the status before
// the reconcile; each is sent only on the change, so it is once, not per tick.
func (r *AgentScheduleReconciler) scheduleNotices(s *agentorcav1alpha1.AgentSchedule, orig *agentorcav1alpha1.AgentScheduleStatus) []notifyJob {
	n := scheduleNotify(s)
	if n == nil {
		return nil
	}
	on := notifyOn(n)
	rose := func(cond string) *metav1.Condition {
		before := meta.IsStatusConditionTrue(orig.Conditions, cond)
		if c := meta.FindStatusCondition(s.Status.Conditions, cond); c != nil && c.Status == metav1.ConditionTrue && !before {
			return c
		}
		return nil
	}
	var out []notifyJob
	add := func(event string, c *metav1.Condition) {
		if c != nil && slices.Contains(on, event) {
			out = append(out, notifyJob{event: event, msg: notify.Message{
				Schedule: s.Name, Namespace: s.Namespace, Agent: s.Spec.AgentRef, Outcome: event,
				Reason: c.Message, Link: r.scheduleLink(), Time: r.now(),
			}})
		}
	}
	add(agentorcav1alpha1.ScheduleNotifySkipped, rose(agentorcav1alpha1.ScheduleConditionBudgetExhausted))
	add(agentorcav1alpha1.ScheduleNotifyBreakerOpen, rose(agentorcav1alpha1.ScheduleConditionDegraded))
	return out
}

func (r *AgentScheduleReconciler) uiBase() string { return strings.TrimRight(r.UIURL, "/") }

// runLink is the run's page in the UI, empty when the UI's address is unknown.
func (r *AgentScheduleReconciler) runLink(ns, name string) string {
	if r.uiBase() == "" {
		return ""
	}
	return r.uiBase() + "/runs/live/" + url.PathEscape(ns) + "/" + url.PathEscape(name)
}

func (r *AgentScheduleReconciler) scheduleLink() string {
	if r.uiBase() == "" {
		return ""
	}
	return r.uiBase() + "/operate/schedules"
}

// dispatch sends the notifications in the background: a slow receiver, with its
// retries, must not hold up the schedule's own work. The result is an event on
// the schedule; a failure never touches a run.
func (r *AgentScheduleReconciler) dispatch(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig, jobs []notifyJob) {
	if len(jobs) == 0 || r.Notifier == nil {
		return
	}
	log := logf.FromContext(ctx)
	ch, _, err := r.resolveChannel(ctx, s, tenant)
	if err != nil {
		log.Info("not sending a notification", "schedule", s.Name, "reason", err.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(s, corev1.EventTypeWarning, "NotifyFailed", "not sent: %v", err)
		}
		return
	}
	sc := s.DeepCopy()
	bg := context.WithoutCancel(ctx)
	r.notifyWG.Go(func() {
		for _, j := range jobs {
			if err := r.Notifier.Send(bg, ch, j.msg); err != nil {
				// The error is already free of Secret values.
				log.Info("notification failed", "schedule", sc.Name, "event", j.event, "run", j.msg.Run, "error", err.Error())
				if r.Recorder != nil {
					r.Recorder.Eventf(sc, corev1.EventTypeWarning, "NotifyFailed", "%s notification for %s: %v", j.event, runOrSchedule(j.msg.Run), err)
				}
			}
		}
	})
}

func runOrSchedule(s string) string {
	if s == "" {
		return "the schedule"
	}
	return s
}

// WaitNotifications blocks until the notifications in flight are done. Tests use
// it; the operator does not need to.
func (r *AgentScheduleReconciler) WaitNotifications() { r.notifyWG.Wait() }
