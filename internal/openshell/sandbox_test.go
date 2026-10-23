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
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const routerHost = "agentorca-router-r1.agents.svc.cluster.local"

func TestSandboxName(t *testing.T) {
	// The gateway rejects names over 19 characters and non-DNS-1123 labels.
	dns1123 := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	name := SandboxName("a-very-long-namespace-name", "a-very-long-agent-run-name-abcde", 0)
	if len(name) > 19 || !dns1123.MatchString(name) {
		t.Fatalf("SandboxName = %q, want a DNS-1123 label of at most 19 chars", name)
	}
	if name != SandboxName("a-very-long-namespace-name", "a-very-long-agent-run-name-abcde", 0) {
		t.Fatal("SandboxName must be stable so Create is idempotent across reconciles")
	}
	if SandboxName("ns1", "run", 0) == SandboxName("ns2", "run", 0) {
		t.Fatal("same run name in different namespaces must not share a sandbox")
	}
	if SandboxName("ns", "run", 0) == SandboxName("ns", "run", 1) {
		t.Fatal("a retry must not reuse the previous attempt's sandbox name")
	}
}

func TestCompilePolicy_RouterOnlyByDefault(t *testing.T) {
	p := CompilePolicy(routerHost, &agentorcav1alpha1.SandboxSpec{Provider: agentorcav1alpha1.SandboxProviderOpenShell}, nil)

	if len(p.NetworkPolicies) != 1 {
		t.Fatalf("want only the router rule, got %v", p.NetworkPolicies)
	}
	rule := p.NetworkPolicies["agentorca-router"]
	if len(rule.Endpoints) != 1 {
		t.Fatalf("router rule endpoints = %+v", rule.Endpoints)
	}
	ep := rule.Endpoints[0]
	if ep.Host != routerHost || !reflect.DeepEqual(ep.Ports, []uint32{8080, 8082}) {
		t.Fatalf("router endpoint = %s %v, want %s [8080 8082]", ep.Host, ep.Ports, routerHost)
	}
	if ep.Protocol != "rest" || ep.Access != osv1.NetworkAccessPresetFull {
		t.Fatalf("router endpoint protocol/access = %s/%v", ep.Protocol, ep.Access)
	}
	if ep.Enforcement != osv1.NetworkEnforcementModeAudit {
		t.Fatalf("enforcement defaults to audit, got %v", ep.Enforcement)
	}
	// An empty binary list matches no binary, which would cut the agent off.
	if !reflect.DeepEqual(rule.Binaries, []osv1.PolicyNetworkBinary{{Path: "/**"}}) {
		t.Fatalf("router rule must apply to every binary, got %+v", rule.Binaries)
	}
	if p.Landlock == nil || p.Landlock.Compatibility != "hard_requirement" {
		t.Fatalf("Landlock must be a hard requirement, got %+v", p.Landlock)
	}
	if p.Filesystem == nil || !p.Filesystem.IncludeWorkdir {
		t.Fatalf("workdir must be writable, got %+v", p.Filesystem)
	}
}

func TestCompilePolicy_NilSandboxStillHasRouter(t *testing.T) {
	p := CompilePolicy(routerHost, nil, nil)
	if _, ok := p.NetworkPolicies["agentorca-router"]; !ok || len(p.NetworkPolicies) != 1 {
		t.Fatalf("got %v", p.NetworkPolicies)
	}
}

func TestCompilePolicy_Egress(t *testing.T) {
	p := CompilePolicy(routerHost, &agentorcav1alpha1.SandboxSpec{
		Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce,
		Egress: []agentorcav1alpha1.SandboxEgressRule{
			{Host: "pypi.org"},
			{Host: "github.com", Ports: []int32{443, 8443}, Access: agentorcav1alpha1.SandboxAccessReadWrite,
				Binaries: []string{"/usr/bin/git", "/usr/lib/git-core/git-remote-https"}},
			{Host: "*.example.com", Access: agentorcav1alpha1.SandboxAccessFull},
		},
	}, nil)

	if len(p.NetworkPolicies) != 4 {
		t.Fatalf("want router + 3 egress rules, got %d", len(p.NetworkPolicies))
	}
	for name, rule := range p.NetworkPolicies {
		if rule.Endpoints[0].Enforcement != osv1.NetworkEnforcementModeEnforce {
			t.Fatalf("rule %s: enforce must apply to every endpoint", name)
		}
	}

	pypi := p.NetworkPolicies["agentorca-egress-0"]
	if ep := pypi.Endpoints[0]; ep.Host != "pypi.org" || !reflect.DeepEqual(ep.Ports, []uint32{443}) ||
		ep.Access != osv1.NetworkAccessPresetReadOnly {
		t.Fatalf("default egress must be read-only on 443, got %+v", ep)
	}
	if !reflect.DeepEqual(pypi.Binaries, []osv1.PolicyNetworkBinary{{Path: "/**"}}) {
		t.Fatalf("egress without binaries must apply to every binary, got %+v", pypi.Binaries)
	}

	gh := p.NetworkPolicies["agentorca-egress-1"]
	if ep := gh.Endpoints[0]; !reflect.DeepEqual(ep.Ports, []uint32{443, 8443}) || ep.Access != osv1.NetworkAccessPresetReadWrite {
		t.Fatalf("github endpoint = %+v", ep)
	}
	if !reflect.DeepEqual(gh.Binaries, []osv1.PolicyNetworkBinary{
		{Path: "/usr/bin/git"}, {Path: "/usr/lib/git-core/git-remote-https"},
	}) {
		t.Fatalf("binaries not carried: %+v", gh.Binaries)
	}

	if p.NetworkPolicies["agentorca-egress-2"].Endpoints[0].Access != osv1.NetworkAccessPresetFull {
		t.Fatal("full access not carried")
	}
}

