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
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestRolesAreCumulative(t *testing.T) {
	tests := []struct {
		role Role
		want []string
	}{
		{RoleViewer, []string{"viewer"}},
		{RoleRunner, []string{"viewer", "runner"}},
		{RoleBuilder, []string{"viewer", "runner", "builder"}},
		{RoleAdmin, []string{"viewer", "runner", "builder", "admin"}},
		{Role("root"), nil},
	}
	for _, tt := range tests {
		if got := tt.role.Expand(); !slices.Equal(got, tt.want) {
			t.Errorf("%s expands to %v, want %v", tt.role, got, tt.want)
		}
	}
	if !RoleAdmin.Includes(RoleViewer) || !RoleBuilder.Includes(RoleBuilder) || RoleRunner.Includes(RoleBuilder) || Role("x").Includes(RoleViewer) {
		t.Error("Includes does not follow the order")
	}
}

func TestRolesFor(t *testing.T) {
	spec := &agentorcav1alpha1.TenantRoles{
		Viewer:  []string{"everyone"},
		Runner:  []string{"ops"},
		Builder: []string{"devs"},
		Admin:   []string{"platform", "devs-lead"},
	}
	withDefault := *spec
	withDefault.DefaultRole = "viewer"

	tests := []struct {
		name   string
		spec   *agentorcav1alpha1.TenantRoles
		groups []string
		want   []string
	}{
		{"roles unset gives runner", nil, nil, []string{"viewer", "runner"}},
		{"roles unset ignores groups", nil, []string{"platform"}, []string{"viewer", "runner"}},
		{"viewer group", spec, []string{"everyone"}, []string{"viewer"}},
		{"runner group", spec, []string{"ops"}, []string{"viewer", "runner"}},
		{"builder group", spec, []string{"devs"}, []string{"viewer", "runner", "builder"}},
		{"admin group", spec, []string{"platform"}, []string{"viewer", "runner", "builder", "admin"}},
		{"highest of several wins", spec, []string{"everyone", "devs", "ops"}, []string{"viewer", "runner", "builder"}},
		{"admin beats builder", spec, []string{"devs", "devs-lead"}, []string{"viewer", "runner", "builder", "admin"}},
		{"no match and no default is no access", spec, []string{"strangers"}, nil},
		{"no groups and no default is no access", spec, nil, nil},
		{"no match takes the default", &withDefault, []string{"strangers"}, []string{"viewer"}},
		{"a match beats the default", &withDefault, []string{"ops"}, []string{"viewer", "runner"}},
		{"empty roles block, default only", &agentorcav1alpha1.TenantRoles{DefaultRole: "builder"}, nil, []string{"viewer", "runner", "builder"}},
		{"unknown default is no access", &agentorcav1alpha1.TenantRoles{DefaultRole: "root"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RolesFor(tt.spec, tt.groups); !slices.Equal(got, tt.want) {
				t.Errorf("RolesFor = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMachineRoles(t *testing.T) {
	for in, want := range map[string][]string{
		"":        {"viewer", "runner"},
		"runner":  {"viewer", "runner"},
		"viewer":  {"viewer"},
		"admin":   {"viewer", "runner", "builder", "admin"},
		"garbage": {"viewer", "runner"},
	} {
		if got := MachineRoles(in); !slices.Equal(got, want) {
			t.Errorf("MachineRoles(%q) = %v, want %v", in, got, want)
		}
	}
}
