package judge

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	tnt      = "tnt_01935c7e-0000-7000-8000-00000000000a"
	otherTnt = "tnt_01935c7e-0000-7000-8000-00000000000b"
)

func providerObj(ns, tenant string, labels map[string]string) *agentorcav1alpha1.ModelProvider {
	l := map[string]string{tenancy.LabelTenantID: tenant}
	maps.Copy(l, labels)
	return &agentorcav1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "judge", Namespace: ns, Labels: l},
		Spec: agentorcav1alpha1.ModelProviderSpec{
			LiteLLMModel:   "openai/judge-model",
			CredentialsRef: agentorcav1alpha1.SecretKeyRef{Name: "judge-key", Key: "api-key", Namespace: "elsewhere"},
			Constraints:    agentorcav1alpha1.ModelConstraints{CostPerMillionInputTokens: "1000000", CostPerMillionOutputTokens: "2000000"},
		},
	}
}

func TestRouterProviderCallsTheProviderPathAndPricesTheUsage(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"rationale\":\"x\",\"score\":1,\"pass\":true}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	mp := providerObj("team-a", tnt, nil)
	mp.Spec.BaseURL = srv.URL
	p := RouterProvider{Config: ProviderConfigFrom(mp, "sk-test")}
	rep, err := p.Complete(context.Background(), Request{System: "sys", User: "usr", MaxTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if rep.CostUSD != 10*1+5*2 || rep.PromptTokens != 10 || rep.CompletionTokens != 5 || !strings.Contains(rep.Text, `"score":1`) {
		t.Errorf("reply = %+v", rep)
	}
	if _, hasTools := gotBody["tools"]; hasTools || gotBody["max_completion_tokens"] != float64(64) || gotBody["temperature"] != float64(0) {
		t.Errorf("request body = %v", gotBody)
	}
	if msgs, _ := gotBody["messages"].([]any); len(msgs) != 2 {
		t.Errorf("messages = %v", gotBody["messages"])
	}
	if got := p.PriceUSD(100, 10); got != 100+20 {
		t.Errorf("price = %v", got)
	}
}

func TestRouterProviderSurfacesProviderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	mp := providerObj("team-a", tnt, nil)
	mp.Spec.BaseURL = srv.URL
	if _, err := (RouterProvider{Config: ProviderConfigFrom(mp, "k")}).Complete(context.Background(), Request{System: "s", User: "u", MaxTokens: 16}); err == nil {
		t.Error("a 401 was not an error")
	}
}

func resolverFor(t *testing.T, objs ...runtime.Object) KubeResolver {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return KubeResolver{Reader: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()}
}

func secretIn(ns, key, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "judge-key", Namespace: ns}, Data: map[string][]byte{key: []byte(value)}}
}

func TestResolverReadsTheKeyFromTheProvidersOwnNamespace(t *testing.T) {
	r := resolverFor(t, providerObj("team-a", tnt, nil), secretIn("team-a", "api-key", "sk-a"), secretIn("elsewhere", "api-key", "sk-elsewhere"))
	caller := tenancy.Caller{TenantID: tnt, Namespaces: []string{"team-a"}}
	p, err := r.Resolve(context.Background(), caller, "team-a", "judge")
	if err != nil {
		t.Fatal(err)
	}
	rp := p.(RouterProvider)
	if rp.Config.APIKey != "sk-a" || rp.Config.LiteLLMModel != "openai/judge-model" || rp.Config.CostPerInputToken != 1 || rp.Config.CostPerOutputToken != 2 {
		t.Errorf("config = %+v", rp.Config)
	}
}

func TestResolverRefusesWhatTheTenantMayNotUse(t *testing.T) {
	caller := tenancy.Caller{TenantID: tnt, Namespaces: []string{"team-a"}}
	cases := map[string]KubeResolver{
		"another tenant's provider in the namespace": resolverFor(t, providerObj("team-a", otherTnt, nil), secretIn("team-a", "api-key", "k")),
		"a provider in another namespace":            resolverFor(t, providerObj("team-b", tnt, nil), secretIn("team-b", "api-key", "k")),
		"no provider":                                resolverFor(t),
		"no secret":                                  resolverFor(t, providerObj("team-a", tnt, nil)),
		"a secret without the key":                   resolverFor(t, providerObj("team-a", tnt, nil), secretIn("team-a", "other", "k")),
	}
	for name, r := range cases {
		if _, err := r.Resolve(context.Background(), caller, "team-a", "judge"); err == nil {
			t.Errorf("%s: resolved", name)
		} else if name != "no secret" && !errors.Is(err, ErrProvider) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResolverFindsAPlatformProvider(t *testing.T) {
	tenancy.SetPlatformNamespaces([]string{"platform"})
	t.Cleanup(func() { tenancy.SetPlatformNamespaces(nil) })
	shared := providerObj("platform", "", map[string]string{tenancy.LabelScope: tenancy.ScopePlatform})
	delete(shared.Labels, tenancy.LabelTenantID)
	r := resolverFor(t, shared, secretIn("platform", "api-key", "sk-platform"))
	caller := tenancy.Caller{TenantID: tnt, Namespaces: []string{"team-a"}}
	p, err := r.Resolve(context.Background(), caller, "team-a", "judge")
	if err != nil || p.(RouterProvider).Config.APIKey != "sk-platform" {
		t.Fatalf("platform provider = %v, %v", p, err)
	}
	// A provider in the platform namespace that is not shared stays hidden.
	hidden := providerObj("platform", otherTnt, nil)
	if _, err := resolverFor(t, hidden, secretIn("platform", "api-key", "k")).Resolve(context.Background(), caller, "team-a", "judge"); err == nil {
		t.Error("an unshared platform provider resolved")
	}
}
