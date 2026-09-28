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

// Package mcp implements a client for the Model Context Protocol (MCP).
// It connects to MCP servers (via stdio or HTTP), discovers available tools,
// and dispatches tool calls. Tool definitions are exposed to agents as OpenAI
// function definitions injected into every chat completion request.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heddles/agent-orca/internal/oauth"
)

// Transport is the MCP connection transport type.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
	TransportSSE   Transport = "sse"
)

// EnvFileMapping maps an env var name to a file containing its secret value.
type EnvFileMapping struct {
	Name     string
	FilePath string
}

// AuthHeaderFile maps an HTTP header to a file containing its secret value.
type AuthHeaderFile struct {
	HeaderName string
	FilePath   string
	Prefix     string // prepended to file contents (e.g. "Bearer ")
}

// OAuthConfig drives OAuth 2.0 token acquisition for an HTTP/SSE MCP server. The
// model-router performs the exchange itself and holds the access token in memory, so
// NO access-token Secret is required — only the credentials Secret is mounted as
// files at CredentialsDir (client_id, client_secret, redirect_uri, refresh_token).
// This is what makes OAuth-only MCP servers (e.g. Slack, which rejects static
// bearer tokens with -32001 invalid_token) work without a static bearerSecret.
type OAuthConfig struct {
	// CredentialsDir holds the OAuth client credentials as files (client_id,
	// client_secret, redirect_uri, refresh_token).
	CredentialsDir string
	// Scopes overrides the server's scopes_supported; if empty, discovery scopes win.
	Scopes []string
}

// ServerConfig describes a running MCP server to connect to.
type ServerConfig struct {
	// Name is the Tool CRD name this server corresponds to.
	Name      string
	Transport Transport
	// URL is used for http/sse transports.
	URL string
	// Cmd and Args are used for stdio transport.
	Cmd  string
	Args []string
	Env  []string
	// EnvFiles maps env var names to files containing secret values.
	// Read at stdio subprocess startup.
	EnvFiles []EnvFileMapping
	// AuthHeaderFiles maps HTTP headers to files containing secret values.
	// Read at HTTP/SSE connection time and injected into every request.
	AuthHeaderFiles []AuthHeaderFile
	// OAuth, when set, makes the client perform the OAuth 2.0 exchange (in-memory
	// bearer) instead of using a static authHeaderFile. Only for http/sse transports.
	OAuth *OAuthConfig
	// AllowApps enables MCP App iframe rendering for tools from this server.
	AllowApps bool
	// IncludePatterns and ExcludePatterns filter the tool names the model-router
	// exposes to the LLM after discovering them via tools/list. When IncludePatterns
	// is non-empty, only tools matching at least one pattern are kept. ExcludePatterns
	// always wins. Empty (both unset) means no filtering — all discovered tools are
	// exposed (backward compatible).
	IncludePatterns []string
	ExcludePatterns []string
}

// Tool is a tool discovered from an MCP server.
type Tool struct {
	// ServerName is the ServerConfig.Name that exposes this tool.
	ServerName  string
	Name        string
	Description string
	InputSchema json.RawMessage
	// AppResourceURI is the ui:// resource URI from _meta.ui.resourceUri, if present.
	AppResourceURI string
	// AllowApps is propagated from the server's ServerConfig.AllowApps.
	AllowApps bool
}

// Client manages connections to one or more MCP servers and provides a unified
// tool discovery and invocation interface.
type Client struct {
	mu      sync.RWMutex
	configs []ServerConfig // original configs; used to retry connecting failed servers
	servers []*serverConn
	tools   []Tool
}

// New creates an MCP client and connects to all configured servers.
// Servers that fail to connect are skipped with a warning.
func New(ctx context.Context, configs []ServerConfig) *Client {
	c := &Client{}
	for _, cfg := range configs {
		conn, err := connect(ctx, cfg)
		// HTTP/SSE servers (incl. OAuth-managed ones) may not have their bearer token
		// populated yet at this exact instant; the operator writes it shortly after pod
		// start. Retry briefly so the initial tools/list succeeds once the token appears.
		if err != nil && (cfg.Transport == TransportHTTP || cfg.Transport == TransportSSE) {
			for attempt := 0; attempt < 5 && err != nil; attempt++ {
				slog.Warn("MCP server connect retry", "server", cfg.Name, "attempt", attempt+1, "err", err)
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					err = ctx.Err()
				}
				conn, err = connect(ctx, cfg)
			}
		}
		if err != nil {
			slog.Warn("MCP server connect failed", "server", cfg.Name, "err", err)
			continue
		}
		tools, err := conn.listTools(ctx)
		if err != nil {
			slog.Warn("MCP tools/list failed", "server", cfg.Name, "err", err)
			_ = conn.close()
			continue
		}
		for i := range tools {
			tools[i].ServerName = cfg.Name
			tools[i].AllowApps = cfg.AllowApps
		}
		c.servers = append(c.servers, conn)
		c.tools = append(c.tools, tools...)
		slog.Info("MCP server connected", "server", cfg.Name, "tools", len(tools))
	}
	return c
}

