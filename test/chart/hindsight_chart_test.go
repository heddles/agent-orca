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

// Package chart renders the agent-orca Helm chart and asserts how the bundled
// Hindsight subchart is wired into the operator. It needs the helm binary and
// registry access (the dependency is an OCI chart) and skips without them.
package chart

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

var (
	buildOnce sync.Once
	chartDir  string
	buildErr  error
)

// builtChart copies the chart to a temp dir and resolves its dependencies there,
// so the test leaves no charts/ or Chart.lock in the working tree.
func builtChart(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agent-orca-chart-")
		if err != nil {
			buildErr = err
			return
		}
		chartDir = filepath.Join(dir, "agent-orca")
		if err := copyDir(filepath.Join("..", "..", "charts", "agent-orca"), chartDir); err != nil {
			buildErr = err
			return
		}
		out, err := exec.Command("helm", "dependency", "build", chartDir).CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("helm dependency build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Skipf("cannot resolve chart dependencies (offline?): %v", buildErr)
	}
	return chartDir
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer func() { _ = out.Close() }()
		_, err = io.Copy(out, in)
		return err
	})
}

type obj struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
	Spec struct {
		Template struct {
			Spec corev1.PodSpec `json:"spec"`
		} `json:"template"`
		PodSelector metav1.LabelSelector                    `json:"podSelector"`
		Ingress     []networkingv1.NetworkPolicyIngressRule `json:"ingress"`
	} `json:"spec"`
}

// render runs helm template and returns the manifests keyed by "Kind/name".
func render(t *testing.T, sets ...string) map[string]obj {
	t.Helper()
	args := []string{"template", "agent-orca", builtChart(t), "-n", "agent-orca-system"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", sets, err, stderr.String())
	}
	objs := map[string]obj{}
	for _, doc := range strings.Split(string(out), "\n---") {
		var o obj
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("unparseable manifest: %v", err)
		}
		// UnmarshalStrict into a map rejects duplicate keys, which a Secret built
		// from preserved + generated entries could otherwise emit.
		var generic map[string]any
		if err := yaml.UnmarshalStrict([]byte(doc), &generic); err != nil {
			t.Fatalf("%s/%s is not strictly valid YAML (duplicate keys?): %v", o.Kind, o.Metadata.Name, err)
		}
		if o.Kind != "" {
			objs[o.Kind+"/"+o.Metadata.Name] = o
		}
	}
	return objs
}

