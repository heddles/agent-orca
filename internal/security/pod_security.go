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
// All pods created by agent-orca must comply with the Kubernetes PodSecurityStandards
// "restricted" profile and are additionally hardened with Tetragon eBPF policies and
// per-run NetworkPolicies.
//
// The restricted baseline can be *scoped-relaxed* for the agent container of an Agent
// via Spec.Runtime.SecurityContextOverride (e.g. to bring up a VPN tun device). The
// override applies only to the container named "agent" (never the model-router sidecar
// or tool/router pods) and is gated at admission by a validating webhook that requires
// the namespace label `agentorca.io/enable-privileged-pods: "true"` and an Agent-level
// GuardrailPolicyRef.
package security

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const (
	// agentRunUserID is the UID/GID used for all agent and tool containers.
	// 65532 is the standard nonroot user used by distroless images.
	agentRunUserID int64 = 65532

	// LabelEnablePrivilegedPods is the namespace label that opts a namespace into
	// allowing PodSecurityOverride on its agents. Admission of an Agent (or AgentRun
	// against such an Agent) whose runtime requests an override is rejected unless
	// the namespace carries this label set to "true".
	LabelEnablePrivilegedPods = "agentorca.io/enable-privileged-pods"
	// PrivilegedPodsAllowedValue is the only value the label may take.
	PrivilegedPodsAllowedValue = "true"

	// saTokenVolumeName is the volume name for the projected SA token.
	saTokenVolumeName = "agentorca-sa-token"

	// tmpVolumeName is the volume name for the writable /tmp emptyDir.
	tmpVolumeName = "tmp"

	// tunDeviceVolumeName is the volume name for the /dev/net/tun hostPath device
	// mounted into privileged/VPN agent containers.
	tunDeviceVolumeName = "agentorca-tun"

	// tunDeviceMountPath is the in-container path of the tun device.
	tunDeviceMountPath = "/dev/net/tun"

	// tunDeviceHostPath is the host path of the tun device node.
	tunDeviceHostPath = "/dev/net/tun"

	// ModelRouterTokenAudience is the audience used for projected SA tokens.
	// The model-router validates tokens with this audience via TokenReview.
	ModelRouterTokenAudience = "agentorca/model-router"

	// UITokenAudience is the audience used by the UIProxy pod's projected SA token.
	// The operator validates requests from the proxy with this audience via TokenReview.
	UITokenAudience = "agentorca/ui"

	// saTokenExpirySeconds is the SA token expiry. Kubelet auto-refreshes before expiry.
	saTokenExpirySeconds int64 = 900 // 15 minutes
)

// hostPathCharDevice is the HostPathType used to expose the tun device node to a
// privileged/agent container. We construct it from a string literal (rather than
// corev1.HostPathCharDevice) so we don't depend on the exact symbolic-constant
// availability across k8s API versions; the kubelet validates the string value.
var hostPathCharDevice = corev1.HostPathType("CharDevice")

// EnforcePodSecurity applies the restricted PodSecurityStandards profile to pod and
// injects the projected ServiceAccount token volume for model-router authentication,
// with one scoped exception: when agentOverride describes a PodSecurityOverride, it is
// applied to the container named "agent" only (mounting /dev/net/tun and relaxing the
// restricted baseline for that container). All other containers — including the
// model-router sidecar and any framework init containers — always receive the full
// restricted profile.
//
// This is called for every AgentRun pod, every tool pod, and every AgentDeployment pod.
func EnforcePodSecurity(pod *corev1.Pod, agentOverride *agentorcav1alpha1.PodSecurityOverride) {
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

	// /dev/net/tun hostPath device, mounted only into the agent container when an
	// override (e.g. VPN) has been requested. HostPath for a char device is safe to
	// expose to a privileged container; non-privileged elevated-cap containers also
	// receive the device node so they can open it.
	if agentOverride != nil {
		tunVolume := corev1.Volume{
			Name: tunDeviceVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: tunDeviceHostPath,
					Type: &hostPathCharDevice,
				},
			},
		}
		pod.Spec.Volumes = appendVolumeIfAbsent(pod.Spec.Volumes, tunVolume)
	}

	// Per-container security context.
	for i := range pod.Spec.Containers {
		// The override is scoped to the "agent" container only; sidecars/init
		// containers always take the restricted baseline.
		co := containerOverride(agentOverride, pod.Spec.Containers[i].Name)
		enforceContainerSecurity(&pod.Spec.Containers[i], co)
		mountSAToken(&pod.Spec.Containers[i])
		mountTmp(&pod.Spec.Containers[i])
		if co != nil {
			mountTunDevice(&pod.Spec.Containers[i])
		}
	}
	for i := range pod.Spec.InitContainers {
		enforceContainerSecurity(&pod.Spec.InitContainers[i], nil)
		// Native sidecars (restartPolicy=Always) run for the lifetime of the pod
		// and need the same volume mounts as regular containers.
		if pod.Spec.InitContainers[i].RestartPolicy != nil && *pod.Spec.InitContainers[i].RestartPolicy == corev1.ContainerRestartPolicyAlways {
			mountSAToken(&pod.Spec.InitContainers[i])
			mountTmp(&pod.Spec.InitContainers[i])
		}
	}
}

