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

// Git access to GitHub with a GitHub App installation token. The operator
// mints a short-lived token narrowed to the Agent's repositories and stores it
// in a per-run provider; the workload holds only a placeholder. git asks its
// credential helper for a password, gets the placeholder, and sends it in a
// Basic Authorization header; the supervisor decodes Basic credentials and
// swaps in the real token, but only on github.com:443, for the git binary, and
// for the listed repositories' paths. Pushing to protected branches is still
// governed by the repository's rulesets on GitHub.

import (
	"strings"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
)

const (
	// GitHubHost is the git transport host the token is bound to.
	GitHubHost = "github.com"
	// GitTokenEnvVar holds the token placeholder in the workload.
	GitTokenEnvVar = "AGENTORCA_GIT_TOKEN"
	// gitPolicyName is the sandbox policy rule for the git endpoint.
	gitPolicyName = "agentorca-git-github"
	// gitSource is the credential source name for profiles and providers.
	gitSource = "github-git"
)

// gitBinaries are the processes the git endpoint admits. git's HTTPS helper
// (git-remote-https) matches through its parent.
var gitBinaries = []string{"/usr/bin/git", "/usr/local/bin/git"}

// GitAccess grants a sandbox git access to GitHub repositories.
type GitAccess struct {
	// Repositories are "owner/name" slugs.
	Repositories []string
	// Push allows git push (git-receive-pack) as well as clone and fetch.
	Push bool
	// Provider is the run's provider holding the token.
	Provider string
}

// GitProfileID is the gateway profile declaring the git token env var. It is
// shared by every run in the namespace.
func GitProfileID(namespace string) string {
	return ProfileID(namespace, gitSource)
}

// GitProviderName is the run's provider instance holding its git token.
func GitProviderName(namespace, runName string) string {
	return ProviderName(namespace, runName, gitSource)
}

// GitProfile declares the token as a Basic-auth credential. Like the tool
// profiles it has no endpoints: the sandbox policy's credential binding is the
// only place the token may go.
func GitProfile(namespace string) *osv1.ProviderProfile {
	return &osv1.ProviderProfile{
		ID:          GitProfileID(namespace),
		DisplayName: "agent-orca git (GitHub) " + namespace,
		Description: "GitHub App installation token for git over HTTPS, managed by the agent-orca operator",
		Category:    types.ProfileCategoryOther,
		Annotations: map[string]string{
			"agentorca.io/namespace": namespace,
			"agentorca.io/source":    gitSource,
		},
		Credentials: []osv1.ProfileCredential{{
			Name:        strings.ToLower(GitTokenEnvVar),
			Description: "GitHub App installation token (git password for x-access-token)",
			EnvVars:     []string{GitTokenEnvVar},
			Required:    true,
			Secret:      true,
			AuthStyle:   "basic",
			HeaderName:  "authorization",
		}},
	}
}

// GitProvider is the run's provider holding token. Updating it with a fresh
// token reaches the running sandbox: each git command is a new process and
// resolves the placeholder to the provider's current value.
func GitProvider(namespace, runName, token string, labels map[string]string) *osv1.Provider {
	return &osv1.Provider{
		Name:   GitProviderName(namespace, runName),
		Type:   GitProfileID(namespace),
		Labels: labels,
		Spec:   osv1.ProviderSpec{Credentials: map[string]string{GitTokenEnvVar: token}},
	}
}

// GitEnv configures git, through its GIT_CONFIG_* environment, to answer
// github.com credential prompts with x-access-token and the placeholder. It
// replaces any other helper for github.com so nothing prompts or caches.
func GitEnv() map[string]string {
	return map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "credential.https://" + GitHubHost + ".helper",
		"GIT_CONFIG_VALUE_0": "",
		"GIT_CONFIG_KEY_1":   "credential.https://" + GitHubHost + ".helper",
		"GIT_CONFIG_VALUE_1": `!f() { test "$1" = get && echo username=x-access-token && echo "password=$` +
			GitTokenEnvVar + `"; }; f`,
		"GIT_TERMINAL_PROMPT": "0",
	}
}

// ApplyGitAccess adds the credential-bound git endpoint to policy. A
// destination may have only one endpoint, so any plain egress rule for
// github.com:443 gives way to it.
func ApplyGitAccess(policy *osv1.SandboxPolicy, git *GitAccess) {
	if policy == nil || git == nil || len(git.Repositories) == 0 {
		return
	}
	// The router rule always exists and carries the sandbox's enforcement mode.
	enforcement := osv1.NetworkEnforcementModeAudit
	if r, ok := policy.NetworkPolicies["agentorca-router"]; ok && len(r.Endpoints) > 0 {
		enforcement = r.Endpoints[0].Enforcement
	}
	for name, rule := range policy.NetworkPolicies {
		if !strings.HasPrefix(name, "agentorca-egress-") {
			continue
		}
		kept := rule.Endpoints[:0]
		for _, ep := range rule.Endpoints {
			if strings.EqualFold(ep.Host, GitHubHost) {
				ep.Ports = deletePort(ep.Ports, 443)
				if len(ep.Ports) == 0 {
					continue
				}
			}
			kept = append(kept, ep)
		}
		if len(kept) == 0 {
			delete(policy.NetworkPolicies, name)
			continue
		}
		rule.Endpoints = kept
		policy.NetworkPolicies[name] = rule
	}

	var rules []types.L7Rule
	allow := func(method, path string) {
		rules = append(rules, types.L7Rule{Allow: &types.L7Allow{Method: method, Path: path}})
	}
	for _, repo := range git.Repositories {
		for _, base := range []string{"/" + repo, "/" + repo + ".git"} {
			allow("GET", base+"/info/refs")
			allow("POST", base+"/git-upload-pack")
			if git.Push {
				allow("POST", base+"/git-receive-pack")
			}
		}
	}
	binaries := make([]osv1.PolicyNetworkBinary, len(gitBinaries))
	for i, b := range gitBinaries {
		binaries[i] = osv1.PolicyNetworkBinary{Path: b}
	}
	policy.NetworkPolicies[gitPolicyName] = osv1.NetworkPolicyRule{
		Name: gitPolicyName,
		Endpoints: []osv1.PolicyNetworkEndpoint{{
			Host:              GitHubHost,
			Ports:             []uint32{443},
			Protocol:          "rest",
			Enforcement:       enforcement,
			Rules:             rules,
			CredentialBinding: &types.NetworkCredentialBinding{Provider: git.Provider},
		}},
		Binaries: binaries,
	}
}

func deletePort(ports []uint32, port uint32) []uint32 {
	out := ports[:0]
	for _, p := range ports {
		if p != port {
			out = append(out, p)
		}
	}
	return out
}
