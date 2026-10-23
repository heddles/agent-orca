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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Static tool credentials reach the sandbox as OpenShell providers. The
// workload's environment holds an opaque placeholder per credential; the
// supervisor swaps in the real value only on requests to the tool's own
// endpoint (credential_binding in the sandbox policy). The agent can call its
// authenticated tools directly but cannot read or exfiltrate their secrets.
//
// Each credential source gets an endpointless provider profile declaring its
// env var names (OpenShell withholds credential keys a profile does not
// declare), and each run gets its own provider instance holding the values, so
// they are revoked when the run ends.

// CredentialsEnvVar names the non-secret env var describing the credentialed
// tools, so an agent can build its MCP clients without hardcoding env names.
const CredentialsEnvVar = "AGENTORCA_TOOL_CREDENTIALS"

// ToolCredential is the static secret material a tool sends to its MCP server.
type ToolCredential struct {
	// Source owns the credential: the MCPServer the tool was generated from, or
	// the tool itself. Tools from one MCPServer share one credential.
	Source string
	// Tool is the first tool (in spec.tools order) that declared the credential.
	Tool string
	// URL is the MCP server URL, and Host/Port its endpoint the credential binds to.
	URL  string
	Host string
	Port uint32
	Keys []CredentialKey
}

// CredentialKey is one secret header value.
type CredentialKey struct {
	// EnvVar is the workload env var holding the placeholder.
	EnvVar string
	// Header is the HTTP header the value goes in, and Prefix what precedes it.
	Header string
	Prefix string
	// SecretName and SecretKey locate the value in the tool's namespace.
	SecretName string
	SecretKey  string
}

// ToolCredentials returns the static credentials of a remote MCP tool, or nil
// when the tool has none. OAuth tools are skipped: their access token rotates,
// and a snapshot would go stale mid-run. Secret references to other namespaces
// are skipped, matching the router, which mounts only same-namespace Secrets.
// source is the credential owner (see ToolCredential.Source).
func ToolCredentials(tool *agentorcav1alpha1.Tool, source string) *ToolCredential {
	cfg := tool.Spec.MCPConfig
	if tool.Spec.Type != agentorcav1alpha1.ToolTypeMCP || cfg == nil || cfg.Auth == nil || cfg.Auth.OAuth != nil {
		return nil
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || isLoopback(u.Hostname()) {
		return nil
	}
	eps := ToolEndpoints(&agentorcav1alpha1.Tool{ObjectMeta: tool.ObjectMeta, Spec: agentorcav1alpha1.ToolSpec{
		Type: tool.Spec.Type, MCPConfig: cfg,
	}})
	if len(eps) != 1 {
		return nil
	}

	cred := &ToolCredential{Source: source, Tool: tool.Name, URL: cfg.URL, Host: eps[0].Host, Port: eps[0].Port}
	prefix := "AGENTORCA_" + envName(source) + "_"
	add := func(suffix, header, valuePrefix string, ref *agentorcav1alpha1.SecretKeyRef) {
		if ref == nil || ref.Name == "" || ref.Key == "" || (ref.Namespace != "" && ref.Namespace != tool.Namespace) {
			return
		}
		key := CredentialKey{
			EnvVar: prefix + suffix, Header: header, Prefix: valuePrefix,
			SecretName: ref.Name, SecretKey: ref.Key,
		}
		for _, k := range cred.Keys {
			if k.EnvVar == key.EnvVar || strings.EqualFold(k.Header, key.Header) {
				return
			}
		}
		cred.Keys = append(cred.Keys, key)
	}
	auth := cfg.Auth
	add("TOKEN", "Authorization", "Bearer ", auth.BearerToken)
	if auth.APIKey != nil {
		header := auth.APIKey.HeaderName
		if header == "" {
			header = "X-API-Key"
		}
		add("API_KEY", header, "", &auth.APIKey.SecretKeyRef)
	}
	for i := range auth.Headers {
		h := &auth.Headers[i]
		add(envName(h.Name), h.Name, "", &h.SecretKeyRef)
	}
	if len(cred.Keys) == 0 {
		return nil
	}
	return cred
}

// envName upper-cases s and replaces everything but letters and digits with _.
func envName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "/")))
	return hex.EncodeToString(sum[:])[:16]
}

// ProfileID is the gateway profile for a credential source in a namespace.
// It is shared by every run that uses the source.
func ProfileID(namespace, source string) string {
	return "agentorca-tool-" + shortHash(namespace, source)
}

