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
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Placement chooses the nodes sandbox pods run on. It is operator-wide
// configuration (chart value openshell.placement), not part of the Agent: a
// toleration can admit a sandbox to any tainted node, including the control
// plane, so only cluster admins set it.
//
// The gateway applies it only with drivers.kubernetes.allowDriverConfig=true;
// otherwise it rejects every sandbox that sets it.
type Placement struct {
	NodeSelector      map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations       []corev1.Toleration `json:"tolerations,omitempty"`
	RuntimeClassName  string              `json:"runtimeClassName,omitempty"`
	PriorityClassName string              `json:"priorityClassName,omitempty"`
}

// IsZero reports whether the placement sets nothing.
func (p Placement) IsZero() bool {
	return len(p.NodeSelector) == 0 && len(p.Tolerations) == 0 &&
		p.RuntimeClassName == "" && p.PriorityClassName == ""
}

// PlacementFromEnv reads the placement from OPENSHELL_SANDBOX_PLACEMENT, a JSON
// Placement. Unset or empty means no placement.
func PlacementFromEnv() (Placement, error) {
	var p Placement
	raw := strings.TrimSpace(os.Getenv("OPENSHELL_SANDBOX_PLACEMENT"))
	if raw == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("parsing OPENSHELL_SANDBOX_PLACEMENT: %w", err)
	}
	return p, nil
}

// driverConfig renders the placement as the Kubernetes driver's config. The
// driver reads snake_case pod fields and passes tolerations to the pod spec
// as-is. Nil when the placement is empty, so gateways without
// allowDriverConfig keep accepting sandboxes.
func (p Placement) driverConfig() map[string]any {
	if p.IsZero() {
		return nil
	}
	pod := map[string]any{}
	if len(p.NodeSelector) > 0 {
		sel := make(map[string]any, len(p.NodeSelector))
		for k, v := range p.NodeSelector {
			sel[k] = v
		}
		pod["node_selector"] = sel
	}
	if len(p.Tolerations) > 0 {
		pod["tolerations"] = toStructValue(p.Tolerations)
	}
	if p.RuntimeClassName != "" {
		pod["runtime_class_name"] = p.RuntimeClassName
	}
	if p.PriorityClassName != "" {
		pod["priority_class_name"] = p.PriorityClassName
	}
	return map[string]any{"kubernetes": map[string]any{"pod": pod}}
}

// templateResources renders the agent's resource requirements in the shape
// the gateway reads from SandboxTemplate.Resources: Kubernetes limits and
// requests with string quantities. CPU and memory are applied as typed fields;
// other resources (e.g. ephemeral-storage) pass through to the pod.
func templateResources(rr *corev1.ResourceRequirements) map[string]any {
	if rr == nil || (len(rr.Limits) == 0 && len(rr.Requests) == 0) {
		return nil
	}
	out := map[string]any{}
	for section, list := range map[string]corev1.ResourceList{"limits": rr.Limits, "requests": rr.Requests} {
		if len(list) == 0 {
			continue
		}
		m := make(map[string]any, len(list))
		for name, q := range list {
			m[string(name)] = q.String()
		}
		out[section] = m
	}
	return out
}

// toStructValue converts v to the plain maps, slices, strings, numbers and
// bools the SDK can encode as a protobuf Struct, through its JSON form.
func toStructValue(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}
