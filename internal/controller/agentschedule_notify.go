package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/redact"
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
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
)

// Notifier delivers a notification. *notify.Sender is one.
type Notifier interface {
	Send(ctx context.Context, ch *notify.Channel, msg notify.Message) error
}

// ChatPoster posts through a tenant's shared chat app. *inbound.Pipeline is one.
type ChatPoster interface {
	// CheckApp says whether the app can be used. The error is safe to show.
	CheckApp(ctx context.Context, tenantID, kind, app string) error
	// PostToApp posts to a place the app can write in. render makes the text,
	// masking the secrets it is given. The error is safe to show.
	PostToApp(ctx context.Context, tenantID, kind, app, place string, render func(*redact.Registry) string) error
}

// chatKindSlack is the chat platform of spec.result.notify.slack.
const chatKindSlack = "slack"

// deliverFunc sends one notification to a resolved destination.
type deliverFunc func(context.Context, notify.Message) error

// DestinationStore holds the channels administrators chose in the UI.
// *postgresql.Store is one.
type DestinationStore interface {
	GetScheduleDestination(ctx context.Context, tenantID, namespace, name string) (postgresql.ScheduleDestination, bool, error)
	DeleteScheduleDestinationsOf(ctx context.Context, namespace, name string) error
}

// overrideRefresh is how soon a schedule that may have a saved choice is looked
// at again, so that its NotifyReady condition follows a change in the UI.
const overrideRefresh = time.Minute

// errDestinationUnknown is shown when the saved choice cannot be read.
var errDestinationUnknown = errors.New("the channel chosen in the UI could not be read, so nothing is sent: the destination in Git may no longer be the right one")

// overridable reports whether a channel chosen in the UI can replace this
// schedule's destination: it has one, and it is not edited in the UI itself.
func (r *AgentScheduleReconciler) overridable(s *agentorcav1alpha1.AgentSchedule) bool {
	return r.Destinations != nil && scheduleNotify(s) != nil && tenancy.Detect(s, r.OwnDetect).Owner != tenancy.OwnerUI
}

// destinationOf is the schedule's notify settings with the destination that is
// in force: the one chosen in the UI when there is one, else the spec's. On an
// error the destination is unknown and nothing may be sent; the settings
// returned then say only which events were asked for.
func (r *AgentScheduleReconciler) destinationOf(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig) (*agentorcav1alpha1.ScheduleNotify, error) {
	n := scheduleNotify(s)
	if tenant == nil || !r.overridable(s) {
		return n, nil
	}
	d, found, err := r.Destinations.GetScheduleDestination(ctx, tenancy.TenantIDOf(tenant), s.Namespace, s.Name)
	if err != nil {
		logf.FromContext(ctx).Error(err, "reading the chosen destination", "schedule", s.Name)
		return n, errDestinationUnknown
	}
	if !found {
		return n, nil
	}
	return schedule.WithSlackDestination(n, agentorcav1alpha1.ScheduleSlackTarget{App: d.App, ChannelID: d.ChannelID, ChannelName: d.ChannelName}), nil
}

// canDeliver reports whether this operator can send the schedule's kind of
// notification at all. Without that, a run is not claimed as announced.
func (r *AgentScheduleReconciler) canDeliver(n *agentorcav1alpha1.ScheduleNotify) bool {
	if n != nil && n.Slack != nil {
		return r.Chat != nil
	}
	return r.Notifier != nil
}

// resolveDestination makes the sender for n, the destination in force. The
// error is safe to show.
func (r *AgentScheduleReconciler) resolveDestination(ctx context.Context, n *agentorcav1alpha1.ScheduleNotify, tenant *agentorcav1alpha1.TenantConfig) (deliverFunc, string, error) {
	t := n.Slack
	if t == nil {
		ch, reason, err := r.resolveChannel(ctx, n.Channel, tenant)
		if err != nil {
			return nil, reason, err
		}
		return func(ctx context.Context, m notify.Message) error { return r.Notifier.Send(ctx, ch, m) }, "", nil
	}
	if tenant == nil {
		return nil, "NoTenant", fmt.Errorf("no TenantConfig owns this schedule, so there are no chat apps")
	}
	if r.Chat == nil {
		return nil, "ChatUnavailable", fmt.Errorf("chat apps are not available on this installation: they need the database")
	}
	// Only the schedule's own tenant's apps are looked at.
	tenantID := tenancy.TenantIDOf(tenant)
	if err := r.Chat.CheckApp(ctx, tenantID, chatKindSlack, t.App); err != nil {
		return nil, "ChatAppUnavailable", err
	}
	app, place := t.App, t.ChannelID
	return func(ctx context.Context, m notify.Message) error {
		return r.Chat.PostToApp(ctx, tenantID, chatKindSlack, app, place, func(secrets *redact.Registry) string {
			return m.Scrub(secrets).SlackText()
		})
	}, "", nil
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
func (r *AgentScheduleReconciler) resolveChannel(ctx context.Context, name string, tenant *agentorcav1alpha1.TenantConfig) (*notify.Channel, string, error) {
	if tenant == nil {
		return nil, "NoTenant", fmt.Errorf("no TenantConfig owns this schedule, so there are no notification channels")
	}
	var def *agentorcav1alpha1.NotificationChannel
	if tenant.Spec.Notifications != nil {
		for i := range tenant.Spec.Notifications.Channels {
			if tenant.Spec.Notifications.Channels[i].Name == name {
				def = &tenant.Spec.Notifications.Channels[i]
			}
		}
	}
	if def == nil {
		return nil, "ChannelNotFound", fmt.Errorf("the tenant has no notification channel %q: an administrator adds it to the TenantConfig", name)
	}
	ns := def.SecretRef.Namespace
	if ns == "" {
		ns = tenant.Namespace
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: def.SecretRef.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "SecretNotFound", fmt.Errorf("the Secret of channel %q was not found", name)
		}
		return nil, "SecretUnreadable", fmt.Errorf("the Secret of channel %q could not be read", name)
	}
	ch, err := notify.FromSecret(def.Name, def.Type, def.Insecure, def.AllowPrivateAddresses, sec.Data)
	if err != nil {
		return nil, "SecretInvalid", fmt.Errorf("channel %q: %w", name, err)
	}
	return ch, "", nil
}

// setNotifyCondition keeps the NotifyReady condition: present while
// spec.result.notify is set, true when the channel resolves. It never blocks a
// run, and the message never holds a Secret value.
func (r *AgentScheduleReconciler) setNotifyCondition(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig,
	dest *agentorcav1alpha1.ScheduleNotify, destErr error) {
	if dest == nil {
		meta.RemoveStatusCondition(&s.Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
		return
	}
	reason, err := "DestinationUnreadable", destErr
	if err == nil {
		_, reason, err = r.resolveDestination(ctx, dest, tenant)
	}
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
func (r *AgentScheduleReconciler) dispatch(ctx context.Context, s *agentorcav1alpha1.AgentSchedule, tenant *agentorcav1alpha1.TenantConfig,
	dest *agentorcav1alpha1.ScheduleNotify, destErr error, jobs []notifyJob) {
	if len(jobs) == 0 || (destErr == nil && !r.canDeliver(dest)) {
		return
	}
	log := logf.FromContext(ctx)
	var deliver deliverFunc
	err := destErr
	if err == nil {
		deliver, _, err = r.resolveDestination(ctx, dest, tenant)
	}
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
			if err := deliver(bg, j.msg); err != nil {
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
