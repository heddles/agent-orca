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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxEgressPreset names a vetted set of package-registry and source hosts.
// +kubebuilder:validation:Enum=go;npm;pypi;crates;maven;github-read;mise
type SandboxEgressPreset string

// Egress presets. Each expands to read-only rules for the ecosystem's hosts
// (see internal/openshell/presets.go).
const (
	SandboxEgressPresetGo         SandboxEgressPreset = "go"
	SandboxEgressPresetNPM        SandboxEgressPreset = "npm"
	SandboxEgressPresetPyPI       SandboxEgressPreset = "pypi"
	SandboxEgressPresetCrates     SandboxEgressPreset = "crates"
	SandboxEgressPresetMaven      SandboxEgressPreset = "maven"
	SandboxEgressPresetGitHubRead SandboxEgressPreset = "github-read"
	SandboxEgressPresetMise       SandboxEgressPreset = "mise"
)

// SandboxClassSpec is a reusable sandbox profile set by cluster admins.
type SandboxClassSpec struct {
	// NamespaceSelector limits which namespaces' Agents may use the class. Empty
	// allows every namespace that has opted in to sandboxes.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// Enforcement is the minimum network enforcement for Agents of this class:
	// with enforce, an Agent cannot weaken it to audit.
	// +optional
	Enforcement SandboxEnforcement `json:"enforcement,omitempty"`

	// EgressPresets add vetted read-only host lists, e.g. go or npm.
	// +optional
	EgressPresets []SandboxEgressPreset `json:"egressPresets,omitempty"`

	// Egress adds endpoints every Agent of the class may reach, on top of the
	// Agent's own.
	// +optional
	Egress []SandboxEgressRule `json:"egress,omitempty"`

	// Filesystem adds paths every Agent of the class gets.
	// +optional
	Filesystem *SandboxFilesystem `json:"filesystem,omitempty"`

	// Resources are the defaults for Agents that set no spec.resources.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector, Tolerations, RuntimeClassName and PriorityClassName place the
	// class's sandbox pods, replacing the operator-wide openshell.placement. They
	// live here, not on the Agent, because a toleration can admit a pod to any
	// tainted node; the class is cluster-scoped, so only cluster admins set them.
	// They need the gateway's drivers.kubernetes.allowDriverConfig.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Enforcement",type=string,JSONPath=`.spec.enforcement`
// +kubebuilder:printcolumn:name="Presets",type=string,JSONPath=`.spec.egressPresets`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandboxClass is a cluster-wide sandbox profile: egress, filesystem, resource
// defaults, placement and minimum enforcement. Agents use one with
// spec.runtime.sandbox.classRef and may add to it, but not weaken it.
type SandboxClass struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxClassSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxClassList contains a list of SandboxClass.
type SandboxClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClass{}, &SandboxClassList{})
}