// setAuthHeaders resolves the bearer/credential files fresh from disk on every
// request. Re-reading per request (rather than once at connect) lets the operator
// rotate the access token in the mounted Secret and have running pods use the new
// value without a restart. A not-yet-populated secret file is skipped (logged) so it
// does not crash the router; the operator's reconnect (Refresh) retries then.
func (s *serverConn) setAuthHeaders(h http.Header) {
	for _, ah := range s.authHeaderFiles {
		val, err := readSecretFile(ah.FilePath)
		if err != nil {
			slog.Warn("MCP auth header file not ready; omitting header", "server", s.name, "header", ah.HeaderName, "err", err)
			continue
		}
		h.Set(ah.HeaderName, ah.Prefix+val)
	}
}

// Tools returns all tools discovered from connected MCP servers.
func (c *Client) Tools() []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tools
}

// Call invokes a tool on the appropriate MCP server and returns the result as a JSON string.
func (c *Client) Call(ctx context.Context, toolName, arguments string) (string, error) {
	// toolName format: "<serverName>:<tool>" or just "<tool>" for unique names.
	serverName, localName := parseToolName(toolName)

	c.mu.RLock()
	var target *serverConn
	for _, s := range c.servers {
		if serverName != "" && s.name != serverName {
			continue
		}
		for _, t := range s.availableTools {
			if t == localName || t == toolName {
				target = s
				break
			}
		}
		if target != nil {
			break
		}
	}
	c.mu.RUnlock()

	if target == nil {
		return "", fmt.Errorf("tool %q not found in any MCP server", toolName)
	}

	var args map[string]any
	if arguments == "" || arguments == "{}" {
		args = map[string]any{}
	} else if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parsing tool arguments: %w", err)
	}

	result, err := target.callTool(ctx, localName, args)
	if err != nil {
		return "", err
	}
	return result, nil
}

// Close disconnects from all MCP servers.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.servers {
		_ = s.close()
	}
}

// --- serverConn: manages a connection to a single MCP server ---

type serverConn struct {
	name            string
	transport       Transport
	includePatterns []string // glob filters applied to discovered tool names
	excludePatterns []string
	availableTools  []string
	// stdio fields
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	// http fields
	httpURL         string
	authHeaderFiles []AuthHeaderFile // bearer/credential configs, resolved per request (supports token rotation)
	oauth           *OAuthConfig     // OAuth config; token acquired in-memory (no token Secret)
	oauthToken      string           // current bearer access token (in-memory)
	// JSON-RPC ID counter
	nextID atomic.Int64
}

// exchangeOAuthToken performs the OAuth 2.0 token exchange against the server's
// published endpoints (RFC 8414 discovery + refresh_token grant) and returns the
// access token, read from the mounted CredentialsDir credential files.
func (s *serverConn) exchangeOAuthToken(ctx context.Context) (string, error) {
	tm := oauth.New(&http.Client{Timeout: 15 * time.Second}, nil)
	meta, err := tm.Discover(ctx, s.httpURL)
	if err != nil {
		return "", fmt.Errorf("discovering oauth metadata for %s: %w", s.name, err)
	}
	read := func(key string) string {
		b, err := os.ReadFile(filepath.Join(s.oauth.CredentialsDir, key))
		if err != nil {
			return ""
		}
		return string(b)
	}
	cfg := &oauth.Config{
		ServerURL:    s.httpURL,
		ClientID:     read("client_id"),
		ClientSecret: read("client_secret"),
		RedirectURI:  read("redirect_uri"),
		Scopes:       s.oauth.Scopes,
		RefreshToken: read("refresh_token"),
	}
	resp, err := tm.Refresh(ctx, meta, cfg)
	if err != nil {
		return "", fmt.Errorf("oauth token exchange for %s: %w", s.name, err)
	}
	return resp.AccessToken, nil
}

