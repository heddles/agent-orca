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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSDiffFor(t *testing.T) {
	old := "line1\nline2\n"
	newC := "line1\nline2 changed\n"

	t.Run("modify keeps old", func(t *testing.T) {
		d := fsDiffFor("/w/f.go", old, newC, true)
		if d == nil || d.Old == nil || *d.Old != old || d.New != newC {
			t.Fatalf("want modify diff, got %+v", d)
		}
		cc := d.toolCallContent()
		if cc.Type != "diff" || cc.Path != "/w/f.go" || cc.NewText != newC || cc.OldText != d.Old {
			t.Fatalf("bad toolCallContent: %+v", cc)
		}
	})
	t.Run("create renders full add", func(t *testing.T) {
		d := fsDiffFor("/w/new.go", "", newC, false)
		if d == nil || d.Old != nil || d.New != newC {
			t.Fatalf("want full-add diff, got %+v", d)
		}
	})
	t.Run("no-op write is skipped", func(t *testing.T) {
		if d := fsDiffFor("/w/f.go", old, old, true); d != nil {
			t.Fatalf("want nil for identical content, got %+v", d)
		}
	})
	t.Run("oversized new content is skipped", func(t *testing.T) {
		big := strings.Repeat("x", fsDiffMaxBytes+1)
		if d := fsDiffFor("/w/f.go", old, big, true); d != nil {
			t.Fatal("want nil for oversized new content")
		}
	})
	t.Run("oversized old content is skipped", func(t *testing.T) {
		big := strings.Repeat("x", fsDiffMaxBytes+1)
		if d := fsDiffFor("/w/f.go", big, newC, true); d != nil {
			t.Fatal("want nil for oversized old content")
		}
	})
	t.Run("oversized new file is skipped", func(t *testing.T) {
		big := strings.Repeat("x", fsDiffMaxBytes+1)
		if d := fsDiffFor("/w/f.go", "", big, false); d != nil {
			t.Fatal("want nil for oversized new file")
		}
	})
}

func TestFSDiffStashRoundtrip(t *testing.T) {
	b := &acpBridge{sessions: map[string]*acpSession{}}
	if got := b.popSessionFSDiff("s1"); got != nil {
		t.Fatalf("empty session: want nil, got %+v", got)
	}
	b.sessions["s1"] = &acpSession{}
	d := fsDiffFor("p", "old", "new", true)
	b.stashSessionFSDiff("s1", d)
	if got := b.popSessionFSDiff("s1"); got != d {
		t.Fatalf("pop mismatch: %+v", got)
	}
	if got := b.popSessionFSDiff("s1"); got != nil {
		t.Fatalf("double pop: want nil, got %+v", got)
	}
	// nil stash must be a no-op, not wipe a previously stashed diff.
	d2 := fsDiffFor("p", "", "new", false)
	b.stashSessionFSDiff("s1", d2)
	b.stashSessionFSDiff("s1", nil)
	if got := b.popSessionFSDiff("s1"); got != d2 {
		t.Fatalf("nil stash must not clear: %+v", got)
	}
}

func TestFSDisplayPath(t *testing.T) {
	root := t.TempDir()
	b := &acpBridge{sessions: map[string]*acpSession{"s": {Cwd: root}}}
	inside := filepath.Join(root, "sub", "file.txt")
	if got := b.fsDisplayPath(inside); got != filepath.Join("sub", "file.txt") {
		t.Fatalf("want workspace-relative, got %q", got)
	}
	// A path outside every root stays absolute (best-effort display).
	outside := filepath.Join(os.TempDir(), "fs-diff-elsewhere.txt")
	if got := b.fsDisplayPath(outside); got != outside {
		t.Fatalf("want absolute fallback, got %q", got)
	}
}