func TestCompilePolicy_Filesystem(t *testing.T) {
	fs := &agentorcav1alpha1.SandboxFilesystem{ReadOnly: []string{"/opt/venv"}, ReadWrite: []string{"/workspace"}}
	p := CompilePolicy(routerHost, &agentorcav1alpha1.SandboxSpec{Filesystem: fs}, nil)
	if !reflect.DeepEqual(p.Filesystem.ReadOnly, []string{"/opt/venv"}) ||
		!reflect.DeepEqual(p.Filesystem.ReadWrite, []string{"/workspace"}) {
		t.Fatalf("filesystem = %+v", p.Filesystem)
	}
	// The policy must not alias the Agent's slices.
	p.Filesystem.ReadOnly[0] = "/mutated"
	if fs.ReadOnly[0] != "/opt/venv" {
		t.Fatal("CompilePolicy aliased the Agent spec")
	}
}

func TestBuildSpec(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{Runtime: agentorcav1alpha1.AgentRuntime{
		OCIRef:  "ghcr.io/acme/coder:1.0",
		Command: []string{"python", "-m"},
		Args:    []string{"agent"},
		Sandbox: &agentorcav1alpha1.SandboxSpec{},
	}}}
	env := map[string]string{"AGENTORC_INPUT": "hi"}
	spec := BuildSpec(SpecInput{Agent: agent, RouterHost: routerHost, Env: env})

	if spec.Template == nil || spec.Template.Image != "ghcr.io/acme/coder:1.0" {
		t.Fatalf("template = %+v", spec.Template)
	}
	if !reflect.DeepEqual(spec.Command, []string{"python", "-m", "agent"}) {
		t.Fatalf("command = %v, want command followed by args", spec.Command)
	}
	if spec.RestartPolicy != osv1.SandboxRestartNever {
		t.Fatalf("restart policy = %v: the operator owns retries", spec.RestartPolicy)
	}
	if spec.Environment["AGENTORC_INPUT"] != "hi" || spec.Policy == nil {
		t.Fatalf("env/policy not set: %+v", spec)
	}
}

func TestObserve(t *testing.T) {
	code := func(c int32) *int32 { return &c }
	tests := []struct {
		name   string
		status types.SandboxStatus
		want   Outcome
		reason string
	}{
		{"provisioning", types.SandboxStatus{Phase: osv1.SandboxProvisioning}, OutcomeRunning, ""},
		{"ready", types.SandboxStatus{Phase: osv1.SandboxReady}, OutcomeRunning, ""},
		{"exit 0", types.SandboxStatus{Phase: osv1.SandboxCompleted, ExitCode: code(0)}, OutcomeSucceeded, ""},
		{"completed without exit code", types.SandboxStatus{Phase: osv1.SandboxCompleted}, OutcomeSucceeded, ""},
		{"nonzero exit", types.SandboxStatus{Phase: osv1.SandboxCompleted, ExitCode: code(2)}, OutcomeFailed, "exited with code 2"},
		{"error with condition", types.SandboxStatus{Phase: osv1.SandboxError, Conditions: []types.SandboxCondition{
			{Type: "Ready", Status: "False"},
			{Type: "MainProcessFailed", Status: "True", Reason: "MainProcessFailed", Message: "exit 1"},
		}}, OutcomeFailed, "MainProcessFailed: exit 1"},
		{"error reason skips unrelated true conditions", types.SandboxStatus{Phase: osv1.SandboxError, ExitCode: code(1), Conditions: []types.SandboxCondition{
			{Type: "PodScheduled", Status: "True", Reason: "PodScheduled"},
			{Type: "Ready", Status: "False", Reason: "RuntimeGenerationFailed", Message: "workload pod deleted"},
		}}, OutcomeFailed, "Ready (RuntimeGenerationFailed): workload pod deleted; main process exited with code 1"},
		{"error without detail", types.SandboxStatus{Phase: osv1.SandboxError}, OutcomeFailed, "unknown"},
		{"stopped externally", types.SandboxStatus{Phase: osv1.SandboxStopped}, OutcomeFailed, "before the agent finished"},
		{"policy rejected", types.SandboxStatus{Phase: osv1.SandboxProvisioning, ConfigurationAdmission: &types.SandboxConfigurationAdmission{
			State: types.ConfigurationAdmissionRejected, Error: "bad host",
		}}, OutcomeFailed, "policy rejected: bad host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Observe(&osv1.Sandbox{Status: tt.status})
			if got != tt.want || !strings.Contains(reason, tt.reason) {
				t.Fatalf("Observe = %v %q, want %v containing %q", got, reason, tt.want, tt.reason)
			}
		})
	}
}