// jsonrpcRequest is a JSON-RPC 2.0 request.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcResponse is a JSON-RPC 2.0 response.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func connect(ctx context.Context, cfg ServerConfig) (*serverConn, error) {
	conn := &serverConn{
		name:            cfg.Name,
		transport:       cfg.Transport,
		includePatterns: cfg.IncludePatterns,
		excludePatterns: cfg.ExcludePatterns,
	}

	// Store the auth-header configs; they are resolved fresh on each HTTP request via
	// setAuthHeaders so the operator can rotate the bearer token in the mounted Secret
	// and have running pods pick up the new value without a restart.
	conn.authHeaderFiles = cfg.AuthHeaderFiles

	switch cfg.Transport {
	case TransportStdio:
		if err := validateStdioCommand(cfg.Cmd, cfg.Args); err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", cfg.Name, err)
		}
		return conn, conn.connectStdio(ctx, cfg)
	case TransportHTTP, TransportSSE:
		conn.httpURL = cfg.URL
		if cfg.OAuth != nil {
			conn.oauth = cfg.OAuth
			if tok, err := conn.exchangeOAuthToken(ctx); err != nil {
				slog.Warn("MCP oauth token exchange failed; connecting without token", "server", conn.name, "err", err)
			} else {
				conn.oauthToken = tok
			}
		}
		return conn, conn.initHTTP(ctx)
	default:
		return nil, fmt.Errorf("unsupported transport: %s", cfg.Transport)
	}
}

func (s *serverConn) connectStdio(ctx context.Context, cfg ServerConfig) error {
	s.cmd = exec.CommandContext(ctx, cfg.Cmd, cfg.Args...)
	s.cmd.Env = cfg.Env

	// Resolve secret-backed env vars from mounted files.
	for _, ef := range cfg.EnvFiles {
		val, err := readSecretFile(ef.FilePath)
		if err != nil {
			return fmt.Errorf("reading env file for %q: %w", ef.Name, err)
		}
		s.cmd.Env = append(s.cmd.Env, ef.Name+"="+val)
	}

	stdin, err := s.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("starting MCP server: %w", err)
	}
	s.stdin = stdin
	s.stdout = bufio.NewReader(stdout)

	// Send initialize.
	return s.initialize(ctx)
}

func (s *serverConn) initHTTP(ctx context.Context) error {
	return s.initialize(ctx)
}

func (s *serverConn) initialize(ctx context.Context) error {
	resp, err := s.send(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "agent-orca", "version": "v1alpha1"},
	})
	if err != nil {
		return fmt.Errorf("MCP initialize: %w", err)
	}
	slog.Debug("MCP initialized", "server", s.name, "result", string(resp))

	// Send initialized notification.
	return s.notify(ctx, "notifications/initialized", nil)
}

