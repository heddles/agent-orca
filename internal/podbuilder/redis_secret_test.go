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

package podbuilder

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func redisTestConfig(secretName string) PodConfig {
	return PodConfig{
		PodName:          "agentorca-run-r",
		Namespace:        "default",
		Agent:            testAgent(),
		TokenSecretName:  "agentorca-run-r-token",
		ServiceAccount:   "sa",
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: "registry.example.com/model-router:v1",
		RouterConfigName: "agentorca-run-r",
		RedisSecretName:  secretName,
	}
}

func hasRedisMount(mounts []corev1.VolumeMount) bool {
	for _, m := range mounts {
		if m.Name == RedisSecretVolume {
			return m.MountPath == RedisSecretDir && m.ReadOnly
		}
	}
	return false
}

func redisVolume(pod *corev1.Pod) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == RedisSecretVolume {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// TestBuild_RedisSecretRouterOnly verifies the Redis password Secret is mounted
// read-only into the model-router sidecar and never into the agent container.
func TestBuild_RedisSecretRouterOnly(t *testing.T) {
	pod := Build(redisTestConfig("agentorca-run-r-redis"))

	vol := redisVolume(pod)
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != "agentorca-run-r-redis" {
		t.Fatalf("expected Secret volume for agentorca-run-r-redis, got %+v", vol)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != RedisSecretKey {
		t.Errorf("expected only the %q key projected, got %+v", RedisSecretKey, vol.Secret.Items)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o400 {
		t.Errorf("expected DefaultMode 0400, got %v", vol.Secret.DefaultMode)
	}

	for _, c := range pod.Spec.InitContainers {
		if c.Name == "model-router" && !hasRedisMount(c.VolumeMounts) {
			t.Error("model-router is missing the read-only Redis password mount")
		}
	}
	for _, c := range pod.Spec.Containers {
		if hasRedisMount(c.VolumeMounts) {
			t.Errorf("container %q must not mount the Redis password", c.Name)
		}
	}
}

func TestBuildRouterOnly_RedisSecret(t *testing.T) {
	pod := BuildRouterOnly(redisTestConfig("agentorca-run-r-redis"))
	if redisVolume(pod) == nil {
		t.Fatal("split-pod router is missing the Redis password volume")
	}
	if !hasRedisMount(pod.Spec.Containers[0].VolumeMounts) {
		t.Error("split-pod router is missing the read-only Redis password mount")
	}
}

func TestBuild_NoRedisSecretWithoutPassword(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"Build":           Build(redisTestConfig("")),
		"BuildRouterOnly": BuildRouterOnly(redisTestConfig("")),
	} {
		if redisVolume(pod) != nil {
			t.Errorf("%s: unexpected Redis password volume when RedisSecretName is empty", name)
		}
	}
}
