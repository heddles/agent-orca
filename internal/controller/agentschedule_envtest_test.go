package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
)

// The reconciler and the AgentSchedule CRD's validation rules against a real API
// server, with a clock the test moves.
func TestAgentScheduleEnvtest(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: ownershipEnvtestAssets(t),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	must(t, agentorcav1alpha1.AddToScheme(scheme))
	must(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	must(t, err)
	ctx := context.Background()
	must(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}))
	must(t, cl.Create(ctx, &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "digest-agent", Namespace: "team-a"},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "default", Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "ghcr.io/example/agent:1"}},
	}))

	newSchedule := func(name string, mod func(*agentorcav1alpha1.AgentSchedule)) *agentorcav1alpha1.AgentSchedule {
		s := &agentorcav1alpha1.AgentSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
			Spec: agentorcav1alpha1.AgentScheduleSpec{
				AgentRef: "digest-agent", Schedule: "0 * * * *", Input: "run on {{schedule.date}}",
				Budget: &agentorcav1alpha1.RunBudget{MaxUSD: "1.00"},
			},
		}
		if mod != nil {
			mod(s)
		}
		return s
	}

	t.Run("the CRD rejects invalid targets and inputs", func(t *testing.T) {
		for name, mod := range map[string]func(*agentorcav1alpha1.AgentSchedule){
			"no target": func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = "" },
			"two targets": func(s *agentorcav1alpha1.AgentSchedule) {
				s.Spec.WorkflowTemplateRef = "tpl"
			},
			"agent without input": func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Input = "" },
			"bad concurrency policy": func(s *agentorcav1alpha1.AgentSchedule) {
				s.Spec.ConcurrencyPolicy = "Sometimes"
			},
			"oversized input": func(s *agentorcav1alpha1.AgentSchedule) {
				b := make([]byte, 65537)
				for i := range b {
					b[i] = 'x'
				}
				s.Spec.Input = string(b)
			},
		} {
			if err := cl.Create(ctx, newSchedule("bad", mod)); err == nil {
				t.Errorf("%s: accepted", name)
				_ = cl.Delete(ctx, newSchedule("bad", nil))
			}
		}
	})

	t.Run("spec.result.notify is validated and defaults to Failed", func(t *testing.T) {
		withNotify := func(n agentorcav1alpha1.ScheduleNotify) func(*agentorcav1alpha1.AgentSchedule) {
			return func(s *agentorcav1alpha1.AgentSchedule) {
				s.Spec.Result = &agentorcav1alpha1.ScheduleResult{Notify: &n}
			}
		}
		for name, n := range map[string]agentorcav1alpha1.ScheduleNotify{
			"no channel":           {},
			"unknown event":        {Channel: "ops", On: []string{"Exploded"}},
			"output without a run": {Channel: "ops", On: []string{"BreakerOpen"}, IncludeOutput: true},
		} {
			if err := cl.Create(ctx, newSchedule("bad-notify", withNotify(n))); err == nil {
				t.Errorf("%s: accepted", name)
				_ = cl.Delete(ctx, newSchedule("bad-notify", nil))
			}
		}
		good := newSchedule("good-notify", withNotify(agentorcav1alpha1.ScheduleNotify{Channel: "ops"}))
		must(t, cl.Create(ctx, good))
		if on := good.Spec.Result.Notify.On; len(on) != 1 || on[0] != agentorcav1alpha1.ScheduleNotifyFailed {
			t.Errorf("on = %v, want the default [Failed]", on)
		}
		must(t, cl.Delete(ctx, good))
		// TenantConfig takes channels, and refuses an unknown type.
		tc := func(typ string) *agentorcav1alpha1.TenantConfig {
			return &agentorcav1alpha1.TenantConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "tc-notify", Namespace: "team-a"},
				Spec: agentorcav1alpha1.TenantConfigSpec{AuthMode: "issued", AllowedNamespaces: []string{"team-a"},
					Issued: &agentorcav1alpha1.IssuedAuthConfig{ClientID: "c", ClientSecretRef: agentorcav1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
					Notifications: &agentorcav1alpha1.TenantNotifications{Channels: []agentorcav1alpha1.NotificationChannel{
						{Name: "ops", Type: typ, SecretRef: agentorcav1alpha1.NotificationSecretRef{Name: "hook"}},
					}}},
			}
		}
		if err := cl.Create(ctx, tc("pager")); err == nil {
			t.Error("a channel of an unknown type was accepted")
			_ = cl.Delete(ctx, tc("pager"))
		}
		must(t, cl.Create(ctx, tc("slack")))
		must(t, cl.Delete(ctx, tc("slack")))
	})

	s := newSchedule("digest", func(s *agentorcav1alpha1.AgentSchedule) {
		one := int32(1)
		s.Spec.SuccessfulRunsHistoryLimit = &one
	})
	must(t, cl.Create(ctx, s))
	if s.Spec.ConcurrencyPolicy != agentorcav1alpha1.ScheduleConcurrencyForbid || s.Spec.TimeZone != "UTC" {
		t.Fatalf("defaults not applied: %+v", s.Spec)
	}

	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	r := &AgentScheduleReconciler{Client: cl, Scheme: scheme, Now: func() time.Time { return now }}
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "digest"}})
		must(t, err)
	}
	runs := func() []agentorcav1alpha1.AgentRun {
		var l agentorcav1alpha1.AgentRunList
		must(t, cl.List(ctx, &l, client.InNamespace("team-a"), client.MatchingLabels{schedule.LabelSchedule: "digest"}))
		return l.Items
	}

	reconcile()
	if n := len(runs()); n != 0 {
		t.Fatalf("%d runs before the first tick", n)
	}

	tick := now.Truncate(time.Hour).Add(time.Hour)
	now = tick.Add(3 * time.Second)
	reconcile()
	reconcile() // a repeated reconcile must not duplicate
	got := runs()
	if len(got) != 1 || got[0].Name != schedule.RunName("digest", tick) {
		t.Fatalf("runs = %v, want exactly one for the tick", runNames(got))
	}
	if !metav1.IsControlledBy(&got[0], s) {
		t.Fatalf("owner references = %v", got[0].OwnerReferences)
	}
	var cur agentorcav1alpha1.AgentSchedule
	must(t, cl.Get(ctx, client.ObjectKeyFromObject(s), &cur))
	if cur.Status.LastScheduleTime == nil || !cur.Status.LastScheduleTime.Time.Equal(tick) || len(cur.Status.Active) != 1 {
		t.Fatalf("status = %+v", cur.Status)
	}

	// Finish it, then let two more ticks pass: history keeps one successful run.
	finish := func(name string, at time.Time) {
		var run agentorcav1alpha1.AgentRun
		must(t, cl.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: name}, &run))
		ct := metav1.NewTime(at)
		run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
		run.Status.CompletionTime = &ct
		must(t, cl.Status().Update(ctx, &run))
	}
	finish(got[0].Name, tick.Add(time.Minute))
	for i := 1; i <= 2; i++ {
		now = tick.Add(time.Duration(i) * time.Hour).Add(3 * time.Second)
		reconcile()
		finish(schedule.RunName("digest", tick.Add(time.Duration(i)*time.Hour)), now)
		reconcile()
		reconcile()
	}
	got = runs()
	if len(got) != 1 || got[0].Name != schedule.RunName("digest", tick.Add(2*time.Hour)) {
		t.Fatalf("after pruning runs = %v, want only the newest", runNames(got))
	}

	// Suspend: the next tick starts nothing.
	must(t, cl.Get(ctx, client.ObjectKeyFromObject(s), &cur))
	cur.Spec.Suspend = true
	must(t, cl.Update(ctx, &cur))
	now = tick.Add(5 * time.Hour).Add(time.Second)
	reconcile()
	if n := len(runs()); n != 1 {
		t.Fatalf("%d runs while suspended, want the 1 kept", n)
	}

	// Deleting the schedule is what deletes its runs; the API server's garbage
	// collector is not part of envtest, so check the owner reference that drives it.
	if ref := metav1.GetControllerOf(&got[0]); ref == nil || ref.Kind != "AgentSchedule" || ref.Name != schedName {
		t.Fatalf("controller owner = %+v", ref)
	}
}
