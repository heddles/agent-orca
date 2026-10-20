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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file implements the workstation half of the ACP filesystem relay
// (issue #34, "pod drives the workstation"): while `aoctl acp serve` is
// watching a run, it polls the agent-orca ACP API for fs/* requests issued by
// the agent pod, validates them against the local policy, and forwards them
// to the ACP client (e.g. Zed) — which already answers fs/read_text_file and
// fs/write_text_file exactly as Claude Code or Gemini CLI would.
//
// The bridge is the enforcement point, because it is where the files live:
//   - workspace-root confinement: only paths under the session working
//     directory (from session/new "cwd") plus any --fs-roots are allowed;
//   - writes require fs_write_text_file capability (readOnly policy never
//     reaches the editor: the pod-side router refuses them first, and the
//     bridge refuses them again as defense in depth);
//   - write approval: when the agent's policy (or the --fs-require-approval
//     flag) asks for it, every write is confirmed via an ACP elicitation
//     before it reaches the editor.

// fsPollInterval is the cadence for relaying fs requests off the ACP API during
// an active run. Read latency budget is ~1 poll; the relayed tool call itself
// waits up to 300s pod-side (internal/router defaultFSToolTimeoutSeconds; not
// configurable on the Agent CRD).
const fsPollInterval = 500 * time.Millisecond

// fsBridgeOptions carries the operator-side (workstation) filesystem policy
// knobs for `aoctl acp serve` that narrow — never widen — the agent's own
// Agent.spec.filesystem policy advertised in the manifest.
type fsBridgeOptions struct {
	// extraRoots are additional directory trees the user opted into via
	// --fs-roots, on top of the session workspace (cwd).
	extraRoots []string
	// requireApproval, when true, forces an approval elicitation for every
	// write regardless of what the agent policy says.
	requireApproval bool
	// rememberApproval (--fs-remember-approval): after the user approves one
	// write, subsequent writes in the same session skip the elicitation.
	rememberApproval bool
}

