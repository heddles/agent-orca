package apiserver

import (
	"context"
	"maps"
	"slices"
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// A federated bearer token's user and groups come through the tenant's claim
// mappings, so its holder gets roles from spec.roles like an interactive login.
func TestValidateFederatedTokenRoles(t *testing.T) {
	key := newTestKey(t)
	issuerURL := githubIssuerURL
	startOIDCIssuer(t, key, &issuerURL)

	tests := []struct {
		name        string
		roles       *agentorcav1alpha1.TenantRoles
		mappings    agentorcav1alpha1.FederatedClaimMappings
		machineRole string
		claims      map[string]any
		wantUser    string
		wantGroups  []string
		wantRoles   []string
	}{
		{
			name: "default mappings: groups claim picks the role", claims: map[string]any{"groups": []string{"devs"}},
			roles:    &agentorcav1alpha1.TenantRoles{Builder: []string{"devs"}, Admin: []string{"platform"}},
			wantUser: "test-subject", wantGroups: []string{"devs"}, wantRoles: []string{"viewer", "runner", "builder"},
		},
		{
			name: "custom mappings", claims: map[string]any{"email": "ada@example.com", "teams": []string{"platform"}},
			mappings: agentorcav1alpha1.FederatedClaimMappings{UserID: "email", Groups: "teams"},
			roles:    &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}},
			wantUser: "ada@example.com", wantGroups: []string{"platform"}, wantRoles: []string{"viewer", "runner", "builder", "admin"},
		},
		{
			name: "no groups claim falls back to defaultRole", claims: map[string]any{},
			roles:    &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}, DefaultRole: "viewer"},
			wantUser: "test-subject", wantRoles: []string{"viewer"},
		},
		{
			name: "no groups claim and no defaultRole is no access", claims: map[string]any{},
			roles:    &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}},
			wantUser: "test-subject", wantRoles: nil,
		},
		{
			name: "roles unset: everyone is a runner", claims: map[string]any{"groups": []string{"anything"}},
			wantUser: "test-subject", wantGroups: []string{"anything"}, wantRoles: []string{"viewer", "runner"},
		},
		{
			name: "no user claim is a machine caller", claims: map[string]any{"groups": []string{"platform"}},
			mappings:    agentorcav1alpha1.FederatedClaimMappings{UserID: "not-in-the-token"},
			roles:       &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}},
			machineRole: "builder", wantGroups: []string{"platform"}, wantRoles: []string{"viewer", "runner", "builder"},
		},
		{
			name: "no user claim, default machine role", claims: map[string]any{},
			mappings:  agentorcav1alpha1.FederatedClaimMappings{UserID: "not-in-the-token"},
			roles:     &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}},
			wantRoles: []string{"viewer", "runner"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := seedFederatedTenant(t, issuerURL)
			for _, tc := range a.tenantCache {
				tc.Spec.Roles = tt.roles
				tc.Spec.Federated.ClaimMappings = tt.mappings
			}
			if tt.machineRole != "" {
				if err := a.SetMachineDefaultRole(tt.machineRole); err != nil {
					t.Fatal(err)
				}
			}
			claims := map[string]any{"actor": "floppyfish14"}
			maps.Copy(claims, tt.claims)
			ident, err := a.validateFederatedToken(context.Background(), mintOIDCToken(t, key, issuerURL, "agent-orca-dev", claims))
			if err != nil {
				t.Fatal(err)
			}
			if ident.UserID != tt.wantUser || !slices.Equal(ident.Groups, tt.wantGroups) || !slices.Equal(ident.Roles, tt.wantRoles) {
				t.Errorf("user %q groups %v roles %v; want %q %v %v", ident.UserID, ident.Groups, ident.Roles, tt.wantUser, tt.wantGroups, tt.wantRoles)
			}
		})
	}
}
