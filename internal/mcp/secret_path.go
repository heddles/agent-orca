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

	"github.com/floppyfish14/agent-orca/internal/podbuilder"
)

// secretPathRelative returns the path relative to the tool secret mount root,
// or an error if absPath escapes that root (CWE-22).
func secretPathRelative(absPath string) (string, error) {
	clean := filepath.Clean(absPath)
	root := filepath.Clean(podbuilder.ToolSecretsDir)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("secret file path %q is outside %s", absPath, podbuilder.ToolSecretsDir)
	}
	if clean == root {
		return "", fmt.Errorf("secret path must be a file under %s", podbuilder.ToolSecretsDir)
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("invalid secret path %q", absPath)
	}
	return rel, nil
}

// readSecretFile reads a secret value from a path that must lie under the
// operator-mounted tool secret directory, using os.Root to block traversal.
func readSecretFile(path string) (string, error) {
	rel, err := secretPathRelative(path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(podbuilder.ToolSecretsDir)
	if err != nil {
		return "", fmt.Errorf("opening MCP secret root: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.ToSlash(rel))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}
