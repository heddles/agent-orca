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

package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/githubapp"
	"github.com/heddles/agent-orca/internal/openshell"
)

const testGitToken = "ghs_token1"

// stubMinter returns numbered tokens and records each request.
type stubMinter struct {
	reqs []githubapp.TokenRequest
	ttl  time.Duration
}

func (m *stubMinter) InstallationToken(_ context.Context, req githubapp.TokenRequest) (*githubapp.Token, error) {
	m.reqs = append(m.reqs, req)
	return &githubapp.Token{
		Value:     "ghs_token" + string(rune('0'+len(m.reqs))),
		ExpiresAt: time.Now().Add(m.ttl),
	}, nil
}

func gitAgent() *agentorcav1alpha1.Agent {
	a := sandboxedAgent()
	a.Spec.Runtime.Sandbox.GitHub = &agentorcav1alpha1.SandboxGitHub{
		Repositories: []string{"heddles/agent-orca"},
		Push:         true,
	}
	// A plain github.com rule must give way to the credential-bound endpoint.
	a.Spec.Runtime.Sandbox.Egress = append(a.Spec.Runtime.Sandbox.Egress,
		agentorcav1alpha1.SandboxEgressRule{Host: "github.com", Access: agentorcav1alpha1.SandboxAccessReadWrite})
	return a
}

func gitRun() (*agentorcav1alpha1.AgentRun, *corev1.Secret) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "push a fix"},
	}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"},
		Data:       map[string][]byte{"OPENAI_API_KEY": []byte("run-token")},
	}
	return run, token
}

func TestEnsureSandbox_GitHubToken(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, cl := sandboxReconciler(t, gw, run, token)
	minter := &stubMinter{ttl: time.Hour}
	r.GitHubApp = minter

	sb, err := r.ensureSandbox(context.Background(), run, gitAgent(), "tok", "http://router:8080")
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}

	// The token is narrowed to the repository and to contents: write.
	want := githubapp.TokenRequest{
		Repositories: []string{"agent-orca"},
		Permissions:  map[string]string{"contents": "write", "metadata": "read"},
	}
	if len(minter.reqs) != 1 || !reflect.DeepEqual(minter.reqs[0], want) {
		t.Fatalf("token requests = %+v, want one %+v", minter.reqs, want)
	}

	// The value lives only in the run's provider; the workload gets the
	// placeholder env var through git's credential helper config.
	provider := gw.providers[openshell.GitProviderName("default", "run-1")]
	if provider == nil || provider.Spec.Credentials[openshell.GitTokenEnvVar] != testGitToken {
		t.Fatalf("git provider = %+v", provider)
	}
	if gw.profiles[openshell.GitProfileID("default")] == nil {
		t.Error("git profile not registered")
	}
	spec := gw.specs[sb.Name]
	if !strings.Contains(strings.Join(spec.Providers, ","), provider.Name) {
		t.Errorf("sandbox providers %v do not attach %s", spec.Providers, provider.Name)
	}
	for k, v := range spec.Environment {
		if strings.Contains(v, "ghs_") {
			t.Errorf("token value leaked into sandbox env %s", k)
		}
	}
	if !strings.Contains(spec.Environment["GIT_CONFIG_VALUE_1"], "$"+openshell.GitTokenEnvVar) {
		t.Errorf("git credential helper = %q", spec.Environment["GIT_CONFIG_VALUE_1"])
	}

	// One endpoint for github.com:443, bound to the provider.
	var bound int
	for name, rule := range spec.Policy.NetworkPolicies {
		for _, ep := range rule.Endpoints {
			if strings.EqualFold(ep.Host, "github.com") && slices.Contains(ep.Ports, 443) {
				bound++
				if ep.CredentialBinding == nil || ep.CredentialBinding.Provider != provider.Name {
					t.Errorf("rule %s reaches github.com:443 without the credential binding", name)
				}
			}
		}
	}
	if bound != 1 {
		t.Errorf("github.com:443 endpoints = %d, want exactly the bound one", bound)
	}

	if got := fetchSandboxRun(t, cl, run); got.Status.SandboxGitTokenExpiry == nil {
		t.Error("token expiry not recorded")
	}
}

func TestRefreshSandboxGit(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, _ := sandboxReconciler(t, gw, run, token)
	minter := &stubMinter{ttl: time.Hour}
	r.GitHubApp = minter
	agent := gitAgent()

	// A token with plenty of time left is kept.
	fresh := metav1.NewTime(time.Now().Add(50 * time.Minute))
	run.Status.SandboxGitTokenExpiry = &fresh
	r.refreshSandboxGit(context.Background(), run, agent)
	if len(minter.reqs) != 0 {
		t.Fatalf("refreshed a token with 50m left")
	}

	// One close to expiry is replaced in the run's provider.
	soon := metav1.NewTime(time.Now().Add(5 * time.Minute))
	run.Status.SandboxGitTokenExpiry = &soon
	r.refreshSandboxGit(context.Background(), run, agent)
	if len(minter.reqs) != 1 {
		t.Fatalf("token near expiry not refreshed")
	}
	if p := gw.providers[openshell.GitProviderName("default", "run-1")]; p == nil || p.Spec.Credentials[openshell.GitTokenEnvVar] != testGitToken {
		t.Errorf("provider not updated: %+v", p)
	}
	if time.Until(run.Status.SandboxGitTokenExpiry.Time) < 30*time.Minute {
		t.Errorf("expiry not moved forward: %v", run.Status.SandboxGitTokenExpiry)
	}
}

func TestEnsureSandbox_GitHubWithoutAppFails(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	r, _ := sandboxReconciler(t, gw, run, token)

	_, err := r.ensureSandbox(context.Background(), run, gitAgent(), "tok", "http://router:8080")
	if !errors.Is(err, errNoGitHubApp) {
		t.Fatalf("err = %v, want errNoGitHubApp", err)
	}
	if len(gw.sandboxes) != 0 {
		t.Error("sandbox created without its GitHub credential")
	}
}
