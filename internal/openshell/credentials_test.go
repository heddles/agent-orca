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
	"encoding/json"
	"fmt"
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

func mcpTool(name, url string, auth *agentorcav1alpha1.MCPAuthConfig) *agentorcav1alpha1.Tool {
	return &agentorcav1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents"},
		Spec: agentorcav1alpha1.ToolSpec{
			Type:      agentorcav1alpha1.ToolTypeMCP,
			MCPConfig: &agentorcav1alpha1.MCPConfig{Transport: "http", URL: url, Auth: auth},
		},
	}
}

func ref(name, key string) agentorcav1alpha1.SecretKeyRef {
	return agentorcav1alpha1.SecretKeyRef{Name: name, Key: key}
}

func TestToolCredentials(t *testing.T) {
	bearer := ref("gh", "token")
	cred := ToolCredentials(mcpTool("github", "https://api.githubcopilot.com/mcp/", &agentorcav1alpha1.MCPAuthConfig{
		BearerToken: &bearer,
		APIKey:      &agentorcav1alpha1.APIKeyAuth{SecretKeyRef: ref("gh", "key")},
		Headers: []agentorcav1alpha1.AuthHeader{
			{Name: "X-Org-Id", SecretKeyRef: ref("gh", "org")},
			{Name: "authorization", SecretKeyRef: ref("gh", "other")}, // already set by bearerToken
		},
	}), "github-mcp")
	if cred == nil {
		t.Fatal("want a credential")
	}
	if cred.Source != "github-mcp" || cred.Tool != "github" || cred.Host != "api.githubcopilot.com" || cred.Port != 443 {
		t.Fatalf("cred = %+v", cred)
	}
	want := []CredentialKey{
		{EnvVar: "AGENTORCA_GITHUB_MCP_TOKEN", Header: "Authorization", Prefix: "Bearer ", SecretName: "gh", SecretKey: "token"},
		{EnvVar: "AGENTORCA_GITHUB_MCP_API_KEY", Header: "X-API-Key", SecretName: "gh", SecretKey: "key"},
		{EnvVar: "AGENTORCA_GITHUB_MCP_X_ORG_ID", Header: "X-Org-Id", SecretName: "gh", SecretKey: "org"},
	}
	if !reflect.DeepEqual(cred.Keys, want) {
		t.Fatalf("keys = %+v\nwant %+v", cred.Keys, want)
	}
	envRE := regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	for _, k := range cred.Keys {
		if !envRE.MatchString(k.EnvVar) {
			t.Errorf("env var %q is not a valid name", k.EnvVar)
		}
	}
}

func TestToolCredentials_NotStatic(t *testing.T) {
	bearer := ref("s", "k")
	crossNS := agentorcav1alpha1.SecretKeyRef{Name: "s", Key: "k", Namespace: "other"}
	tests := map[string]*agentorcav1alpha1.Tool{
		"no auth": mcpTool("t", "https://mcp.example.com", nil),
		"oauth rotates its token": mcpTool("t", "https://mcp.example.com", &agentorcav1alpha1.MCPAuthConfig{
			BearerToken: &bearer, OAuth: &agentorcav1alpha1.MCPOAuthConfig{},
		}),
		"loopback sidecar": mcpTool("t", "http://localhost:3000", &agentorcav1alpha1.MCPAuthConfig{BearerToken: &bearer}),
		"stdio":            mcpTool("t", "", &agentorcav1alpha1.MCPAuthConfig{BearerToken: &bearer}),
		"secret in another namespace": mcpTool("t", "https://mcp.example.com", &agentorcav1alpha1.MCPAuthConfig{
			BearerToken: &crossNS,
		}),
		"not an MCP tool": {ObjectMeta: metav1.ObjectMeta{Name: "t"}},
	}
	for name, tool := range tests {
		t.Run(name, func(t *testing.T) {
			if cred := ToolCredentials(tool, "t"); cred != nil {
				t.Fatalf("want no credential, got %+v", cred)
			}
		})
	}
}

func TestProfile(t *testing.T) {
	cred := &ToolCredential{Source: "github", Keys: []CredentialKey{
		{EnvVar: "AGENTORCA_GITHUB_TOKEN", Header: "Authorization", Prefix: "Bearer "},
		{EnvVar: "AGENTORCA_GITHUB_X_ORG_ID", Header: "X-Org-Id"},
	}}
	p := Profile("agents", cred)
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(p.ID) || len(p.ID) > 64 {
		t.Fatalf("profile id %q is not valid kebab-case of at most 64 chars", p.ID)
	}
	if p.ID == Profile("other-ns", cred).ID {
		t.Fatal("namespaces must not share a profile")
	}
	// Endpointless: the sandbox policy's credential_binding is the only boundary.
	if len(p.Endpoints) != 0 || len(p.Binaries) != 0 {
		t.Fatalf("profile must be endpointless, got %+v %+v", p.Endpoints, p.Binaries)
	}
	if len(p.Credentials) != 2 {
		t.Fatalf("credentials = %+v", p.Credentials)
	}
	tok := p.Credentials[0]
	if !reflect.DeepEqual(tok.EnvVars, []string{"AGENTORCA_GITHUB_TOKEN"}) || !tok.Secret || tok.AuthStyle != "bearer" {
		t.Fatalf("token credential = %+v", tok)
	}
	if org := p.Credentials[1]; org.AuthStyle != "header" || org.HeaderName != "x-org-id" {
		t.Fatalf("header credential = %+v", org)
	}
}

