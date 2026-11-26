package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file is the workstation half of the local_exec tool: it takes the
// terminal/run requests an agent pod relays, asks the user to approve each
// command, and has the editor run it with ACP's terminal/* methods, so the
// user watches it in their editor.
//
// A command cannot be confined the way file access is: it runs with the user's
// own permissions. The approval is the safeguard, so:
//   - every command is asked about, and nothing the cluster sends can waive it;
//   - the only thing remembered is the user's own "always allow", for exactly
//     this command in exactly this folder, until the session ends;
//   - anything but an explicit allow refuses the command;
//   - the editor is given the very string the user was shown, and a command
//     holding characters that could make the two look different is refused;
//   - a request is acted on once, whatever the relay lists again.

// ACP v1 methods the bridge calls on the editor for local_exec.
const (
	acpMethodRequestPermission = "session/request_permission"
	acpMethodTerminalCreate    = "terminal/create"
	acpMethodTerminalWait      = "terminal/wait_for_exit"
	acpMethodTerminalOutput    = "terminal/output"
	acpMethodTerminalKill      = "terminal/kill"
	acpMethodTerminalRelease   = "terminal/release"

	// acpMethodTerminalRun is the agent-orca relay method for one command. The
	// editor never sees it.
	acpMethodTerminalRun = "terminal/run"

	localExecToolName = "local_exec"
)

// Permission option ids the bridge offers.
const (
	permissionAllowOnce   = "allow_once"
	permissionAllowAlways = "allow_always"
	permissionReject      = "reject_once"
)

const (
	terminalDefaultTimeout = 120 * time.Second
	// terminalTimeoutLimit mirrors the router's: past it the pod stops waiting.
	terminalTimeoutLimit = 20 * time.Minute
	// terminalOutputLimit is how much of the end of the output is kept.
	terminalOutputLimit = 30_000
	// terminalCommandLimit keeps a command short enough to be read before it is
	// approved.
	terminalCommandLimit = 4_000
	// terminalCleanupTimeout bounds the calls made after a command is over, which
	// must go through even when the turn was cancelled.
	terminalCleanupTimeout = 10 * time.Second
)

// terminalApprovalWindow is how long the user has to answer. It mirrors
// acpfs.TerminalApprovalWindow, which the pod's wait is built on. A var so
// tests can shorten it.
var terminalApprovalWindow = 5 * time.Minute

// ACPTerminalPolicy mirrors the terminal policy in the ACP agent manifest.
type ACPTerminalPolicy struct {
	MaxTimeoutSeconds int `json:"maxTimeoutSeconds"`
}

// terminalRunParams mirrors acpfs.TerminalParams.
type terminalRunParams struct {
	Command        string `json:"command"`
	WorkDir        string `json:"workDir,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// terminalRunResult mirrors acpfs.TerminalResult.
type terminalRunResult struct {
	ExitCode  *int   `json:"exitCode,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	WorkDir   string `json:"workDir,omitempty"`
	OS        string `json:"os,omitempty"`
}

type acpTerminalCreateParams struct {
	SessionID string `json:"sessionId"`
	// Command is the whole shell script. Args are deliberately not sent: Zed
	// hands a command without args to its shell unchanged, and quotes it as a
	// single program name when there are args.
	Command         string `json:"command"`
	Cwd             string `json:"cwd"`
	OutputByteLimit int    `json:"outputByteLimit"`
}

type acpTerminalRef struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type acpTerminalExit struct {
	ExitCode *int    `json:"exitCode"`
	Signal   *string `json:"signal"`
}

type acpTerminalOutput struct {
	Output     string           `json:"output"`
	Truncated  bool             `json:"truncated"`
	ExitStatus *acpTerminalExit `json:"exitStatus"`
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type acpPermissionParams struct {
	SessionID string                `json:"sessionId"`
	ToolCall  acpToolCallUpdate     `json:"toolCall"`
	Options   []acpPermissionOption `json:"options"`
}

type acpPermissionResult struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"`
	} `json:"outcome"`
}

// terminalRefusal is why a command was not run, or not finished.
type terminalRefusal struct {
	code    int
	message string
}

func (e *terminalRefusal) Error() string { return e.message }

func refuse(code int, format string, args ...any) error {
	return &terminalRefusal{code: code, message: fmt.Sprintf(format, args...)}
}

