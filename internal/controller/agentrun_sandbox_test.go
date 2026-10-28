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
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgofake "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/openshell"
)

// stubGateway is an in-memory openshell.Gateway whose sandbox status tests set directly.
type stubGateway struct {
	sandboxes map[string]*osv1.Sandbox
	specs     map[string]*osv1.SandboxSpec
	deleted   []string
	getErr    error
	logs      string
	profiles  map[string]*osv1.ProviderProfile
	providers map[string]*osv1.Provider
	// drafts are the pending policy proposals per sandbox; approved and
	// rejected record what was decided.
	drafts map[string][]osv1.PolicyChunk
	// decisions are the supervisor decisions per sandbox.
	decisions map[string][]openshell.Decision
	approved  []string
	rejected  []string
	// providerConflicts makes the next N DeleteProviders calls fail as the
	// gateway does while a sandbox being deleted still holds the provider.
	providerConflicts int
	// execFn is what Exec does; nil makes it fail.
	execFn func(ctx context.Context, sandbox string, command []string, opts openshell.ExecOptions) (*openshell.ExecResult, error)
}

func newStubGateway() *stubGateway {
	return &stubGateway{
		sandboxes: map[string]*osv1.Sandbox{}, specs: map[string]*osv1.SandboxSpec{},
		profiles: map[string]*osv1.ProviderProfile{}, providers: map[string]*osv1.Provider{},
	}
}

func (g *stubGateway) EnsureProfile(_ context.Context, p *osv1.ProviderProfile) error {
	g.profiles[p.ID] = p
	return nil
}

func (g *stubGateway) EnsureProvider(_ context.Context, p *osv1.Provider) error {
	g.providers[p.Name] = p
	return nil
}

func (g *stubGateway) DeleteProviders(_ context.Context, labels map[string]string) error {
	if g.providerConflicts > 0 {
		g.providerConflicts--
		return fmt.Errorf("deleting provider: %w", &types.StatusError{Code: types.ErrorConflict, Message: "provider is attached to sandbox(es)"})
	}
	for name, p := range g.providers {
		match := true
		for k, v := range labels {
			match = match && p.Labels[k] == v
		}
		if match {
			delete(g.providers, name)
		}
	}
	return nil
}

func (g *stubGateway) Create(_ context.Context, name string, spec *osv1.SandboxSpec, labels map[string]string) (*osv1.Sandbox, error) {
	if sb, ok := g.sandboxes[name]; ok {
		return sb, nil
	}
	sb := &osv1.Sandbox{Name: name, ID: "id-" + name, Labels: labels, Status: types.SandboxStatus{Phase: osv1.SandboxProvisioning}}
	g.sandboxes[name] = sb
	g.specs[name] = spec
	return sb, nil
}

func (g *stubGateway) Get(_ context.Context, name string) (*osv1.Sandbox, error) {
	if g.getErr != nil {
		return nil, g.getErr
	}
	sb, ok := g.sandboxes[name]
	if !ok {
		return nil, &types.StatusError{Code: types.ErrorNotFound, Message: "not found"}
	}
	return sb, nil
}

func (g *stubGateway) Delete(_ context.Context, name string) error {
	delete(g.sandboxes, name)
	g.deleted = append(g.deleted, name)
	return nil
}

func (g *stubGateway) MainProcessLogs(context.Context, *osv1.Sandbox) (string, error) {
	return g.logs, nil
}

func (g *stubGateway) SandboxNamespace() string { return "openshell" }

func sandboxScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := costScheme(t)
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func sandboxedAgent() *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec: agentorcav1alpha1.AgentSpec{
			NetworkIsolation: &agentorcav1alpha1.NetworkIsolationSpec{SplitPod: true},
			Runtime: agentorcav1alpha1.AgentRuntime{
				OCIRef:    "ghcr.io/acme/coder:1.0",
				Framework: "langgraph",
				Sandbox:   &agentorcav1alpha1.SandboxSpec{Provider: agentorcav1alpha1.SandboxProviderOpenShell},
			},
		},
	}
}