// containerOverride returns the override to apply to a container, scoping it to the
// "agent" container only.
func containerOverride(agentOverride *agentorcav1alpha1.PodSecurityOverride, name string) *agentorcav1alpha1.PodSecurityOverride {
	if agentOverride == nil || name != agentContainerName {
		return nil
	}
	return agentOverride
}

const (
	// agentContainerName is the name of the agent container produced by podbuilder.
	agentContainerName = "agent"
)

// enforceContainerSecurity applies the restricted-profile settings to a single
// container, unless override is non-nil in which case the requested (scoped) privilege
// posture is applied. The override is only ever passed for the "agent" container.
func enforceContainerSecurity(c *corev1.Container, override *agentorcav1alpha1.PodSecurityOverride) {
	if c.SecurityContext == nil {
		c.SecurityContext = &corev1.SecurityContext{}
	}
	csc := c.SecurityContext

	if override == nil {
		// Default restricted baseline (PodSecurityStandards "restricted").
		csc.AllowPrivilegeEscalation = ptr.To(false)
		csc.ReadOnlyRootFilesystem = ptr.To(true)
		csc.RunAsNonRoot = ptr.To(true)
		csc.RunAsUser = ptr.To(agentRunUserID)
		csc.RunAsGroup = ptr.To(agentRunUserID)
		csc.Capabilities = &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		}
		csc.SeccompProfile = &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		}
		return
	}

	// --- Override path: relax the restricted baseline for the agent container ---
	//
	// Admission (webhook) guarantees:
	//   - at most one of privileged / addCapabilities is set,
	//   - runAsUser == 0 is only set with privileged (or a root-requiring cap).

	if override.Privileged {
		// Full privileges: seccomp/capability enforcement is bypassed by the runtime.
		csc.Privileged = ptr.To(true)
		csc.AllowPrivilegeEscalation = ptr.To(true)
		// Do not set Capabilities.Drop: [ALL] — it is meaningless under privileged
		// and could surprise tooling that inspects the pod spec.
		if override.RunAsUser != nil {
			csc.RunAsUser = override.RunAsUser
			csc.RunAsNonRoot = ptr.To(*override.RunAsUser != 0)
		} else {
			// OpenVPN traditionally runs as root; default to 0 (root) when privileged.
			csc.RunAsUser = ptr.To(int64(0))
			csc.RunAsNonRoot = ptr.To(false)
		}
		if override.ReadOnlyRootFilesystem != nil {
			csc.ReadOnlyRootFilesystem = override.ReadOnlyRootFilesystem
		} else {
			// Some VPN/security tooling writes helper scripts and logs under /.
			csc.ReadOnlyRootFilesystem = ptr.To(false)
		}
		// Leave SeccompProfile untouched; privileged mode ignores it.
		return
	}

	// Granular capability-add path: keep the restricted baseline but ADD caps.
	caps := &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
	}
	for _, s := range override.AddCapabilities {
		caps.Add = append(caps.Add, corev1.Capability(s))
	}
	csc.Capabilities = caps
	csc.AllowPrivilegeEscalation = ptr.To(false)
	csc.SeccompProfile = &corev1.SeccompProfile{
		Type: corev1.SeccompProfileTypeRuntimeDefault,
	}
	if override.ReadOnlyRootFilesystem != nil {
		csc.ReadOnlyRootFilesystem = override.ReadOnlyRootFilesystem
	} else {
		csc.ReadOnlyRootFilesystem = ptr.To(true)
	}
	if override.RunAsUser != nil {
		csc.RunAsUser = override.RunAsUser
		csc.RunAsNonRoot = ptr.To(*override.RunAsUser != 0)
	} else {
		csc.RunAsNonRoot = ptr.To(true)
		csc.RunAsUser = ptr.To(agentRunUserID)
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
		MountPath: "/var/run/secrets/agentorca",
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

// mountTunDevice injects the /dev/net/tun hostPath device mount into a container.
// Only intended to be called for the agent container when a PodSecurityOverride is active.
func mountTunDevice(c *corev1.Container) {
	for _, vm := range c.VolumeMounts {
		if vm.MountPath == tunDeviceMountPath {
			return // already mounted
		}
	}
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      tunDeviceVolumeName,
		MountPath: tunDeviceMountPath,
		ReadOnly:  false,
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
