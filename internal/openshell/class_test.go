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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// sandboxPool is the node pool the test class places sandboxes on.
const sandboxPool = "sandbox"

func classAgent() *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{Runtime: agentorcav1alpha1.AgentRuntime{
		OCIRef: "img",
		Sandbox: &agentorcav1alpha1.SandboxSpec{
			Enforcement:   agentorcav1alpha1.SandboxEnforcementAudit,
			EgressPresets: []agentorcav1alpha1.SandboxEgressPreset{agentorcav1alpha1.SandboxEgressPresetNPM},
			Egress:        []agentorcav1alpha1.SandboxEgressRule{{Host: "proxy.golang.org", Binaries: []string{"/usr/local/go/bin/go"}}},
			Filesystem:    &agentorcav1alpha1.SandboxFilesystem{ReadOnly: []string{"/opt/agent"}},
		},
	}}}
}

func hosts(rules []agentorcav1alpha1.SandboxEgressRule) map[string]agentorcav1alpha1.SandboxEgressRule {
	m := map[string]agentorcav1alpha1.SandboxEgressRule{}
	for _, r := range rules {
		m[r.Host] = r
	}
	return m
}

func TestApplyClass(t *testing.T) {
	class := &agentorcav1alpha1.SandboxClass{Spec: agentorcav1alpha1.SandboxClassSpec{
		Enforcement:   agentorcav1alpha1.SandboxEnforcementEnforce,
		EgressPresets: []agentorcav1alpha1.SandboxEgressPreset{agentorcav1alpha1.SandboxEgressPresetGo},
		Egress:        []agentorcav1alpha1.SandboxEgressRule{{Host: "internal.example.com"}},
		Filesystem:    &agentorcav1alpha1.SandboxFilesystem{ReadOnly: []string{"/opt/tools"}},
		Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		}},
		NodeSelector: map[string]string{"pool": sandboxPool},
		Tolerations:  []corev1.Toleration{{Key: "openshell", Operator: corev1.TolerationOpExists}},
	}}
	agent := classAgent()
	operator := Placement{NodeSelector: map[string]string{"pool": "default"}}

	got, placement := ApplyClass(agent, class, operator)
	sb := got.Spec.Runtime.Sandbox

	if sb.Enforcement != agentorcav1alpha1.SandboxEnforcementEnforce {
		t.Errorf("enforcement = %s: an Agent cannot weaken its class", sb.Enforcement)
	}
	if len(sb.EgressPresets) != 0 {
		t.Errorf("presets not expanded: %v", sb.EgressPresets)
	}
	h := hosts(sb.Egress)
	for _, want := range []string{"internal.example.com", "proxy.golang.org", "sum.golang.org", "registry.npmjs.org"} {
		if _, ok := h[want]; !ok {
			t.Errorf("egress lacks %s: %v", want, sb.Egress)
		}
	}
	// The class's go preset and the Agent's binary-pinned proxy.golang.org rule
	// merge into one rule that allows any binary, as the preset does.
	if len(h["proxy.golang.org"].Binaries) != 0 {
		t.Errorf("merged proxy.golang.org rule = %+v, want any binary", h["proxy.golang.org"])
	}
	if n := countHost(sb.Egress, "proxy.golang.org"); n != 1 {
		t.Errorf("proxy.golang.org appears %d times, want one merged rule", n)
	}
	if fs := sb.Filesystem; fs == nil || len(fs.ReadOnly) != 2 {
		t.Errorf("filesystem = %+v, want the class's and the Agent's paths", fs)
	}
	if got.Spec.Resources == nil || got.Spec.Resources.Limits.Memory().String() != "8Gi" {
		t.Errorf("resources = %+v, want the class default", got.Spec.Resources)
	}
	if placement.NodeSelector["pool"] != sandboxPool || len(placement.Tolerations) != 1 {
		t.Errorf("placement = %+v, want the class's", placement)
	}
	if agent.Spec.Runtime.Sandbox.Enforcement != agentorcav1alpha1.SandboxEnforcementAudit || len(agent.Spec.Runtime.Sandbox.EgressPresets) != 1 {
		t.Error("ApplyClass modified the Agent passed in")
	}
}

func TestApplyClassKeepsAgentResourcesAndOperatorPlacement(t *testing.T) {
	agent := classAgent()
	agent.Spec.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}
	class := &agentorcav1alpha1.SandboxClass{Spec: agentorcav1alpha1.SandboxClassSpec{
		Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}},
	}}
	operator := Placement{NodeSelector: map[string]string{"pool": "default"}}
	got, placement := ApplyClass(agent, class, operator)
	if got.Spec.Resources.Limits.Memory().String() != "2Gi" {
		t.Errorf("Agent resources overridden: %v", got.Spec.Resources)
	}
	if placement.NodeSelector["pool"] != "default" {
		t.Errorf("placement = %+v, want the operator's when the class sets none", placement)
	}
}