// sandboxedRun returns a Running run whose sandbox already exists on gw.
func sandboxedRun(t *testing.T, gw *stubGateway) *agentorcav1alpha1.AgentRun {
	t.Helper()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default", Finalizers: []string{agentRunFinalizer}},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "fix the tests"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseRunning},
	}
	sb, err := gw.Create(context.Background(), openshell.SandboxName(run.Namespace, run.Name, 0), &osv1.SandboxSpec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.SandboxName = sb.Name
	return run
}

// newFakeClientsetWithLogs avoids client-go's default no-reaction error for pod
// logs. Besides returning the response these tests expect, it avoids the
// generated PodLogOptions.String recursion in the Go 1.26 error-formatting path.
func newFakeClientsetWithLogs() *clientgofake.Clientset {
	k8s := clientgofake.NewClientset()
	k8s.PrependReactor("get", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "log" {
			return true, &runtime.Unknown{Raw: []byte("fake logs")}, nil
		}
		return false, nil, nil
	})
	return k8s
}

func sandboxReconciler(t *testing.T, gw openshell.Gateway, objs ...client.Object) (*AgentRunReconciler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(sandboxScheme(t)).
		WithStatusSubresource(&agentorcav1alpha1.AgentRun{}).
		WithObjects(objs...).
		Build()
	return &AgentRunReconciler{Client: cl, Scheme: cl.Scheme(), K8s: newFakeClientsetWithLogs(), Sandboxes: gw}, cl
}

func fetchSandboxRun(t *testing.T, cl client.Client, run *agentorcav1alpha1.AgentRun) *agentorcav1alpha1.AgentRun {
	t.Helper()
	var got agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestEnsureSandbox_CarriesRunEnvAndRouterCredential(t *testing.T) {
	gw := newStubGateway()
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "fix the tests"},
		Status:     agentorcav1alpha1.AgentRunStatus{RestartCount: 2},
	}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"},
		Data: map[string][]byte{
			"OPENAI_BASE_URL": []byte("http://router/v1"),
			"OPENAI_API_KEY":  []byte("run-token"),
		},
	}
	r, _ := sandboxReconciler(t, gw, run, token)

	sb, err := r.ensureSandbox(context.Background(), run, sandboxedAgent(), "tok", "http://router:8080")
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	if sb.Name != openshell.SandboxName("default", "run-1", 2) {
		t.Fatalf("sandbox name %q must be derived from the current attempt", sb.Name)
	}
	if sb.Labels[openshell.LabelRunName] != "run-1" || sb.Labels[openshell.LabelRunNamespace] != "default" {
		t.Fatalf("sandbox labels = %v", sb.Labels)
	}
	env := gw.specs[sb.Name].Environment
	for k, want := range map[string]string{
		"AGENTORC_RUN_ID": "run-1",
		"AGENTORC_INPUT":  "fix the tests",
		"OPENAI_BASE_URL": "http://router/v1",
		"OPENAI_API_KEY":  "run-token",
		"OPENAI_API_BASE": "http://router:8080", // langgraph framework env
	} {
		if env[k] != want {
			t.Errorf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
	router := gw.specs[sb.Name].Policy.NetworkPolicies["agentorca-router"]
	if host := router.Endpoints[0].Host; host != "agentorca-router-run-1.default.svc.cluster.local" {
		t.Fatalf("policy pins router host %q", host)
	}
}

func TestCheckSandboxProgress_SuccessCollectsOutputAndAdmitsSupervisor(t *testing.T) {
	gw := newStubGateway()
	gw.logs = "all tests pass"
	run := sandboxedRun(t, gw)
	sb := gw.sandboxes[run.Status.SandboxName]
	sb.Status.Phase = osv1.SandboxCompleted
	exit := int32(0)
	sb.Status.ExitCode = &exit
	r, cl := sandboxReconciler(t, gw, run)

	if _, err := r.checkProgress(context.Background(), run, sandboxedAgent()); err != nil {
		t.Fatalf("checkProgress: %v", err)
	}

	got := fetchSandboxRun(t, cl, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded || got.Status.Output != "all tests pass" {
		t.Fatalf("run = %s %q, want Succeeded with the sandbox output", got.Status.Phase, got.Status.Output)
	}

	var np networkingv1.NetworkPolicy
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "agentorca-router-run-1-sandbox"}, &np); err != nil {
		t.Fatalf("router sandbox ingress policy: %v", err)
	}
	from := np.Spec.Ingress[0].From[0]
	if from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "openshell" {
		t.Fatalf("ingress namespace = %v", from.NamespaceSelector.MatchLabels)
	}
	if from.PodSelector.MatchLabels[openshell.SandboxIDLabel] != sb.ID ||
		from.PodSelector.MatchLabels[openshell.SupervisorComponentLabel] != openshell.SupervisorComponentValue {
		t.Fatalf("ingress must admit only this sandbox's supervisor, got %v", from.PodSelector.MatchLabels)
	}
}

