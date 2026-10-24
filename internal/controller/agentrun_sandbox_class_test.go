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
	"testing"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func coderClass(sel *metav1.LabelSelector) *agentorcav1alpha1.SandboxClass {
	return &agentorcav1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "coder"},
		Spec: agentorcav1alpha1.SandboxClassSpec{
			NamespaceSelector: sel,
			Enforcement:       agentorcav1alpha1.SandboxEnforcementEnforce,
			EgressPresets:     []agentorcav1alpha1.SandboxEgressPreset{agentorcav1alpha1.SandboxEgressPresetGo},
			NodeSelector:      map[string]string{"pool": "sandbox"},
		},
	}
}

func classedAgent() *agentorcav1alpha1.Agent {
	a := sandboxedAgent()
	a.Spec.Runtime.Sandbox.ClassRef = "coder"
	return a
}

func TestEnsureSandbox_AppliesSandboxClass(t *testing.T) {
	gw := newStubGateway()
	run, token := gitRun()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"team": "a"}}}
	class := coderClass(&metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}})
	r, _ := sandboxReconciler(t, gw, run, token, ns, class)

	sb, err := r.ensureSandbox(context.Background(), run, classedAgent(), "tok", "http://router:8080")
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}
	spec := gw.specs[sb.Name]
	// The class's placement reaches the driver config...
	pod, _ := spec.Template.DriverConfig["kubernetes"].(map[string]any)["pod"].(map[string]any)
	if sel, _ := pod["node_selector"].(map[string]any); sel["pool"] != "sandbox" {
		t.Errorf("driver config = %v, want the class's node selector", spec.Template.DriverConfig)
	}
	// ...its preset reaches the policy, in enforce mode.
	found := false
	for _, rule := range spec.Policy.NetworkPolicies {
		for _, ep := range rule.Endpoints {
			if ep.Host == "sum.golang.org" {
				found = true
				if ep.Enforcement != osv1.NetworkEnforcementModeEnforce {
					t.Errorf("sum.golang.org enforcement = %v, want the class's enforce", ep.Enforcement)
				}
			}
		}
	}
	if !found {
		t.Error("the class's go preset is not in the sandbox policy")
	}
}

func TestEnsureSandbox_SandboxClassUnusable(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"team": "b"}}}
	for name, objs := range map[string][]client.Object{
		"missing class":         {ns},
		"namespace not allowed": {ns, coderClass(&metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}})},
	} {
		t.Run(name, func(t *testing.T) {
			gw := newStubGateway()
			run, token := gitRun()
			r, _ := sandboxReconciler(t, gw, append(objs, run, token)...)
			_, err := r.ensureSandbox(context.Background(), run, classedAgent(), "tok", "http://router:8080")
			if !errors.Is(err, errSandboxClassUnusable) {
				t.Fatalf("err = %v, want errSandboxClassUnusable", err)
			}
			if len(gw.sandboxes) != 0 {
				t.Error("sandbox created despite an unusable class")
			}
		})
	}
}