func (s *serverConn) listTools(ctx context.Context) ([]Tool, error) {
	result, err := s.send(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Meta        struct {
				UI *struct {
					ResourceURI string `json:"resourceUri"`
				} `json:"ui,omitempty"`
			} `json:"_meta"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parsing tools/list response: %w", err)
	}

	tools := make([]Tool, 0, len(resp.Tools))
	for _, t := range resp.Tools {
		tool := Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
		if t.Meta.UI != nil {
			tool.AppResourceURI = t.Meta.UI.ResourceURI
		}
		tools = append(tools, tool)
		s.availableTools = append(s.availableTools, t.Name)
	}
	// Apply name-based include/exclude filters, keeping the availableTools
	// name list in sync with the surviving tools.
	kept := len(tools)
	tools, s.availableTools = filterTools(tools, s.availableTools, s.includePatterns, s.excludePatterns)
	if dropped := kept - len(tools); dropped > 0 {
		slog.Info("MCP tools filtered by name patterns", "server", s.name,
			"discovered", len(resp.Tools), "kept", len(tools), "dropped", dropped)
	}
	return tools, nil
}

// filterTools applies the include/exclude glob patterns to discovered tools.
// It drops tools whose name does not pass matchToolName and keeps the parallel
// availableToolNames list in sync with the surviving tools. When no patterns are
// configured it returns the inputs unchanged.
func filterTools(tools []Tool, availableToolNames []string, include, exclude []string) ([]Tool, []string) {
	if len(include) == 0 && len(exclude) == 0 {
		return tools, availableToolNames
	}
	kept := make(map[string]struct{}, len(tools))
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if !matchToolName(t.Name, include, exclude) {
			continue
		}
		out = append(out, t)
		kept[t.Name] = struct{}{}
	}
	names := make([]string, 0, len(availableToolNames))
	for _, n := range availableToolNames {
		if _, ok := kept[n]; ok {
			names = append(names, n)
		}
	}
	return out, names
}

// FetchResource fetches a ui:// resource from the named MCP server via resources/read
// and returns the text content (typically HTML).
func (c *Client) FetchResource(ctx context.Context, serverName, uri string) (string, error) {
	c.mu.RLock()
	var target *serverConn
	for _, s := range c.servers {
		if s.name == serverName {
			target = s
			break
		}
	}
	c.mu.RUnlock()

	if target == nil {
		return "", fmt.Errorf("MCP server %q not found", serverName)
	}

	result, err := target.send(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", fmt.Errorf("resources/read: %w", err)
	}

	var resp struct {
		Contents []json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("parsing resources/read response: %w", err)
	}
	for _, block := range resp.Contents {
		if t := extractContentText(block); t != "" {
			return t, nil
		}
	}
	return "", fmt.Errorf("no text content in resources/read response")
}

func (s *serverConn) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	result, err := s.send(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	return parseToolResult(name, result)
}

// parseToolResult turns an MCP tools/call result into a string suitable for
// returning to the LLM. Extracted from callTool so it can be unit-tested without
// a live MCP server.
func parseToolResult(name string, result []byte) (string, error) {
	var resp struct {
		Content []json.RawMessage `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		// Not a standard MCP tools/call response shape (e.g. a bare `[]` from a
		// misconfigured / erroring server). Surface it as a tool error instead of
		// silently returning the raw bytes: otherwise the LLM receives an empty
		// result, can't tell the tool actually failed, and bails ("exit too soon").
		raw := string(result)
		if len(raw) > 200 {
			raw = raw[:200] + "… (truncated)"
		}
		return "", fmt.Errorf("MCP tool %q returned a malformed result (expected an object with a \"content\" array): %s", name, raw)
	}

	var texts []string
	for _, block := range resp.Content {
		if t := extractContentText(block); t != "" {
			texts = append(texts, t)
		}
	}

	if resp.IsError {
		return "", fmt.Errorf("MCP tool error: %s", strings.Join(texts, "; "))
	}
	if len(texts) == 0 && len(resp.Content) > 0 {
		// Content blocks exist but none yielded text — return raw JSON
		// so the LLM can still use the data.
		return string(result), nil
	}
	return strings.Join(texts, "\n"), nil
}

// extractContentText extracts text from an MCP content block regardless of type.
// MCP content blocks vary by server and type (text, resource, image, etc.).
// This function generically walks the JSON to find text content without
// hardcoding the structure of any specific content type.
func extractContentText(block json.RawMessage) string {
	var obj map[string]any
	if err := json.Unmarshal(block, &obj); err != nil {
		return ""
	}
	// Direct "text" field (covers type:"text" blocks).
	if t, ok := obj["text"].(string); ok && t != "" {
		return t
	}
	// Walk one level of nested objects to find a "text" field.
	// Covers type:"resource" ({resource:{text:"..."}}) and similar patterns.
	for _, v := range obj {
		if nested, ok := v.(map[string]any); ok {
			if t, ok := nested["text"].(string); ok && t != "" {
				return t
			}
		}
	}
	return ""
}

// send sends a JSON-RPC request and returns the raw result bytes.
func (s *serverConn) send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	switch s.transport {
	case TransportStdio:
		return s.sendStdio(reqBytes)
	case TransportHTTP, TransportSSE:
		return s.sendHTTP(ctx, reqBytes)
	default:
		return nil, fmt.Errorf("unsupported transport: %s", s.transport)
	}
}