// watchFSSession polls the ACP API for pending fs requests of the run a
// session is currently driving and answers each of them. It re-resolves the
// run ID each cycle so continuation runs (awaiting→resume chains) are followed;
// it exits when ctx is cancelled or the run disappears. A permanently
// unavailable relay (501: the operator was started without a state store) ends
// polling quietly — the pod side surfaces a clear error instead of timing out.
func (b *acpBridge) watchFSSession(ctx context.Context, sessionID string) {
	fsPolicy := b.fsPolicy()
	if fsPolicy == nil {
		// The manifest is cached from initialize: a bridge process started
		// before the agent gained spec.filesystem never polls, and pod-side fs
		// calls sit pending until the router's fs timeout. Make that visible.
		b.srv.logf("acp: filesystem relay inactive for this session (agent manifest has no filesystem policy) — " +
			"restart the editor agent so a fresh bridge picks it up\n")
		return
	}
	b.srv.logf("acp: filesystem relay active (mode %s) — polling fs requests\n", fsPolicy.Mode)
	ticker := time.NewTicker(fsPollInterval)
	defer ticker.Stop()
	for {
		runID := b.sessionRunID(sessionID)
		if runID == "" {
			return
		}
		reqs, err := b.client.ListFSPending(ctx, runID, fsPollInterval)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if isFSRelayUnavailable(err) {
				b.srv.logf("acp: filesystem relay not available on this agent-orca deployment; disabling fs/* polling\n")
				return
			}
			if isFSRunNotFound(err) {
				// Run replaced by a continuation (resume flow) or terminated:
				// keep watching the session for a successor run.
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				continue
			}
			b.srv.logf("acp: fs pending poll failed: %v\n", err)
		}
		for i := range reqs {
			// Pass the session this watcher belongs to directly: reverse-mapping
			// run→session per request would race with session record updates
			// (new prompt / re-sent session/new) and orphan the answer.
			b.serveFSRequest(ctx, sessionID, runID, reqs[i], fsPolicy)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fsPolicy resolves the fs policy from the cached agent manifest; nil means
// the agent has no filesystem surface.
func (b *acpBridge) fsPolicy() *ACPFSPolicy {
	b.mu.Lock()
	man := b.manifest
	b.mu.Unlock()
	if man.Filesystem == nil {
		return nil
	}
	return man.Filesystem
}

// serveFSRequest validates, forwards, and answers one relayed fs request.
// sessionID is the watching session (known to the watcher); runID is the run
// the request is scoped to.
func (b *acpBridge) serveFSRequest(
	ctx context.Context, sessionID, runID string, fr acpfsRelayRequest, policy *ACPFSPolicy,
) {
	isWrite := fr.Method == acpMethodFSWrite

	switch fr.Method {
	case acpMethodFSRead, acpMethodFSList, acpMethodFSWrite:
		// relayable
	default:
		b.respondFSErr(ctx, runID, fr.ID, -32601, "not a relayable fs method: "+fr.Method)
		return
	}
	if isWrite && policy.Mode != "readWrite" {
		b.respondFSErr(ctx, runID, fr.ID, -32002, "workspace writes are not enabled for this agent")
		return
	}

	roots := b.workspaceRoots(sessionID)
	if len(roots) == 0 {
		b.respondFSErr(ctx, runID, fr.ID, -32004,
			fmt.Sprintf("no workspace root is known for session %q (session/new had no cwd); "+
				"runs watched by this bridge: %s", sessionID, b.watchedRuns()))
		return
	}

	switch fr.Method {
	case acpMethodFSRead:
		b.serveFSRead(ctx, runID, sessionID, roots, fr)
	case acpMethodFSList:
		b.serveFSList(ctx, runID, roots, fr)
	case acpMethodFSWrite:
		b.serveFSWrite(ctx, runID, sessionID, roots, fr, policy)
	}
}

// serveFSList answers an fs/list_dir request locally — ACP v1 defines no
// directory-listing method for the editor, so the bridge (which already runs
// with the user's permissions) enumerates the confined path itself. Read-only
// metadata, so no approval elicitation.
func (b *acpBridge) serveFSList(ctx context.Context, runID string, roots []string, fr acpfsRelayRequest) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(fr.Params, &p); err != nil {
		b.respondFSErr(ctx, runID, fr.ID, -32602, "invalid fs_list_dir params")
		return
	}
	if strings.TrimSpace(p.Path) == "" {
		p.Path = "."
	}
	dir, ok := fsConfine(roots, p.Path)
	if !ok {
		b.respondFSErr(ctx, runID, fr.ID, -32002,
			"path escapes the workspace roots this session exposes: "+p.Path)
		return
	}
	// The bridge reads the disk itself here (no editor in the loop), so the
	// lexical confinement is not enough: a symlink inside the workspace could
	// point anywhere on the host. Resolve it and re-check containment.
	realDir, err := fsWithinRealRoots(roots, dir)
	if errors.Is(err, errFSSymlinkEscape) {
		b.respondFSErr(ctx, runID, fr.ID, -32002,
			"path resolves outside the workspace roots this session exposes (symlink): "+p.Path)
		return
	}
	if err != nil {
		b.respondFSErr(ctx, runID, fr.ID, -32000, fmt.Sprintf("listing directory: %v", err))
		return
	}
	entries, err := os.ReadDir(realDir)
	if err != nil {
		b.respondFSErr(ctx, runID, fr.ID, -32000, fmt.Sprintf("listing directory: %v", err))
		return
	}
	type fsEntry struct {
		Name string `json:"name"`
		Type string `json:"type"` // "file" | "dir"
		Size int64  `json:"size,omitempty"`
	}
	out := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		entry := fsEntry{Name: e.Name(), Type: "file"}
		if e.IsDir() {
			entry.Type = "dir"
		} else if info, ierr := e.Info(); ierr == nil {
			entry.Size = info.Size()
		}
		out = append(out, entry)
	}
	result, err := json.Marshal(map[string]any{"path": dir, "entries": out})
	if err != nil {
		b.respondFSErr(ctx, runID, fr.ID, -32000, "encoding fs_list_dir result")
		return
	}
	b.respondFSResult(ctx, runID, fr.ID, result)
}

