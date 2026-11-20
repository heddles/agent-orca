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

import agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"

// mcpServerNameOf is the MCPServer a connector or declared Tool belongs to (its
// controller owner), which keys a per-user connection. A Tool that no MCPServer
// owns is keyed by its own name.
func mcpServerNameOf(tool *agentorcav1alpha1.Tool) string {
	for _, ref := range tool.OwnerReferences {
		if ref.Kind == "MCPServer" {
			return ref.Name
		}
	}
	return tool.Name
}
