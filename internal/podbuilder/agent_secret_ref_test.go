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

package podbuilder

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func agentWithSecrets(name string, refs ...agentorcav1alpha1.SecretMount) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: agentorcav1alpha1.AgentSpec{
			Runtime: agentorcav1alpha1.AgentRuntime{
				SecretRefs: refs,
			},
		},
	}
}

func TestResolveAgentSecretRefs(t *testing.T) {
	tests := []struct {
		name          string
		agent         *agentorcav1alpha1.Agent
		wantVol       int
		wantMount     int
		wantPath      string // a mount path that must exist
		wantNoSubPath bool   // mount should be whole-secret (SubPath=="")
	}{
		{name: "nil agent returns nothing", agent: nil, wantVol: 0, wantMount: 0},
		{name: "empty refs returns nothing", agent: agentWithSecrets("a"), wantVol: 0, wantMount: 0},
		{name: "mount path set", agent: agentWithSecrets("red-pwnbox",
			agentorcav1alpha1.SecretMount{Name: "htb-ovpn", MountPath: "/etc/htb"}),
			wantVol: 1, wantMount: 1, wantPath: "/etc/htb", wantNoSubPath: true},
		{name: "mount path defaults to AgentSecretMountDir", agent: agentWithSecrets("red-pwnbox",
			agentorcav1alpha1.SecretMount{Name: "htb-ovpn"}),
			wantVol: 1, wantMount: 1, wantPath: AgentSecretMountDir + "/htb-ovpn", wantNoSubPath: true},
		{name: "empty Name is skipped", agent: agentWithSecrets("red-pwnbox",
			agentorcav1alpha1.SecretMount{Name: "", MountPath: "/x"}),
			wantVol: 0, wantMount: 0},
		{name: "multiple refs each get a volume+mount", agent: agentWithSecrets("red-pwnbox",
			agentorcav1alpha1.SecretMount{Name: "htb-ovpn", MountPath: "/etc/htb"},
			agentorcav1alpha1.SecretMount{Name: "tls", MountPath: "/etc/tls"}),
			wantVol: 2, wantMount: 2, wantPath: "/etc/tls", wantNoSubPath: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vols, mounts := ResolveAgentSecretRefs(tt.agent)
			if len(vols) != tt.wantVol {
				t.Fatalf("volumes = %d, want %d (vols=%v)", len(vols), tt.wantVol, vols)
			}
			if len(mounts) != tt.wantMount {
				t.Fatalf("mounts = %d, want %d (mounts=%v)", len(mounts), tt.wantMount, mounts)
			}
			for _, m := range mounts {
				if !m.ReadOnly {
					t.Fatalf("agent secret mount %q must be read-only", m.MountPath)
				}
			}
			if tt.wantPath != "" {
				found := false
				for _, m := range mounts {
					if m.MountPath == tt.wantPath {
						found = true
						if tt.wantNoSubPath && m.SubPath != "" {
							t.Fatalf("agent secret mount at %q should be whole-secret (SubPath==\"\"), got %q", m.MountPath, m.SubPath)
						}
					}
				}
				if !found {
					t.Fatalf("expected a mount at %q, mounts=%v", tt.wantPath, mounts)
				}
			}
		})
	}
}
