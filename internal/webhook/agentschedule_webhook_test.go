package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	agentB       = "agent-b"
	deletedAgent = "deleted-agent"
)

func agentLabelled(ns, name, tenant string) *agentorcav1alpha1.Agent {
	a := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if tenant != "" {
		a.Labels = map[string]string{tenancy.LabelTenantID: tenant}
	}
	return a
}

func templateIn(ns, name string, mode agentorcav1alpha1.WorkflowMode, agent string, budget bool) *agentorcav1alpha1.AgentWorkflow {
	wf := &agentorcav1alpha1.AgentWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentorcav1alpha1.AgentWorkflowSpec{
			Mode:  mode,
			Steps: []agentorcav1alpha1.WorkflowStep{{Name: "s", AgentRef: agent, Input: "x"}},
		},
	}
	if budget {
		wf.Spec.BudgetCap = &agentorcav1alpha1.WorkflowBudgetCap{Total: "2"}
	}
	return wf
}

func scheduleIn(ns string, mod func(*agentorcav1alpha1.AgentSchedule)) *agentorcav1alpha1.AgentSchedule {
	s := &agentorcav1alpha1.AgentSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "digest", Namespace: ns, Labels: map[string]string{tenancy.LabelTenantID: tenA}},
		Spec: agentorcav1alpha1.AgentScheduleSpec{
			AgentRef: "agent-a", Schedule: "0 8 * * 1-5", TimeZone: "Europe/Berlin", Input: "go",
			Budget: &agentorcav1alpha1.RunBudget{MaxUSD: "1"},
		},
	}
	if mod != nil {
		mod(s)
	}
	return s
}

