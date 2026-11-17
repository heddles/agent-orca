package apiserver

import "github.com/heddles/agent-orca/internal/tenancy"

// testAdminRoles is the role set of identities that tests build by hand and
// that are not about roles: every role, so no handler gate gets in the way.
var testAdminRoles = tenancy.RoleAdmin.Expand()