func TestCheckSandboxProgress_StillRunning(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	r, cl := sandboxReconciler(t, gw, run)

	res, err := r.checkProgress(context.Background(), run, sandboxedAgent())
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("checkProgress = %+v, %v; want a requeue", res, err)
	}
	if got := fetchSandboxRun(t, cl, run); got.Status.Phase != agentorcav1alpha1.AgentRunPhaseRunning {
		t.Fatalf("phase = %s", got.Status.Phase)
	}
}

func TestCheckSandboxProgress_GatewayOutageDoesNotFailRun(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	gw.getErr = errors.New("connection refused")
	r, cl := sandboxReconciler(t, gw, run)

	res, err := r.checkProgress(context.Background(), run, sandboxedAgent())
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("checkProgress = %+v, %v; want a requeue", res, err)
	}
	if got := fetchSandboxRun(t, cl, run); got.Status.Phase != agentorcav1alpha1.AgentRunPhaseRunning || got.Status.RestartCount != 0 {
		t.Fatalf("a gateway outage changed the run: %s restarts=%d", got.Status.Phase, got.Status.RestartCount)
	}
}

func TestCheckSandboxProgress_FailureRetriesWithFreshSandbox(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	first := run.Status.SandboxName
	exit := int32(1)
	gw.sandboxes[first].Status = types.SandboxStatus{Phase: osv1.SandboxCompleted, ExitCode: &exit}
	gw.logs = "step 1\nboom: urllib.error.HTTPError"
	r, cl := sandboxReconciler(t, gw, run)

	if _, err := r.checkProgress(context.Background(), run, sandboxedAgent()); err != nil {
		t.Fatalf("checkProgress: %v", err)
	}

	got := fetchSandboxRun(t, cl, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhasePending || got.Status.RestartCount != 1 {
		t.Fatalf("run = %s restarts=%d, want Pending for a retry", got.Status.Phase, got.Status.RestartCount)
	}
	if got.Status.SandboxName != "" {
		t.Fatalf("SandboxName = %q, want cleared so startRun creates a new sandbox", got.Status.SandboxName)
	}
	if !strings.Contains(got.Status.LastRestartReason, "[agent] step 1\nboom: urllib.error.HTTPError") {
		t.Fatalf("restart reason %q must keep the agent's last output: the sandbox and its logs are deleted", got.Status.LastRestartReason)
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != first {
		t.Fatalf("deleted = %v, want the failed sandbox", gw.deleted)
	}
	if openshell.SandboxName(run.Namespace, run.Name, got.Status.RestartCount) == first {
		t.Fatal("the retry would reuse the failed sandbox's name")
	}
}

func TestCheckSandboxProgress_MissingSandboxCountsAsFailure(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	delete(gw.sandboxes, run.Status.SandboxName)
	run.Status.RestartCount = 3 // retries exhausted
	r, cl := sandboxReconciler(t, gw, run)

	if _, err := r.checkProgress(context.Background(), run, sandboxedAgent()); err != nil {
		t.Fatalf("checkProgress: %v", err)
	}
	if got := fetchSandboxRun(t, cl, run); got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
}

func TestEnsureCleanup_DeletesSandbox(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	name := run.Status.SandboxName
	r, _ := sandboxReconciler(t, gw, run)

	if _, err := r.ensureCleanup(context.Background(), run); err != nil {
		t.Fatalf("ensureCleanup: %v", err)
	}
	if _, ok := gw.sandboxes[name]; ok {
		t.Fatal("the gateway retains completed sandboxes, so cleanup must delete it")
	}
}

func sandboxTools() []client.Object {
	return []client.Object{
		&agentorcav1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
			Spec: agentorcav1alpha1.ToolSpec{NetworkEgress: []agentorcav1alpha1.EgressRule{
				{Host: "api.github.com", Port: 443},
			}},
		},
		&agentorcav1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "docs-search", Namespace: "default",
				Labels: map[string]string{LabelMCPServer: "docs"}},
			Spec: agentorcav1alpha1.ToolSpec{
				Type:      agentorcav1alpha1.ToolTypeMCP,
				MCPConfig: &agentorcav1alpha1.MCPConfig{Transport: "http", URL: "http://mcp-docs.tools.svc:8000/mcp"},
			},
		},
		&agentorcav1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "default"},
			Spec: agentorcav1alpha1.MCPServerSpec{NetworkEgress: []agentorcav1alpha1.EgressRule{
				{Host: "docs.example.com", Port: 443},
			}},
		},
	}
}

