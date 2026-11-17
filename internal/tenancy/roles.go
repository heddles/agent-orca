/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tenancy

import (
	"slices"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Role is what a caller may do inside a tenant. Roles are cumulative: each
// includes every role below it.
type Role string

// The roles, lowest to highest.
const (
	RoleViewer  Role = "viewer"
	RoleRunner  Role = "runner"
	RoleBuilder Role = "builder"
	RoleAdmin   Role = "admin"
)

// roleOrder is the roles lowest first.
var roleOrder = []Role{RoleViewer, RoleRunner, RoleBuilder, RoleAdmin}

// DefaultRole is the role of a signed-in user when a tenant sets no roles: the
// behaviour from before roles existed, where anyone in the tenant could run
// agents but not change resources.
const DefaultRole = RoleRunner

// ParseRole returns the role named s, or false.
func ParseRole(s string) (Role, bool) {
	r := Role(s)
	return r, slices.Contains(roleOrder, r)
}

// rank is the position of r in the order; -1 for an unknown role.
func (r Role) rank() int { return slices.Index(roleOrder, r) }

// Includes reports whether holding r grants the permissions of want.
func (r Role) Includes(want Role) bool {
	return r.rank() >= 0 && want.rank() >= 0 && r.rank() >= want.rank()
}

// Expand returns the role and every role below it, lowest first. An unknown
// role expands to nothing.
func (r Role) Expand() []string {
	i := r.rank()
	if i < 0 {
		return nil
	}
	out := make([]string, 0, i+1)
	for _, lower := range roleOrder[:i+1] {
		out = append(out, string(lower))
	}
	return out
}

// RolesFor resolves the roles of a signed-in user from their groups against a
// tenant's spec.roles. The result is cumulative and lowest first; empty means
// no access.
//
//   - roles unset: DefaultRole.
//   - a group in several role lists: the highest of them.
//   - no group listed: spec.roles.defaultRole, or no access when that is unset.
func RolesFor(spec *agentorcav1alpha1.TenantRoles, groups []string) []string {
	if spec == nil {
		return DefaultRole.Expand()
	}
	in := func(list []string) bool {
		return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(list, g) })
	}
	switch {
	case in(spec.Admin):
		return RoleAdmin.Expand()
	case in(spec.Builder):
		return RoleBuilder.Expand()
	case in(spec.Runner):
		return RoleRunner.Expand()
	case in(spec.Viewer):
		return RoleViewer.Expand()
	}
	if r, ok := ParseRole(spec.DefaultRole); ok {
		return r.Expand()
	}
	return nil
}

// MachineRoles is the roles of a caller with no user behind it (a service
// account token or an issued client-credentials token): the operator-wide
// machine default. An unknown value falls back to DefaultRole.
func MachineRoles(machineDefault string) []string {
	if r, ok := ParseRole(machineDefault); ok {
		return r.Expand()
	}
	return DefaultRole.Expand()
}
