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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// mcpToolResolutionHint returns an actionable hint when an agent references a Tool
// name that does not exist as a Tool CR, but looks like an MCPServer per-tool
// reference ("<mcpserver>-<tool>") while that MCPServer is in auto-discovery mode
// (discoverability enabled with no spec.tools declared). In that mode the controller
// creates a single connector Tool CR named after the MCPServer — not one per tool —
// so agents must reference the server by name instead of a per-tool CR name.
//
// Returns "" when no useful hint applies (e.g. the referenced object is not a
// discovery-mode MCPServer, or the lookup failed for a non-NotFound reason).
func mcpToolResolutionHint(ctx context.Context, c client.Client, namespace, toolName string) string {
	var servers agentorcav1alpha1.MCPServerList
	if err := c.List(ctx, &servers, client.InNamespace(namespace)); err != nil {
		return ""
	}
	for i := range servers.Items {
		s := &servers.Items[i]
		if !isDiscoveryMode(s) {
			continue
		}
		if strings.HasPrefix(toolName, s.Name+"-") {
			return fmt.Sprintf(
				"; MCPServer %q is in auto-discovery mode (spec.discoverability: enabled) "+
					"and does not create per-tool Tool CRs — reference the server by name %q in agent.spec.tools "+
					"instead of %q",
				s.Name, s.Name, toolName)
		}
	}
	return ""
}

// annotatedToolError wraps a Tool lookup error. For NotFound errors it appends an
// MCP discovery hint (see mcpToolResolutionHint) so operators see an actionable
// message instead of an opaque "Tool ... not found" retry loop.
func annotatedToolError(ctx context.Context, c client.Client, namespace, toolName string, err error) error {
	if errors.IsNotFound(err) {
		if hint := mcpToolResolutionHint(ctx, c, namespace, toolName); hint != "" {
			return fmt.Errorf("getting Tool %q: %w%s", toolName, err, hint)
		}
	}
	return fmt.Errorf("getting Tool %q: %w", toolName, err)
}