// terminalPolicy returns the agent's terminal policy if this bridge can serve
// it: the agent has local_exec, the editor offers a terminal and the user has
// not turned it off. nil otherwise.
func (b *acpBridge) terminalPolicy() *ACPTerminalPolicy {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.terminalDisabled || !b.editorTerminal {
		return nil
	}
	return b.manifest.Terminal
}

// runCapabilities is what the bridge offers to do for a run it starts.
func (b *acpBridge) runCapabilities() *ACPRunClientCapabilities {
	return &ACPRunClientCapabilities{Terminal: b.terminalPolicy() != nil}
}

// claimTerminalRequest reports whether this is the first time the session sees
// the request. A request stays listed as pending until it is answered, and a
// listing fetched before the answer can be read after it, so claims are kept
// for the life of the session: a command must never run twice.
func (b *acpBridge) claimTerminalRequest(sessionID, runID, requestID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	if !ok {
		return false
	}
	key := runID + "/" + requestID
	if s.terminalSeen[key] {
		return false
	}
	if s.terminalSeen == nil {
		s.terminalSeen = map[string]bool{}
	}
	s.terminalSeen[key] = true
	return true
}

func (b *acpBridge) commandAlwaysAllowed(sessionID, key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	return ok && s.allowedCommands[key]
}

func (b *acpBridge) alwaysAllowCommand(sessionID, key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		if s.allowedCommands == nil {
			s.allowedCommands = map[string]bool{}
		}
		s.allowedCommands[key] = true
	}
}

// serveTerminalRequest handles one relayed command from start to answer. ctx
// is the turn's: cancelling it stops the command.
func (b *acpBridge) serveTerminalRequest(ctx context.Context, sessionID, runID string, fr acpfsRelayRequest) {
	result, err := b.runTerminalRequest(ctx, sessionID, fr)

	// The answer must reach the pod even when the turn was just cancelled.
	replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalCleanupTimeout)
	defer cancel()
	if err != nil {
		code := -32000
		var r *terminalRefusal
		if errors.As(err, &r) {
			code = r.code
		}
		b.respondFSErr(replyCtx, runID, fr.ID, code, err.Error())
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		b.respondFSErr(replyCtx, runID, fr.ID, -32000, "encoding the command result")
		return
	}
	b.respondFSResult(replyCtx, runID, fr.ID, raw)
}

func (b *acpBridge) runTerminalRequest(ctx context.Context, sessionID string, fr acpfsRelayRequest) (*terminalRunResult, error) { //nolint:lll
	policy := b.terminalPolicy()
	if policy == nil {
		return nil, refuse(-32002, "the user's computer is not accepting commands from this agent")
	}
	var p terminalRunParams
	if err := json.Unmarshal(fr.Params, &p); err != nil {
		return nil, refuse(-32602, "invalid local_exec params")
	}
	if err := checkCommand(p.Command); err != nil {
		return nil, err
	}
	dir, err := b.terminalWorkDir(sessionID, p.WorkDir)
	if err != nil {
		return nil, err
	}
	timeout := terminalTimeout(p.TimeoutSeconds, policy.MaxTimeoutSeconds)

	toolCallID := localExecToolName + "_" + fr.ID
	call := acpToolCallUpdate{
		SessionUpdate: acpUpdateToolCall,
		ToolCallID:    toolCallID,
		Title:         commandTitle(p.Command),
		Kind:          acpToolKindExecute,
		Status:        acpToolStatusPending,
		Content:       []acpToolCallContent{textContent(b.commandPrompt(p.Command, dir, timeout))},
	}
	b.notifyToolCall(sessionID, call)

	if err := b.approveCommand(ctx, sessionID, dir, p.Command, call); err != nil {
		b.finishToolCall(sessionID, toolCallID, acpToolStatusFailed, textContent(err.Error()))
		return nil, err
	}
	return b.runInEditor(ctx, sessionID, toolCallID, dir, p.Command, timeout)
}