func TestApplyClassWithoutClassExpandsPresets(t *testing.T) {
	got, _ := ApplyClass(classAgent(), nil, Placement{})
	if _, ok := hosts(got.Spec.Runtime.Sandbox.Egress)["registry.npmjs.org"]; !ok {
		t.Errorf("Agent preset not expanded without a class: %v", got.Spec.Runtime.Sandbox.Egress)
	}
	if got.Spec.Runtime.Sandbox.Enforcement != agentorcav1alpha1.SandboxEnforcementAudit {
		t.Error("enforcement changed without a class")
	}
}

func TestMergeEgressBroadensAccess(t *testing.T) {
	out := mergeEgress([]agentorcav1alpha1.SandboxEgressRule{
		{Host: "github.com"},
		{Host: "GitHub.com", Ports: []int32{443}, Access: agentorcav1alpha1.SandboxAccessReadWrite},
		{Host: "github.com", Ports: []int32{22}},
	})
	if len(out) != 2 {
		t.Fatalf("merged = %+v, want github.com:443 and github.com:22", out)
	}
	if out[0].Access != agentorcav1alpha1.SandboxAccessReadWrite {
		t.Errorf("merged access = %s, want the broader read-write", out[0].Access)
	}
}

func TestPresetsAreValidEgress(t *testing.T) {
	for name, rules := range presets {
		if len(rules) == 0 {
			t.Errorf("preset %s is empty", name)
		}
		for _, r := range rules {
			if r.Host == "" || len(r.Binaries) != 0 {
				t.Errorf("preset %s rule %+v", name, r)
			}
		}
	}
}

func TestPresetEgress(t *testing.T) {
	tests := []struct {
		name     string
		preset   agentorcav1alpha1.SandboxEgressPreset
		wantHost string
	}{
		{name: "go", preset: agentorcav1alpha1.SandboxEgressPresetGo, wantHost: "proxy.golang.org"},
		{name: "npm", preset: agentorcav1alpha1.SandboxEgressPresetNPM},
		{name: "pypi", preset: agentorcav1alpha1.SandboxEgressPresetPyPI},
		{name: "github-read", preset: agentorcav1alpha1.SandboxEgressPresetGitHubRead},
		// A literal, not a constant: registering a real preset of this name
		// must revisit this test.
		{name: "unknown", preset: agentorcav1alpha1.SandboxEgressPreset("no-such-preset")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := PresetEgress(tt.preset)
			if tt.name == "unknown" {
				if len(rules) != 0 {
					t.Errorf("PresetEgress(%q) = %+v, want no rules for an unknown preset", tt.preset, rules)
				}
				return
			}
			if len(rules) == 0 {
				t.Fatalf("PresetEgress(%q) returned no rules", tt.preset)
			}
			if tt.wantHost != "" {
				if _, ok := hosts(rules)[tt.wantHost]; !ok {
					t.Errorf("PresetEgress(%q) lacks %s: %+v", tt.preset, tt.wantHost, rules)
				}
			}
		})
	}

	// PresetEgress must return a copy: mutating one result cannot leak into
	// the presets map, so a later call still sees the original rules.
	t.Run("copy", func(t *testing.T) {
		want := hosts(PresetEgress(agentorcav1alpha1.SandboxEgressPresetGo))
		rules := PresetEgress(agentorcav1alpha1.SandboxEgressPresetGo)
		rules[0].Host = "mutated.example.com"
		rules = append(rules, agentorcav1alpha1.SandboxEgressRule{Host: "extra.example.com"})
		got := hosts(PresetEgress(agentorcav1alpha1.SandboxEgressPresetGo))
		for h := range want {
			if _, ok := got[h]; !ok {
				t.Errorf("preset go lost %s after an earlier result was mutated: %v", h, got)
			}
		}
		for _, bad := range []string{"mutated.example.com", "extra.example.com"} {
			if _, ok := got[bad]; ok {
				t.Errorf("preset go kept a mutation: %s in %+v", bad, got)
			}
		}
	})
}

func countHost(rules []agentorcav1alpha1.SandboxEgressRule, host string) int {
	n := 0
	for _, r := range rules {
		if r.Host == host {
			n++
		}
	}
	return n
}
