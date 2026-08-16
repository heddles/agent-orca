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

package mcp

import (
	"path/filepath"
	"testing"

	"github.com/floppyfish14/agent-orc/internal/podbuilder"
)

func TestValidateStdioCommand(t *testing.T) {
	mount := filepath.Join(podbuilder.MCPBinDir, "srv", "bin", "mcp")
	t.Run("mounted_ok", func(t *testing.T) {
		if err := validateStdioCommand(mount, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("shell_rejected_even_with_env", func(t *testing.T) {
		t.Setenv("AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO", "true")
		if err := validateStdioCommand("/bin/bash", []string{"-c", "id"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("non_mount_without_env", func(t *testing.T) {
		t.Setenv("AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO", "")
		if err := validateStdioCommand("/usr/bin/node", []string{"app.js"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("non_mount_with_env_allowlist", func(t *testing.T) {
		t.Setenv("AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO", "true")
		if err := validateStdioCommand("/usr/bin/node", []string{"app.js"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("non_mount_dash_c", func(t *testing.T) {
		t.Setenv("AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO", "true")
		if err := validateStdioCommand("/usr/bin/python3", []string{"-c", "print(1)"}); err == nil {
			t.Fatal("expected error")
		}
	})
}
