package apiserver

import (
	"net/http"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// notificationChannelView is a channel as the UI sees it: its name and type.
// Where it goes (the Secret) is never sent, and the channels themselves are set
// by an administrator on the TenantConfig, which the UI cannot write.
type notificationChannelView struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// handleNotificationChannels answers GET /api/notification-channels[?namespace=]
// with the names and types of the caller's tenant's channels, for the schedule
// builder's "Send the result to" choice. Builder role: only a person who can
// write a schedule needs the list.
func (s *UIServer) handleNotificationChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRole(w, r, tenancy.RoleBuilder) {
		return
	}
	var tcs agentorcav1alpha1.TenantConfigList
	if err := s.crdClient.List(r.Context(), &tcs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var tc *agentorcav1alpha1.TenantConfig
	if id, ok := TenantFromContext(r.Context()); ok && id != nil {
		for i := range tcs.Items {
			c := &tcs.Items[i]
			byID := id.TenantID != "" && tenancy.TenantIDOf(c) == id.TenantID
			byName := id.TenantID == "" && id.TenantName != "" && c.Name == id.TenantName // a legacy token has no ID
			if c.DeletionTimestamp.IsZero() && (byID || byName) {
				tc = c
			}
		}
	} else if ns := r.URL.Query().Get("namespace"); ns != "" {
		// No identity (authentication off): the tenant that claims the namespace.
		tc = tenancy.TenantConfigFor(tcs.Items, &metav1.ObjectMeta{Namespace: ns})
	}
	out := []notificationChannelView{}
	if tc != nil && tc.Spec.Notifications != nil {
		for _, c := range tc.Spec.Notifications.Channels {
			out = append(out, notificationChannelView{Name: c.Name, Type: c.Type})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	jsonResponse(w, out)
}