func toolRuleHosts(spec *osv1.SandboxSpec) map[string][]string {
	out := map[string][]string{}
	for name, rule := range spec.Policy.NetworkPolicies {
		for _, ep := range rule.Endpoints {
			out[name] = append(out[name], ep.Host)
		}
	}
	return out
}

func TestEnsureSandbox_ToolEgress(t *testing.T) {
	no := false
	tests := []struct {
		name  string
		tools *agentorcav1alpha1.SandboxTools
		want  map[string][]string // tool rules only
	}{
		{name: "inherited by default", want: map[string][]string{
			"agentorca-tool-github":      {"api.github.com"},
			"agentorca-tool-docs-search": {"docs.example.com", "mcp-docs.tools.svc"},
		}},
		{name: "opted out", tools: &agentorcav1alpha1.SandboxTools{Inherit: &no}, want: map[string][]string{}},
		{name: "tool excluded", tools: &agentorcav1alpha1.SandboxTools{ExcludeTools: []string{"github"}}, want: map[string][]string{
			"agentorca-tool-docs-search": {"docs.example.com", "mcp-docs.tools.svc"},
		}},
		{name: "host excluded", tools: &agentorcav1alpha1.SandboxTools{ExcludeHosts: []string{"docs.example.com"}}, want: map[string][]string{
			"agentorca-tool-github":      {"api.github.com"},
			"agentorca-tool-docs-search": {"mcp-docs.tools.svc"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := newStubGateway()
			run := &agentorcav1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
				Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "go"},
			}
			token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"}}
			r, _ := sandboxReconciler(t, gw, append(sandboxTools(), run, token)...)
			agent := sandboxedAgent()
			agent.Spec.Tools = []string{"github", "docs-search", "_clarify"} // builtins have no Tool CR
			agent.Spec.Runtime.Sandbox.Tools = tt.tools

			sb, err := r.ensureSandbox(context.Background(), run, agent, "tok", "http://router:8080")
			if err != nil {
				t.Fatalf("ensureSandbox: %v", err)
			}
			got := toolRuleHosts(gw.specs[sb.Name])
			delete(got, "agentorca-router")
			for name := range tt.want {
				if !reflect.DeepEqual(sortedCopy(got[name]), sortedCopy(tt.want[name])) {
					t.Errorf("%s hosts = %v, want %v", name, got[name], tt.want[name])
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("tool rules = %v, want %v", got, tt.want)
			}
		})
	}
}

func sortedCopy(s []string) []string {
	return slices.Sorted(slices.Values(s))
}