func TestSDKGateway(t *testing.T) {
	ctx := context.Background()
	var gotNS, gotPod, gotContainer string
	gw := NewGateway(fake.NewClient().Sandboxes(), nil, "team-a", "openshell", func(_ context.Context, ns, pod, container string) (string, error) {
		gotNS, gotPod, gotContainer = ns, pod, container
		return "the answer", nil
	})

	labels := map[string]string{LabelRunName: "r1"}
	sb, err := gw.Create(ctx, "ao-1", &osv1.SandboxSpec{Template: &osv1.SandboxTemplate{Image: "img"}}, labels)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.Workspace != "team-a" || sb.Labels[LabelRunName] != "r1" {
		t.Fatalf("sandbox = %+v", sb)
	}
	// A reconcile retry re-creates: that must return the existing sandbox.
	if again, err := gw.Create(ctx, "ao-1", &osv1.SandboxSpec{}, labels); err != nil || again.Name != "ao-1" {
		t.Fatalf("idempotent Create = %v, %v", again, err)
	}

	sb.Status.AgentPod = "ao-1-workload"
	out, err := gw.MainProcessLogs(ctx, sb)
	if err != nil || out != "the answer" {
		t.Fatalf("MainProcessLogs = %q, %v", out, err)
	}
	if gotNS != "openshell" || gotPod != "ao-1-workload" || gotContainer != "agent" {
		t.Fatalf("read logs from %s/%s[%s]", gotNS, gotPod, gotContainer)
	}

	if err := gw.Delete(ctx, "ao-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := gw.Get(ctx, "ao-1"); !osv1.IsNotFound(err) {
		t.Fatalf("Get after Delete = %v, want NotFound", err)
	}
	if err := gw.Delete(ctx, "ao-1"); err != nil {
		t.Fatalf("deleting a missing sandbox must succeed, got %v", err)
	}
}

func TestSDKGateway_MainProcessLogsAfterFailure(t *testing.T) {
	var gotPod string
	gw := NewGateway(fake.NewClient().Sandboxes(), nil, "default", "openshell", func(_ context.Context, _, pod, _ string) (string, error) {
		gotPod = pod
		return "Traceback", nil
	})
	// A failed sandbox no longer reports its workload pod.
	if _, err := gw.MainProcessLogs(context.Background(), &osv1.Sandbox{Name: "ao-1"}); err != nil {
		t.Fatal(err)
	}
	if gotPod != "default--ao-1" {
		t.Fatalf("read logs from %q, want the driver's workload pod name default--ao-1", gotPod)
	}
}

func TestToolEndpoints(t *testing.T) {
	tool := &agentorcav1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "github"},
		Spec: agentorcav1alpha1.ToolSpec{
			Type: agentorcav1alpha1.ToolTypeMCP,
			MCPConfig: &agentorcav1alpha1.MCPConfig{
				Transport: "http",
				URL:       "https://api.githubcopilot.com/mcp/",
			},
			NetworkEgress: []agentorcav1alpha1.EgressRule{
				{Host: "API.GitHub.com", Port: 443},
				{Host: "api.github.com", Port: 443},                  // duplicate after lowercasing
				{Port: 5432},                                         // port-only: not expressible per host
				{Host: "dns.example.com", Port: 53, Protocol: "UDP"}, // sandbox policy is TCP
				{Host: "localhost", Port: 9000},                      // the router pod's own sidecar
			},
		},
	}
	got := ToolEndpoints(tool)
	want := []ToolEndpoint{
		{Tool: "github", Host: "api.github.com", Port: 443},
		{Tool: "github", Host: "api.githubcopilot.com", Port: 443},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolEndpoints = %+v, want %+v", got, want)
	}
}

