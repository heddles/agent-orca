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

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/acpfs"
)

// Built-in tools that relay ACP v1 agent→client filesystem methods to the
// user's workstation. The model-router does not touch files itself: each call
// is forwarded (via the operator's internal API relay, internal/acpfs) to the
// `aoctl acp serve` bridge, which forwards to the ACP client (e.g. Zed), the
// arbiter of real file access. See internal/acpfs and docs/filesystem-access.md.
const (
	fsToolNameRead  = "fs_read_text_file"
	fsToolNameWrite = "fs_write_text_file"
	fsToolNameList  = "fs_list_dir"

	fsModeReadOnly  = "readOnly"
	fsModeReadWrite = "readWrite"
)

// defaultFSToolTimeoutSeconds bounds the wait for one relayed fs call. It is
// deliberately far above the generic toolTimeout (60s): a write gated by user
// approval in the editor can legitimately take minutes.
const defaultFSToolTimeoutSeconds = 300

// fsPollInterval is the cadence of response polls against the operator API.
const fsPollInterval = 500 * time.Millisecond

// FilesystemConfig, when non-nil on Config, enables the relayed filesystem
// tools. Populated by the operator from Agent.spec.filesystem.
type FilesystemConfig struct {
	// Mode is "readOnly" or "readWrite" (Agent.spec.filesystem.mode). A write
	// tool call under readOnly fails inside the pod before any traffic leaves
	// the cluster; the bridge enforces it again as defense in depth.
	Mode string `json:"mode,omitempty"`
	// ToolTimeoutSeconds bounds one relayed fs call (waiting for the bridge,
	// including user-approval waits). 0 = defaultFSToolTimeoutSeconds.
	ToolTimeoutSeconds int `json:"toolTimeoutSeconds,omitempty"`
}

// allowsRead reports whether reads are enabled.
func (c *FilesystemConfig) allowsRead() bool {
	return c != nil && (c.Mode == fsModeReadOnly || c.Mode == fsModeReadWrite)
}

// allowsWrite reports whether writes (mode readWrite) are enabled.
func (c *FilesystemConfig) allowsWrite() bool {
	return c != nil && c.Mode == fsModeReadWrite
}

// timeout resolves the effective per-call fs timeout.
func (c *FilesystemConfig) timeout() time.Duration {
	if c != nil && c.ToolTimeoutSeconds > 0 {
		return time.Duration(c.ToolTimeoutSeconds) * time.Second
	}
	return defaultFSToolTimeoutSeconds * time.Second
}

// FilesystemToolDefinitions returns the OpenAI function definitions for the
// fs tools this config enables (none when c is nil). The operator appends
// these to Config.ToolDefinitions so the LLM sees them alongside Tool-CR tools.
func (c *FilesystemConfig) ToolDefinitions() []ToolDefinition {
	if c == nil || !c.allowsRead() {
		return nil
	}
	defs := []ToolDefinition{
		{
			Name: fsToolNameList,
			Description: "List the entries of a directory in the user's workspace through the editor bridge. " +
				"Use this to explore the workspace structure before reading files. " +
				"Paths must point inside the workspace folder the editor opened.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative or absolute path of the directory to list (\".\" for the workspace root)"}
  },
  "required": ["path"]
}`),
			BackendType: backendTypeBuiltin,
		},
		{
			Name: fsToolNameRead,
			Description: "Read a text file from the user's workspace through the editor bridge. " +
				"Paths must point inside the workspace folder the editor opened.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative or absolute path of the file to read"},
    "line": {"type": "integer", "description": "Optional 1-based line number to start reading from"},
    "limit": {"type": "integer", "description": "Optional maximum number of lines to read"}
  },
  "required": ["path"]
}`),
			BackendType: backendTypeBuiltin,
		},
	}
	if c.allowsWrite() {
		defs = append(defs, ToolDefinition{
			Name: fsToolNameWrite,
			Description: "Create or overwrite a text file in the user's workspace through the editor bridge. " +
				"Paths must point inside the workspace folder the editor opened; " +
				"depending on policy the user may be asked to approve each write.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative or absolute path of the file to write"},
    "content": {"type": "string", "description": "Full file content to write"}
  },
  "required": ["path", "content"]
}`),
			BackendType: backendTypeBuiltin,
		})
	}
	return defs
}

// executeFSRead handles the fs_read_text_file built-in tool.
func (r *Router) executeFSRead(ctx context.Context, args string) string {
	if !r.cfg.Filesystem.allowsRead() {
		return `{"error": "filesystem access is not enabled for this agent (Agent.spec.filesystem)"}` //nolint:lll
	}
	var p struct {
		Path  string `json:"path"`
		Line  *int   `json:"line"`
		Limit *int   `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf(`{"error": "invalid arguments: %v"}`, err)
	}
	if strings.TrimSpace(p.Path) == "" {
		return `{"error": "path is required"}`
	}
	params := map[string]any{"path": p.Path}
	if p.Line != nil {
		if *p.Line < 1 {
			return `{"error": "line must be >= 1"}`
		}
		params["line"] = *p.Line
	}
	if p.Limit != nil {
		if *p.Limit < 1 {
			return `{"error": "limit must be >= 1"}`
		}
		params["limit"] = *p.Limit
	}
	raw, _ := json.Marshal(params)
	return r.relayFSCall(ctx, acpfs.MethodReadTextFile, raw)
}

