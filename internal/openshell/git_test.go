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

package openshell

import (
	"slices"
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func gitRules(t *testing.T, push bool) []string {
	t.Helper()
	sb := &agentorcav1alpha1.SandboxSpec{
		Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce,
		Egress: []agentorcav1alpha1.SandboxEgressRule{
			{Host: "github.com", Ports: []int32{443, 22}},
			{Host: "proxy.golang.org"},
		},
	}
	p := CompilePolicy(routerHost, sb, nil)
	ApplyGitAccess(p, &GitAccess{Repositories: []string{"heddles/agent-orca"}, Push: push, Provider: "prov"})

	rule, ok := p.NetworkPolicies[gitPolicyName]
	if !ok || len(rule.Endpoints) != 1 {
		t.Fatalf("git rule = %+v", rule)
	}
	ep := rule.Endpoints[0]
	if ep.Host != "github.com" || !slices.Equal(ep.Ports, []uint32{443}) || ep.CredentialBinding.Provider != "prov" ||
		ep.Enforcement != osv1.NetworkEnforcementModeEnforce {
		t.Errorf("git endpoint = %+v", ep)
	}
	if len(rule.Binaries) == 0 || rule.Binaries[0].Path != "/usr/bin/git" {
		t.Errorf("git binaries = %+v", rule.Binaries)
	}
	// The plain egress rule keeps github.com:22 but gives up :443.
	for name, r := range p.NetworkPolicies {
		if name == gitPolicyName {
			continue
		}
		for _, e := range r.Endpoints {
			if e.Host == "github.com" && slices.Contains(e.Ports, 443) {
				t.Errorf("rule %s still allows github.com:443 without the credential", name)
			}
		}
	}
	out := make([]string, 0, len(ep.Rules))
	for _, r := range ep.Rules {
		out = append(out, r.Allow.Method+" "+r.Allow.Path)
	}
	return out
}

func TestApplyGitAccess(t *testing.T) {
	read := gitRules(t, false)
	for _, want := range []string{
		"GET /heddles/agent-orca/info/refs", "POST /heddles/agent-orca.git/git-upload-pack",
	} {
		if !slices.Contains(read, want) {
			t.Errorf("read-only rules %v lack %q", read, want)
		}
	}
	for _, r := range read {
		if r == "POST /heddles/agent-orca/git-receive-pack" || r == "POST /heddles/agent-orca.git/git-receive-pack" {
			t.Errorf("read-only access allows push: %q", r)
		}
	}
	if push := gitRules(t, true); !slices.Contains(push, "POST /heddles/agent-orca.git/git-receive-pack") {
		t.Errorf("push rules %v lack git-receive-pack", push)
	}
}

func TestGitEnvNeverHoldsTheToken(t *testing.T) {
	env := GitEnv()
	if env["GIT_CONFIG_KEY_1"] != "credential.https://github.com.helper" || env["GIT_CONFIG_VALUE_0"] != "" {
		t.Errorf("helper config = %v", env)
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Error("git may prompt for credentials")
	}
}
