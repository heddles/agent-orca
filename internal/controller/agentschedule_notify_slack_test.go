package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/redact"
	"github.com/heddles/agent-orca/internal/schedule"
)

type chatPost struct{ tenant, kind, app, place, text string }

type fakeChat struct {
	mu      sync.Mutex
	posts   []chatPost
	checked []chatPost
	missing error
}

func (f *fakeChat) CheckApp(_ context.Context, tenant, kind, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, chatPost{tenant: tenant, kind: kind, app: app})
	return f.missing
}

func (f *fakeChat) PostToApp(_ context.Context, tenant, kind, app, place string, render func(*redact.Registry) string) error {
	secrets := &redact.Registry{}
	secrets.Register("xoxb-the-bot-token-value")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, chatPost{tenant, kind, app, place, render(secrets)})
	return nil
}

func (f *fakeChat) all() []chatPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chatPost(nil), f.posts...)
}

func slackNotify() *agentorcav1alpha1.ScheduleNotify {
	return &agentorcav1alpha1.ScheduleNotify{
		Slack: &agentorcav1alpha1.ScheduleSlackTarget{App: "orca-bot", ChannelID: "C0REPORTS", ChannelName: "#reports"},
		On:    []string{"Succeeded"}, IncludeOutput: true,
	}
}

func TestNotifyThroughASlackAppPostsOnceToTheChosenChannel(t *testing.T) {
	e, webhooks := notifyEnv(t, slackNotify())
	chat := &fakeChat{}
	e.r.Chat = chat
	run := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, func(r *agentorcav1alpha1.AgentRun) {
		r.Status.Output = "all good <!channel> token xoxb-the-bot-token-value"
	})
	for range 3 {
		e.reconcile()
	}
	e.settle()
	got := chat.all()
	if len(got) != 1 {
		t.Fatalf("%d posts, want 1: %+v", len(got), got)
	}
	p := got[0]
	if p.tenant != schedTenant || p.kind != "slack" || p.app != "orca-bot" || p.place != "C0REPORTS" {
		t.Errorf("posted to %+v", p)
	}
	if !strings.Contains(p.text, "all good &lt;!channel&gt;") || strings.Contains(p.text, "<!channel>") {
		t.Errorf("the output must not be able to mention anyone: %q", p.text)
	}
	if strings.Contains(p.text, "xoxb-the-bot-token-value") {
		t.Errorf("the app's token was posted: %q", p.text)
	}
	if len(webhooks.all()) != 0 {
		t.Errorf("a TenantConfig channel was used too: %+v", webhooks.all())
	}
	var stored agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: run}, &stored); err != nil {
		t.Fatal(err)
	}
	if v := stored.Annotations[schedule.AnnotationNotified]; v != "slack:orca-bot/C0REPORTS" {
		t.Errorf("claim = %q", v)
	}
	cond := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
	if cond == nil || cond.Status != "True" {
		t.Errorf("NotifyReady = %+v", cond)
	}
}

func TestNotifySlackAppThatCannotBeUsedSetsTheCondition(t *testing.T) {
	e, _ := notifyEnv(t, slackNotify())
	e.r.Recorder = record.NewFakeRecorder(50)
	chat := &fakeChat{missing: errors.New(`the chat app "orca-bot" is turned off`)}
	e.r.Chat = chat
	e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, nil)
	e.reconcile()
	e.settle()
	if len(chat.all()) != 0 {
		t.Fatalf("posted although the app cannot be used: %+v", chat.all())
	}
	cond := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
	if cond == nil || cond.Status != "False" || cond.Reason != "ChatAppUnavailable" || !strings.Contains(cond.Message, "turned off") {
		t.Errorf("NotifyReady = %+v", cond)
	}
}

func TestNotifySlackWithoutChatAppsClaimsNothing(t *testing.T) {
	e, webhooks := notifyEnv(t, slackNotify()) // no e.r.Chat: no database
	run := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, nil)
	e.reconcile()
	e.settle()
	var stored agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: run}, &stored); err != nil {
		t.Fatal(err)
	}
	if v := stored.Annotations[schedule.AnnotationNotified]; v != "" || len(webhooks.all()) != 0 {
		t.Errorf("claim = %q, webhooks = %+v", v, webhooks.all())
	}
	cond := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
	if cond == nil || cond.Reason != "ChatUnavailable" {
		t.Errorf("NotifyReady = %+v", cond)
	}
}
