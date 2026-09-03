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
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// This file wires the `aoctl acp` command tree:
//
//   - `aoctl acp serve --agent <name>`  — runs the ACP JSON-RPC-over-stdio
//     bridge that Zed (and other ACP editors) launch as a subprocess. It reads
//     newline-delimited JSON-RPC 2.0 requests from stdin and translates them
//     into calls against agent-orca's HTTP ACP API. Stdout carries only JSON-RPC
//     messages; all logging goes to stderr.
//
//   - `aoctl acp setup --editor zed`    — writes a Zed agent_servers entry so the
//     editor launches `aoctl acp serve` as an External Agent subprocess. This is
//     the local-stdio analogue of a remotely-hosted ACP agent, which Zed cannot
//     connect to directly today.

// serveHTTPTimeout is the HTTP client timeout used by the serve process. SSE
// streams and long-running polls can exceed the default 30 s, so serve uses a
// longer window; if a stream still times out the bridge falls back to polling.
const serveHTTPTimeout = 10 * time.Minute

// newACPCommand builds the `aoctl acp` command tree and its subcommands.
func newACPCommand(s *settings) *cobra.Command {
	acp := &cobra.Command{
		Use:   "acp",
		Short: "ACP bridge: expose agent-orca agents to editors (Zed, etc.) over stdio JSON-RPC",
		Long: `Expose agent-orca agents to ACP-compatible editors via a local stdio bridge.

Zed and other ACP editors launch External Agents as stdio subprocesses: the editor
sends JSON-RPC 2.0 requests on stdin and reads responses/notifications on stdout.
agent-orca itself is an HTTP service behind bearer-token auth, which editors cannot
reach directly. The 'acp' subcommands bridge this gap:

  aoctl acp serve   start the stdio bridge (run by Zed as a subprocess)
  aoctl acp setup   write editor config (e.g. Zed settings.json) to launch the bridge`,
	}

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the ACP JSON-RPC-over-stdio bridge to agent-orca's HTTP ACP API",
		Long: `Run a long-lived ACP JSON-RPC 2.0 server on stdio.

Zed launches this process as an External Agent subprocess: it sends JSON-RPC
requests on stdin and reads responses + server-to-client notifications on stdout.
All logging is written to stderr so it never corrupts the JSON-RPC stream.

The agent name is pinned at launch because ACP v1 is single-agent-per-server.

This is the command to put in your editor's agent_servers config. Use
'aoctl acp setup --editor zed' to write that config for you.
`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return s.runACPServe()
		},
	}
	serve.Flags().StringVar(&s.agent, "agent", "", "agent-orca agent to serve (required)")
	_ = serve.MarkFlagRequired("agent")

	setup := &cobra.Command{
		Use:   "setup",
		Short: "Configure an editor to use an agent-orca ACP external agent",
		Long: `Write an editor configuration for the ACP stdio bridge.

Currently supports Zed, which is configured via agent_servers in settings.json.
After running this command, restart Zed (or just re-open the agent picker — Zed
auto-detects settings.json changes).
`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return s.runACPSetup()
		},
	}
	setup.Flags().StringVar(&s.editor, "editor", "", "editor to configure (required; e.g. zed)")
	_ = setup.MarkFlagRequired("editor")
	setup.Flags().StringVar(&s.agent, "agent", "", "agent-orca agent name to expose (if omitted, picks interactively)")

	acp.AddCommand(serve, setup)
	return acp
}

