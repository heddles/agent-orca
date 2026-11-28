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
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func sessionVolume() agentorcav1alpha1.SandboxVolume {
	return agentorcav1alpha1.SandboxVolume{Name: "sessions", ClaimName: "assistant-sessions", MountPath: "/var/lib/agent"}
}

func TestBuildSpecMountsClassVolumes(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "img", Sandbox: &agentorcav1alpha1.SandboxSpec{}},
	}}
	cache := agentorcav1alpha1.SandboxVolume{Name: "cache", ClaimName: "pkg-cache", MountPath: "/var/cache/pkg", SubPath: "npm", ReadOnly: true}
	spec := BuildSpec(SpecInput{Agent: agent, RouterHost: routerHost, Placement: Placement{
		Volumes: []agentorcav1alpha1.SandboxVolume{sessionVolume(), cache},
	}})

	want := map[string]any{"kubernetes": map[string]any{
		"volumes": []any{
			map[string]any{"name": "sessions", "persistent_volume_claim": map[string]any{"claim_name": "assistant-sessions", "read_only": false}},
			map[string]any{"name": "cache", "persistent_volume_claim": map[string]any{"claim_name": "pkg-cache", "read_only": true}},
		},
		"containers": map[string]any{"agent": map[string]any{"volume_mounts": []any{
			map[string]any{"name": "sessions", "mount_path": "/var/lib/agent", "read_only": false},
			map[string]any{"name": "cache", "mount_path": "/var/cache/pkg", "read_only": true, "sub_path": "npm"},
		}}},
	}}
	if !reflect.DeepEqual(spec.Template.DriverConfig, want) {
		t.Errorf("driver config = %#v, want %#v", spec.Template.DriverConfig, want)
	}
	if _, err := structpb.NewStruct(spec.Template.DriverConfig); err != nil {
		t.Errorf("driver config is not encodable as a Struct: %v", err)
	}
	fs := spec.Policy.Filesystem
	if !slices.Contains(fs.ReadWrite, "/var/lib/agent") || slices.Contains(fs.ReadOnly, "/var/lib/agent") {
		t.Errorf("read-write volume path: read_write=%v read_only=%v", fs.ReadWrite, fs.ReadOnly)
	}
	if !slices.Contains(fs.ReadOnly, "/var/cache/pkg") || slices.Contains(fs.ReadWrite, "/var/cache/pkg") {
		t.Errorf("read-only volume path: read_write=%v read_only=%v", fs.ReadWrite, fs.ReadOnly)
	}
}

func TestBuildSpecVolumesKeepPlacementAndAgentPaths(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "img", Sandbox: &agentorcav1alpha1.SandboxSpec{
			Filesystem: &agentorcav1alpha1.SandboxFilesystem{ReadWrite: []string{"/tmp/x", "/var/lib/agent"}},
		}},
	}}
	spec := BuildSpec(SpecInput{Agent: agent, RouterHost: routerHost, Placement: Placement{
		RuntimeClassName: "gvisor", Volumes: []agentorcav1alpha1.SandboxVolume{sessionVolume()},
	}})
	k8s := spec.Template.DriverConfig["kubernetes"].(map[string]any)
	if k8s["pod"].(map[string]any)["runtime_class_name"] != "gvisor" || k8s["volumes"] == nil {
		t.Errorf("driver config = %#v, want both the pod placement and the volumes", k8s)
	}
	if got := spec.Policy.Filesystem.ReadWrite; !reflect.DeepEqual(got, []string{"/tmp/x", "/var/lib/agent"}) {
		t.Errorf("read_write = %v, want the Agent's paths once each", got)
	}
}

func TestBuildSpecWithoutVolumesHasNoDriverConfig(t *testing.T) {
	agent := &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
		Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "img", Sandbox: &agentorcav1alpha1.SandboxSpec{}},
	}}
	if dc := BuildSpec(SpecInput{Agent: agent, RouterHost: routerHost}).Template.DriverConfig; dc != nil {
		t.Errorf("driver config = %#v, want none: gateways without allowDriverConfig must keep accepting it", dc)
	}
}

func TestApplyClassAddsVolumesAndKeepsOperatorPlacement(t *testing.T) {
	class := &agentorcav1alpha1.SandboxClass{Spec: agentorcav1alpha1.SandboxClassSpec{
		Volumes: []agentorcav1alpha1.SandboxVolume{sessionVolume()},
	}}
	_, placement := ApplyClass(classAgent(), class, Placement{NodeSelector: map[string]string{"pool": "general"}})
	if placement.NodeSelector["pool"] != "general" {
		t.Errorf("placement = %+v: a class with only volumes must keep the operator's nodes", placement)
	}
	if !reflect.DeepEqual(placement.Volumes, class.Spec.Volumes) {
		t.Errorf("volumes = %+v, want the class's", placement.Volumes)
	}
}

func TestApplyClassTwiceUnionsVolumesFirstWins(t *testing.T) {
	first := &agentorcav1alpha1.SandboxClass{Spec: agentorcav1alpha1.SandboxClassSpec{
		Volumes: []agentorcav1alpha1.SandboxVolume{sessionVolume()},
	}}
	other := sessionVolume()
	other.ClaimName = "elsewhere"
	second := &agentorcav1alpha1.SandboxClass{Spec: agentorcav1alpha1.SandboxClassSpec{
		RuntimeClassName: "gvisor",
		Volumes:          []agentorcav1alpha1.SandboxVolume{other, {Name: "cache", ClaimName: "c", MountPath: "/c"}},
	}}
	agent, placement := ApplyClass(classAgent(), first, Placement{})
	_, placement = ApplyClass(agent, second, placement)
	if len(placement.Volumes) != 2 || placement.Volumes[0].ClaimName != "assistant-sessions" || placement.Volumes[1].Name != "cache" {
		t.Errorf("volumes = %+v, want the first class's sessions plus the second's cache", placement.Volumes)
	}
	if placement.RuntimeClassName != "gvisor" {
		t.Errorf("placement = %+v, want the second class's nodes", placement)
	}
}

func TestOperatorPlacementCannotMountVolumes(t *testing.T) {
	t.Setenv("OPENSHELL_SANDBOX_PLACEMENT", `{"nodeSelector":{"a":"b"},"Volumes":[{"name":"x","claimName":"x","mountPath":"/x"}]}`)
	p, err := PlacementFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Volumes) != 0 {
		t.Errorf("volumes = %+v: only a class may mount a volume", p.Volumes)
	}
	if b, _ := json.Marshal(p); strings.Contains(strings.ToLower(string(b)), "volume") {
		t.Errorf("placement JSON carries volumes: %s", b)
	}
}