func TestProviderNameStableAcrossRetries(t *testing.T) {
	name := ProviderName("ns", "run", "github")
	if name != ProviderName("ns", "run", "github") {
		t.Fatal("provider name must be stable so retries reuse it")
	}
	if ProviderName("ns", "run-1", "github") == ProviderName("ns", "run-2", "github") {
		t.Fatal("runs must not share a provider: each is revoked when its run ends")
	}
}

func TestBindCredentials(t *testing.T) {
	gh := &ToolCredential{Source: "github", Tool: "github", Host: "api.githubcopilot.com", Port: 443}
	other := &ToolCredential{Source: "other", Tool: "other", Host: "api.githubcopilot.com", Port: 443}
	gone := &ToolCredential{Source: "jira", Tool: "jira", Host: "acme.atlassian.net", Port: 443}
	eps := []ToolEndpoint{
		{Tool: "github", Host: "api.githubcopilot.com", Port: 443},
		{Tool: "github", Host: "api.github.com", Port: 443},
		{Tool: "other", Host: "api.githubcopilot.com", Port: 443},
	}
	name := func(c *ToolCredential) string { return "p-" + c.Source }

	got, used, err := BindCredentials(eps, []*ToolCredential{gh, other, gone}, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 1 || used[0] != gh {
		t.Fatalf("used = %+v: the first source binds, an excluded host's credential is dropped", used)
	}
	for _, ep := range got {
		want := ""
		if ep.Host == "api.githubcopilot.com" {
			want = "p-github"
		}
		if ep.CredentialProvider != want {
			t.Errorf("%s:%d bound to %q, want %q", ep.Host, ep.Port, ep.CredentialProvider, want)
		}
	}
	if eps[0].CredentialProvider != "" {
		t.Fatal("BindCredentials mutated its input")
	}
}

func TestBindCredentials_ProviderLimit(t *testing.T) {
	eps := make([]ToolEndpoint, 0, maxProviders+1)
	creds := make([]*ToolCredential, 0, maxProviders+1)
	for i := range maxProviders + 1 {
		host := fmt.Sprintf("mcp%d.example.com", i)
		eps = append(eps, ToolEndpoint{Tool: host, Host: host, Port: 443})
		creds = append(creds, &ToolCredential{Source: host, Tool: host, Host: host, Port: 443})
	}
	if _, _, err := BindCredentials(eps, creds, func(c *ToolCredential) string { return c.Source }); err == nil {
		t.Fatal("want an error past the gateway's provider limit")
	}
}

func TestCompilePolicy_CredentialBinding(t *testing.T) {
	p := CompilePolicy(routerHost, &agentorcav1alpha1.SandboxSpec{
		Egress: []agentorcav1alpha1.SandboxEgressRule{
			{Host: "API.githubcopilot.com"},                            // same host:port as the binding
			{Host: "api.githubcopilot.com", Ports: []int32{443, 8443}}, // keeps 8443 only
		},
	}, []ToolEndpoint{
		{Tool: "github", Host: "api.githubcopilot.com", Port: 443, CredentialProvider: "p-github"},
		{Tool: "other", Host: "api.githubcopilot.com", Port: 443}, // would disagree on credentials
	})

	gh := p.NetworkPolicies["agentorca-tool-github"].Endpoints
	if len(gh) != 1 || gh[0].CredentialBinding == nil || gh[0].CredentialBinding.Provider != "p-github" {
		t.Fatalf("github endpoint = %+v", gh)
	}
	if _, ok := p.NetworkPolicies["agentorca-tool-other"]; ok {
		t.Fatal("an unbound endpoint on a credentialed host:port must be dropped")
	}
	if _, ok := p.NetworkPolicies["agentorca-egress-0"]; ok {
		t.Fatal("an egress rule fully covered by the binding must be dropped")
	}
	if ports := p.NetworkPolicies["agentorca-egress-1"].Endpoints[0].Ports; !reflect.DeepEqual(ports, []uint32{8443}) {
		t.Fatalf("egress ports = %v, want only the unbound port", ports)
	}
}

func TestDescribeCredentials(t *testing.T) {
	if DescribeCredentials(nil) != "" {
		t.Fatal("no credentials, no env var")
	}
	out := DescribeCredentials([]*ToolCredential{{
		Source: "github", URL: "https://api.githubcopilot.com/mcp/",
		Keys: []CredentialKey{
			{EnvVar: "AGENTORCA_GITHUB_TOKEN", Header: "Authorization", Prefix: "Bearer ", SecretName: "gh", SecretKey: "token"},
		},
	}})
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{
		"tool": "github", "url": "https://api.githubcopilot.com/mcp/",
		"headers": map[string]any{"Authorization": "Bearer ${AGENTORCA_GITHUB_TOKEN}"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor = %s", out)
	}
	if strings.Contains(out, `"gh"`) {
		t.Fatal("descriptor must not leak secret references")
	}
}

// stubProfiles implements the profile calls EnsureProfile makes; the SDK fake
// leaves them unimplemented.
type stubProfiles struct {
	osv1.ProfileInterface
	profiles map[string]*osv1.ProviderProfile
	imports  int
	updates  int
	reject   bool
}

func (s *stubProfiles) Get(_ context.Context, _, id string) (*osv1.ProviderProfile, error) {
	if p, ok := s.profiles[id]; ok {
		return p, nil
	}
	return nil, &types.StatusError{Code: types.ErrorNotFound, Message: "not found"}
}

func (s *stubProfiles) Import(_ context.Context, _ string, items []osv1.ProfileImportItem) (*osv1.ImportResult, error) {
	s.imports++
	if s.reject {
		return &osv1.ImportResult{Diagnostics: []osv1.ProfileDiagnostic{{Field: "credentials", Message: "bad"}}}, nil
	}
	p := items[0].Profile
	p.ResourceVersion = 1
	s.profiles[p.ID] = &p
	return &osv1.ImportResult{Imported: true}, nil
}

func (s *stubProfiles) Update(_ context.Context, _, id string, rv uint64, item osv1.ProfileImportItem) (*osv1.UpdateResult, error) {
	s.updates++
	if s.profiles[id].ResourceVersion != rv {
		return nil, &types.StatusError{Code: types.ErrorInvalidArgument, Message: "stale"}
	}
	p := item.Profile
	p.ResourceVersion = rv + 1
	s.profiles[id] = &p
	return &osv1.UpdateResult{Updated: true}, nil
}

type stubProviders struct {
	osv1.ProviderInterface
	profiles *stubProfiles
}

func (s stubProviders) Profiles() osv1.ProfileInterface { return s.profiles }

func TestSDKGateway_EnsureProfile(t *testing.T) {
	ctx := context.Background()
	profiles := &stubProfiles{profiles: map[string]*osv1.ProviderProfile{}}
	gw := NewGateway(nil, stubProviders{ProviderInterface: fake.NewClient().Providers(), profiles: profiles}, "default", "openshell", nil)

	cred := &ToolCredential{Source: "github", Keys: []CredentialKey{{EnvVar: "AGENTORCA_GITHUB_TOKEN", Header: "Authorization", Prefix: "Bearer "}}}
	if err := gw.EnsureProfile(ctx, Profile("agents", cred)); err != nil {
		t.Fatal(err)
	}
	if err := gw.EnsureProfile(ctx, Profile("agents", cred)); err != nil {
		t.Fatal(err)
	}
	if profiles.imports != 1 || profiles.updates != 0 {
		t.Fatalf("imports=%d updates=%d: an unchanged profile must not be rewritten", profiles.imports, profiles.updates)
	}

	cred.Keys = append(cred.Keys, CredentialKey{EnvVar: "AGENTORCA_GITHUB_API_KEY", Header: "X-API-Key"})
	if err := gw.EnsureProfile(ctx, Profile("agents", cred)); err != nil {
		t.Fatal(err)
	}
	if profiles.updates != 1 || len(profiles.profiles[ProfileID("agents", "github")].Credentials) != 2 {
		t.Fatal("a changed tool auth config must update the profile")
	}

	profiles.reject = true
	if err := gw.EnsureProfile(ctx, Profile("agents", &ToolCredential{Source: "new", Keys: cred.Keys})); err == nil ||
		!strings.Contains(err.Error(), "credentials: bad") {
		t.Fatalf("a rejected import must surface its diagnostics, got %v", err)
	}
}

func TestSDKGateway_EnsureProvider(t *testing.T) {
	ctx := context.Background()
	providers := fake.NewClient().Providers()
	gw := NewGateway(nil, providers, "default", "openshell", nil)
	cred := &ToolCredential{Source: "github"}

	labels := map[string]string{LabelRunName: "run-1"}
	if err := gw.EnsureProvider(ctx, Provider("agents", "run-1", cred, map[string]string{"K": "v1"}, labels)); err != nil {
		t.Fatal(err)
	}
	// A retry with a rotated secret refreshes the value in place.
	if err := gw.EnsureProvider(ctx, Provider("agents", "run-1", cred, map[string]string{"K": "v2"}, labels)); err != nil {
		t.Fatal(err)
	}
	p, err := providers.Get(ctx, "default", ProviderName("agents", "run-1", "github"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Credentials["K"] != "v2" || p.Type != ProfileID("agents", "github") || p.Labels[LabelRunName] != "run-1" ||
		p.Spec.ProfileWorkspace != "default" {
		t.Fatalf("provider = %+v", p)
	}
}
