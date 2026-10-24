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
	"reflect"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestBuildSpecResourcesAndPlacement(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "img", Sandbox: &agentorcav1alpha1.SandboxSpec{}},
		Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("4"),
				corev1.ResourceMemory:           resource.MustParse("16Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			},
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		},
	}}
	placement := Placement{
		NodeSelector: map[string]string{"agentorca.io/sandbox": "true"},
		Tolerations: []corev1.Toleration{{
			Key: "agentorca.io/sandbox", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		}},
		RuntimeClassName: "gvisor",
	}
	spec := BuildSpec(SpecInput{Agent: agent, RouterHost: routerHost, Placement: placement})

	wantResources := map[string]any{
		"limits":   map[string]any{"cpu": "4", "memory": "16Gi", "ephemeral-storage": "20Gi"},
		"requests": map[string]any{"cpu": "1"},
	}
	if !reflect.DeepEqual(spec.Template.Resources, wantResources) {
		t.Errorf("resources = %#v, want %#v", spec.Template.Resources, wantResources)
	}
	wantDriver := map[string]any{"kubernetes": map[string]any{"pod": map[string]any{
		"node_selector":      map[string]any{"agentorca.io/sandbox": "true"},
		"tolerations":        []any{map[string]any{"key": "agentorca.io/sandbox", "operator": "Exists", "effect": "NoSchedule"}},
		"runtime_class_name": "gvisor",
	}}}
	if !reflect.DeepEqual(spec.Template.DriverConfig, wantDriver) {
		t.Errorf("driver config = %#v, want %#v", spec.Template.DriverConfig, wantDriver)
	}
	// The SDK sends both as protobuf Structs; every value must be encodable.
	for name, m := range map[string]map[string]any{"resources": spec.Template.Resources, "driver config": spec.Template.DriverConfig} {
		if _, err := structpb.NewStruct(m); err != nil {
			t.Errorf("%s is not encodable as a Struct: %v", name, err)
		}
	}
}

func TestBuildSpecWithoutResourcesOrPlacement(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "img", Sandbox: &agentorcav1alpha1.SandboxSpec{}},
	}}
	spec := BuildSpec(SpecInput{Agent: agent, RouterHost: routerHost})
	// A gateway without allowDriverConfig rejects any driver config, so none
	// may be sent when no placement is configured.
	if spec.Template.Resources != nil || spec.Template.DriverConfig != nil {
		t.Errorf("template = %+v, want no resources and no driver config", spec.Template)
	}
}

func TestPlacementFromEnv(t *testing.T) {
	t.Setenv("OPENSHELL_SANDBOX_PLACEMENT", "")
	if p, err := PlacementFromEnv(); err != nil || !p.IsZero() {
		t.Fatalf("unset: %+v, %v", p, err)
	}

	t.Setenv("OPENSHELL_SANDBOX_PLACEMENT",
		`{"nodeSelector":{"pool":"sandbox"},"priorityClassName":"","runtimeClassName":"","tolerations":[{"key":"pool","operator":"Equal","value":"sandbox","effect":"NoSchedule"}]}`)
	p, err := PlacementFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeSelector["pool"] != "sandbox" || len(p.Tolerations) != 1 || p.Tolerations[0].Value != "sandbox" {
		t.Errorf("placement = %+v", p)
	}

	t.Setenv("OPENSHELL_SANDBOX_PLACEMENT", `{"nodeSelector":{"pool":true}}`)
	if _, err := PlacementFromEnv(); err == nil {
		t.Error("a non-string node selector value was accepted")
	}
}