// runACPServe starts the stdio ACP bridge. It is a long-running process: it
// blocks until stdin closes (editor terminates the subprocess) or a signal is
// received.
func (s *settings) runACPServe() error {
	if s.agent == "" {
		return errors.New("--agent is required for `aoctl acp serve`")
	}
	if s.token == "" {
		return errors.New("no bearer token — run `aoctl login` first, or pass --token")
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Use a generous HTTP timeout so SSE token streams and long-running polls
	// don't time out prematurely. The bridge falls back to polling if SSE fails.
	client := newClient(s.endpoint, s.acp, s.token, serveHTTPTimeout, s.insecure)

	// A long-lived ACP session (the bridge process Zed keeps running) outlives
	// its short-lived OIDC id_token. For OIDC sessions that captured a refresh
	// token, wire in automatic refresh so the session rides out token expiry
	// without the user re-logging in and re-creating the agent:
	//   - a background refresher renews the token ahead of expiry, and
	//   - the HTTP transport transparently refreshes + retries any request that
	//     is rejected with a 401.
	// Explicit --token passes are left untouched (the caller owns that token).
	if !s.tokenFromFlag {
		if refresh, ok := s.oidcRefreshCallback(); ok {
			client.refresh = refresh
			base := client.HTTP.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			client.HTTP.Transport = &refreshableTransport{base: base, owner: client}
			go s.runTokenRefresher(ctx, client)
		}
	}

	// The server and bridge hold mutual references (bridge responds via the
	// server; server dispatches via the bridge). Create the server first with a
	// nil handler, wire the bridge, then assign it as the handler.
	srv := newACPStdioServer(nil, os.Stdin, os.Stdout, os.Stderr)
	bridge := newACPBridge(srv, client, s.agent)
	srv.handler = bridge

	_, _ = fmt.Fprintf(s.errw, "aoctl acp serve — agent %q | ACP endpoint %s | JSON-RPC on stdin/stdout, logs on stderr\n",
		s.agent, s.acp)
	return srv.Serve(ctx)
}

// runACPSetup dispatches to the per-editor config writer.
func (s *settings) runACPSetup() error {
	switch s.editor {
	case "zed":
		return s.setupZed(s.agent)
	default:
		return fmt.Errorf("unsupported editor %q (supported: zed)", s.editor)
	}
}

// setupZed writes (or merges) a Zed agent_servers entry that launches
// `aoctl acp serve` as an External Agent subprocess for the given agent.
//
// If agentName is empty and stdin is a terminal, the available agents are listed
// and the user picks one interactively. The Zed config path honours ZED_CONFIG_DIR
// (for testing) then XDG_CONFIG_HOME / ~/Library/Application Support / %APPDATA%.
func (s *settings) setupZed(agentName string) error {
	c := s.client()

	// If no agent was supplied, try to list + prompt interactively.
	if agentName == "" {
		if s.isTerminal == nil || !s.isTerminal() {
			return errors.New("--agent is required when not running in a terminal")
		}
		agents, err := c.ListAgents(context.Background())
		if err != nil {
			return fmt.Errorf("listing agents via ACP API (%s): %w — "+
				"your --acp-endpoint should be the host root only (e.g. "+
				"http://agent-orca.local); the CLI appends /agents automatically. "+
				"If omitted, it is derived from --endpoint",
				s.acp, err)
		}
		if len(agents) == 0 {
			return errors.New("no agents found for your tenant")
		}
		labels := make([]promptOption, len(agents))
		for i, a := range agents {
			labels[i] = promptOption{
				Value: a.Name,
				Label: a.Name + " — " + a.Description,
			}
		}
		agentName, err = promptSelection(s.out, s.stdin,
			"Select the agent to expose to Zed:", labels)
		if err != nil {
			return err
		}
	}

	if agentName == "" {
		return errors.New("an agent name is required")
	}

	if s.token == "" {
		_, _ = fmt.Fprintln(s.errw, "Warning: no bearer token is cached. Run `aoctl login` (or pass --token) before using the agent in Zed.")
	}

	path, err := zedSettingsPath()
	if err != nil {
		return err
	}

	// Read existing settings (tolerate a missing file; error on malformed JSON).
	var settings map[string]any
	if data, rerr := os.ReadFile(path); rerr == nil {
		if jerr := json.Unmarshal(data, &settings); jerr != nil {
			return fmt.Errorf("parsing existing %s: %w (fix manually or remove the file)", path, jerr)
		}
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("reading existing %s: %w", path, rerr)
	}
	if settings == nil {
		settings = map[string]any{}
	}

	// Build the env map for the Zed-launched `aoctl acp serve` subprocess.
	// Propagate the resolved endpoints so the subprocess reaches the same
	// agent-orca instance without relying on inherited shell env vars. The
	// token is included only when explicitly provided (flag / AOCTL_TOKEN env /
	// resolved from config during login) so the subprocess is self-contained.
	env := map[string]any{
		"AOCTL_ENDPOINT":     s.endpoint,
		"AOCTL_ACP_ENDPOINT": s.acp,
	}
	if s.token != "" {
		env["AOCTL_TOKEN"] = s.token
	}

	// Merge or overwrite the agent_servers entry for this agent.
	agentServers, _ := settings["agent_servers"].(map[string]any)
	if agentServers == nil {
		agentServers = map[string]any{}
	}
	agentServers[agentName] = map[string]any{
		"type":    "custom",
		"command": "aoctl",
		"args":    []string{"acp", "serve", "--agent", agentName},
		"env":     env,
	}
	settings["agent_servers"] = agentServers

	// Write back with stable, pretty-printed indentation.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("serialising Zed settings: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	_, _ = fmt.Fprintf(s.out, "Configured Zed external agent %q → `aoctl acp serve --agent %s`\n",
		agentName, agentName)
	_, _ = fmt.Fprintf(s.out, "Wrote settings to %s\n", path)
	_, _ = io.WriteString(s.out,
		"No need to restart Zed — it detects the change automatically. "+
			"Open the Agent Panel, start a new thread, and select your agent.\n")
	return nil
}

// zedSettingsPath returns the path to Zed's settings.json, respecting the
// ZED_CONFIG_DIR override (used in tests), then platform conventions matching
// `pool acp setup --editor zed`:
//   - macOS / Linux: $XDG_CONFIG_HOME/zed/settings.json or ~/.config/zed/settings.json
//   - Windows:       %APPDATA%\Zed\settings.json
func zedSettingsPath() (string, error) {
	if dir := os.Getenv("ZED_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA environment variable is not set")
		}
		return filepath.Join(appData, "Zed", "settings.json"), nil
	}
	// macOS and Linux — Zed uses ~/.config/zed even on macOS.
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "zed", "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "zed", "settings.json"), nil
}