// serveFSRead confines, forwards, and answers an fs/read_text_file request.
func (b *acpBridge) serveFSRead(ctx context.Context, runID, sessionID string, roots []string, fr acpfsRelayRequest) {
	var p struct {
		Path  string `json:"path"`
		Line  *int   `json:"line"`
		Limit *int   `json:"limit"`
	}
	if err := json.Unmarshal(fr.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
		b.respondFSErr(ctx, runID, fr.ID, -32602, "invalid fs_read_text_file params")
		return
	}
	path, ok := fsConfine(roots, p.Path)
	if !ok {
		b.respondFSErr(ctx, runID, fr.ID, -32002,
			"path escapes the workspace roots this session exposes: "+p.Path)
		return
	}
	params := acpFSReadParams{
		SessionID: sessionID,
		Path:      path,
		Line:      p.Line,
		Limit:     p.Limit,
	}
	msg, err := b.srv.sendRequest(ctx, acpMethodFSRead, params)
	if err != nil {
		b.respondFSErr(ctx, runID, fr.ID, -32000, err.Error())
		return
	}
	var out acpFSReadResult
	raw, _ := json.Marshal(msg.Result)
	if err := json.Unmarshal(raw, &out); err != nil {
		b.srv.logf("acp: unexpected fs/read_text_file result: %v\n", err)
		b.respondFSErr(ctx, runID, fr.ID, -32000, "editor returned an unreadable fs/read_text_file result") //nolint:lll
		return
	}
	result, err := json.Marshal(out)
	if err != nil {
		b.respondFSErr(ctx, runID, fr.ID, -32000, "encoding fs/read_text_file result")
		return
	}
	b.respondFSResult(ctx, runID, fr.ID, result)
}

// serveFSWrite confines, approves (when required), forwards, and answers an
// fs/write_text_file request.
func (b *acpBridge) serveFSWrite(
	ctx context.Context, runID, sessionID string, roots []string, fr acpfsRelayRequest, policy *ACPFSPolicy,
) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(fr.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
		b.respondFSErr(ctx, runID, fr.ID, -32602, "invalid fs_write_text_file params")
		return
	}
	abs, ok := fsConfine(roots, p.Path)
	if !ok {
		b.respondFSErr(ctx, runID, fr.ID, -32002,
			"path escapes the workspace roots this session exposes: "+p.Path)
		return
	}
	// Each side can only narrow the other (issue #41): a prompt is required
	// when either the workstation or the agent asks for one, and an approval
	// is remembered only when both allow it.
	remember := b.fsOpts.rememberApproval && policy.rememberAllowed()
	if b.fsOpts.requireApproval || policy.WriteApproval {
		if remember && b.sessionWritesAllowed(sessionID) {
			// The user already approved a write in this session — auto-allow.
		} else {
			approved, err := b.fsApproveWrite(ctx, sessionID, b.agentName, abs, len(p.Content))
			if err != nil {
				// The prompt itself failed (editor without elicitation support,
				// transport error, timeout): distinct from a user decline so the
				// agent (and the #34 writeup) can tell "no prompt appeared" apart
				// from "the user said no".
				b.respondFSErr(ctx, runID, fr.ID, -32000, fmt.Sprintf("write approval prompt failed: %v", err))
				return
			}
			if !approved {
				b.respondFSErr(ctx, runID, fr.ID, -32003, "write declined by the user in the editor")
				return
			}
			if remember {
				b.markSessionWritesAllowed(sessionID)
			}
		}
	}
	// Issue #40: snapshot the about-to-be-replaced content and stash a
	// before/after diff for the trace translation (acp_trace.go) to attach to
	// the fs_write_text_file tool_call entry, so the editor renders the edit
	// instead of a bare confirmation. Deliberately after approval — the diff
	// describes state the user just allowed to change — and best-effort: any
	// failure leaves no diff and the write proceeds exactly as before. A nil
	// diff still replaces the slot so a stale one never attaches to this call.
	b.stashSessionFSDiff(sessionID, b.prepareFSDiff(ctx, sessionID, roots, abs, p.Content))
	params := acpFSWriteParams{
		SessionID: sessionID,
		Path:      abs,
		Content:   p.Content,
	}
	msg, err := b.srv.sendRequest(ctx, acpMethodFSWrite, params)
	if err != nil {
		// The editor refused the write: drop the diff so the failed tool result
		// never renders a change that did not happen.
		b.popSessionFSDiff(sessionID)
		b.respondFSErr(ctx, runID, fr.ID, -32000, err.Error())
		return
	}
	// ACP fs/write_text_file returns null on success.
	rawResult, _ := json.Marshal(msg.Result)
	b.respondFSResult(ctx, runID, fr.ID, rawResult)
}

