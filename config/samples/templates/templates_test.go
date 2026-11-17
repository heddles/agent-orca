// Package templates_test checks that every shipped template is valid: it
// decodes strictly into the API types (unknown fields are an error), carries
// the labels the catalog relies on, and is accepted by a real API server with
// the project's CRDs installed.
package templates_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	labelTemplate = "agentorca.io/template"
	labelScope    = "agentorca.io/scope"
	labelGroup    = "agentorca.io/template-group"
)

// docs returns every YAML document in the template files, as raw JSON.
func docs(t *testing.T) map[string][][]byte {
	t.Helper()
	files, err := filepath.Glob("*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no template files found: %v", err)
	}
	out := map[string][][]byte{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := dec.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			j, err := yaml.YAMLToJSON(doc)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if string(j) == "null" {
				continue
			}
			out[f] = append(out[f], j)
		}
	}
	return out
}

func TestTemplatesDecodeStrictlyAndCarryTheCatalogMetadata(t *testing.T) {
	all := docs(t)
	if len(all) < 5 || len(all) > 8 {
		t.Errorf("want 5 to 8 templates, have %d", len(all))
	}
	for file, list := range all {
		agents := 0
		for _, j := range list {
			var meta metav1.TypeMeta
			if err := json.Unmarshal(j, &meta); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			var obj client.Object
			switch meta.Kind {
			case "Agent":
				obj = &agentorcav1alpha1.Agent{}
			case "AgentDeployment":
				obj = &agentorcav1alpha1.AgentDeployment{}
			case "Tool":
				obj = &agentorcav1alpha1.Tool{}
			case "GuardrailPolicy":
				obj = &agentorcav1alpha1.GuardrailPolicy{}
			default:
				t.Fatalf("%s: unexpected kind %q", file, meta.Kind)
			}
			dec := json.NewDecoder(bytes.NewReader(j))
			dec.DisallowUnknownFields()
			if err := dec.Decode(obj); err != nil {
				t.Errorf("%s %s: %v", file, meta.Kind, err)
				continue
			}
			l := obj.GetLabels()
			group := strings.TrimSuffix(file, ".yaml")
			if l[labelGroup] != group {
				t.Errorf("%s %s: %s should be %q, got %q", file, meta.Kind, labelGroup, group, l[labelGroup])
			}
			if l[labelScope] != "platform" {
				t.Errorf("%s %s: %s must be platform", file, meta.Kind, labelScope)
			}
			if meta.Kind != "Agent" {
				continue
			}
			agents++
			if l[labelTemplate] != "true" {
				t.Errorf("%s: the Agent must carry %s=true", file, labelTemplate)
			}
			ann := obj.GetAnnotations()
			if ann["agentorca.io/description"] == "" || ann["agentorca.io/maintainer"] == "" {
				t.Errorf("%s: the Agent needs description and maintainer annotations", file)
			}
			var examples []string
			if err := json.Unmarshal([]byte(ann["agentorca.io/example-inputs"]), &examples); err != nil || len(examples) == 0 {
				t.Errorf("%s: example-inputs must be a non-empty JSON array of strings: %v", file, err)
			}
			if s := ann["agentorca.io/input-schema"]; s != "" {
				var schema map[string]any
				if err := json.Unmarshal([]byte(s), &schema); err != nil || schema["type"] != "object" {
					t.Errorf("%s: input-schema must be a JSON Schema object: %v", file, err)
				}
			}
		}
		if agents != 1 {
			t.Errorf("%s: want exactly one template Agent, have %d", file, agents)
		}
	}
}

// The API server, with the CRDs installed, accepts every template. Skipped
// without envtest binaries (KUBEBUILDER_ASSETS or bin/k8s).
func TestTemplatesAreAcceptedByTheAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		entries, err := os.ReadDir(filepath.Join("..", "..", "..", "bin", "k8s"))
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					assets = filepath.Join("..", "..", "..", "bin", "k8s", e.Name())
				}
			}
		}
	}
	if assets == "" {
		t.Skip("no envtest binaries: run make setup-envtest or set KUBEBUILDER_ASSETS")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		env.BinaryAssetsDirectory = assets
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentorcav1alpha1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-orca-system"}}); err != nil {
		t.Fatal(err)
	}
	for file, list := range docs(t) {
		for _, j := range list {
			u := &unstructured.Unstructured{}
			if err := u.UnmarshalJSON(j); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if err := c.Create(ctx, u); err != nil {
				t.Errorf("%s %s/%s: %v", file, u.GetKind(), u.GetName(), err)
			}
		}
	}
}