func TestAgentScheduleValidator(t *testing.T) {
	scheme := isolationScheme(t)
	tenantAllow := tenantCfg("a", tenA, "a-ns", "shared-ns")
	tenantAllow.Spec.AllowedAgents = []string{"agent-a", "agent-a2"}
	rich := tenantCfg("rich", tenB, "b-ns")
	rich.Spec.BudgetPerDayUSD = "100"
	existing := []client.Object{
		tenantAllow, rich,
		agentLabelled("a-ns", "agent-a", tenA), agentLabelled("a-ns", "agent-a2", tenA), agentLabelled("a-ns", "agent-denied", tenA),
		agentLabelled("b-ns", agentB, tenB),
		templateIn("a-ns", "tpl", agentorcav1alpha1.WorkflowModeTemplate, "agent-a", false),
		templateIn("a-ns", "tpl-budget", agentorcav1alpha1.WorkflowModeTemplate, "agent-a", true),
		templateIn("a-ns", "tpl-denied", agentorcav1alpha1.WorkflowModeTemplate, "agent-denied", true),
		templateIn("a-ns", "not-a-template", agentorcav1alpha1.WorkflowModeWorkflow, "agent-a", true),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing...).Build()
	now := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	v := &AgentScheduleValidator{Decoder: admission.NewDecoder(scheme), Client: reader, Now: func() time.Time { return now }}
	user := authenticationv1.UserInfo{Username: "dev"}

	forB := func(mod func(*agentorcav1alpha1.AgentSchedule)) *agentorcav1alpha1.AgentSchedule {
		s := scheduleIn("b-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Labels = map[string]string{tenancy.LabelTenantID: tenB}
			s.Spec.AgentRef, s.Spec.Budget = agentB, nil
			if mod != nil {
				mod(s)
			}
		})
		return s
	}
	tests := []struct {
		name      string
		obj, old  *agentorcav1alpha1.AgentSchedule
		wantDeny  string // substring of the denial; "" means allowed
		wantWarns int
	}{
		{name: "a valid weekday schedule", obj: scheduleIn("a-ns", nil)},
		{name: "bad cron", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = "every monday" }), wantDeny: "spec.schedule"},
		{name: "six fields", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = "0 0 8 * * *" }), wantDeny: "five fields"},
		{name: "bad time zone", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.TimeZone = "Mars/Olympus" }), wantDeny: "time zone"},
		{name: "every minute is under the minimum interval", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = "* * * * *" }), wantDeny: "minimum interval"},
		{name: "every two minutes too", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = "*/2 * * * *" }), wantDeny: "minimum interval"},
		{name: "every five minutes is the minimum", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = "*/5 * * * *" })},
		{name: "a name that makes run names too long", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Name = strings.Repeat("x", 53) }), wantDeny: "at most 52"},
		{name: "no target", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = "" }), wantDeny: "exactly one"},
		{name: "agent of another namespace", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = agentB }), wantDeny: "not found in namespace"},
		{name: "agent outside the tenant allowlist", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = "agent-denied" }), wantDeny: "allowed agents"},
		{name: "no budget on a tenant with no daily budget", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Budget = nil }), wantDeny: "spending ceiling"},
		{name: "a schedule spend cap is a ceiling", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.Budget = nil
			s.Spec.Spend = &agentorcav1alpha1.ScheduleSpendCap{MaxUSDPerMonth: "20"}
		})},
		{name: "the tenant's daily budget is a ceiling", obj: forB(nil)},
		{name: "a zero run budget is no ceiling", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Budget = &agentorcav1alpha1.RunBudget{MaxUSD: "0"} }), wantDeny: "maxUSD"},
		{name: "a non-positive spend cap", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.Spend = &agentorcav1alpha1.ScheduleSpendCap{MaxUSDPerDay: "0"}
		}), wantDeny: "positive"},
		{name: "pause warns", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.Budget = &agentorcav1alpha1.RunBudget{MaxUSD: "1", OnExceeded: agentorcav1alpha1.RunBudgetPause}
		}), wantWarns: 1},
		{name: "a template with a budget cap", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.AgentRef, s.Spec.Input, s.Spec.Budget, s.Spec.WorkflowTemplateRef = "", "", nil, "tpl-budget"
		})},
		{name: "a template with no ceiling", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.AgentRef, s.Spec.Input, s.Spec.Budget, s.Spec.WorkflowTemplateRef = "", "", nil, "tpl"
		}), wantDeny: "spending ceiling"},
		{name: "a template whose agent the tenant may not run", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.AgentRef, s.Spec.Input, s.Spec.Budget, s.Spec.WorkflowTemplateRef = "", "", nil, "tpl-denied"
		}), wantDeny: "allowed agents"},
		{name: "a workflow that is not a template", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.AgentRef, s.Spec.Input, s.Spec.Budget, s.Spec.WorkflowTemplateRef = "", "", nil, "not-a-template"
		}), wantDeny: "not a template"},
		{name: "a missing template", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.AgentRef, s.Spec.Input, s.Spec.WorkflowTemplateRef = "", "", "ghost"
		}), wantDeny: "not found"},
		{name: "Replace needs a run to cancel", obj: scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.AgentRef, s.Spec.Input, s.Spec.WorkflowTemplateRef = "", "", "tpl-budget"
			s.Spec.ConcurrencyPolicy = agentorcav1alpha1.ScheduleConcurrencyReplace
		}), wantDeny: "cannot be cancelled"},
		{
			name: "suspending is allowed although the target is gone",
			old:  scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = deletedAgent }),
			obj:  scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = deletedAgent; s.Spec.Suspend = true }),
		},
		{
			name:     "changing the target re-checks it",
			old:      scheduleIn("a-ns", nil),
			obj:      scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = deletedAgent }),
			wantDeny: "not found",
		},
		{
			name: "an edit of the input does not re-litigate the target",
			old:  scheduleIn("a-ns", nil),
			obj:  scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Input = "changed" }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := admissionv1.Create
			if tt.old != nil {
				op = admissionv1.Update
			}
			var old client.Object
			if tt.old != nil {
				old = tt.old
			}
			resp := v.Handle(context.Background(), request(t, op, "AgentSchedule", tt.obj.Namespace, tt.obj, old, user))
			if tt.wantDeny == "" {
				if !resp.Allowed {
					t.Fatalf("denied: %v", resp.Result)
				}
			} else {
				if resp.Allowed {
					t.Fatalf("allowed, want a denial containing %q", tt.wantDeny)
				}
				if !strings.Contains(resp.Result.Message, tt.wantDeny) {
					t.Fatalf("denied with %q, want it to contain %q", resp.Result.Message, tt.wantDeny)
				}
			}
			if len(resp.Warnings) != tt.wantWarns {
				t.Errorf("warnings = %v, want %d", resp.Warnings, tt.wantWarns)
			}
		})
	}
}

func TestAgentScheduleMinIntervalIsConfigurable(t *testing.T) {
	scheme := isolationScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantCfg("a", tenA, "a-ns"), agentLabelled("a-ns", "agent-a", tenA)).Build()
	for _, tc := range []struct {
		min    time.Duration
		expr   string
		denied bool
	}{
		{time.Minute, "* * * * *", false},
		{15 * time.Minute, "*/10 * * * *", true},
		{15 * time.Minute, "*/15 * * * *", false},
		{0, "*/4 * * * *", true}, // the default is five minutes
	} {
		v := &AgentScheduleValidator{Decoder: admission.NewDecoder(scheme), Client: reader, MinInterval: tc.min}
		s := scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.Schedule = tc.expr })
		resp := v.Handle(context.Background(), request(t, admissionv1.Create, "AgentSchedule", "a-ns", s, nil, authenticationv1.UserInfo{}))
		if resp.Allowed == tc.denied {
			t.Errorf("min %s, %q: allowed=%v", tc.min, tc.expr, resp.Allowed)
		}
	}
	_ = schedule.DefaultMinInterval
}