func TestToolEndpoints_MCPURLPorts(t *testing.T) {
	cases := map[string]ToolEndpoint{
		"http://mcp-docs.tools.svc:8000/mcp": {Tool: "t", Host: "mcp-docs.tools.svc", Port: 8000},
		"http://mcp.example.com/sse":         {Tool: "t", Host: "mcp.example.com", Port: 80},
		"https://mcp.example.com":            {Tool: "t", Host: "mcp.example.com", Port: 443},
	}
	for u, want := range cases {
		tool := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "t"}, Spec: agentorcav1alpha1.ToolSpec{
			Type: agentorcav1alpha1.ToolTypeMCP, MCPConfig: &agentorcav1alpha1.MCPConfig{URL: u},
		}}
		if got := ToolEndpoints(tool); len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %+v, want %+v", u, got, want)
		}
	}
	stdio := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "t"}, Spec: agentorcav1alpha1.ToolSpec{
		Type: agentorcav1alpha1.ToolTypeMCP, MCPConfig: &agentorcav1alpha1.MCPConfig{URL: "http://127.0.0.1:3000"},
	}}
	if got := ToolEndpoints(stdio); len(got) != 0 {
		t.Errorf("loopback MCP URL must be skipped, got %+v", got)
	}
}

func TestFilterToolEndpoints(t *testing.T) {
	eps := []ToolEndpoint{
		{Tool: "github", Host: "api.github.com", Port: 443},
		{Tool: "github", Host: "uploads.github.com", Port: 443},
		{Tool: "jira", Host: "acme.atlassian.net", Port: 443},
	}
	f := false
	tests := []struct {
		name string
		sb   *agentorcav1alpha1.SandboxSpec
		want int
	}{
		{"inherit by default", &agentorcav1alpha1.SandboxSpec{}, 3},
		{"inherit with empty tools block", &agentorcav1alpha1.SandboxSpec{Tools: &agentorcav1alpha1.SandboxTools{}}, 3},
		{"opted out", &agentorcav1alpha1.SandboxSpec{Tools: &agentorcav1alpha1.SandboxTools{Inherit: &f}}, 0},
		{"exclude a tool", &agentorcav1alpha1.SandboxSpec{Tools: &agentorcav1alpha1.SandboxTools{ExcludeTools: []string{"jira"}}}, 2},
		{"exclude a host", &agentorcav1alpha1.SandboxSpec{Tools: &agentorcav1alpha1.SandboxTools{ExcludeHosts: []string{"Uploads.GitHub.com"}}}, 2},
		{"no sandbox", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FilterToolEndpoints(tt.sb, eps); len(got) != tt.want {
				t.Fatalf("got %+v, want %d endpoints", got, tt.want)
			}
		})
	}
	if len(eps) != 3 {
		t.Fatal("FilterToolEndpoints mutated its input")
	}
}

func TestCompilePolicy_Tools(t *testing.T) {
	p := CompilePolicy(routerHost, &agentorcav1alpha1.SandboxSpec{Enforcement: agentorcav1alpha1.SandboxEnforcementEnforce}, []ToolEndpoint{
		{Tool: "github", Host: "api.github.com", Port: 443},
		{Tool: "github", Host: "api.githubcopilot.com", Port: 443},
		{Tool: "docs", Host: "mcp-docs.tools.svc", Port: 8000},
	})
	gh, ok := p.NetworkPolicies["agentorca-tool-github"]
	if !ok || len(gh.Endpoints) != 2 {
		t.Fatalf("want one rule with both github endpoints, got %+v", p.NetworkPolicies)
	}
	for _, ep := range gh.Endpoints {
		if ep.Access != osv1.NetworkAccessPresetFull || ep.Enforcement != osv1.NetworkEnforcementModeEnforce || ep.Protocol != "rest" {
			t.Fatalf("tool endpoint = %+v", ep)
		}
	}
	if !reflect.DeepEqual(gh.Binaries, []osv1.PolicyNetworkBinary{{Path: "/**"}}) {
		t.Fatalf("tool rule binaries = %+v", gh.Binaries)
	}
	if docs := p.NetworkPolicies["agentorca-tool-docs"]; docs.Endpoints[0].Ports[0] != 8000 {
		t.Fatalf("docs rule = %+v", docs)
	}
}