func (s *serverConn) sendStdio(reqBytes []byte) (json.RawMessage, error) {
	// Extract the request ID so we can match the response.
	var reqMsg struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(reqBytes, &reqMsg)

	_, err := fmt.Fprintf(s.stdin, "%s\n", reqBytes)
	if err != nil {
		return nil, fmt.Errorf("writing to MCP stdin: %w", err)
	}

	// Read lines until we find the JSON-RPC response matching our request ID.
	// MCP servers may emit notifications (no id) or log lines on stdout before
	// the actual response.
	for {
		line, err := s.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading from MCP stdout: %w", err)
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var resp jsonrpcResponse
		if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
			// Not valid JSON-RPC — skip (could be log output).
			slog.Debug("skipping non-JSON line from MCP stdout", "server", s.name, "line", trimmed)
			continue
		}

		// Notifications have no ID (or ID == 0). Skip them.
		if resp.ID != reqMsg.ID {
			slog.Debug("skipping MCP notification/mismatched response", "server", s.name, "gotID", resp.ID, "wantID", reqMsg.ID)
			continue
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (s *serverConn) sendHTTP(ctx context.Context, reqBytes []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.httpURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.setAuthHeaders(req.Header)
	if s.oauthToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.oauthToken)
	}

	resp, err := http.DefaultClient.Do(req)
	defer func() { _ = resp.Body.Close() }()

	var rpcResp jsonrpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("parsing MCP HTTP response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// notify sends a JSON-RPC notification (no response expected).
func (s *serverConn) notify(ctx context.Context, method string, params any) error {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	reqBytes, _ := json.Marshal(req)

	switch s.transport {
	case TransportStdio:
		_, err := fmt.Fprintf(s.stdin, "%s\n", reqBytes)
		return err
	case TransportHTTP, TransportSSE:
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.httpURL, bytes.NewReader(reqBytes))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		s.setAuthHeaders(httpReq.Header)
		if s.oauthToken != "" {
			httpReq.Header.Set("Authorization", "Bearer "+s.oauthToken)
		}
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	return nil
}

func (s *serverConn) close() error {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil {
		return s.cmd.Wait()
	}
	return nil
}

// parseToolName splits "serverName:toolName" into components.
// If no ":" is present the full name is treated as the local tool name.
func parseToolName(name string) (serverName, localName string) {
	if before, after, ok := strings.Cut(name, ":"); ok {
		return before, after
	}
	return "", name
}

// matchToolName reports whether a discovered tool name should be exposed to the
// LLM given the configured include/exclude glob patterns. Semantics mirror the
// includePatterns/excludePatterns convention used by KnowledgeBase ingestion:
//   - If includePatterns is non-empty, the name must match at least one pattern.
//   - If the name matches any excludePatterns entry, it is dropped even if it was
//     admitted by includePatterns (exclude always wins).
//
// An empty pattern set (the default) admits everything.
func matchToolName(name string, includePatterns, excludePatterns []string) bool {
	if len(includePatterns) > 0 && !matchesAny(name, includePatterns) {
		return false
	}
	if matchesAny(name, excludePatterns) {
		return false
	}
	return true
}

// matchesAny reports whether name matches at least one pattern.
func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if matchGlob(p, name) {
			return true
		}
	}
	return false
}

// matchGlob supports standard glob patterns ("*" and "?") plus "**" for
// multi-segment matching, mirroring cmd/mcp-ingester's matchGlob. Tool names are
// single-segment identifiers, so the "**" branch only matters for completeness.
func matchGlob(pattern, name string) bool {
	if strings.Contains(pattern, "**") {
		return matchDoublestar(pattern, name)
	}
	if matched, _ := filepath.Match(pattern, name); matched {
		return true
	}
	// Also match against the base name, so a bare "*.go"-style pattern works even
	// if the path were to contain separators.
	if matched, _ := filepath.Match(pattern, filepath.Base(name)); matched {
		return true
	}
	return false
}

// matchDoublestar supports "**" for recursive multi-segment matching, porting
// cmd/mcp-ingester's matchDoublestar. This mirrors the project's existing glob
// convention so MCP tool filtering behaves identically to KnowledgeBase ingestion
// (e.g. "internal/**" matches "internal" and anything nested under it).
func matchDoublestar(pattern, name string) bool {
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")

	// Prefix must match the start of the name as a path segment.
	if prefix != "" && !strings.HasPrefix(name, prefix+"/") && name != prefix {
		return false
	}

	// If no suffix, ** matches everything remaining after the prefix.
	if suffix == "" {
		return true
	}

	// Otherwise the name must end with the suffix.
	return strings.HasSuffix(name, suffix)
}