// A schedule can reference only objects of its own tenant: the isolation
// webhook checks the references of an AgentSchedule like those of any other kind.
func TestTenantIsolationCoversSchedules(t *testing.T) {
	scheme := isolationScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := func(ns, name string, labels map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	}
	tplB := templateIn("shared-ns", "tpl-b", agentorcav1alpha1.WorkflowModeTemplate, agentB, true)
	tplB.Labels = map[string]string{tenancy.LabelTenantID: tenB}
	existing := []client.Object{
		tenantCfg("a", tenA, "a-ns", "shared-ns"),
		tenantCfg("b", tenB, "b-ns", "shared-ns"),
		agentLabelled("shared-ns", "agent-a", tenA), agentLabelled("shared-ns", agentB, tenB),
		tplB,
		secret("shared-ns", "sink-b", map[string]string{tenancy.LabelTenantID: tenB}),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing...).Build()
	v := &TenantIsolationValidator{Decoder: admission.NewDecoder(scheme), TenantConfigs: reader, Reader: reader, AdminGroups: []string{adminGrp}}
	user := authenticationv1.UserInfo{Username: "dev"}
	inShared := func(mod func(*agentorcav1alpha1.AgentSchedule)) *agentorcav1alpha1.AgentSchedule {
		return scheduleIn("shared-ns", mod)
	}
	for name, tc := range map[string]struct {
		obj  *agentorcav1alpha1.AgentSchedule
		rule string
	}{
		"its own tenant's agent":    {inShared(nil), ""},
		"another tenant's agent":    {inShared(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef = agentB }), tenancy.RuleCrossTenantRef},
		"another tenant's template": {inShared(func(s *agentorcav1alpha1.AgentSchedule) { s.Spec.AgentRef, s.Spec.WorkflowTemplateRef = "", "tpl-b" }), tenancy.RuleCrossTenantRef},
		"another tenant's sink secret": {inShared(func(s *agentorcav1alpha1.AgentSchedule) {
			s.Spec.Result = &agentorcav1alpha1.ScheduleResult{Egress: &agentorcav1alpha1.EgressConfig{Type: "kafka", Topic: "t", SecretRef: &agentorcav1alpha1.SecretKeyRef{Name: "sink-b"}}}
		}), tenancy.RuleCrossTenantRef},
		"no tenant label in a shared namespace":                 {inShared(func(s *agentorcav1alpha1.AgentSchedule) { s.Labels = nil }), tenancy.RuleLabelMissing},
		"a label of a tenant that does not claim the namespace": {scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Labels = map[string]string{tenancy.LabelTenantID: tenB} }), tenancy.RuleLabelNamespace},
	} {
		for _, mode := range []tenancy.Mode{tenancy.ModeEnforce, tenancy.ModeAudit} {
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				withMode(t, mode)
				resp := v.Handle(context.Background(), request(t, admissionv1.Create, "AgentSchedule", tc.obj.Namespace, tc.obj, nil, user))
				switch {
				case tc.rule == "":
					if !resp.Allowed || len(resp.Warnings) > 0 {
						t.Fatalf("want a clean allow, got %v %v %v", resp.Allowed, resp.Warnings, resp.Result)
					}
				case mode == tenancy.ModeEnforce:
					if resp.Allowed {
						t.Fatalf("rule %s allowed in enforce mode", tc.rule)
					}
				default:
					if !resp.Allowed || len(resp.Warnings) == 0 {
						t.Fatalf("rule %s: audit mode should allow with a warning", tc.rule)
					}
				}
			})
		}
	}
}

func TestTenantLabelerStampsSchedules(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(isolationScheme(t)).WithObjects(tenantCfg("a", tenA, "a-ns")).Build()
	l := &TenantLabeler{TenantConfigs: reader}
	s := scheduleIn("a-ns", func(s *agentorcav1alpha1.AgentSchedule) { s.Labels = nil })
	resp := l.Handle(context.Background(), request(t, admissionv1.Create, "AgentSchedule", "a-ns", s, nil, authenticationv1.UserInfo{}))
	if !resp.Allowed || len(resp.Patches) == 0 {
		t.Fatalf("no stamp: %+v", resp)
	}
}
