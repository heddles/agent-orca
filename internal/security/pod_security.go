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

// Package security provides helpers that apply security policies to agent and tool pods.
// All pods created by agent-orc must comply with the Kubernetes PodSecurityStandards
// "restricted" profile and are additionally hardened with Tetragon eBPF policies and
// per-run NetworkPolicies.
package security

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const (
	// agentRunUserID is the UID/GID used for all agent and tool containers.
	// 65532 is the standard nonroot user used by distroless images.
	agentRunUserID int64 = 65532

	// saTokenPath is the mount path for the projected ServiceAccount token used
	// to authenticate with the model-router sidecar.
	saTokenPath = "/var/run/secrets/agentorc/token"

	// saTokenVolumeName is the volume name for the projected SA token.
	saTokenVolumeName = "agentorc-sa-token"

	// tmpVolumeName is the volume name for the writable /tmp emptyDir.
	tmpVolumeName = "tmp"

	// ModelRouterTokenAudience is the audience used for projected SA tokens.
	// The model-router validates tokens with this audience via TokenReview.
	ModelRouterTokenAudience = "agentorc/model-router"

	// UITokenAudience is the audience used by the UIProxy pod's projected SA token.
	// The operator validates requests from the proxy with this audience via TokenReview.
	UITokenAudience = "agentorc/ui"

	// saTokenExpirySeconds is the SA token expiry. Kubelet auto-refreshes before expiry.
	saTokenExpirySeconds int64 = 900 // 15 minutes
)

// EnforcePodSecurity applies the restricted PodSecurityStandards profile to pod and
// injects the projected ServiceAccount token volume for model-router authentication.
// This is called for every AgentRun pod and every tool pod.
func EnforcePodSecurity(pod *corev1.Pod) {
	// Pod-level security context.
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	sc := pod.Spec.SecurityContext
	sc.RunAsNonRoot = ptr.To(true)
	sc.RunAsUser = ptr.To(agentRunUserID)
	sc.RunAsGroup = ptr.To(agentRunUserID)
	sc.FSGroup = ptr.To(agentRunUserID)
	sc.SeccompProfile = &corev1.SeccompProfile{
		Type: corev1.SeccompProfileTypeRuntimeDefault,
	}

	// Projected SA token volume for model-router JWT auth.
	saTokenVolume := corev1.Volume{
		Name: saTokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          ModelRouterTokenAudience,
							ExpirationSeconds: ptr.To(saTokenExpirySeconds),
							Path:              "token",
						},
					},
				},
			},
		},
	}

	// Writable /tmp as memory-backed emptyDir (readOnlyRootFilesystem requires this).
	tmpVolume := corev1.Volume{
		Name: tmpVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: nil, // bounded by container memory limit
			},
		},
	}

	pod.Spec.Volumes = appendVolumeIfAbsent(pod.Spec.Volumes, saTokenVolume)
	pod.Spec.Volumes = appendVolumeIfAbsent(pod.Spec.Volumes, tmpVolume)

	// Per-container security context.
	for i := range pod.Spec.Containers {
		enforceContainerSecurity(&pod.Spec.Containers[i])
		mountSAToken(&pod.Spec.Containers[i])
		mountTmp(&pod.Spec.Containers[i])
	}
	for i := range pod.Spec.InitContainers {
		enforceContainerSecurity(&pod.Spec.InitContainers[i])
		// Native sidecars (restartPolicy=Always) run for the lifetime of the pod
		// and need the same volume mounts as regular containers.
		if pod.Spec.InitContainers[i].RestartPolicy != nil && *pod.Spec.InitContainers[i].RestartPolicy == corev1.ContainerRestartPolicyAlways {
			mountSAToken(&pod.Spec.InitContainers[i])
			mountTmp(&pod.Spec.InitContainers[i])
		}
	}
}

// enforceContainerSecurity applies restricted-profile settings to a single container.
func enforceContainerSecurity(c *corev1.Container) {
	if c.SecurityContext == nil {
		c.SecurityContext = &corev1.SecurityContext{}
	}
	csc := c.SecurityContext
	csc.AllowPrivilegeEscalation = ptr.To(false)
	csc.ReadOnlyRootFilesystem = ptr.To(true)
	csc.RunAsNonRoot = ptr.To(true)
	csc.RunAsUser = ptr.To(agentRunUserID)
	csc.Capabilities = &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
	}
	csc.SeccompProfile = &corev1.SeccompProfile{
		Type: corev1.SeccompProfileTypeRuntimeDefault,
	}
}

// mountSAToken injects the projected SA token volume mount into a container.
func mountSAToken(c *corev1.Container) {
	for _, vm := range c.VolumeMounts {
		if vm.Name == saTokenVolumeName {
			return // already mounted
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      saTokenVolumeName,
		MountPath: "/var/run/secrets/agentorc",
		ReadOnly:  true,
	})
}

// mountTmp injects a writable /tmp volume mount into a container.
func mountTmp(c *corev1.Container) {
	for _, vm := range c.VolumeMounts {
		if vm.MountPath == "/tmp" {
			return // already has a /tmp mount
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      tmpVolumeName,
		MountPath: "/tmp",
	})
}

// appendVolumeIfAbsent appends v to volumes only if no volume with the same name exists.
func appendVolumeIfAbsent(volumes []corev1.Volume, v corev1.Volume) []corev1.Volume {
	for _, existing := range volumes {
		if existing.Name == v.Name {
			return volumes
		}
	}
	return append(volumes, v)
}

// SATokenPath returns the file path of the projected SA token within agent pods.
func SATokenPath() string { return saTokenPath }
