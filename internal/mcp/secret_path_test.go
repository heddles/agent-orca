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
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/podbuilder"
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
