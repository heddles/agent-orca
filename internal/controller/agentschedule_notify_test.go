package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/notify"
	"github.com/heddles/agent-orca/internal/schedule"
)

type sentNotice struct {
	ch  notify.Channel
	msg notify.Message
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []sentNotice
}

func (f *fakeNotifier) Send(_ context.Context, ch *notify.Channel, msg notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentNotice{*ch, msg})
	return nil
}

func (f *fakeNotifier) all() []sentNotice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentNotice(nil), f.sent...)
}

const notifySecretURL = "https://hooks.slack.com/services/T000/B000/topsecrettokenvalue"

func notifyTenant(channels ...agentorcav1alpha1.NotificationChannel) *agentorcav1alpha1.TenantConfig {
	return &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "operator"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			TenantID: schedTenant, AuthMode: "issued", AllowedNamespaces: []string{"team-a"},
			Notifications: &agentorcav1alpha1.TenantNotifications{Channels: channels},
		},
	}
}

func slackChannel(name, secret string) agentorcav1alpha1.NotificationChannel {
	return agentorcav1alpha1.NotificationChannel{
		Name: name, Type: agentorcav1alpha1.NotificationChannelSlack,
		SecretRef: agentorcav1alpha1.NotificationSecretRef{Name: secret},
	}
}

func notifySecret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "operator"}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func withNotify(n *agentorcav1alpha1.ScheduleNotify) func(*agentorcav1alpha1.AgentSchedule) {
	return func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.Result = &agentorcav1alpha1.ScheduleResult{Notify: n}
	}
}

// notifyEnv is a schedule with a channel "ops" on its tenant and a recording notifier.
func notifyEnv(t *testing.T, n *agentorcav1alpha1.ScheduleNotify) (*schedEnv, *fakeNotifier) {
	t.Helper()
	e := newSchedEnv(t, utc(8, 30),
		hourly(withNotify(n)), schedAgent(), notifyTenant(slackChannel("ops", "ops-hook")),
		notifySecret("ops-hook", map[string]string{"url": notifySecretURL}))
	f := &fakeNotifier{}
	e.r.Notifier, e.r.Recorder, e.r.UIURL = f, record.NewFakeRecorder(50), "https://orca.example.com/"
	e.reconcile() // sets the cursor
	return e, f
}

// tickAndFinish starts the run of the hour h and finishes it.
func (e *schedEnv) tickAndFinish(h int, phase agentorcav1alpha1.AgentRunPhase, mod func(*agentorcav1alpha1.AgentRun)) string {
	e.t.Helper()
	tick := utc(h, 0)
	e.now = tick.Add(time.Second)
	e.reconcile()
	name := schedule.RunName("digest", tick)
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		e.t.Fatal(err)
	}
	st, ct := metav1.NewTime(tick.Add(5*time.Second)), metav1.NewTime(tick.Add(95*time.Second))
	run.Status.Phase, run.Status.StartTime, run.Status.CompletionTime = phase, &st, &ct
	run.Status.SpendUSD = "0.42"
	if mod != nil {
		mod(&run)
	}
	if err := e.cl.Status().Update(context.Background(), &run); err != nil {
		e.t.Fatal(err)
	}
	return name
}

func (e *schedEnv) settle() { e.r.WaitNotifications() }

func TestNotifyFailedRunOnceWithTheDefaultOutcomeFilter(t *testing.T) {
	e, f := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops"}) // on defaults to Failed
	ok := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, nil)
	e.reconcile()
	bad := e.tickAndFinish(10, agentorcav1alpha1.AgentRunPhaseFailed, func(r *agentorcav1alpha1.AgentRun) {
		r.Status.FailureReason = "tool timed out"
		r.Status.Output = "partial output"
	})
	for range 4 { // the same finished run seen again and again
		e.reconcile()
	}
	e.settle()
	got := f.all()
	if len(got) != 1 {
		t.Fatalf("%d notifications, want exactly 1 (only the failed run, once): %+v", len(got), got)
	}
	m := got[0].msg
	if m.Run != bad || m.Outcome != "Failed" || m.Schedule != "digest" || m.Agent != "digest-agent" || m.CostUSD != "0.42" ||
		m.Duration != 90*time.Second || m.Reason != "tool timed out" {
		t.Errorf("message = %+v", m)
	}
	if want := "https://orca.example.com/runs/live/team-a/" + bad; m.Link != want {
		t.Errorf("link = %q, want %q", m.Link, want)
	}
	if m.Output != "" {
		t.Errorf("output %q was sent although includeOutput is off", m.Output)
	}
	if got[0].ch.URL != notifySecretURL || got[0].ch.Type != notify.TypeSlack {
		t.Errorf("channel = %+v", got[0].ch)
	}
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: bad}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Annotations[schedule.AnnotationNotified] != "ops" {
		t.Errorf("the run is not marked as notified: %v", run.Annotations)
	}
	_ = ok
}

