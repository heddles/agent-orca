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

func channelTenant(id string, names ...string) *agentorcav1alpha1.TenantConfig {
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-" + id[len(id)-3:], Namespace: "operator"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			TenantID: id, AllowedNamespaces: []string{sharedNS},
			Notifications: &agentorcav1alpha1.TenantNotifications{},
		},
	}
	for _, n := range names {
		tc.Spec.Notifications.Channels = append(tc.Spec.Notifications.Channels, agentorcav1alpha1.NotificationChannel{
			Name: n, Type: agentorcav1alpha1.NotificationChannelSlack,
			SecretRef: agentorcav1alpha1.NotificationSecretRef{Name: "hook-secret-" + n},
		})
	}
	return tc
}

func TestNotificationChannelsRoleMatrix(t *testing.T) {
	for _, role := range allRoles {
		f := newWriteFixture(t, channelTenant(isoA, "ops"))
		rec := f.act(f.as(role), http.MethodGet, "/api/notification-channels")
		if denied, want := isRoleDenial(rec), !role.Includes(tenancy.RoleBuilder); denied != want {
			t.Errorf("as %s: %d %s; role denial = %v, want %v", role, rec.Code, rec.Body, denied, want)
		}
	}
	f := newWriteFixture(t)
	if rec := f.act(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/notification-channels"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405: the list is read-only", rec.Code)
	}
}

func TestNotificationChannelsListNamesAndTypesOfTheCallersTenantOnly(t *testing.T) {
	f := newWriteFixture(t, channelTenant(isoA, "ops", "alerts"), channelTenant(isoB, "other-tenant"))
	rec := f.act(f.as(tenancy.RoleBuilder), http.MethodGet, "/api/notification-channels")
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	var got []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["name"] != "alerts" || got[1]["name"] != "ops" || got[0]["type"] != "slack" {
		t.Fatalf("channels = %v", got)
	}
	for _, c := range got {
		if len(c) != 2 {
			t.Errorf("a channel exposes %v: only name and type may leave", c)
		}
	}
	if body := rec.Body.String(); strings.Contains(body, "hook-secret") || strings.Contains(body, "other-tenant") {
		t.Errorf("the answer leaks a Secret name or another tenant's channel: %s", body)
	}
}

func TestNotificationsAreNotUIWritable(t *testing.T) {
	// TenantConfig is not a kind of the generic resource endpoints, so a builder or
	// admin cannot create or change one (or its channels) through the UI.
	if _, ok := crdKinds["tenantconfigs"]; ok {
		t.Fatal("tenantconfigs must not be a UI-writable kind")
	}
	f := newWriteFixture(t)
	for _, role := range allRoles {
		rec := f.do(f.as(role), http.MethodPost, "/api/resources/tenantconfigs", `{}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown resource kind") {
			t.Errorf("as %s: create tenantconfig = %d %s", role, rec.Code, rec.Body)
		}
	}
}