// fsApproveWrite elicits explicit user approval for one write. It returns
// (false, nil) when the user declined, (true, nil) when the user accepted,
// and (false, err) when the approval prompt itself failed (editor without
// elicitation support, invalid params, timeout) — a distinct outcome so the
// pod-side tool error can say "no prompt appeared" instead of "declined".
// An accept approves the write regardless of the form content: clients render
// elicitation forms differently (some submit without the enum field), and the
// accept action is the approval signal. Any failure refuses the write: it
// never proceeds unapproved.
func (b *acpBridge) fsApproveWrite(ctx context.Context, sessionID, agentName, absPath string, n int) (bool, error) {
	// Show the path relative to the workspace where possible to keep the
	// prompt readable.
	display := b.fsDisplayPath(absPath)
	elicitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := b.srv.sendRequest(elicitCtx, acpMethodElicitationCreate, acpElicitParams{
		SessionID: sessionID,
		Message:   fmt.Sprintf("Agent %q wants to write %d bytes to %s. Allow?", agentName, n, display),
		Mode:      "form",
		// requestedSchema must be an ElicitationSchema object (type/properties/
		// required) — passing the properties map directly is rejected by the
		// client with Invalid params (-32602).
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer": map[string]any{
					"type":        "string",
					"enum":        []string{"allow", "deny"},
					"description": "Allow the agent to write this file?",
				},
			},
			"required": []string{"answer"},
		},
	})
	if err != nil {
		b.srv.logf("acp: write approval elicitation failed: %v\n", err)
		return false, err
	}
	var elicit acpElicitResponse
	raw, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(raw, &elicit); err != nil {
		b.srv.logf("acp: unreadable write-approval response: %v\n", err)
		return false, fmt.Errorf("unreadable approval response from the editor: %v", err)
	}
	if elicit.Action != "accept" {
		return false, nil
	}
	if answer, _ := elicit.Content["answer"].(string); answer != "allow" {
		// Approved via the accept action even though the form field is
		// missing or unexpected — log it so the behavior is explainable.
		b.srv.logf("acp: write approved by elicitation accept (form answer %q)", answer)
	}
	return true, nil
}

// watchedRuns lists the run IDs the bridge's sessions currently point at —
// used in fs error messages so a scope mismatch is diagnosable from the trace.
func (b *acpBridge) watchedRuns() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	runs := make([]string, 0, len(b.sessions))
	for id, s := range b.sessions {
		runs = append(runs, fmt.Sprintf("%s→%s", id, s.RunID))
	}
	if len(runs) == 0 {
		return "(none)"
	}
	return strings.Join(runs, ", ")
}

// sessionWritesAllowed reports whether --fs-remember-approval has already
// recorded a user-approved write for this session.
func (b *acpBridge) sessionWritesAllowed(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		return s.WritesAllowed
	}
	return false
}

// markSessionWritesAllowed records that the user approved a write in this
// session, so --fs-remember-approval can auto-allow subsequent writes.
func (b *acpBridge) markSessionWritesAllowed(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		s.WritesAllowed = true
	}
}

// sessionRunID returns the run a session is currently driving (which moves
// forward across continuation runs during the awaiting→resume flow).
func (b *acpBridge) sessionRunID(sessionID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		return s.RunID
	}
	return ""
}

// setSessionRun moves a session's record to a continuation run.
func (b *acpBridge) setSessionRun(sessionID, runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		s.RunID = runID
	}
}

// registerRun points a session at a new run with a cancellable context,
// creating the record if needed but preserving existing state (notably the
// workspace cwd captured by session/new).
func (b *acpBridge) registerRun(sessionID, runID string, cancel context.CancelFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		sess = &acpSession{SessionID: sessionID}
		b.sessions[sessionID] = sess
	}
	// A new prompt supersedes any turn still running on this session: cancel
	// it so its fs watcher stops and session/cancel keeps reaching the live
	// turn (otherwise two watchers race on the same session).
	if sess.Cancel != nil {
		sess.Cancel()
	}
	sess.RunID = runID
	sess.Cancel = cancel
}

// workspaceRoots returns the roots for a session: the session working
// directory first (session/new "cwd"), then the operator-supplied --fs-roots.
// When the client never delivered a cwd (no session/new, or one without one),
// the bridge process's own working directory is used as the primary root —
// Zed spawns `aoctl acp serve` with the project root as the subprocess cwd —
// so the relay degrades to a still-confined workspace instead of failing.
func (b *acpBridge) workspaceRoots(sessionID string) []string {
	b.mu.Lock()
	roots := append([]string{}, b.fsOpts.extraRoots...)
	s, ok := b.sessions[sessionID]
	var cwd string
	if ok {
		cwd = s.Cwd
	}
	b.mu.Unlock()
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			b.srv.logf("acp: session %q has no workspace cwd — falling back to the bridge "+
				"process working directory %q (pass --fs-roots to override)\n", sessionID, wd)
			cwd = wd
		}
	}
	if cwd != "" {
		// The session cwd is the primary workspace; prefer it over extras.
		roots = append([]string{cwd}, roots...)
	}
	expanded := make([]string, 0, len(roots))
	for _, r := range roots {
		if abs, err := abspath(r); err == nil {
			expanded = append(expanded, abs)
		}
	}
	return expanded
}

