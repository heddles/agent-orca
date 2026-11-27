package apiserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func TestScheduleReportDestinationNeedsAdmin(t *testing.T) {
	f := newWriteFixture(t, schedAgentObj())
	const path = "/api/resources/agentschedules"
	create := func(name, result string) string {
		return `{"metadata":{"name":"` + name + `","namespace":"shared"},"spec":{"agentRef":"helper","schedule":"0 8 * * *","input":"x","budget":{"maxUSD":"1"}` + result + `}}`
	}
	slack := `,"result":{"notify":{"slack":{"app":"orca-bot","channelID":"C0REPORTS"},"on":["Failed","Succeeded"],"includeOutput":true}}`
	for _, result := range []string{slack, `,"result":{"notify":{"channel":"team-ops"}}`} {
		if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, path, create("s", result)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin") {
			t.Fatalf("builder chose a destination: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodPost, path, create("s", slack)); rec.Code != http.StatusCreated {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, path, create("quiet", "")); rec.Code != http.StatusCreated {
		t.Fatalf("builder without a destination: %d %s", rec.Code, rec.Body)
	}

	put := func(role tenancy.Role, input, result string) int {
		body := create("s", result)
		body = strings.Replace(body, `"metadata":{`, `"metadata":{"resourceVersion":"`+f.sched("s").ResourceVersion+`",`, 1)
		body = strings.Replace(body, `"input":"x"`, `"input":"`+input+`"`, 1)
		return f.do(f.as(role), http.MethodPut, path+"/shared/s", body).Code
	}
	// Keeping it, written in another order, is not a change.
	same := strings.Replace(slack, `["Failed","Succeeded"]`, `["Succeeded","Failed"]`, 1)
	if code := put(tenancy.RoleBuilder, "y", same); code != http.StatusOK {
		t.Fatalf("builder edit keeping the destination: %d", code)
	}
	for what, result := range map[string]string{
		"another channel":     strings.Replace(slack, "C0REPORTS", "C0EVERYONE", 1),
		"another app":         strings.Replace(slack, "orca-bot", "other", 1),
		"to a tenant channel": `,"result":{"notify":{"channel":"team-ops"}}`,
		"output on or off":    strings.Replace(slack, `"includeOutput":true`, `"includeOutput":false`, 1),
		"removed":             "",
	} {
		if code := put(tenancy.RoleBuilder, "z", result); code != http.StatusForbidden {
			t.Errorf("builder changed the destination (%s): %d", what, code)
		}
	}
	if code := put(tenancy.RoleBuilder, "z", `,"result":{"notify":"ops"}`); code != http.StatusBadRequest {
		t.Errorf("an unreadable destination: %d", code)
	}
	if code := put(tenancy.RoleAdmin, "z", strings.Replace(slack, "C0REPORTS", "C0OTHER", 1)); code != http.StatusOK {
		t.Fatalf("admin change: %d", code)
	}
	if got := f.sched("s").Spec.Result.Notify.Slack.ChannelID; got != "C0OTHER" {
		t.Errorf("channel = %q", got)
	}
}

func TestDuplicatingAScheduleCopiesTheDestinationForAdminsOnly(t *testing.T) {
	git := uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Annotations = map[string]string{tenancy.AnnotationOwner: tenancy.OwnerGit}
		s.Spec.Result = &agentorcav1alpha1.ScheduleResult{Notify: &agentorcav1alpha1.ScheduleNotify{Channel: "team-ops", IncludeOutput: true}}
	})
	f := newWriteFixture(t, git, schedAgentObj())
	for name, role := range map[string]tenancy.Role{"by-builder": tenancy.RoleBuilder, "by-admin": tenancy.RoleAdmin} {
		rec := f.do(f.as(role), http.MethodPost, "/api/resources/agentschedules/shared/digest/duplicate", `{"name":"`+name+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if r := f.sched("by-builder").Spec.Result; r != nil {
		t.Errorf("a builder's copy kept the destination: %+v", r.Notify)
	}
	if r := f.sched("by-admin").Spec.Result; r == nil || r.Notify == nil || r.Notify.Channel != "team-ops" {
		t.Errorf("an admin's copy lost the destination: %+v", r)
	}
}

func TestScheduleListNamesTheDestinationAndItsProblem(t *testing.T) {
	sc := uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Spec.Result = &agentorcav1alpha1.ScheduleResult{Notify: &agentorcav1alpha1.ScheduleNotify{
			Slack: &agentorcav1alpha1.ScheduleSlackTarget{App: "orca-bot", ChannelID: "C0REPORTS", ChannelName: "#reports"},
		}}
		s.Status.Conditions = []metav1.Condition{{Type: agentorcav1alpha1.ScheduleConditionNotifyReady, Status: metav1.ConditionFalse, Reason: "ChatAppUnavailable", Message: "the app is turned off"}}
	})
	f := newWriteFixture(t, sc, schedAgentObj())
	rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules")
	var list []scheduleSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %s", rec.Body)
	}
	n := list[0].Notify
	if n == nil || n.Slack == nil || n.Slack.ChannelName != "#reports" || n.Problem != "the app is turned off" {
		t.Errorf("notify = %+v", n)
	}
}
