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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

func newPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "red-team", Name: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent", Image: "busybox"},
				// A second (sidecar) container that must NOT be affected by overrides.
				{Name: "model-router", Image: "busybox"},
			},
			InitContainers: []corev1.Container{
				{Name: "init-helper", Image: "busybox"},
			},
		},
	}
}

func containerByName(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func hasVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func hasTunMount(c *corev1.Container) bool {
	if c == nil {
		return false
	}
	for _, m := range c.VolumeMounts {
		if m.MountPath == tunDeviceMountPath {
			return true
		}
	}
	return false
}

func TestEnforcePodSecurity_DefaultRestricted(t *testing.T) {
	pod := newPod()
	EnforcePodSecurity(pod, nil)

	agent := containerByName(pod, "agent")
	if agent == nil { //nolint:staticcheck

		t.Fatal("agent container not found")
	}
	sc := agent.SecurityContext //nolint:staticcheck

	if sc == nil || sc.Privileged == nil || *sc.Privileged != false {
		// Privileged unset/nil is acceptable (defaults to false); ensure not true.
		if sc != nil && sc.Privileged != nil && *sc.Privileged {
			t.Fatalf("agent container should not be privileged by default")
		}
	}
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != agentRunUserID {
		t.Fatalf("agent container RunAsUser = %v, want %d", sc.RunAsUser, agentRunUserID)
	}
	if sc == nil || !*sc.RunAsNonRoot {
		t.Fatalf("agent container should be RunAsNonRoot by default")
	}
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("agent container should have ReadOnlyRootFilesystem=true by default")
	}
	if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("agent container capabilities = %v, want Drop:[ALL]", sc.Capabilities)
	}
	// Sidecar must remain restricted even when no override present.
	router := containerByName(pod, "model-router")
	if router == nil || router.SecurityContext == nil || !*router.SecurityContext.RunAsNonRoot {
		t.Fatalf("model-router sidecar should retain restricted profile")
	}
	// No tun volume when there's no override.
	if hasVolume(pod, tunDeviceVolumeName) {
		t.Fatalf("tun volume must not be present without an override")
	}
	if hasTunMount(agent) {
		t.Fatalf("tun mount must not be present on agent without an override")
	}
}

func TestEnforcePodSecurity_PrivilegedOverride(t *testing.T) {
	pod := newPod()
	override := &agentorcav1alpha1.PodSecurityOverride{
		Privileged: true,
	}
	EnforcePodSecurity(pod, override)

	agent := containerByName(pod, "agent")
	if agent == nil { //nolint:staticcheck

		t.Fatal("agent container not found")
	}
	sc := agent.SecurityContext //nolint:staticcheck

	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Fatalf("agent container should be privileged, sc=%v", sc)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Fatalf("privileged agent should default to root (uid 0), got %v", sc.RunAsUser)
	}
	if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
		t.Fatalf("privileged agent should allow root (RunAsNonRoot=false), got %v", sc.RunAsNonRoot)
	}
	if sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem {
		t.Fatalf("privileged agent should default to writable root FS, got %v", sc.ReadOnlyRootFilesystem)
	}
	// Capabilities.Drop must NOT be forced to [ALL] (privileged ignores caps; forcing
	// Drop:[ALL] alongside Privileged=true is contradictory noise).
	if sc.Capabilities != nil && len(sc.Capabilities.Drop) > 0 {
		t.Fatalf("privileged agent should not have a capabilities drop set, got %v", sc.Capabilities.Drop)
	}
	// tun device mounted on agent only.
	if !hasVolume(pod, tunDeviceVolumeName) {
		t.Fatalf("tun volume should be present with a privileged override")
	}
	if !hasTunMount(agent) {
		t.Fatalf("agent container should have /dev/net/tun mounted")
	}
	// Sidecar unaffected.
	router := containerByName(pod, "model-router")
	if router == nil || router.SecurityContext == nil || router.SecurityContext.Privileged != nil && *router.SecurityContext.Privileged {
		t.Fatalf("model-router sidecar must NOT become privileged: %v", router.SecurityContext)
	}
	if hasTunMount(router) {
		t.Fatalf("model-router sidecar must NOT get the tun mount")
	}
}

func TestEnforcePodSecurity_AddCapabilitiesOnly(t *testing.T) {
	pod := newPod()
	uid := int64(1000)
	override := &agentorcav1alpha1.PodSecurityOverride{
		AddCapabilities: []string{"NET_ADMIN"},
		RunAsUser:       &uid,
	}
	EnforcePodSecurity(pod, override)

	agent := containerByName(pod, "agent")
	sc := agent.SecurityContext
	if sc == nil || sc.Capabilities == nil {
		t.Fatalf("agent capabilities not set: %v", sc)
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != corev1.Capability("NET_ADMIN") {
		t.Fatalf("agent Add capabilities = %v, want [NET_ADMIN]", sc.Capabilities.Add)
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("agent Drop should remain [ALL], got %v", sc.Capabilities.Drop)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Fatalf("agent RunAsUser = %v, want 1000", sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("non-privileged override with non-root uid should keep RunAsNonRoot=true")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Fatalf("granular override should not set privileged")
	}
	// tun device still mounted so the capability is actually usable.
	if !hasVolume(pod, tunDeviceVolumeName) || !hasTunMount(agent) {
		t.Fatalf("tun device should be mounted for capability-based override")
	}
}

func TestEnforcePodSecurity_ReadOnlyRootFilesystemOverride(t *testing.T) {
	pod := newPod()
	rofs := false
	override := &agentorcav1alpha1.PodSecurityOverride{
		Privileged:             true,
		ReadOnlyRootFilesystem: &rofs,
	}
	EnforcePodSecurity(pod, override)

	agent := containerByName(pod, "agent")
	sc := agent.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || *sc.ReadOnlyRootFilesystem != false {
		t.Fatalf("agent ReadOnlyRootFilesystem = %v, want false", sc.ReadOnlyRootFilesystem)
	}
}

func TestEnforcePodSecurity_SeccompRetained(t *testing.T) {
	pod := newPod()
	EnforcePodSecurity(pod, nil)

	agent := containerByName(pod, "agent")
	if agent.SecurityContext == nil || agent.SecurityContext.SeccompProfile == nil {
		t.Fatal("agent should retain a seccomp profile by default")
	}
	if agent.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("agent seccomp = %v, want RuntimeDefault", agent.SecurityContext.SeccompProfile.Type)
	}
}