func TestNotifySucceededWithOutputAndNoLinkWithoutUIURL(t *testing.T) {
	e, f := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{
		Channel: "ops", On: []string{"Succeeded", "Failed"}, IncludeOutput: true,
	})
	e.r.UIURL = ""
	e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, func(r *agentorcav1alpha1.AgentRun) { r.Status.Output = "all done" })
	e.reconcile()
	e.settle()
	got := f.all()
	if len(got) != 1 || got[0].msg.Outcome != "Succeeded" || got[0].msg.Output != "all done" {
		t.Fatalf("notifications = %+v", got)
	}
	if got[0].msg.Link != "" {
		t.Errorf("link = %q, want none: the UI address is unknown", got[0].msg.Link)
	}
}

func TestNotifySkipsARunTheReplacePolicyCancelled(t *testing.T) {
	e, f := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops"})
	name := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseFailed, nil)
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		t.Fatal(err)
	}
	run.Annotations = map[string]string{annotationScheduleReplaced: "true"}
	if err := e.cl.Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.settle()
	if got := f.all(); len(got) != 0 {
		t.Errorf("a replaced run was announced: %+v", got)
	}
}

func TestNotifyNeverClaimsARunWhenNoNotifierIsConfigured(t *testing.T) {
	e, _ := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops"})
	e.r.Notifier = nil
	name := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseFailed, nil)
	e.reconcile()
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Annotations[schedule.AnnotationNotified] != "" {
		t.Errorf("claimed with nobody to send: %v", run.Annotations)
	}
}

func TestNotifyUnresolvedChannelSetsAConditionAndNeverBlocksRuns(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"no such channel":  {notifyTenant(slackChannel("other", "s")), notifySecret("s", map[string]string{"url": notifySecretURL})},
		"no secret":        {notifyTenant(slackChannel("ops", "missing"))},
		"invalid secret":   {notifyTenant(slackChannel("ops", "bad")), notifySecret("bad", map[string]string{"url": "http://insecure.example.com/x"})},
		"no tenant config": {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSchedEnv(t, utc(8, 30), append([]client.Object{
				hourly(withNotify(&agentorcav1alpha1.ScheduleNotify{Channel: "ops"})), schedAgent(),
			}, objs...)...)
			f := &fakeNotifier{}
			rec := record.NewFakeRecorder(50)
			e.r.Notifier, e.r.Recorder = f, rec
			e.reconcile()
			e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseFailed, nil)
			e.reconcile()
			e.settle()
			if len(e.runs()) != 1 {
				t.Fatalf("runs = %d: an unresolved channel must not block runs", len(e.runs()))
			}
			c := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Message == "" {
				t.Fatalf("NotifyReady = %+v, want false with a reason", c)
			}
			if strings.Contains(c.Message, "topsecrettokenvalue") || strings.Contains(c.Message, "insecure.example.com") {
				t.Errorf("the condition leaks a Secret value: %q", c.Message)
			}
			if len(f.all()) != 0 {
				t.Error("something was sent through an unresolved channel")
			}
		})
	}
}

func TestNotifyConditionIsTrueWhenResolvedAndGoneWithoutNotify(t *testing.T) {
	e, _ := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops"})
	if c := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("NotifyReady = %+v", c)
	}
	s := e.schedule()
	s.Spec.Result = nil
	s.Generation++
	if err := e.cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if c := meta.FindStatusCondition(e.schedule().Status.Conditions, agentorcav1alpha1.ScheduleConditionNotifyReady); c != nil {
		t.Errorf("NotifyReady = %+v, want none", c)
	}
}

func TestNotifyBreakerOpenAndSpendCapAreSentOnceOnTheChange(t *testing.T) {
	two := 2
	e, f := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops", On: []string{"Failed", "BreakerOpen", "Skipped"}})
	e.r.MaxConsecutiveFailures = two
	e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseFailed, nil)
	e.reconcile()
	e.tickAndFinish(10, agentorcav1alpha1.AgentRunPhaseFailed, nil)
	for range 3 {
		e.reconcile()
	}
	e.settle()
	events := make([]string, 0, 3)
	for _, s := range f.all() {
		events = append(events, s.msg.Outcome)
	}
	if want := "Failed Failed BreakerOpen"; strings.Join(events, " ") != want {
		t.Fatalf("events = %v, want %q (the breaker once, however often it is reconciled)", events, want)
	}
	last := f.all()[2].msg
	if last.Run != "" || !strings.Contains(last.Reason, "stopped") {
		t.Errorf("breaker message = %+v", last)
	}

	// The spend cap, on a schedule that has not failed.
	e2, f2 := notifyEnv(t, &agentorcav1alpha1.ScheduleNotify{Channel: "ops", On: []string{"Skipped"}})
	s := e2.schedule()
	s.Spec.Spend = &agentorcav1alpha1.ScheduleSpendCap{MaxUSDPerDay: "0.40"}
	s.Generation++
	if err := e2.cl.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e2.reconcile()
	e2.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseSucceeded, nil) // spends 0.42, over the cap
	for range 3 {
		e2.reconcile()
	}
	e2.now = utc(10, 0).Add(time.Second)
	e2.reconcile() // a tick that is skipped for the budget
	e2.reconcile()
	e2.settle()
	got := f2.all()
	if len(got) != 1 || got[0].msg.Outcome != "Skipped" {
		t.Fatalf("notifications = %+v, want one Skipped for the spend cap", got)
	}
}

