package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/inbound"
	inboundslack "github.com/heddles/agent-orca/internal/inbound/slack"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

type memDestinations struct {
	rows map[string]postgresql.ScheduleDestination
	fail error
}

func (m *memDestinations) SaveScheduleDestination(_ context.Context, d postgresql.ScheduleDestination) error {
	d.SetAt = time.Now()
	m.rows[destinationKey(d.TenantID, d.Namespace, d.Name)] = d
	return m.fail
}

func (m *memDestinations) ListScheduleDestinations(context.Context) ([]postgresql.ScheduleDestination, error) {
	out := make([]postgresql.ScheduleDestination, 0, len(m.rows))
	for _, d := range m.rows {
		out = append(out, d)
	}
	return out, m.fail
}

func (m *memDestinations) DeleteScheduleDestination(_ context.Context, tenant, ns, name string) (bool, error) {
	k := destinationKey(tenant, ns, name)
	_, ok := m.rows[k]
	delete(m.rows, k)
	return ok, m.fail
}

// appsStore is the one call CheckApp makes.
type appsStore struct {
	inbound.Store
	apps []postgresql.InboundConnection
}

func (a appsStore) ListInboundConnections(context.Context, []string, string) ([]postgresql.InboundConnection, error) {
	return a.apps, nil
}

func destinationFixture(t *testing.T, owner string, notify *agentorcav1alpha1.ScheduleNotify) (*writeFixture, *memDestinations) {
	t.Helper()
	sc := uiSchedule(func(s *agentorcav1alpha1.AgentSchedule) {
		s.Annotations = map[string]string{tenancy.AnnotationOwner: owner}
		if notify != nil {
			s.Spec.Result = &agentorcav1alpha1.ScheduleResult{Notify: notify}
		}
	})
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: sharedNS},
		Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: isoA, AuthMode: "issued", AllowedNamespaces: []string{sharedNS}},
	}
	f := newWriteFixture(t, sc, schedAgentObj(), tc)
	store := &memDestinations{rows: map[string]postgresql.ScheduleDestination{}}
	reg := inbound.NewRegistry()
	reg.Register(&inboundslack.Adapter{})
	f.s.destinations = store
	f.s.inboundPipe = &inbound.Pipeline{Registry: reg, Store: appsStore{apps: []postgresql.InboundConnection{
		{ID: "a", TenantID: isoA, Kind: "slack", Name: "orca-bot", Enabled: true, Config: []byte(`{}`)},
		{ID: "b", TenantID: "tnt_someone-else", Kind: "slack", Name: "their-bot", Enabled: true, Config: []byte(`{}`)},
	}}}
	return f, store
}

// routed sends a request with a body through the whole mux.
func routed(f *writeFixture, id *TenantIdentity, method, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(rec, asTenant(httptest.NewRequest(method, destinationPath, strings.NewReader(body)), id))
	return rec
}

const gitTeamChannel = "team-ops"

const destinationPath = schedPath + "/destination"
const chosenBody = `{"app":"orca-bot","channelID":"C0CHOSEN","channelName":"#team-reports"}`

func TestChoosingAChannelForAGitScheduleNeedsAdminAndLeavesTheScheduleAlone(t *testing.T) {
	f, store := destinationFixture(t, tenancy.OwnerGit, &agentorcav1alpha1.ScheduleNotify{Channel: gitTeamChannel, IncludeOutput: true})
	before := f.sched("digest").ResourceVersion
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			if rec := routed(f, f.as(role), method, chosenBody); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d", role, method, rec.Code)
			}
		}
	}
	if len(store.rows) != 0 {
		t.Fatalf("saved: %v", store.rows)
	}
	if rec := routed(f, f.as(tenancy.RoleAdmin), http.MethodPut, chosenBody); rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	d := store.rows[destinationKey(isoA, sharedNS, "digest")]
	if d.ChannelID != "C0CHOSEN" || d.App != "orca-bot" || d.SetBy == "" {
		t.Fatalf("stored = %+v", store.rows)
	}
	if f.sched("digest").ResourceVersion != before {
		t.Error("the schedule object was written")
	}

	list := func() *scheduleNotifyView {
		rec := f.act(f.as(tenancy.RoleViewer), http.MethodGet, "/api/schedules")
		var l []scheduleSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || len(l) != 1 || l[0].Notify == nil {
			t.Fatalf("list = %s", rec.Body)
		}
		return l[0].Notify
	}
	n := list()
	if n.Slack == nil || n.Slack.ChannelID != "C0CHOSEN" || n.Channel != "" || n.Override == nil || n.Override.Git.Channel != gitTeamChannel {
		t.Errorf("list says %+v", n)
	}
	store.fail = errors.New("down")
	if n := list(); n.Problem == "" {
		t.Errorf("an unreadable choice must be said: %+v", n)
	}
	store.fail = nil

	if rec := routed(f, f.as(tenancy.RoleAdmin), http.MethodDelete, ""); rec.Code != http.StatusOK || len(store.rows) != 0 {
		t.Fatalf("reset: %d %s %v", rec.Code, rec.Body, store.rows)
	}
	if n := list(); n.Channel != gitTeamChannel || n.Override != nil {
		t.Errorf("after reset the list says %+v", n)
	}
	if got := len(f.audit.events); got != 2 {
		t.Errorf("%d audit events, want 2 (chosen and reset): %+v", got, f.audit.events)
	}
}

func TestChoosingAChannelIsRefusedWhenItMakesNoSense(t *testing.T) {
	git := &agentorcav1alpha1.ScheduleNotify{Channel: gitTeamChannel}
	for name, tc := range map[string]struct {
		owner  string
		notify *agentorcav1alpha1.ScheduleNotify
		body   string
		code   int
		says   string
	}{
		"a UI schedule is edited itself":   {tenancy.OwnerUI, git, chosenBody, http.StatusConflict, "edited here"},
		"Git sends nothing":                {tenancy.OwnerGit, nil, chosenBody, http.StatusConflict, "add spec.result.notify"},
		"another tenant's app":             {tenancy.OwnerGit, git, `{"app":"their-bot","channelID":"C0CHOSEN"}`, http.StatusUnprocessableEntity, "no shared"},
		"a channel name instead of the id": {tenancy.OwnerGit, git, `{"app":"orca-bot","channelID":"#money"}`, http.StatusBadRequest, "channelID"},
		"more than a destination":          {tenancy.OwnerGit, git, `{"app":"orca-bot","channelID":"C0CHOSEN","includeOutput":true}`, http.StatusBadRequest, "body"},
		"a label on two lines":             {tenancy.OwnerGit, git, `{"app":"orca-bot","channelID":"C0CHOSEN","channelName":"a\nb"}`, http.StatusBadRequest, "one line"},
	} {
		t.Run(name, func(t *testing.T) {
			f, store := destinationFixture(t, tc.owner, tc.notify.DeepCopy())
			rec := routed(f, f.as(tenancy.RoleAdmin), http.MethodPut, tc.body)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.says) || len(store.rows) != 0 {
				t.Errorf("%d %s (saved %v), want %d containing %q", rec.Code, rec.Body, store.rows, tc.code, tc.says)
			}
		})
	}
}
