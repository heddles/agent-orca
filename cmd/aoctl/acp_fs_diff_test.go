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
	"context"
	"encoding/json"
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
	// A nil stash (diff skipped for this write) must clear the slot so the
	// write never inherits an earlier write's leftover diff.
	b.stashSessionFSDiff("s1", fsDiffFor("p", "", "new", false))
	b.stashSessionFSDiff("s1", nil)
	if got := b.popSessionFSDiff("s1"); got != nil {
		t.Fatalf("nil stash must clear the slot, got %+v", got)
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

// --- end-to-end diff rendering (issue #40 regressions) ----------------------

// fsDiffWriteTurn drives one fs_write_text_file the way a live run does:
// toolCall trace event → relayed write → toolResult trace event. It returns
// the completed tool_call_update the editor received.
func fsDiffWriteTurn(t *testing.T, tb *fsTestBridge, toolName, content string) acpToolCallUpdate {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"path": "f.txt", "content": content})
	tb.bridge.handleTraceEvent(acpTraceEvent{Type: "toolCall", Name: toolName, BackendType: "builtin"}, fsTestSessionID)
	tb.bridge.serveFSRequest(context.Background(), fsTestSessionID, "run-1", acpfsRelayRequest{
		ID: "req-1", Method: acpMethodFSWrite, Params: params,
	}, tb.bridge.fsPolicy())
	tb.api.responseRecorded(t)
	tb.bridge.handleTraceEvent(acpTraceEvent{
		Type: "toolResult", Name: toolName, BackendType: "builtin", Result: `{"written":true}`,
	}, fsTestSessionID)

	var done *acpToolCallUpdate
	fsWaitFor(t, func() bool {
		tb.editor.mu.Lock()
		defer tb.editor.mu.Unlock()
		for _, raw := range tb.editor.updates {
			var u acpToolCallUpdate
			if json.Unmarshal(raw, &u) == nil && u.SessionUpdate == acpUpdateToolCallUpd && u.Status == acpToolStatusCompleted {
				done = &u
				return true
			}
		}
		return false
	})
	if done == nil {
		t.Fatal("editor never received the completed tool_call_update")
	}
	return *done
}

func fsDiffEntry(u acpToolCallUpdate) *acpToolCallContent {
	for i := range u.Content {
		if u.Content[i].Type == "diff" {
			return &u.Content[i]
		}
	}
	return nil
}

func TestFSWriteRendersDiffForExistingFile(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	target := filepath.Join(tb.workspace, "f.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tb.editor.readOut = "old (unsaved buffer)\n" // the editor's view wins over disk

	u := fsDiffWriteTurn(t, tb, "fs_write_text_file", "new\n")
	if u.Kind != acpToolKindEdit {
		t.Fatalf("want kind %q, got %q", acpToolKindEdit, u.Kind)
	}
	d := fsDiffEntry(u)
	if d == nil || d.Path != target || d.OldText == nil || *d.OldText != "old (unsaved buffer)\n" || d.NewText != "new\n" {
		t.Fatalf("want before/after diff from the editor buffer, got %+v", u.Content)
	}
}

func TestFSWriteRendersFullAddForNewFile(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	u := fsDiffWriteTurn(t, tb, "fs_write_text_file", "hello\n")
	d := fsDiffEntry(u)
	if d == nil || d.OldText != nil || d.NewText != "hello\n" {
		t.Fatalf("want full-add diff (no oldText), got %+v", u.Content)
	}
	// A created file needs no editor read of the (missing) original.
	tb.editor.mu.Lock()
	defer tb.editor.mu.Unlock()
	if len(tb.editor.reads) != 0 {
		t.Fatalf("new file must not trigger an editor read, got %d", len(tb.editor.reads))
	}
}

func TestFSWriteUnderscoreAliasRendersDiff(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	u := fsDiffWriteTurn(t, tb, "_fs_write_text_file", "x\n")
	if u.Kind != acpToolKindEdit || fsDiffEntry(u) == nil {
		t.Fatalf("legacy _fs_write_text_file alias must render as an edit diff, got %+v", u)
	}
}

// A null fs/read_text_file result must not read as an empty file (which would
// render the whole original as deleted): fall back to the on-disk content.
func TestFSWriteNullEditorReadFallsBackToDisk(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	if err := os.WriteFile(filepath.Join(tb.workspace, "f.txt"), []byte("on disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tb.editor.readNull = true

	d := fsDiffEntry(fsDiffWriteTurn(t, tb, "fs_write_text_file", "new\n"))
	if d == nil || d.OldText == nil || *d.OldText != "on disk\n" {
		t.Fatalf("want disk fallback as oldText, got %+v", d)
	}
}

// An editor-refused write must not render a diff for a change that never
// happened.
func TestFSWriteRefusedRendersNoDiff(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	tb.editor.writeOK = false
	if d := fsDiffEntry(fsDiffWriteTurn(t, tb, "fs_write_text_file", "new\n")); d != nil {
		t.Fatalf("refused write must not carry a diff, got %+v", d)
	}
}

// A write whose diff is skipped (oversized) must not inherit a stale diff
// left in the session slot by an earlier write.
func TestFSWriteSkippedDiffClearsStaleSlot(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	tb.bridge.stashSessionFSDiff(fsTestSessionID, fsDiffFor("/stale", "a", "b", true))

	big := strings.Repeat("x", fsDiffMaxBytes+1)
	u := fsDiffWriteTurn(t, tb, "fs_write_text_file", big)
	if d := fsDiffEntry(u); d != nil {
		t.Fatalf("oversized write must render as plain text, got stale diff %+v", d)
	}
}

// Regression (PR #45 review): the write-diff disk fallback is the bridge
// reading the host itself, so a workspace symlink pointing outside must not
// leak the outside file's content into the editor diff.
func TestFSWriteDiffDiskFallbackRespectsSymlinkConfinement(t *testing.T) {
	tb := newFSTestBridge(t, false, nil)
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("TOP SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(tb.workspace, "f.txt")); err != nil {
		t.Fatal(err)
	}
	tb.editor.readNull = true // force the disk fallback

	u := fsDiffWriteTurn(t, tb, "fs_write_text_file", "new\n")
	if d := fsDiffEntry(u); d != nil && d.OldText != nil && strings.Contains(*d.OldText, "TOP SECRET") {
		t.Fatalf("disk fallback leaked a file outside the workspace: %q", *d.OldText)
	}
}
