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

// Command coder-agent is a coding agent for OpenShell sandboxes. It gives the
// model a shell and file tools over the sandbox workspace (pkg/agent/devtools)
// and works on AGENTORC_INPUT until the model calls _done or _fail.
//
// It needs an Agent with spec.runtime.localTools and spec.runtime.sandbox; the
// Agent's systemPrompt, tools and governance come from the model-router as for
// any agent. Environment:
//
//	AGENTORC_INPUT     the task (set by the operator; AGENTORC_INPUT_B64 when multi-line)
//	CODER_WORKSPACE    directory the tools work in (default /sandbox)
//	CODER_MAX_TURNS    maximum requests to the router (default 300)
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/heddles/agent-orca/pkg/agent"
	"github.com/heddles/agent-orca/pkg/agent/devtools"
)

// operatingPrompt tells the model how this agent works. The Agent's own
// systemPrompt (injected by the router) says what to work on and how.
const operatingPrompt = `You are a software engineering agent working autonomously in a sandboxed Linux workspace at %s.

Tools:
- shell runs a command (bash -c) and returns its exit code and output. Use it to clone repositories,
  build, test, lint, run git and install what you need. Package registries and hosts outside the
  sandbox policy are blocked; if a download fails with a 403 or connection error, the host is not allowed.
- read_file, write_file, edit_file, list_dir and grep work on files. Prefer edit_file for changes
  to existing files.

How to work:
- Explore before changing: read the relevant code and the project's build and test setup.
- Make the smallest change that does the task, in the style of the surrounding code.
- Verify your change by building it and running the relevant tests. Fix failures you caused.
- Long output is truncated to its head and tail; narrow commands (e.g. a single package's tests)
  when you need detail.
- Nobody is watching: do not ask questions. When the task is done and verified, call _done with a
  summary of what you changed and how you verified it. If it cannot be done, call _fail with the reason.`

const continuePrompt = "Continue working on the task. When it is complete and verified, call _done with a summary; " +
	"if it cannot be completed, call _fail with the reason."

func main() {
	input := agent.Input()
	if strings.TrimSpace(input) == "" {
		fmt.Fprintln(os.Stderr, "coder-agent: AGENTORC_INPUT is empty")
		os.Exit(2)
	}
	workspace := envOr("CODER_WORKSPACE", "/sandbox")
	maxTurns, err := strconv.Atoi(envOr("CODER_MAX_TURNS", "300"))
	if err != nil || maxTurns < 1 {
		fmt.Fprintf(os.Stderr, "coder-agent: invalid CODER_MAX_TURNS %q\n", os.Getenv("CODER_MAX_TURNS"))
		os.Exit(2)
	}

	tools, err := devtools.New(workspace, devtools.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "coder-agent: %v\n", err)
		os.Exit(2)
	}
	a := agent.New(
		agent.WithSystemPrompt(fmt.Sprintf(operatingPrompt, workspace)),
		agent.WithToolObserver(logToolCall),
	)
	tools.Register(a)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	res, err := a.Run(ctx, input, agent.WithContinuePrompt(continuePrompt), agent.WithMaxTurns(maxTurns))
	if err != nil {
		fmt.Fprintf(os.Stderr, "coder-agent: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("== %s\n", res.Phase)
	if res.Output != "" {
		fmt.Println(res.Output)
	}
	if res.Phase == agent.PhaseFailed {
		if res.FailureReason != "" {
			fmt.Fprintln(os.Stderr, res.FailureReason)
		}
		os.Exit(1)
	}
}

// logToolCall prints one line per tool call plus the start of its result, so
// the run's logs show what the agent did.
func logToolCall(ev agent.ToolEvent) {
	fmt.Printf("-> %s %s (%s)\n%s\n", ev.Name, oneLine(ev.Arguments, 300),
		ev.Duration.Round(time.Millisecond), indent(firstLines(ev.Result, 6)))
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(strings.TrimRight(s, "\n"), "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

func indent(s string) string {
	return "   " + strings.ReplaceAll(s, "\n", "\n   ")
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