// ProviderName is the run's provider instance for a credential source. It is
// stable across a run's retries, so a retry reuses (and refreshes) it.
func ProviderName(namespace, runName, source string) string {
	return "agentorca-run-" + shortHash(namespace, runName, source)
}

// Profile returns the endpointless profile declaring cred's env vars. It has
// no endpoints so the only credential boundary is the sandbox policy's
// credential_binding, which names exactly the tool's MCP endpoint.
func Profile(namespace string, cred *ToolCredential) *osv1.ProviderProfile {
	p := &osv1.ProviderProfile{
		ID:          ProfileID(namespace, cred.Source),
		DisplayName: "agent-orca tool " + namespace + "/" + cred.Source,
		Description: "Static credentials of agent-orca tool " + cred.Source + ", managed by the agent-orca operator",
		Category:    types.ProfileCategoryOther,
		Annotations: map[string]string{
			"agentorca.io/namespace": namespace,
			"agentorca.io/source":    cred.Source,
		},
	}
	for _, k := range cred.Keys {
		style := "header"
		if k.Prefix == "Bearer " {
			style = "bearer"
		}
		p.Credentials = append(p.Credentials, osv1.ProfileCredential{
			Name:        strings.ToLower(k.EnvVar),
			Description: k.Header + " header for " + cred.Source,
			EnvVars:     []string{k.EnvVar},
			Required:    true,
			Secret:      true,
			AuthStyle:   style,
			HeaderName:  strings.ToLower(k.Header),
		})
	}
	return p
}

// Provider returns the run's provider instance holding cred's values, keyed by
// env var. labels identify the run, for cleanup.
func Provider(namespace, runName string, cred *ToolCredential, values, labels map[string]string) *osv1.Provider {
	return &osv1.Provider{
		Name:   ProviderName(namespace, runName, cred.Source),
		Type:   ProfileID(namespace, cred.Source),
		Labels: labels,
		Spec:   osv1.ProviderSpec{Credentials: values},
	}
}

// credentialDescriptor is one entry of CredentialsEnvVar.
type credentialDescriptor struct {
	Tool    string            `json:"tool"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// DescribeCredentials renders CredentialsEnvVar: for each credentialed tool,
// its URL and each auth header as a template over the placeholder env var,
// e.g. {"Authorization": "Bearer ${AGENTORCA_GITHUB_TOKEN}"}. It holds no
// secrets.
func DescribeCredentials(creds []*ToolCredential) string {
	if len(creds) == 0 {
		return ""
	}
	out := make([]credentialDescriptor, 0, len(creds))
	for _, c := range creds {
		d := credentialDescriptor{Tool: c.Source, URL: c.URL, Headers: map[string]string{}}
		for _, k := range c.Keys {
			d.Headers[k.Header] = k.Prefix + "${" + k.EnvVar + "}"
		}
		out = append(out, d)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// BindCredentials marks the tool endpoints that a credential binds to, and
// drops credentials whose endpoint is not in eps (its host was excluded).
// Credentials of excluded tools must not be passed in. It returns the endpoints and the credentials still in use.
// When two sources bind the same endpoint, the first wins: an endpoint can
// carry only one binding.
func BindCredentials(eps []ToolEndpoint, creds []*ToolCredential, providerName func(*ToolCredential) string) ([]ToolEndpoint, []*ToolCredential, error) {
	bound := map[string]string{} // host:port -> provider
	var used []*ToolCredential
	for _, c := range creds {
		key := fmt.Sprintf("%s:%d", c.Host, c.Port)
		if _, taken := bound[key]; taken {
			continue
		}
		present := false
		for _, ep := range eps {
			if ep.Host == c.Host && ep.Port == c.Port {
				present = true
				break
			}
		}
		if !present {
			continue
		}
		bound[key] = providerName(c)
		used = append(used, c)
	}
	if len(used) > maxProviders {
		return nil, nil, fmt.Errorf("%d credentialed tools exceed the sandbox limit of %d providers", len(used), maxProviders)
	}
	out := make([]ToolEndpoint, len(eps))
	for i, ep := range eps {
		ep.CredentialProvider = bound[fmt.Sprintf("%s:%d", ep.Host, ep.Port)]
		out[i] = ep
	}
	return out, used, nil
}

// maxProviders is the gateway's limit on providers attached to one sandbox.
const maxProviders = 32
