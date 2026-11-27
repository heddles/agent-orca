package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

type fakeDestinations struct {
	rows    map[string]postgresql.ScheduleDestination
	fail    error
	deleted []string
}

func (f *fakeDestinations) GetScheduleDestination(_ context.Context, tenant, ns, name string) (postgresql.ScheduleDestination, bool, error) {
	if f.fail != nil {
		return postgresql.ScheduleDestination{}, false, f.fail
	}
	d, ok := f.rows[tenant+"/"+ns+"/"+name]
	return d, ok, nil
}

func (f *fakeDestinations) DeleteScheduleDestinationsOf(_ context.Context, ns, name string) error {
	f.deleted = append(f.deleted, ns+"/"+name)
	return f.fail
}

func chose(tenant string) *fakeDestinations {
	return &fakeDestinations{rows: map[string]postgresql.ScheduleDestination{
		tenant + "/team-a/digest": {App: "orca-bot", ChannelID: "C0CHOSEN", ChannelName: "#team-reports"},
	}}
}

const gitChannel = "ops" // the tenant channel notifyEnv sets up

// gitNotify is a schedule Git sends to the tenant channel "ops".
func gitNotify() *agentorcav1alpha1.ScheduleNotify {
	return &agentorcav1alpha1.ScheduleNotify{Channel: gitChannel, On: []string{"Succeeded"}, IncludeOutput: true}
}

func TestAChosenChannelReplacesTheDestinationAndNothingElse(t *testing.T) {
	e, webhooks := notifyEnv(t, gitNotify())
	chat := &fakeChat{}
	e.r.Chat, e.r.Destinations = chat, chose(schedTenant)
	run := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, func(r *agentorcav1alpha1.AgentRun) { r.Status.Output = "the numbers" })
	res := e.reconcile()
	e.tickAndFinish(10, agentorcav1alpha1.AgentRunPhaseFailed, nil) // not in "on"
	e.reconcile()
	e.settle()
	posts := chat.all()
	if len(posts) != 1 || posts[0].place != "C0CHOSEN" || posts[0].app != "orca-bot" || !strings.Contains(posts[0].text, "the numbers") {
		t.Fatalf("posts = %+v", posts)
	}
	if len(webhooks.all()) != 0 {
		t.Errorf("Git's channel was used as well: %+v", webhooks.all())
	}
	var stored agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: run}, &stored); err != nil {
		t.Fatal(err)
	}
	if v := stored.Annotations[schedule.AnnotationNotified]; v != "slack:orca-bot/C0CHOSEN" {
		t.Errorf("claim = %q", v)
	}
	if got := e.schedule().Spec.Result.Notify; got.Channel != gitChannel || got.Slack != nil {
		t.Errorf("the schedule's spec was changed: %+v", got)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Minute {
		t.Errorf("requeue after %s: the condition would not follow a change in the UI", res.RequeueAfter)
	}
}

func TestNothingIsSentWhileTheChosenChannelCannotBeRead(t *testing.T) {
	e, webhooks := notifyEnv(t, gitNotify())
	chat := &fakeChat{}
	e.r.Chat, e.r.Destinations = chat, &fakeDestinations{fail: errors.New("connection refused")}
	e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, nil)
	e.reconcile()
	e.settle()
	if len(webhooks.all()) != 0 || len(chat.all()) != 0 {
		t.Fatalf("sent without knowing where to: webhooks %+v, chat %+v", webhooks.all(), chat.all())
	}
	cond := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
	if cond == nil || cond.Status != "False" || cond.Reason != "DestinationUnreadable" || strings.Contains(cond.Message, "connection refused") {
		t.Errorf("NotifyReady = %+v", cond)
	}
}

func TestAChosenChannelIsIgnoredForAnotherTenantAndForAUIOwnedSchedule(t *testing.T) {
	for name, tc := range map[string]struct {
		store *fakeDestinations
		ui    bool
	}{
		"another tenant's choice": {store: chose("tnt_someone-else")},
		"the UI owns it now":      {store: chose(schedTenant), ui: true},
	} {
		t.Run(name, func(t *testing.T) {
			e, webhooks := notifyEnv(t, gitNotify())
			chat := &fakeChat{}
			e.r.Chat, e.r.Destinations = chat, tc.store
			if tc.ui {
				s := e.schedule()
				if s.Annotations == nil {
					s.Annotations = map[string]string{}
				}
				s.Annotations[tenancy.AnnotationOwner] = tenancy.OwnerUI
				if err := e.cl.Update(context.Background(), s); err != nil {
					t.Fatal(err)
				}
			}
			e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, nil)
			e.reconcile()
			e.settle()
			if len(chat.all()) != 0 || len(webhooks.all()) != 1 {
				t.Errorf("chat %+v, webhooks %+v: the spec's destination should be used", chat.all(), webhooks.all())
			}
		})
	}
}

func TestDeletingAScheduleForgetsItsChosenChannel(t *testing.T) {
	e, _ := notifyEnv(t, gitNotify())
	store := chose(schedTenant)
	e.r.Destinations = store
	e.reconcile()
	if len(store.deleted) != 0 {
		t.Fatalf("forgotten while the schedule exists: %v", store.deleted)
	}
	if err := e.cl.Delete(context.Background(), e.schedule(), client.PropagationPolicy("Background")); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "digest"}}
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "team-a/digest" {
		t.Errorf("deleted = %v", store.deleted)
	}
	// A failure is retried, or a later schedule of the name would inherit it.
	store.fail = errors.New("down")
	if _, err := e.r.Reconcile(context.Background(), req); err == nil {
		t.Error("a failed removal was not retried")
	}
}