func TestEnsureSandbox_StaticToolCredentials(t *testing.T) {
	bearer := agentorcav1alpha1.SecretKeyRef{Name: "gh-mcp", Key: "token"}
	githubMCP := &agentorcav1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: agentorcav1alpha1.ToolSpec{
			Type: agentorcav1alpha1.ToolTypeMCP,
			MCPConfig: &agentorcav1alpha1.MCPConfig{
				Transport: "http", URL: "https://api.githubcopilot.com/mcp/",
				Auth: &agentorcav1alpha1.MCPAuthConfig{BearerToken: &bearer},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-mcp", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("ghp_real_secret\n")},
	}
	newRun := func() *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "coder", Input: "go"},
			Status:     agentorcav1alpha1.AgentRunStatus{RouterPodName: "agentorca-router-run-1"},
		}
	}
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"}}

	t.Run("bound to the tool endpoint as a provider", func(t *testing.T) {
		gw := newStubGateway()
		run := newRun()
		r, _ := sandboxReconciler(t, gw, githubMCP, secret, token, run)
		agent := sandboxedAgent()
		agent.Spec.Tools = []string{"github"}

		sb, err := r.ensureSandbox(context.Background(), run, agent, "tok", "http://router:8080")
		if err != nil {
			t.Fatalf("ensureSandbox: %v", err)
		}
		spec := gw.specs[sb.Name]
		providerName := openshell.ProviderName("default", "run-1", "github")

		provider := gw.providers[providerName]
		if provider == nil || provider.Spec.Credentials["AGENTORCA_GITHUB_TOKEN"] != "ghp_real_secret" {
			t.Fatalf("provider = %+v, want the trimmed secret under its env var", provider)
		}
		if provider.Type != openshell.ProfileID("default", "github") || gw.profiles[provider.Type] == nil {
			t.Fatal("the provider must select the tool's registered profile")
		}
		if !slices.Equal(spec.Providers, []string{providerName}) {
			t.Fatalf("sandbox providers = %v", spec.Providers)
		}
		ep := spec.Policy.NetworkPolicies["agentorca-tool-github"].Endpoints[0]
		if ep.Host != "api.githubcopilot.com" || ep.CredentialBinding == nil || ep.CredentialBinding.Provider != providerName {
			t.Fatalf("tool endpoint = %+v, want it bound to the provider", ep)
		}
		if desc := spec.Environment[openshell.CredentialsEnvVar]; !strings.Contains(desc, "Bearer ${AGENTORCA_GITHUB_TOKEN}") {
			t.Fatalf("credential descriptor = %q", desc)
		}
		for k, v := range spec.Environment {
			if strings.Contains(v, "ghp_real_secret") {
				t.Fatalf("the secret leaked into sandbox env %s", k)
			}
		}
		if _, ok := spec.Environment["AGENTORCA_GITHUB_TOKEN"]; ok {
			t.Fatal("the placeholder env var is OpenShell's to set, not ours")
		}

		// The run ending revokes the credential.
		run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
		if _, err := r.ensureCleanup(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if len(gw.providers) != 0 {
			t.Fatalf("providers after cleanup = %v", gw.providers)
		}
	})

	t.Run("excluded tool gets no credential", func(t *testing.T) {
		gw := newStubGateway()
		run := newRun()
		r, _ := sandboxReconciler(t, gw, githubMCP, secret, token, run)
		agent := sandboxedAgent()
		agent.Spec.Tools = []string{"github"}
		agent.Spec.Runtime.Sandbox.Tools = &agentorcav1alpha1.SandboxTools{ExcludeTools: []string{"github"}}

		sb, err := r.ensureSandbox(context.Background(), run, agent, "tok", "http://router:8080")
		if err != nil {
			t.Fatal(err)
		}
		if len(gw.providers) != 0 || len(gw.specs[sb.Name].Providers) != 0 {
			t.Fatal("an excluded tool must not get a provider")
		}
		if _, ok := gw.specs[sb.Name].Environment[openshell.CredentialsEnvVar]; ok {
			t.Fatal("no credentials, no descriptor")
		}
	})

	t.Run("missing secret retries instead of starting without it", func(t *testing.T) {
		gw := newStubGateway()
		run := newRun()
		r, _ := sandboxReconciler(t, gw, githubMCP, token, run)
		agent := sandboxedAgent()
		agent.Spec.Tools = []string{"github"}

		if _, err := r.ensureSandbox(context.Background(), run, agent, "tok", "http://router:8080"); err == nil {
			t.Fatal("want an error when the tool's secret is missing")
		}
		if len(gw.sandboxes) != 0 {
			t.Fatal("the sandbox must not be created before its credentials exist")
		}
	})
}

