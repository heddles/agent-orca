package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/floppyfish14/agent-orc/internal/podbuilder"
)

func TestSecretPathRelative(t *testing.T) {
	root := podbuilder.ToolSecretsDir
	t.Run("ok", func(t *testing.T) {
		p := filepath.Join(root, "tool", "sec", "key")
		rel, err := secretPathRelative(p)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.ToSlash(rel) != "tool/sec/key" {
			t.Fatalf("rel = %q", rel)
		}
	})
	t.Run("traversal", func(t *testing.T) {
		p := root + "/../etc/passwd"
		_, err := secretPathRelative(p)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "outside") {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("absolute_escape", func(t *testing.T) {
		_, err := secretPathRelative("/etc/passwd")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
