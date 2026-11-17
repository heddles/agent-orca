package apiserver

import (
	"net/http"
)

// meTenant is the tenant block of GET /api/me.
type meTenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// meWorkspace is one namespace the caller may use. The UI calls namespaces
// workspaces.
type meWorkspace struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Primary   bool   `json:"primary"`
}

// meResponse is the body of GET /api/me.
type meResponse struct {
	UserID     string        `json:"userId"`
	Groups     []string      `json:"groups"`
	Tenant     *meTenant     `json:"tenant"`
	Workspaces []meWorkspace `json:"workspaces"`
	// Roles are the caller's roles inside the tenant, cumulative and lowest first
	// (an admin lists viewer, runner, builder and admin). Empty means the
	// tenant's spec.roles give the caller no access.
	Roles []string `json:"roles"`
}

// handleMe answers GET /api/me with who the caller is: user, groups, tenant and
// the namespaces they may use. A caller without a tenant identity (the UI
// proxy's service-account session, or authentication disabled) gets a null
// tenant and no workspaces.
func (s *UIServer) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out := meResponse{Groups: []string{}, Workspaces: []meWorkspace{}, Roles: []string{}}
	if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		out.UserID = t.UserID
		out.Tenant = &meTenant{ID: t.TenantID, Name: t.TenantName}
		if t.Groups != nil {
			out.Groups = t.Groups
		}
		if t.Roles != nil {
			out.Roles = t.Roles
		}
		for i, ns := range tenantNamespaces(t) {
			out.Workspaces = append(out.Workspaces, meWorkspace{Name: ns, Namespace: ns, Primary: i == 0})
		}
	}
	jsonResponse(w, out)
}