func TestRunCleanup_RetriesWhileProviderAttached(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	run.Status.RouterPodName = "agentorca-router-run-1"
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	gw.providers["p"] = &osv1.Provider{Name: "p", Labels: sandboxRunLabels(run)}
	gw.providerConflicts = 1
	r, cl := sandboxReconciler(t, gw, run)

	res, err := r.ensureCleanup(context.Background(), run)
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("ensureCleanup = %+v, %v; want a requeue while the provider is attached", res, err)
	}

	// A deleted AgentRun keeps its finalizer until the provider is gone.
	if err := cl.Delete(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var deleting agentorcav1alpha1.AgentRun
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(run), &deleting); err != nil {
		t.Fatal(err)
	}
	gw.providerConflicts = 1
	if res, err := r.handleRunDeletion(context.Background(), &deleting); err != nil || res.RequeueAfter == 0 {
		t.Fatalf("handleRunDeletion = %+v, %v; want a requeue", res, err)
	}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(run), &deleting); err != nil {
		t.Fatalf("run vanished while its provider was still attached: %v", err)
	}

	// Once the sandbox is gone the provider deletes and the finalizer is released.
	if _, err := r.handleRunDeletion(context.Background(), &deleting); err != nil {
		t.Fatal(err)
	}
	if len(gw.providers) != 0 {
		t.Fatalf("providers = %v, want deleted", gw.providers)
	}
}

// A safeguard trip reported by the model-router fails the run outright; it must
// not wait for (or retry) an agent process that is stuck in the same loop.
func TestCheckProgress_SafeguardTripFailsRun(t *testing.T) {
	gw := newStubGateway()
	run := sandboxedRun(t, gw)
	run.Status.LoopDetected = &agentorcav1alpha1.LoopDetectedInfo{
		Reason: `tool "vault_fortune" was called 50 times with identical arguments`, TripCount: 50, ToolName: "vault_fortune",
	}
	r, cl := sandboxReconciler(t, gw, run)

	if _, err := r.checkProgress(context.Background(), run, sandboxedAgent()); err != nil {
		t.Fatal(err)
	}
	got := fetchSandboxRun(t, cl, run)
	if got.Status.Phase != agentorcav1alpha1.AgentRunPhaseFailed || got.Status.RestartCount != 0 ||
		!strings.Contains(got.Status.LastRestartReason, "safeguard: tool") {
		t.Fatalf("run = %s restarts=%d reason=%q, want Failed without a retry", got.Status.Phase, got.Status.RestartCount, got.Status.LastRestartReason)
	}
}

func (g *stubGateway) PendingDrafts(_ context.Context, sandbox string) ([]osv1.PolicyChunk, error) {
	return g.drafts[sandbox], nil
}

func (g *stubGateway) ApproveDraft(_ context.Context, sandbox, chunkID, reviewToken string) error {
	g.approved = append(g.approved, chunkID+"@"+reviewToken)
	g.removeDraft(sandbox, chunkID)
	return nil
}

func (g *stubGateway) RejectDraft(_ context.Context, sandbox, chunkID, _ string) error {
	g.rejected = append(g.rejected, chunkID)
	g.removeDraft(sandbox, chunkID)
	return nil
}

func (g *stubGateway) removeDraft(sandbox, chunkID string) {
	var keep []osv1.PolicyChunk
	for _, ch := range g.drafts[sandbox] {
		if ch.ID != chunkID {
			keep = append(keep, ch)
		}
	}
	g.drafts[sandbox] = keep
}

// Exec implements openshell.Gateway by delegating to the test's execFn, which
// defaults to failing: tests that run commands say what they return.
func (g *stubGateway) Exec(ctx context.Context, sandbox string, command []string, opts openshell.ExecOptions) (*openshell.ExecResult, error) {
	if g.execFn == nil {
		return nil, fmt.Errorf("stub gateway: no exec behavior set")
	}
	return g.execFn(ctx, sandbox, command, opts)
}

func (g *stubGateway) Decisions(_ context.Context, sandbox string, since time.Time) ([]openshell.Decision, error) {
	var out []openshell.Decision
	for _, d := range g.decisions[sandbox] {
		if d.Time.After(since) {
			out = append(out, d)
		}
	}
	return out, nil
}
