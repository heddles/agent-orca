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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fs write diffs (issue #40): surface relayed fs_write_text_file calls to the
// editor as before/after diffs (ACP ToolCallContent "diff") instead of opaque
// whole-file replacements, while keeping the wire write itself a full-file
// fs/write_text_file (ACP v1 has no patch method; the editor remains the
// arbiter of the final file state). Everything here is best-effort: any
// failure leaves no diff and the write proceeds exactly as before.

// fsWriteDiff is the before/after pair the bridge computes for one
// fs_write_text_file relay (issue #40). Old == nil means the write creates
// the file (the client renders a full add). The toolResult trace translation
// (acp_trace.go) attaches it to the matching tool_call entry.
type fsWriteDiff struct {
	Path string  `json:"path"`
	Old  *string `json:"old,omitempty"` // nil = file created by this write
	New  string  `json:"new"`
}

// toolCallContent renders d as one ACP v1 ToolCallContent "diff" entry.
func (d *fsWriteDiff) toolCallContent() acpToolCallContent {
	return acpToolCallContent{
		Type:    "diff",
		Path:    d.Path,
		OldText: d.Old,
		NewText: d.New,
	}
}

// Pod-side tool names of the relayed fs tools (internal/router/fs_tools.go);
// matched by name in the trace-event translation. Kept local (stringly) so
// cmd/aoctl stays decoupled from internal/router.
const (
	fsWriteToolName = "fs_write_text_file"
	fsReadToolName  = "fs_read_text_file"
	fsListToolName  = "fs_list_dir"
)

// fsDiffMaxBytes caps the old/new file sizes the bridge ships as diff
// payloads. Above it the write renders as a plain tool result (the previous
// behavior) — the diff is a rendering nicety, never a correctness or
// write-path dependency. 100 KiB keeps typical source files on the diff path
// while bounding the session/update payload for huge generated files.
const fsDiffMaxBytes = 100 * 1024

// fsDiffReadTimeout bounds the editor round-trip used to fetch the content
// the write is about to replace.
const fsDiffReadTimeout = 5 * time.Second

// fsDiffFor builds the diff payload for one write, or nil when the write
// should render as a plain tool result: oversized old/new content, a no-op
// write (old == new), or — for created files — oversized content. existed
// must reflect whether the target file existed before the write.
func fsDiffFor(path, oldContent, newContent string, existed bool) *fsWriteDiff {
	if existed && len(oldContent) > fsDiffMaxBytes {
		return nil
	}
	if len(newContent) > fsDiffMaxBytes {
		return nil
	}
	if existed && oldContent == newContent {
		return nil // no-op write: a changeless diff is pure noise
	}
	var old *string
	if existed {
		c := oldContent
		old = &c
	}
	return &fsWriteDiff{Path: path, Old: old, New: newContent}
}

// readFSOriginal fetches the content the editor is about to replace: the
// editor's own fs/read_text_file channel first (it sees unsaved buffer state
// and is the arbiter of access), then a plain disk read as fallback. ok=false
// when neither can provide the before-state.
func (b *acpBridge) readFSOriginal(ctx context.Context, sessionID, abs string) (string, bool) {
	editorCtx, cancel := context.WithTimeout(ctx, fsDiffReadTimeout)
	defer cancel()
	msg, err := b.srv.sendRequest(editorCtx, acpMethodFSRead, acpFSReadParams{
		SessionID: sessionID,
		Path:      abs,
	})
	if err == nil {
		var out acpFSReadResult
		raw, _ := json.Marshal(msg.Result)
		if json.Unmarshal(raw, &out) == nil {
			return out.Content, true
		}
	}
	raw, rerr := os.ReadFile(abs)
	if rerr != nil {
		return "", false
	}
	return string(raw), true
}

// prepareFSDiff computes the diff for one write just before it is forwarded
// to the editor. That timing matters: once fs/write_text_file succeeds, the
// original content is gone from disk. A missing file renders as a full-add
// diff; a directory target (which the editor will refuse anyway) and an
// unreadable original skip the diff rather than guess.
func (b *acpBridge) prepareFSDiff(ctx context.Context, sessionID, abs, content string) *fsWriteDiff {
	info, statErr := os.Stat(abs)
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil
	}
	if statErr == nil && info.IsDir() {
		return nil
	}
	existed := statErr == nil
	if !existed {
		return fsDiffFor(abs, "", content, false)
	}
	old, ok := b.readFSOriginal(ctx, sessionID, abs)
	if !ok {
		return nil
	}
	return fsDiffFor(abs, old, content, true)
}

// fsDiffStore holds the pending write diff per session between the relay
// (serveFSWrite computes it just before forwarding) and the trace translation
// (emitTraceToolResult attaches it to the fs_write tool_call entry). A
// package-level registry keeps acpSession untouched; agent-orca serializes
// tool calls within a run, so the single slot per session cannot interleave.
// Entries are always consumed by the next fs_write toolResult or replaced by
// the next write; a stale entry can at worst attach one old diff to a reused
// session id, which the diff payload's own path makes visible and harmless.
type fsDiffStore struct {
	mu sync.Mutex
	m  map[string]*fsWriteDiff
}

var pendingFSDiffs = &fsDiffStore{m: map[string]*fsWriteDiff{}}

// stashSessionFSDiff stores the diff computed for the in-flight write of a
// session. nil is a no-op (a skipped diff never clobbers an earlier one).
func (b *acpBridge) stashSessionFSDiff(sessionID string, d *fsWriteDiff) {
	if d == nil {
		return
	}
	pendingFSDiffs.mu.Lock()
	defer pendingFSDiffs.mu.Unlock()
	pendingFSDiffs.m[sessionID] = d
}

// popSessionFSDiff returns and clears the stashed write diff, if any.
func (b *acpBridge) popSessionFSDiff(sessionID string) *fsWriteDiff {
	pendingFSDiffs.mu.Lock()
	defer pendingFSDiffs.mu.Unlock()
	d := pendingFSDiffs.m[sessionID]
	delete(pendingFSDiffs.m, sessionID)
	return d
}

// fsDisplayPath renders abs relative to the first workspace root that
// contains it; paths outside every root stay absolute. Cosmetic only — used
// in write approval prompts (and previously inlined there).
func (b *acpBridge) fsDisplayPath(abs string) string {
	for _, root := range b.allWorkspaceRoots() {
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return abs
}

// emitToolCallUpdateWithContent is emitToolCallUpdate for callers that own
// their ToolCallContent entries (the ACP "diff" variant cannot be expressed
// as a wrapped ContentBlock). Shared id/title fallbacks with the sibling.
func (b *acpBridge) emitToolCallUpdateWithContent(
	sessionID string, tc openaiToolCall, update, status, kind string, content []acpToolCallContent,
) {
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("tool_%d", tc.Index)
	}
	title := tc.Name
	if title == "" {
		title = "tool call"
	}
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{
		SessionID: sessionID,
		Update: acpToolCallUpdate{
			SessionUpdate: update,
			ToolCallID:    id,
			Title:         title,
			Kind:          kind,
			Status:        status,
			Content:       content,
		},
	})
}