// With the real sender: one signed request per run, and nothing of the Secret
// reaches the status, the conditions, an event or the run.
func TestNotifyEndToEndWithAWebhookReceiver(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var hdrs []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, hdrs = append(bodies, b), append(hdrs, r.Header.Clone())
		mu.Unlock()
	}))
	defer srv.Close()
	const key = "end-to-end-signing-key"
	ch := agentorcav1alpha1.NotificationChannel{
		Name: "ops", Type: agentorcav1alpha1.NotificationChannelWebhook, Insecure: true, AllowPrivateAddresses: true,
		SecretRef: agentorcav1alpha1.NotificationSecretRef{Name: "ops-hook"},
	}
	e := newSchedEnv(t, utc(8, 30),
		hourly(withNotify(&agentorcav1alpha1.ScheduleNotify{Channel: "ops", IncludeOutput: true})), schedAgent(),
		notifyTenant(ch), notifySecret("ops-hook", map[string]string{"url": srv.URL + "/hook/secretpath", "signingKey": key}))
	rec := record.NewFakeRecorder(50)
	e.r.Notifier, e.r.Recorder = &notify.Sender{Backoff: time.Millisecond}, rec
	e.reconcile()
	e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseFailed, func(r *agentorcav1alpha1.AgentRun) {
		r.Status.Output = "result mentions " + key + " by accident"
	})
	for range 3 {
		e.reconcile()
	}
	e.settle()
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("%d requests, want exactly 1", len(bodies))
	}
	ts := hdrs[0].Get(notify.TimestampHeader)
	if hdrs[0].Get(notify.SignatureHeader) != notify.Sign(key, ts, bodies[0]) {
		t.Error("the signature does not verify")
	}
	var p map[string]any
	if err := json.Unmarshal(bodies[0], &p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p["output"].(string), key) || p["outcome"] != "Failed" {
		t.Errorf("payload = %v", p)
	}
	// Nothing of the Secret in what the operator stores or says.
	var shown strings.Builder
	st, _ := json.Marshal(e.schedule().Status)
	shown.Write(st)
	for _, r := range e.runs() {
		a, _ := json.Marshal(r.Annotations)
		shown.Write(a)
	}
	close(rec.Events)
	for ev := range rec.Events {
		shown.WriteString(ev)
	}
	for _, leak := range []string{key, "secretpath", srv.URL} {
		if strings.Contains(shown.String(), leak) {
			t.Errorf("%q leaked into status, annotations or events", leak)
		}
	}
}

func TestNotifyFailureIsAnEventAndNeverFailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	ch := agentorcav1alpha1.NotificationChannel{
		Name: "ops", Type: agentorcav1alpha1.NotificationChannelSlack, Insecure: true, AllowPrivateAddresses: true,
		SecretRef: agentorcav1alpha1.NotificationSecretRef{Name: "ops-hook"},
	}
	e := newSchedEnv(t, utc(8, 30),
		hourly(withNotify(&agentorcav1alpha1.ScheduleNotify{Channel: "ops"})), schedAgent(),
		notifyTenant(ch), notifySecret("ops-hook", map[string]string{"url": srv.URL + "/services/tokentokentoken"}))
	rec := record.NewFakeRecorder(50)
	e.r.Notifier, e.r.Recorder = &notify.Sender{Backoff: time.Millisecond}, rec
	e.reconcile()
	name := e.tickAndFinish(9, agentorcav1alpha1.AgentRunPhaseFailed, nil)
	e.reconcile()
	e.settle()
	var run agentorcav1alpha1.AgentRun
	if err := e.cl.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed || run.Annotations[schedule.AnnotationNotified] != "ops" {
		t.Errorf("run = %v %v", run.Status.Phase, run.Annotations)
	}
	var sawFailure bool
	close(rec.Events)
	for ev := range rec.Events {
		if strings.Contains(ev, "NotifyFailed") {
			sawFailure = true
			if strings.Contains(ev, "tokentokentoken") || strings.Contains(ev, srv.URL) {
				t.Errorf("event leaks the URL: %s", ev)
			}
		}
	}
	if !sawFailure {
		t.Error("no NotifyFailed event")
	}
}
