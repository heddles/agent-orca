/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/heddles/agent-orca/internal/podbuilder"
)

var shellInterpreterNames = map[string]struct{}{
	"sh": {}, "bash": {}, "dash": {}, "zsh": {}, "fish": {}, "csh": {}, "tcsh": {},
	"cmd": {}, "cmd.exe": {}, "powershell": {}, "powershell.exe": {}, "pwsh": {}, "pwsh.exe": {}, "posh": {},
}

var allowedNonMountStdioBinaries = map[string]struct{}{
	"node": {}, "nodejs": {}, "python": {}, "python3": {}, "python3.11": {}, "python3.12": {},
	"npx": {}, "pnpm": {}, "pnpm.cmd": {}, "deno": {}, "bun": {}, "uv": {}, "uvx": {},
}

// validateStdioCommand rejects arbitrary shell execution for MCP stdio transports (CWE-78).
// Production MCP servers should use an OCI sidecar image so the binary path is under
// podbuilder.MCPBinDir. For development, set AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO=true
// and use only allowlisted interpreter names; -c / -e args are still blocked.
func validateStdioCommand(cmd string, args []string) error {
	if cmd == "" {
		return fmt.Errorf("MCP stdio command is empty")
	}
	clean := filepath.Clean(cmd)
	if strings.HasPrefix(clean, podbuilder.MCPBinDir+string(os.PathSeparator)) {
		return nil
	}
	if os.Getenv("AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO") != "true" {
		return fmt.Errorf("MCP stdio command %q must live under %s (OCI MCP sidecar); set AGENTORC_MCP_ALLOW_NON_MOUNT_STDIO=true for constrained dev use", cmd, podbuilder.MCPBinDir)
	}
	base := filepath.Base(clean)
	baseLower := strings.ToLower(base)
	if _, bad := shellInterpreterNames[baseLower]; bad {
		return fmt.Errorf("MCP stdio cannot execute shell interpreter %q", base)
	}
	if _, ok := allowedNonMountStdioBinaries[baseLower]; !ok {
		return fmt.Errorf("MCP stdio command base name %q is not allowlisted for non-mounted stdio", base)
	}
	for _, a := range args {
		if a == "-c" || a == "-e" {
			return fmt.Errorf("MCP stdio args cannot include %q when using non-mounted stdio", a)
		}
	}
	return nil
}
