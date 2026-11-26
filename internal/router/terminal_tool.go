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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/acpfs"
)

// The local_exec built-in tool runs a shell command on the user's own computer
// (Agent.spec.terminal). The router only relays: the `aoctl acp serve` bridge
// asks the user to approve the command and has the editor run it. It shares the
// workstation relay with the fs_* tools; see docs/local-terminal.md.
const (
	localExecToolName = "local_exec"

	defaultTerminalTimeoutSeconds    = 120
	defaultTerminalMaxTimeoutSeconds = 600
	// terminalMaxTimeoutLimit keeps an approval wait plus a command inside the
	// relay's TTL, whatever the config says.
	terminalMaxTimeoutLimit = 1200
)

// terminalResultSlack is added to the approval window and the command's timeout
// for the bridge to report back. Tests shorten the wait through ctx.
const terminalResultSlack = time.Minute

// TerminalConfig, when non-nil on Config, enables the local_exec tool.
// Populated by the operator from Agent.spec.terminal.
type TerminalConfig struct {
	// MaxTimeoutSeconds caps one command. 0 = defaultTerminalMaxTimeoutSeconds.
	MaxTimeoutSeconds int `json:"maxTimeoutSeconds,omitempty"`
}

func (c *TerminalConfig) maxTimeout() int {
	if c != nil && c.MaxTimeoutSeconds > 0 {
		return min(c.MaxTimeoutSeconds, terminalMaxTimeoutLimit)
	}
	return defaultTerminalMaxTimeoutSeconds
}

// ToolDefinitions returns the local_exec definition (none when c is nil).
func (c *TerminalConfig) ToolDefinitions() []ToolDefinition {
	if c == nil {
		return nil
	}
	return []ToolDefinition{{
		Name: localExecToolName,
		Description: "Run a shell command on the user's own computer, in the terminal of the editor they are using. " +
			"Use it for work that depends on their machine: building and testing their checkout, git, and tools " +
			"installed there. It is not a sandbox: it is the user's real computer, files and accounts" +
			" (if you also have sandbox_exec, that one runs in an isolated Linux sandbox which cannot see their files; " +
			"prefer it for anything that does not need their machine). " +
			"The user sees every command and must approve it, so make each one clear and do one thing with it. " +
			"If they decline, do not retry it or work around the refusal; ask them how to proceed. " +
			"Commands start in the project folder, in a POSIX shell (write sh, not bash or zsh syntax) with no input: " +
			"nothing can answer a prompt, so use non-interactive flags. Each call is a new shell, so `cd` and exported " +
			"variables do not carry over. Do not start servers or watchers that keep running: a command that outlives " +
			"its timeout is stopped. Do not assume Linux; the result names the operating system. " +
			"It only works while the user is working with you from their editor.",
		Parameters: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The shell command or short script to run"},
    "work_dir": {"type": "string", "description": "Optional folder to run in, relative to the project folder; it must be inside the workspace"},
    "timeout_seconds": {"type": "integer", "description": "Optional limit in seconds (default %d, at most %d)"}
  },
  "required": ["command"]
}`, min(defaultTerminalTimeoutSeconds, c.maxTimeout()), c.maxTimeout())),
		BackendType: backendTypeBuiltin,
	}}
}

// executeLocalExec handles the local_exec built-in tool.
func (r *Router) executeLocalExec(ctx context.Context, args string) string {
	if r.cfg.Terminal == nil {
		return `{"error": "this agent was not given a local terminal (declared by Agent.spec.terminal)"}`
	}
	var p struct {
		Command        string `json:"command"`
		WorkDir        string `json:"work_dir"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return terminalError("invalid arguments: " + err.Error())
	}
	if strings.TrimSpace(p.Command) == "" {
		return terminalError("command is required")
	}
	timeout := p.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTerminalTimeoutSeconds
	}
	timeout = min(timeout, r.cfg.Terminal.maxTimeout())

	token, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return terminalError("reading SA token: " + err.Error())
	}
	c := &shellClient{
		base:  fmt.Sprintf("%s/agentrun/%s/%s/fs", strings.TrimRight(r.cfg.OperatorAPIURL, "/"), r.cfg.RunNamespace, r.cfg.RunName),
		token: strings.TrimSpace(string(token)),
	}
	params, err := json.Marshal(acpfs.TerminalParams{Command: p.Command, WorkDir: p.WorkDir, TimeoutSeconds: timeout})
	if err != nil {
		return terminalError("encoding the command: " + err.Error())
	}

	status, body, err := c.do(ctx, http.MethodPost, c.base, acpfs.EnqueueRequest{Method: acpfs.MethodRunTerminal, Params: params})
	if err != nil {
		return terminalError("the operator is unreachable: " + err.Error())
	}
	if status == http.StatusConflict {
		// No editor with a terminal is behind this run; the operator says why.
		return terminalError(strings.TrimSpace(string(body)))
	}
	if status != http.StatusCreated {
		return terminalError(fmt.Sprintf("sending the command failed (HTTP %d): %s", status, strings.TrimSpace(string(body))))
	}
	var created acpfs.Request
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return terminalError("unexpected response when sending the command")
	}

	wait := acpfs.TerminalApprovalWindow + time.Duration(timeout)*time.Second + terminalResultSlack
	return awaitTerminal(ctx, c, created.ID, wait)
}

// awaitTerminal long-polls until the bridge answers or wait passes.
func awaitTerminal(ctx context.Context, c *shellClient, id string, wait time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	const noAnswer = "the editor did not answer in time; the command may not have run. " +
		"Ask the user to check that their editor session is still open"
	for {
		status, body, err := c.do(ctx, http.MethodGet, c.base+"/"+id+"?wait=20", nil)
		if err != nil {
			if ctx.Err() != nil {
				return terminalError(noAnswer)
			}
			return terminalError("the operator is unreachable: " + err.Error())
		}
		switch status {
		case http.StatusOK:
			return formatTerminalResponse(body)
		case http.StatusNoContent:
		case http.StatusGone:
			return terminalError(noAnswer)
		default:
			return terminalError(fmt.Sprintf("the relay returned HTTP %d: %s", status, strings.TrimSpace(string(body))))
		}
		select {
		case <-ctx.Done():
			return terminalError(noAnswer)
		case <-time.After(fsPollInterval):
		}
	}
}

// formatTerminalResponse turns the bridge's answer into the tool result.
func formatTerminalResponse(body []byte) string {
	var resp acpfs.Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return terminalError("decoding the editor's answer: " + err.Error())
	}
	if resp.Error != nil {
		return terminalError(resp.Error.Message)
	}
	var res acpfs.TerminalResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return terminalError("decoding the command result: " + err.Error())
	}
	out := map[string]any{"output": res.Output}
	if res.ExitCode != nil {
		out["exit_code"] = *res.ExitCode
	}
	if res.Signal != "" {
		out["signal"] = res.Signal
	}
	if res.Truncated {
		out["truncated"] = "only the end of the output is shown"
	}
	if res.TimedOut {
		out["timed_out"] = true
		out["error"] = "the command did not finish within its timeout and was stopped"
	}
	if res.WorkDir != "" {
		out["work_dir"] = res.WorkDir
	}
	if res.OS != "" {
		out["os"] = res.OS
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func terminalError(msg string) string {
	raw, _ := json.Marshal(map[string]string{"error": msg})
	return string(raw)
}