// allWorkspaceRoots unions every known session root with the extra roots; used
// for cosmetic path display in approval prompts.
func (b *acpBridge) allWorkspaceRoots() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	roots := append([]string{}, b.fsOpts.extraRoots...)
	for _, s := range b.sessions {
		if s.Cwd != "" {
			roots = append(roots, s.Cwd)
		}
	}
	return roots
}

// fsConfine resolves userPath against the configured roots (relative paths are
// resolved against each root; absolute paths must already sit inside one) and
// returns the absolute path that stays inside a root after lexical cleanup.
// Symlinks are not resolved here. That is sufficient for fs/read_text_file and
// fs/write_text_file, where the editor performs the access; anything the
// bridge reads from disk itself (fs_list_dir, the write-diff disk fallback)
// must also pass fsWithinRealRoots.
func fsConfine(roots []string, userPath string) (string, bool) {
	if strings.TrimSpace(userPath) == "" {
		return "", false
	}
	clean := filepath.Clean(userPath)
	for _, root := range roots {
		// "." addresses the workspace root itself (fs_list_dir default).
		if clean == "." {
			return root, true
		}
		var abs string
		if filepath.IsAbs(clean) {
			abs = filepath.Clean(clean)
		} else {
			abs = filepath.Join(root, clean)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return abs, true
	}
	return "", false
}

// errFSSymlinkEscape marks a lexically confined path whose real (symlink-
// resolved) location lies outside every workspace root.
var errFSSymlinkEscape = errors.New("path resolves outside the workspace roots")

// fsWithinRealRoots resolves every symlink in abs (already lexically confined
// by fsConfine) and returns the real path when it still lies inside one of the
// real roots. It is the enforcement point for operations the bridge performs
// on disk itself: there, unlike for editor-mediated reads/writes, nothing else
// stands between the agent and the host filesystem. A missing path returns the
// underlying stat error.
func fsWithinRealRoots(roots []string, abs string) (string, error) {
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(realRoot, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return real, nil
	}
	return "", errFSSymlinkEscape
}

// abspath expands ~ and makes a path absolute for confinement checks.
func abspath(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

// respondFSResult posts a successful relay response back to the ACP API.
func (b *acpBridge) respondFSResult(ctx context.Context, runID, requestID string, result json.RawMessage) {
	err := b.client.RespondFSPending(ctx, runID, requestID, acpfsRelayResponse{Result: result})
	if err != nil {
		b.srv.logf("acp: storing fs response failed (run %s req %s): %v\n", runID, requestID, err)
	}
}

// respondFSErr posts an error relay response back to the ACP API. The pod-side
// router turns it into a structured tool error the LLM can act on.
func (b *acpBridge) respondFSErr(ctx context.Context, runID, requestID string, code int, message string) {
	b.srv.logf("acp: fs request rejected (run %s req %s code %d): %s\n", runID, requestID, code, message)
	err := b.client.RespondFSPending(ctx, runID, requestID, acpfsRelayResponse{
		Error: &acpfsRelayError{Code: code, Message: message},
	})
	if err != nil {
		b.srv.logf("acp: storing fs error response failed (run %s req %s): %v\n", runID, requestID, err)
	}
}

// isFSRelayUnavailable detects the permanent 501 installed-relay-off response.
func isFSRelayUnavailable(err error) bool {
	var e *fsRelayError
	return errors.As(err, &e) && e.status == 501
}

// isFSRunNotFound detects a 404 (run deleted or never owned by this tenant).
func isFSRunNotFound(err error) bool {
	var e *fsRelayError
	return errors.As(err, &e) && e.status == 404
}

// fsRelayError is an error carrying an HTTP status for the status checks above.
type fsRelayError struct {
	status int
	msg    string
}

func (e *fsRelayError) Error() string       { return e.msg }
func (e *fsRelayError) HTTPStatusCode() int { return e.status }