func envOf(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func operator(t *testing.T, objs map[string]obj) corev1.Container {
	t.Helper()
	d, ok := objs["Deployment/agent-orca-operator"]
	if !ok {
		t.Fatalf("operator Deployment not rendered; have %v", keys(objs))
	}
	return d.Spec.Template.Spec.Containers[0]
}

func keys(m map[string]obj) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestDefault_BundlesHindsightWithAuth(t *testing.T) {
	objs := render(t)

	for _, k := range []string{"Deployment/hindsight-api", "Deployment/hindsight-control-plane", "Secret/hindsight-auth"} {
		if _, ok := objs[k]; !ok {
			t.Fatalf("%s not rendered; have %v", k, keys(objs))
		}
	}

	sec := objs["Secret/hindsight-auth"]
	tenant := sec.Data["HINDSIGHT_API_TENANT_API_KEY"]
	if tenant == "" {
		t.Fatal("tenant API key not generated")
	}
	if sec.Data["HINDSIGHT_CP_DATAPLANE_API_KEY"] != tenant {
		t.Error("control plane dataplane key must equal the tenant key, or the control plane is rejected by the API")
	}
	if sec.Data["HINDSIGHT_CP_ACCESS_KEY"] == "" {
		t.Error("control plane access key not generated")
	}

	api := objs["Deployment/hindsight-api"].Spec.Template.Spec.Containers[0]
	if e := envOf(api, "HINDSIGHT_API_TENANT_EXTENSION"); e == nil || !strings.HasSuffix(e.Value, ":ApiKeyTenantExtension") {
		t.Errorf("API must run ApiKeyTenantExtension so /v1 and /mcp require the key, got %+v", e)
	}
	for _, name := range []string{"Deployment/hindsight-api", "Deployment/hindsight-control-plane"} {
		c := objs[name].Spec.Template.Spec.Containers[0]
		if len(c.EnvFrom) == 0 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "hindsight-auth" {
			t.Errorf("%s must load its keys from Secret hindsight-auth, got %+v", name, c.EnvFrom)
		}
	}

	op := operator(t, objs)
	if e := envOf(op, "HINDSIGHT_URL"); e == nil || e.Value != "http://hindsight-api.agent-orca-system.svc.cluster.local:8888" {
		t.Errorf("operator HINDSIGHT_URL should be the bundled service, got %+v", e)
	}
	k := envOf(op, "HINDSIGHT_API_KEY")
	if k == nil || k.ValueFrom == nil || k.ValueFrom.SecretKeyRef == nil ||
		k.ValueFrom.SecretKeyRef.Name != "hindsight-auth" || k.ValueFrom.SecretKeyRef.Key != "HINDSIGHT_API_TENANT_API_KEY" {
		t.Errorf("operator must read the tenant key from hindsight-auth, got %+v", k)
	}
}

func TestDisabled_NoServerNoClient(t *testing.T) {
	objs := render(t, "hindsight.enabled=false")
	for k := range objs {
		if strings.Contains(k, "hindsight") {
			t.Errorf("hindsight.enabled=false must render no hindsight resources, found %s", k)
		}
	}
	op := operator(t, objs)
	if e := envOf(op, "HINDSIGHT_URL"); e != nil {
		t.Errorf("no URL means memory off; HINDSIGHT_URL should be unset, got %+v", e)
	}
	if e := envOf(op, "HINDSIGHT_API_KEY"); e != nil {
		t.Errorf("no key without a server; HINDSIGHT_API_KEY should be unset, got %+v", e)
	}
}

func TestExternal_ExplicitURLAndKey(t *testing.T) {
	objs := render(t,
		"hindsight.enabled=false",
		"hindsight.url=https://hindsight.example.com",
		"hindsight.apiKeySecret.name=ext-key",
		"hindsight.apiKeySecret.key=api-key",
	)
	for k := range objs {
		if strings.Contains(k, "hindsight") {
			t.Errorf("external mode must not render hindsight resources, found %s", k)
		}
	}
	op := operator(t, objs)
	if e := envOf(op, "HINDSIGHT_URL"); e == nil || e.Value != "https://hindsight.example.com" {
		t.Errorf("explicit url ignored: %+v", e)
	}
	k := envOf(op, "HINDSIGHT_API_KEY")
	if k == nil || k.ValueFrom.SecretKeyRef.Name != "ext-key" || k.ValueFrom.SecretKeyRef.Key != "api-key" {
		t.Errorf("explicit apiKeySecret ignored: %+v", k)
	}
}

func TestBundled_ExplicitOverridesWin(t *testing.T) {
	objs := render(t,
		"hindsight.url=https://hindsight.example.com",
		"hindsight.apiKeySecret.name=other",
	)
	op := operator(t, objs)
	if e := envOf(op, "HINDSIGHT_URL"); e == nil || e.Value != "https://hindsight.example.com" {
		t.Errorf("explicit url must win over the derived one: %+v", e)
	}
	k := envOf(op, "HINDSIGHT_API_KEY")
	if k == nil || k.ValueFrom.SecretKeyRef.Name != "other" || k.ValueFrom.SecretKeyRef.Key != "HINDSIGHT_API_TENANT_API_KEY" {
		t.Errorf("explicit secret name must win and the key must default: %+v", k)
	}
}

func TestBringYourOwnSecret(t *testing.T) {
	objs := render(t, "hindsight.auth.manageSecret=false", "hindsight.existingSecret=my-hindsight")
	if _, ok := objs["Secret/my-hindsight"]; ok {
		t.Error("manageSecret=false must not render the Secret")
	}
	if _, ok := objs["Secret/hindsight-auth"]; ok {
		t.Error("manageSecret=false must not render the default Secret")
	}
	api := objs["Deployment/hindsight-api"].Spec.Template.Spec.Containers[0]
	if len(api.EnvFrom) == 0 || api.EnvFrom[0].SecretRef.Name != "my-hindsight" {
		t.Errorf("API must load the user's Secret, got %+v", api.EnvFrom)
	}
	k := envOf(operator(t, objs), "HINDSIGHT_API_KEY")
	if k == nil || k.ValueFrom.SecretKeyRef.Name != "my-hindsight" {
		t.Errorf("operator must read the key from the user's Secret: %+v", k)
	}
}

func TestDerivedURLFollowsServicePort(t *testing.T) {
	objs := render(t, "hindsight.api.service.port=9000")
	e := envOf(operator(t, objs), "HINDSIGHT_URL")
	if e == nil || !strings.HasSuffix(e.Value, ":9000") {
		t.Errorf("derived URL must follow hindsight.api.service.port, got %+v", e)
	}
}

// Resource requests and limits are a deployment decision, not a chart default
// (repo convention): the hindsight block in our values.yaml must not set any.
func TestValuesSetNoHindsightResources(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "agent-orca", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Hindsight map[string]any `json:"hindsight"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, n any)
	walk = func(path string, n any) {
		m, ok := n.(map[string]any)
		if !ok {
			return
		}
		for k, c := range m {
			if k == "resources" {
				t.Errorf("hindsight values must not set %s.resources by default", path)
			}
			walk(path+"."+k, c)
		}
	}
	walk("hindsight", v.Hindsight)
}

// `helm upgrade --reuse-values` skips the chart's new defaults. The chart must
// fail with an actionable message instead of rendering a broken Hindsight.
func TestReuseValuesWithoutDefaultsFailsClearly(t *testing.T) {
	cmd := exec.Command("helm", "template", "agent-orca", builtChart(t), "-n", "agent-orca-system",
		"--set", "hindsight.existingSecret=", "--set", "hindsight.fullnameOverride=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected render to fail when the hindsight defaults are missing")
	}
	if !strings.Contains(string(out), "--reset-then-reuse-values") {
		t.Errorf("error should tell the user how to fix it, got:\n%s", out)
	}
}

func TestNetworkPolicy_LimitsHindsightAPIIngress(t *testing.T) {
	np, ok := render(t)["NetworkPolicy/hindsight-api"]
	if !ok {
		t.Fatal("NetworkPolicy/hindsight-api not rendered")
	}
	sel := np.Spec.PodSelector.MatchLabels
	if sel["app.kubernetes.io/name"] != "hindsight" || sel["app.kubernetes.io/component"] != "api" || sel["app.kubernetes.io/instance"] != "agent-orca" {
		t.Errorf("policy must select only the bundled API pods, got %v", sel)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("want one ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 8888 {
		t.Errorf("must open only the API port 8888, got %+v", rule.Ports)
	}
	var operator, hindsight, runs bool
	for _, p := range rule.From {
		switch {
		case p.PodSelector != nil && p.PodSelector.MatchLabels["app.kubernetes.io/component"] == "operator":
			operator = true
		case p.PodSelector != nil && p.PodSelector.MatchLabels["app.kubernetes.io/name"] == "hindsight":
			hindsight = true
		case p.NamespaceSelector != nil && p.PodSelector != nil && len(p.PodSelector.MatchExpressions) == 1 &&
			p.PodSelector.MatchExpressions[0].Key == "agentorca.io/run":
			runs = true
		default:
			t.Errorf("unexpected peer %+v", p)
		}
	}
	if !operator || !hindsight || !runs {
		t.Errorf("peers must be the operator, hindsight's own pods and run pods in any namespace: operator=%v hindsight=%v runs=%v", operator, hindsight, runs)
	}
}

func TestNetworkPolicy_ExtraIngressAndOptOut(t *testing.T) {
	np := render(t,
		"hindsight.networkPolicy.extraIngress[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=kube-system",
	)["NetworkPolicy/hindsight-api"]
	if got := len(np.Spec.Ingress[0].From); got != 4 {
		t.Errorf("extraIngress peer should be appended to the three defaults, got %d peers", got)
	}

	objs := render(t, "hindsight.networkPolicy.enabled=false")
	if _, ok := objs["NetworkPolicy/hindsight-api"]; ok {
		t.Error("networkPolicy.enabled=false must not render the policy")
	}
	if _, ok := render(t, "hindsight.enabled=false")["NetworkPolicy/hindsight-api"]; ok {
		t.Error("no policy without the bundled server")
	}
}