// approveCommand returns nil only when the user allowed this command.
func (b *acpBridge) approveCommand(ctx context.Context, sessionID, dir, command string, call acpToolCallUpdate) error { //nolint:lll
	if ctx.Err() != nil {
		return refuse(-32000, "the turn was cancelled; the command did not run")
	}
	key := dir + "\x00" + command
	if b.commandAlwaysAllowed(sessionID, key) {
		return nil
	}
	askCtx, cancel := context.WithTimeout(ctx, terminalApprovalWindow)
	defer cancel()
	call.SessionUpdate = ""
	resp, err := b.srv.sendRequest(askCtx, acpMethodRequestPermission, acpPermissionParams{
		SessionID: sessionID,
		ToolCall:  call,
		Options: []acpPermissionOption{
			{OptionID: permissionAllowOnce, Name: "Allow", Kind: permissionAllowOnce},
			{OptionID: permissionAllowAlways, Name: "Always allow this exact command here (this session)",
				Kind: permissionAllowAlways},
			{OptionID: permissionReject, Name: "Reject", Kind: permissionReject},
		},
	})
	switch {
	case ctx.Err() != nil:
		return refuse(-32000, "the turn was cancelled; the command did not run")
	case askCtx.Err() != nil:
		return refuse(-32003, "the user did not answer the approval prompt in time; the command did not run")
	case err != nil:
		return refuse(-32000, "the approval prompt failed, so the command did not run: %v", err)
	}
	var out acpPermissionResult
	raw, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(raw, &out); err != nil {
		return refuse(-32000, "the editor's answer to the approval prompt was unreadable; the command did not run")
	}
	if out.Outcome.Outcome == "selected" {
		switch out.Outcome.OptionID {
		case permissionAllowOnce:
			return nil
		case permissionAllowAlways:
			b.alwaysAllowCommand(sessionID, key)
			return nil
		}
	}
	return refuse(-32003, "the user declined to run this command")
}

// runInEditor runs an approved command in an editor terminal and collects what
// it did. The terminal is always released, and killed if it is still running.
func (b *acpBridge) runInEditor(ctx context.Context, sessionID, toolCallID, dir, command string, timeout time.Duration) (*terminalRunResult, error) { //nolint:lll
	// Not tied to ctx: a create the editor already acted on must yield its id,
	// or nothing could stop the command.
	createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), terminalCleanupTimeout)
	defer cancelCreate()
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	err := b.editorCall(createCtx, acpMethodTerminalCreate, acpTerminalCreateParams{
		SessionID: sessionID, Command: command, Cwd: dir, OutputByteLimit: terminalOutputLimit,
	}, &created)
	if err != nil || created.TerminalID == "" {
		if err == nil {
			err = errors.New("no terminal id in the editor's answer")
		}
		failure := refuse(-32000, "the editor could not start the command: %v", err)
		b.finishToolCall(sessionID, toolCallID, acpToolStatusFailed, textContent(failure.Error()))
		return nil, failure
	}
	ref := acpTerminalRef{SessionID: sessionID, TerminalID: created.TerminalID}
	terminal := acpToolCallContent{Type: "terminal", TerminalID: created.TerminalID}
	b.finishToolCall(sessionID, toolCallID, acpToolStatusInProgress, terminal)

	cleanup := func(method string, out any) error {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalCleanupTimeout)
		defer cancel()
		return b.editorCall(cctx, method, ref, out)
	}
	defer func() {
		if err := cleanup(acpMethodTerminalRelease, nil); err != nil {
			b.srv.logf("acp: releasing terminal %s: %v\n", created.TerminalID, err)
		}
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()
	var exit acpTerminalExit
	waitErr := b.editorCall(waitCtx, acpMethodTerminalWait, ref, &exit)
	timedOut := waitErr != nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	if waitErr != nil {
		if err := cleanup(acpMethodTerminalKill, nil); err != nil {
			b.srv.logf("acp: stopping terminal %s: %v\n", created.TerminalID, err)
		}
		if !timedOut {
			failure := refuse(-32000, "waiting for the command failed, and it was stopped: %v", waitErr)
			if ctx.Err() != nil {
				failure = refuse(-32000, "the turn was cancelled while the command was running; it was stopped")
			}
			b.finishToolCall(sessionID, toolCallID, acpToolStatusFailed, terminal)
			return nil, failure
		}
	}

	var out acpTerminalOutput
	if err := cleanup(acpMethodTerminalOutput, &out); err != nil {
		b.finishToolCall(sessionID, toolCallID, acpToolStatusFailed, terminal)
		return nil, refuse(-32000, "the command ran, but its output could not be read: %v", err)
	}
	res := &terminalRunResult{TimedOut: timedOut, WorkDir: dir, OS: runtime.GOOS}
	res.Output, res.Truncated = outputTail(out.Output, terminalOutputLimit)
	res.Truncated = res.Truncated || out.Truncated
	if !timedOut {
		res.ExitCode = exit.ExitCode
		if exit.Signal != nil {
			res.Signal = *exit.Signal
		}
	}
	status := acpToolStatusFailed
	if res.ExitCode != nil && *res.ExitCode == 0 {
		status = acpToolStatusCompleted
	}
	b.finishToolCall(sessionID, toolCallID, status, terminal)
	return res, nil
}