// executeFSList handles the fs_list_dir built-in tool. Directory listing is an
// agent-orca relay extension (ACP v1 has no list method): the bridge answers it
// locally against the workspace roots.
func (r *Router) executeFSList(ctx context.Context, args string) string {
	if !r.cfg.Filesystem.allowsRead() {
		return `{"error": "filesystem access is not enabled for this agent (Agent.spec.filesystem)"}` //nolint:lll
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf(`{"error": "invalid arguments: %v"}`, err)
	}
	if strings.TrimSpace(p.Path) == "" {
		p.Path = "." // default to the workspace root
	}
	raw, _ := json.Marshal(map[string]any{"path": p.Path})
	return r.relayFSCall(ctx, acpfs.MethodListDir, raw)
}

// executeFSWrite handles the fs_write_text_file built-in tool.
func (r *Router) executeFSWrite(ctx context.Context, args string) string {
	if !r.cfg.Filesystem.allowsWrite() {
		return `{"error": "filesystem writes are not enabled for this agent (Agent.spec.filesystem.mode)"}` //nolint:lll
	}
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf(`{"error": "invalid arguments: %v"}`, err)
	}
	if strings.TrimSpace(p.Path) == "" {
		return `{"error": "path is required"}`
	}
	raw, _ := json.Marshal(map[string]any{"path": p.Path, "content": p.Content})
	return r.relayFSCall(ctx, acpfs.MethodWriteTextFile, raw)
}

// relayFSCall enqueues one ACP fs/* call on the operator's internal API and
// polls for the bridge's answer, mirroring how other pod→operator callbacks
// (clarify, done, fail) authenticate with the run's SA token.
func (r *Router) relayFSCall(ctx context.Context, method string, params json.RawMessage) string {
	tokenBytes, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return fmt.Sprintf(`{"error": "reading SA token: %v"}`, err)
	}
	saToken := strings.TrimSpace(string(tokenBytes))

	body, _ := json.Marshal(acpfs.EnqueueRequest{Method: method, Params: params})
	url := fmt.Sprintf("%s/agentrun/%s/%s/fs", strings.TrimRight(r.cfg.OperatorAPIURL, "/"),
		r.cfg.RunNamespace, r.cfg.RunName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf(`{"error": "building request: %v"}`, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+saToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf(`{"error": "filesystem bridge unreachable: %v"}`, err)
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return `{"error": "reading filesystem bridge response"}`
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Sprintf(`{"error": "enqueueing filesystem request failed (HTTP %d): %s"}`,
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var created acpfs.Request
	if err := json.Unmarshal(respBody, &created); err != nil || created.ID == "" {
		return fmt.Sprintf(`{"error": "unexpected enqueue response: %s"}`, strings.TrimSpace(string(respBody)))
	}

	return r.awaitFSResponse(ctx, method == acpfs.MethodWriteTextFile, saToken, created.ID)
}

// awaitFSResponse long-polls the queued request until the bridge answers, the
// fs deadline expires, or the request expires (bridge disconnected/policy
// rejected but never answered). Succeeded writes normally carry an ACP null
// (or empty-object) result — Zed's WriteTextFileResponse is `{}` — so those
// are surfaced to the LLM as a positive confirmation instead of a bare
// "null"/"{}" tool result, which the trivial-result annotation would
// misreport as an error.
func (r *Router) awaitFSResponse(ctx context.Context, isWrite bool, saToken, requestID string) string {
	fsCtx, cancel := context.WithTimeout(ctx, r.cfg.Filesystem.timeout())
	defer cancel()

	url := fmt.Sprintf("%s/agentrun/%s/%s/fs/%s", strings.TrimRight(r.cfg.OperatorAPIURL, "/"),
		r.cfg.RunNamespace, r.cfg.RunName, requestID)

	for {
		req, err := http.NewRequestWithContext(fsCtx, http.MethodGet, url+"?wait=20", nil)
		if err != nil {
			return fmt.Sprintf(`{"error": "building request: %v"}`, err)
		}
		req.Header.Set("Authorization", "Bearer "+saToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if fsCtx.Err() != nil {
				break // deadline/cancel — fall through to the timeout error
			}
			return fmt.Sprintf(`{"error": "filesystem bridge unreachable: %v"}`, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return `{"error": "reading filesystem response"}`
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var out acpfs.Response
			if err := json.Unmarshal(body, &out); err != nil {
				return fmt.Sprintf(`{"error": "decoding filesystem response: %v"}`, err)
			}
			if out.Error != nil {
				slog.Warn("fs call rejected by the editor bridge",
					"run", r.cfg.RunName, "code", out.Error.Code, "message", out.Error.Message)
				return fmt.Sprintf(`{"error": "filesystem request rejected by the editor: %s (code %d)"}`,
					out.Error.Message, out.Error.Code)
			}
			result := strings.TrimSpace(string(out.Result))
			if isWrite && (result == "" || result == "null" || result == "{}" || result == "[]") {
				return `{"written": true}`
			}
			return result
		case http.StatusNoContent:
			// not yet answered — keep polling
		case http.StatusGone:
			return `{"error": "filesystem request expired before the editor bridge answered; ` +
				`is 'aoctl acp serve' connected to this cluster?"}`
		default:
			return fmt.Sprintf(`{"error": "filesystem relay returned HTTP %d: %s"}`,
				resp.StatusCode, strings.TrimSpace(string(body)))
		}

		select {
		case <-fsCtx.Done():
			return fmt.Sprintf(`{"error": "filesystem call timed out after %ds waiting for the editor bridge"}`,
				int(r.cfg.Filesystem.timeout().Seconds()))
		case <-time.After(fsPollInterval):
		}
	}
	return fmt.Sprintf(`{"error": "filesystem call timed out after %ds waiting for the editor bridge"}`,
		int(r.cfg.Filesystem.timeout().Seconds()))
}
