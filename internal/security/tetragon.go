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

package security

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TetragonGVR is the GroupVersionResource for Cilium Tetragon TracingPolicy.
var TetragonGVR = schema.GroupVersionResource{
	Group:    "cilium.io",
	Version:  "v1alpha1",
	Resource: "tracingpoliciesnamespaced",
}

// TracingPolicyKind is the Kubernetes kind for namespaced Tetragon policies.
const TracingPolicyKind = "TracingPolicyNamespaced"

// tetragonPolicy describes the structure we write as a TracingPolicyNamespaced.
// Using unstructured avoids a hard dependency on the Tetragon Go client package.
type tetragonPolicy struct {
	Spec tetragonSpec `json:"spec"`
}

type tetragonSpec struct {
	// PodSelector scopes the policy to pods with a specific label.
	PodSelector metav1.LabelSelector `json:"podSelector"`
	// KProbes lists the syscall-level hooks.
	KProbes []tetragonKProbe `json:"kprobes"`
}

type tetragonKProbe struct {
	Call      string             `json:"call"`
	Syscall   bool               `json:"syscall"`
	Selectors []tetragonSelector `json:"selectors,omitempty"`
}

type tetragonSelector struct {
	MatchActions []tetragonAction `json:"matchActions"`
}

type tetragonAction struct {
	Action string `json:"action"`
}

// BuildTracingPolicy returns an unstructured TracingPolicyNamespaced that blocks
// dangerous syscalls for agent pods identified by the given AgentRun name.
//
// Blocked syscalls:
//   - execve / execveat:  prevents spawning arbitrary sub-processes
//   - ptrace:             prevents attaching to other processes
//   - mount:              prevents mounting additional filesystems
//   - open_by_handle_at:  prevents bypassing path-based access controls
//
// If Tetragon is not installed in the cluster the controller skips creation and
// logs a warning. Tetragon is an optional dependency.
func BuildTracingPolicy(runName, namespace string) *unstructured.Unstructured {
	killAction := tetragonSelector{
		MatchActions: []tetragonAction{{Action: "Sigkill"}},
	}

	policy := tetragonPolicy{
		Spec: tetragonSpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAgentRunName: SafeLabelValue(runName),
				},
			},
			KProbes: []tetragonKProbe{
				{Call: "sys_execve", Syscall: true, Selectors: []tetragonSelector{killAction}},
				{Call: "sys_execveat", Syscall: true, Selectors: []tetragonSelector{killAction}},
				{Call: "sys_ptrace", Syscall: true, Selectors: []tetragonSelector{killAction}},
				{Call: "sys_mount", Syscall: true, Selectors: []tetragonSelector{killAction}},
				{Call: "sys_open_by_handle_at", Syscall: true, Selectors: []tetragonSelector{killAction}},
			},
		},
	}

	raw, _ := json.Marshal(policy)
	var specMap map[string]any
	_ = json.Unmarshal(raw, &specMap)

	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cilium.io/v1alpha1",
			"kind":       TracingPolicyKind,
			"metadata": map[string]any{
				"name":      tracingPolicyName(runName),
				"namespace": namespace,
				"labels": map[string]any{
					LabelAgentRunName: SafeLabelValue(runName),
					LabelManagedBy:    ManagedByValue,
				},
			},
		},
	}
	_ = unstructured.SetNestedMap(u.Object, specMap["spec"].(map[string]any), "spec")
	return u
}

// TracingPolicyName returns the deterministic name for a run's TracingPolicy.
func TracingPolicyName(runName string) string { return tracingPolicyName(runName) }

func tracingPolicyName(runName string) string {
	return "agentorc-run-" + runName
}
