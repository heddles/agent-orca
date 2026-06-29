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