// editorCall sends one request to the editor and decodes its result into out,
// which may be nil.
func (b *acpBridge) editorCall(ctx context.Context, method string, params, out any) error {
	resp, err := b.srv.sendRequest(ctx, method, params)
	if err != nil || out == nil {
		return err
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (b *acpBridge) notifyToolCall(sessionID string, call acpToolCallUpdate) {
	b.srv.notify(acpMethodSessionUpdate, acpUpdateParams{SessionID: sessionID, Update: call})
}

func (b *acpBridge) finishToolCall(sessionID, toolCallID, status string, content acpToolCallContent) {
	b.notifyToolCall(sessionID, acpToolCallUpdate{
		SessionUpdate: acpUpdateToolCallUpd, ToolCallID: toolCallID, Status: status,
		Content: []acpToolCallContent{content},
	})
}

func textContent(text string) acpToolCallContent {
	return acpToolCallContent{Type: "content", Content: &acpContentBlock{Type: "text", Text: text}}
}

// terminalWorkDir resolves where a command starts: a folder inside the
// workspace. This says where it starts, not what it can reach.
func (b *acpBridge) terminalWorkDir(sessionID, workDir string) (string, error) {
	roots := b.workspaceRoots(sessionID)
	if len(roots) == 0 {
		return "", refuse(-32004, "no workspace folder is known for this session, so the command did not run")
	}
	if strings.TrimSpace(workDir) == "" {
		workDir = "."
	}
	dir, ok := fsConfine(roots, workDir)
	if !ok {
		return "", refuse(-32002, "work_dir is outside the workspace: %q", workDir)
	}
	if _, err := fsWithinRealRoots(roots, dir); err != nil {
		return "", refuse(-32002, "work_dir cannot be used: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", refuse(-32002, "work_dir is not a folder: %q", workDir)
	}
	return dir, nil
}

// checkCommand refuses commands the user could not judge from the prompt.
func checkCommand(command string) error {
	switch {
	case strings.TrimSpace(command) == "":
		return refuse(-32602, "command is required")
	case len(command) > terminalCommandLimit:
		return refuse(-32602, "the command is %d bytes; the limit is %d so that the user can read it before "+
			"approving. Write long scripts to a file first", len(command), terminalCommandLimit)
	case !utf8.ValidString(command):
		return refuse(-32602, "the command is not valid UTF-8")
	}
	for _, r := range command {
		if r == '\n' || r == '\t' {
			continue
		}
		// Control characters can rewrite a terminal line, and format characters
		// (bidirectional overrides, zero-width) make text read unlike it runs.
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return refuse(-32602, "the command contains the hidden or control character %U, which could make it "+
				"read differently from what would run", r)
		}
	}
	return nil
}

func terminalTimeout(asked, agentMax int) time.Duration {
	limit := terminalTimeoutLimit
	if agentMax > 0 {
		limit = min(limit, time.Duration(agentMax)*time.Second)
	}
	if asked <= 0 {
		return min(terminalDefaultTimeout, limit)
	}
	return min(time.Duration(min(asked, int(terminalTimeoutLimit/time.Second)))*time.Second, limit)
}

// commandTitle is the one-line heading of the tool call.
func commandTitle(command string) string {
	line, rest, _ := strings.Cut(strings.TrimSpace(command), "\n")
	const maxTitle = 120
	if r := []rune(line); len(r) > maxTitle {
		return string(r[:maxTitle]) + "…"
	}
	if rest != "" {
		return line + " …"
	}
	return line
}

// commandPrompt shows the whole command and where it will run. The code fence
// is longer than any run of backticks inside, so the command cannot end it and
// add text of its own to the prompt.
func (b *acpBridge) commandPrompt(command, dir string, timeout time.Duration) string {
	longest, run := 0, 0
	for _, r := range command {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fmt.Sprintf("Agent %q wants to run this on your computer, with your permissions:\n\n%ssh\n%s\n%s\n\n"+
		"In: %s\nStopped after: %s", b.agentName, fence, command, fence, dir, timeout)
}

// outputTail keeps the last limit bytes, whatever the editor sent, without
// splitting a character.
func outputTail(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:], true
}
