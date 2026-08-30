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

// Command aoctl is the CLI for integrating with agent-orca's external APIs.
//
// It targets the External Task API (default http://localhost:8084) for task
// submission/polling/streaming and the ACP API (default http://localhost:8000)
// for agent discovery. Authentication is pluggable:
//   - `oauth` — OAuth2 client_credentials exchange at POST /oauth/token
//     (agent-orca-issued tenant JWT).
//   - `oidc` — OIDC authorization-code flow (browser + loopback callback).
//     The resulting id_token is presented directly as a bearer token; the
//     server validates it via the federated issuer's JWKS.
//
// `aoctl login` (with no credentials) presents an interactive selection menu so
// users can pick their login method; pass --auth-method to run non-interactively.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/floppyfish14/agent-orca/internal/security/oidc"
)

const (
	appName         = "aoctl"
	defaultEndpoint = "http://localhost:8084"
	defaultACP      = "http://localhost:8000"
	defaultTimeout  = 30 * time.Second

	// Config keys.
	tokenKey = "token" //nolint:unused

	endptKey = "endpoint" //nolint:unused

	acpKey = "acp_endpoint" //nolint:unused

)

// configFileName is the on-disk config file name within configDir().
const configFileName = "config.json"

// loginCmd is the cobra command name used to skip token refresh during login.
const loginCmd = "login"

// --- API types (mirror internal/apiserver external_api.go + acp_api.go) ---

type TaskSubmission struct {
	Agent    string            `json:"agent"`
	Input    string            `json:"input"`
	Timeout  string            `json:"timeout,omitempty"`
	Callback *TaskCallback     `json:"callback,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type TaskCallback struct {
	URL       string `json:"url"`
	SecretRef string `json:"secretRef,omitempty"`
}

type TaskLinks struct {
	Self   string `json:"self"`
	Stream string `json:"stream,omitempty"`
}

type TaskResponse struct {
	ID          string            `json:"id"`
	Agent       string            `json:"agent"`
	Status      string            `json:"status"`
	Output      string            `json:"output,omitempty"`
	SpendUSD    string            `json:"spendUSD,omitempty"`
	CreatedAt   string            `json:"createdAt,omitempty"`
	CompletedAt string            `json:"completedAt,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Links       *TaskLinks        `json:"links,omitempty"`
}

// Event is a single parsed Server-Sent Event.
type Event struct {
	Type string
	Data string
}

// ACPAgentManifest mirrors the ACP GET /agents manifest for `aoctl agents list`.
type ACPAgentManifest struct {
	Name               string         `json:"name"`
	Namespace          string         `json:"namespace,omitempty"`
	Description        string         `json:"description"`
	InputContentTypes  []string       `json:"input_content_types"`
	OutputContentTypes []string       `json:"output_content_types"`
	InputSchema        map[string]any `json:"input_schema,omitempty"`
	OutputSchema       map[string]any `json:"output_schema,omitempty"`
	AllowedTools       []ACPToolInfo  `json:"allowed_tools,omitempty"`
	KnowledgeBases     []string       `json:"knowledge_bases,omitempty"`
	GuardrailPolicy    string         `json:"guardrail_policy,omitempty"`
	ClarifyAvailable   bool           `json:"clarify_available"`
}

// ACPToolInfo mirrors the tool info in the ACP agent manifest.
type ACPToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// --- Admin API types (mirror internal/apiserver admin_api.go) ---

// AdminRateLimit mirrors TenantRateLimit for the create request.
type AdminRateLimit struct {
	RequestsPerMinute int `json:"requestsPerMinute,omitempty"`
	ConcurrentRuns    int `json:"concurrentRuns,omitempty"`
}

// AdminTenantCreateRequest is the body for POST /admin/tenants.
type AdminTenantCreateRequest struct {
	Name            string          `json:"name"`
	TargetNamespace string          `json:"targetNamespace"`
	ClientID        string          `json:"clientID"`
	ClientSecret    string          `json:"clientSecret,omitempty"`
	AllowedAgents   []string        `json:"allowedAgents,omitempty"`
	RateLimit       *AdminRateLimit `json:"rateLimit,omitempty"`
	BudgetPerDayUSD string          `json:"budgetPerDayUSD,omitempty"`
}

// AdminTenantResponse is the tenant representation returned by the admin API.
// ClientSecret is included ONLY at create/rotate time (one-time disclosure).
type AdminTenantResponse struct {
	Name            string          `json:"name"`
	Namespace       string          `json:"namespace"`
	ClientID        string          `json:"clientID"`
	ClientSecret    string          `json:"clientSecret,omitempty"`
	TargetNamespace string          `json:"targetNamespace"`
	AllowedAgents   []string        `json:"allowedAgents,omitempty"`
	RateLimit       *AdminRateLimit `json:"rateLimit,omitempty"`
	BudgetPerDayUSD string          `json:"budgetPerDayUSD,omitempty"`
}

// --- HTTP client ---

// Client talks to the agent-orca external task API and ACP API.
type Client struct {
	Endpoint string
	ACP      string
	Token    string
	HTTP     *http.Client
}

func newClient(endpoint, acp, token string, timeout time.Duration, insecure bool) *Client {
	c := &Client{
		Endpoint: normalizeEndpoint(strings.TrimRight(endpoint, "/")),
		ACP:      normalizeEndpoint(strings.TrimRight(acp, "/")),
		Token:    token,
		HTTP:     &http.Client{Timeout: timeout},
	}
	if insecure {
		// A restrictive, intentional set of transport tweaks for local dev
		// (self-signed cluster TLS). Production deployments front the APIs
		// behind an Ingress with a real cert, so this is never used in prod.
		c.HTTP.Transport = insecureTransport()
	}
	return c
}

// normalizeEndpoint strips a trailing known API route path from the endpoint
// URL to prevent double-path issues. Users sometimes include a route segment
// (e.g. "http://host/agents" or "http://host/tasks") when the base URL should
// be just the host root, because the client appends routes like /agents/{name}
// or /v1/tasks itself.
//
// Only exact trailing route matches are stripped, so legitimate proxy prefixes
// (e.g. "http://host/acp") are preserved.
func normalizeEndpoint(ep string) string {
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return ep
	}
	for _, route := range apiPathSuffixes {
		if u.Path == route {
			u.Path = ""
			return u.String()
		}
	}
	return ep
}

// apiPathSuffixes are route prefixes used by either the ACP API or the External
// Task API. If a user includes one of these as a trailing path component in
// --endpoint or --acp-endpoint, it is stripped to prevent double-path URLs.
var apiPathSuffixes = []string{
	// ACP API routes (cmd/aoctl/acp_translate.go appends these)
	"/agents", "/runs", "/sessions", "/session",
	// External Task API routes (cmd/aoctl/main.go appends these)
	"/v1/tasks", "/tasks", "/admin/tenants", "/admin", "/oauth/token",
}

// tokenAuth attaches the bearer token if the caller supplied one.
func (c *Client) tokenAuth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// Login exchanges client credentials for a bearer token and returns it.
func (c *Client) Login(ctx context.Context, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.Endpoint, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("token exchange returned an empty access_token")
	}
	return tok.AccessToken, nil
}

// SubmitTask creates a task and returns the created task (HTTP 201 expected).
func (c *Client) SubmitTask(ctx context.Context, sub TaskSubmission) (TaskResponse, int, error) {
	body, err := json.Marshal(sub)
	if err != nil {
		return TaskResponse{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.Endpoint, "/")+"/v1/tasks", bodyReader(body))
	if err != nil {
		return TaskResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.tokenAuth(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return TaskResponse{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var tr TaskResponse
	if err := decodeJSON(resp, &tr, http.StatusCreated); err != nil {
		return TaskResponse{}, resp.StatusCode, err
	}
	return tr, resp.StatusCode, nil
}

// GetTask retrieves a task by id.
func (c *Client) GetTask(ctx context.Context, id string) (TaskResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/tasks/%s", c.Endpoint, url.PathEscape(id)), nil)
	if err != nil {
		return TaskResponse{}, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return TaskResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var tr TaskResponse
	if err := decodeJSON(resp, &tr, http.StatusOK); err != nil {
		return TaskResponse{}, err
	}
	return tr, nil
}

// ListTasks lists the caller's tasks, optionally filtered.
func (c *Client) ListTasks(ctx context.Context, agent, status string) ([]TaskResponse, error) {
	u := fmt.Sprintf("%s/v1/tasks", c.Endpoint)
	q := u
	if agent != "" || status != "" {
		v := url.Values{}
		if agent != "" {
			v.Set("agent", agent)
		}
		if status != "" {
			v.Set("status", status)
		}
		q = u + "?" + v.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q, nil)
	if err != nil {
		return nil, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Tasks []TaskResponse `json:"tasks"`
	}
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// CancelTask cancels a running/pending task by id.
func (c *Client) CancelTask(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/v1/tasks/%s", c.Endpoint, url.PathEscape(id)), nil)
	if err != nil {
		return err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel task failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// StreamTask subscribes to the SSE stream for a task and returns a channel of
// events. The channel is closed when the stream ends or the context is cancelled.
func (c *Client) StreamTask(ctx context.Context, id string) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/tasks/%s/stream", c.Endpoint, url.PathEscape(id)), nil)
	if err != nil {
		return nil, err
	}
	c.tokenAuth(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stream task failed (HTTP %d)", resp.StatusCode)
	}

	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		// Allow lines up to 1 MiB (token chunks can be large).
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		cur := Event{}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				cur.Type = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				cur.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				if cur.Type != "" || cur.Data != "" {
					select {
					case ch <- cur:
					case <-ctx.Done():
						return
					}
					cur = Event{}
				}
			}
		}
	}()
	return ch, nil
}

// parseSSE reads an io.Reader of SSE text and returns all events. Pure helper
// for testing the parser without a live connection.
func parseSSE(r io.Reader) []Event {
	var events []Event
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	cur := Event{}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.Type = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if cur.Type != "" || cur.Data != "" {
				events = append(events, cur)
				cur = Event{}
			}
		}
	}
	return events
}

// ListAgents lists agents available to the caller via the ACP API.
func (c *Client) ListAgents(ctx context.Context) ([]ACPAgentManifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.ACP, "/")+"/agents", nil)
	if err != nil {
		return nil, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Agents []ACPAgentManifest `json:"agents"`
	}
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// GetAgentManifest fetches the manifest for a specific agent via the ACP API.
func (c *Client) GetAgentManifest(ctx context.Context, name string) (ACPAgentManifest, error) { //nolint:dupl

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/agents/%s", strings.TrimRight(c.ACP, "/"), url.PathEscape(name)), nil)
	if err != nil {
		return ACPAgentManifest{}, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ACPAgentManifest{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var m ACPAgentManifest
	if err := decodeJSON(resp, &m, http.StatusOK); err != nil {
		return ACPAgentManifest{}, err
	}
	return m, nil
}

// ACPMessagePart mirrors the ACP spec message part. A message is made of one or
// more parts, each with a content_type and inline content (or a content_url).
type ACPMessagePart struct {
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type"`
	Content     string `json:"content,omitempty"`
	ContentURL  string `json:"content_url,omitempty"`
}

// ACPMessage mirrors the ACP spec message: a role plus an array of parts.
type ACPMessage struct {
	Role  string           `json:"role"`
	Parts []ACPMessagePart `json:"parts"`
}

// ACPRunRequest is the body for POST /agents/{name}/run.
type ACPRunRequest struct {
	Input     []ACPMessage `json:"input"`
	SessionID string       `json:"session_id,omitempty"`
}

// ACPRunResponse is the response from POST /agents/{name}/run.
type ACPRunResponse struct {
	AgentName  string `json:"agent_name"`
	SessionID  string `json:"session_id,omitempty"`
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// CreateAgentRun creates a new run for an agent via the ACP API.
func (c *Client) CreateAgentRun(ctx context.Context, agentName string, req ACPRunRequest) (ACPRunResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ACPRunResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/agents/%s/run", strings.TrimRight(c.ACP, "/"), url.PathEscape(agentName)), bodyReader(body))
	if err != nil {
		return ACPRunResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.tokenAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return ACPRunResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONBodyBytes))
		// Try to parse an ACP error body ({code, message}) for a friendlier message.
		var acpErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &acpErr)
		detail := strings.TrimSpace(string(respBody))
		if acpErr.Code != "" || acpErr.Message != "" {
			detail = fmt.Sprintf("[%s] %s", acpErr.Code, acpErr.Message)
		}
		hint := ""
		if strings.Contains(string(respBody), "input content is required") {
			hint = "\nHint: the server expects ACP-format input (messages with parts). " +
				"Run `aoctl agents describe " + agentName + "` to see the expected input schema."
		}
		return ACPRunResponse{}, fmt.Errorf("create agent run %q failed (HTTP %d): %s%s", agentName, resp.StatusCode, detail, hint) //nolint:lll
	}
	var out ACPRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ACPRunResponse{}, fmt.Errorf("decoding run response: %w", err)
	}
	return out, nil
}

// --- Admin API client methods (POST /admin/* on the External Task API) ---

// CreateTenant creates a new tenant via the admin API.
func (c *Client) CreateTenant(ctx context.Context, req AdminTenantCreateRequest) (AdminTenantResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return AdminTenantResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.Endpoint, "/")+"/admin/tenants", bodyReader(body))
	if err != nil {
		return AdminTenantResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.tokenAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return AdminTenantResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return AdminTenantResponse{}, fmt.Errorf("create tenant failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody))) //nolint:lll

	}
	var out AdminTenantResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return AdminTenantResponse{}, fmt.Errorf("decoding create tenant response: %w", err)
	}
	return out, nil
}

// ListTenants lists all tenants via the admin API.
func (c *Client) ListTenants(ctx context.Context) ([]AdminTenantResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.Endpoint, "/")+"/admin/tenants", nil)
	if err != nil {
		return nil, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Tenants []AdminTenantResponse `json:"tenants"`
		Count   int                   `json:"count"`
	}
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Tenants, nil
}

// GetTenant retrieves a single tenant by name via the admin API.
func (c *Client) GetTenant(ctx context.Context, name string) (AdminTenantResponse, error) { //nolint:dupl

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/admin/tenants/%s", strings.TrimRight(c.Endpoint, "/"), url.PathEscape(name)), nil)
	if err != nil {
		return AdminTenantResponse{}, err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return AdminTenantResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out AdminTenantResponse
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return AdminTenantResponse{}, err
	}
	return out, nil
}

// RotateTenantSecret rotates the client secret for a tenant via the admin API.
func (c *Client) RotateTenantSecret(ctx context.Context, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/admin/tenants/%s/rotate-secret", strings.TrimRight(c.Endpoint, "/"), url.PathEscape(name)), nil)
	if err != nil {
		return "", err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := decodeJSON(resp, &out, http.StatusOK); err != nil {
		return "", err
	}
	return out.ClientSecret, nil
}

// DeleteTenant deletes a tenant via the admin API.
func (c *Client) DeleteTenant(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/admin/tenants/%s", strings.TrimRight(c.Endpoint, "/"), url.PathEscape(name)), nil)
	if err != nil {
		return err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete tenant failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// --- config persistence ---

// Config stores the user's login + endpoint choices on disk.
type Config struct {
	Endpoint string `json:"endpoint"`
	ACP      string `json:"acp_endpoint"`
	Token    string `json:"token"`

	// AuthMethod is "oauth" (client_credentials) or "oidc" (authorization code).
	// Empty defaults to "oauth" for backward compatibility with configs written
	// before OIDC login existed.
	AuthMethod string `json:"authMethod,omitempty"`

	// The fields below are populated only for authMethod == "oidc" and are used
	// to (re)build the OIDC provider for session refresh. ClientSecret is
	// persisted at the same 0600 file perms as the token (see saveConfig) so
	// refresh grants succeed for confidential clients; a future improvement may
	// move long-lived secrets into a system keyring.
	IssuerURL     string `json:"issuerURL,omitempty"`
	ClientID      string `json:"clientID,omitempty"`
	ClientSecret  string `json:"clientSecret,omitempty"`
	RedirectURI   string `json:"redirectURI,omitempty"`
	RefreshToken  string `json:"refreshToken,omitempty"`
	IDTokenExpiry string `json:"idTokenExpiry,omitempty"` // RFC3339; empty => not refreshed
}

func configDir() (string, error) {
	if d := os.Getenv("AOCTL_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "."+appName), nil
}

func configPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, configFileName), nil
}

// loadConfig reads the saved config, returning an empty Config if absent.
// Defaults for Endpoint/ACP are intentionally NOT applied here — the caller
// (PersistentPreRunE) needs to distinguish "not set" from "explicitly set to
// the default" so it can derive the ACP endpoint from the External Task API
// endpoint when only one was configured.
func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// saveConfig writes the config (and its parent dir) to disk.
func saveConfig(cfg *Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// --- command wiring ---

type settings struct {
	endpoint   string
	acp        string
	token      string
	insecure   bool
	timeout    time.Duration
	stream     bool
	clientID   string
	secret     string
	agent      string
	input      string
	timeoutStr string
	status     string
	id         string //nolint:unused

	sessionID       string
	targetNamespace string
	allowedAgents   []string
	rpm             int
	concurrent      int
	budgetPerDay    string
	file            string // --file: read input from a file instead of --input/stdin
	contentType     string // --content-type: MIME type for the message part (default text/plain)
	editor          string // --editor: editor to configure for `acp setup` (e.g. zed)
	out, errw       io.Writer

	// OIDC interactive-login fields. authMethod/issuerURL/redirectURI/noBrowser
	// mirror the login flags; `in`, `isTerminal` and `openBrowser` are the
	// injectable seams that let login prompts + browser opening be unit-tested.
	authMethod   string
	issuerURL    string
	redirectURI  string
	noBrowser    bool
	jsonOut      bool                      // --json / AOCTL_OUTPUT_FORMAT: emit JSON instead of human tables
	stdin        *bufio.Reader             // interactive stdin (shared reader)
	isTerminal   func() bool               // true when stdin is a TTY (default: real check)
	readPassword func(int) ([]byte, error) // reads a secret without echo (default: term.ReadPassword)
	openBrowser  func(string) error        // launches the system browser (default: cross-platform opener)
}

func (s *settings) client() *Client {
	return newClient(s.endpoint, s.acp, s.token, s.timeout, s.insecure)
}

// runLogin resolves the requested auth method and dispatches to the right
// sub-flow. With no --auth-method and no credential hints it presents an
// interactive selection menu (the CLI analogue of the UI's /oauth/login picker)
// when stdin is a terminal; in a non-interactive context it errors with a clear
// hint instead of blocking on stdin.
func (s *settings) runLogin() error {
	method := s.authMethod
	if method == "" {
		switch {
		case s.issuerURL != "":
			method = loginMethodOIDC
		case s.clientID != "" && s.secret != "":
			method = loginMethodOAuth
		default:
			// No hints at all → interactive picker (requires a TTY).
			if s.isTerminal == nil || !s.isTerminal() {
				return errors.New("no login method specified\nrun `aoctl login` in a terminal for the interactive picker, or pass --auth-method=oauth|oidc with the relevant credentials") //nolint:lll
			}
			m, err := promptSelection(s.out, s.stdin, "How would you like to log in?", []promptOption{
				{Value: loginMethodOAuth, Label: "OAuth (client credentials) — machine-to-machine; needs client id + secret"},
				{Value: loginMethodOIDC, Label: "OIDC (authorization code) — interactive login via an identity provider"},
			})
			if err != nil {
				return err
			}
			method = m
		}
	}
	switch method {
	case loginMethodOAuth:
		return s.loginOAuth()
	case loginMethodOIDC:
		return s.loginOIDCInteractive()
	default:
		return fmt.Errorf("unknown --auth-method %q (use oauth or oidc)", method)
	}
}

// loginOAuth performs the OAuth2 client_credentials exchange. If credentials
// are missing it prompts for them (interactive only).
func (s *settings) loginOAuth() error {
	interactive := s.isTerminal == nil || s.isTerminal()
	if s.clientID == "" {
		v, err := s.promptRequired("client-id", "OAuth client ID: ", interactive)
		if err != nil {
			return err
		}
		s.clientID = v
	}
	if s.secret == "" {
		v, err := s.promptRequiredSecret("client-secret", "OAuth client secret: ", interactive)
		if err != nil {
			return err
		}
		s.secret = v
	}
	if s.clientID == "" || s.secret == "" {
		return errors.New("--client-id and --client-secret are required for oauth")
	}
	c := newClient(s.endpoint, s.acp, "", s.timeout, s.insecure)
	tok, err := c.Login(context.Background(), s.clientID, s.secret)
	if err != nil {
		return err
	}
	s.token = tok
	s.authMethod = loginMethodOAuth
	if err := saveLoginConfig(s.endpoint, s.acp, tok, s.authMethod); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(s.out, "logged in to", s.endpoint)
	return nil
}

// loginOIDCInteractive drives the interactive OIDC authorization-code flow,
// prompting for any missing parameters, then runs the browser + loopback
// callback exchange.
func (s *settings) loginOIDCInteractive() error {
	interactive := s.isTerminal == nil || s.isTerminal()
	if s.issuerURL == "" {
		if !interactive {
			return errors.New("--issuer-url is required for --auth-method=oidc (or run interactively in a terminal)")
		}
		v, err := promptLine(s.out, s.stdin, "OIDC issuer URL (e.g. https://accounts.google.com): ")
		if err != nil {
			return err
		}
		s.issuerURL = v
	}
	if s.clientID == "" {
		v, err := s.promptRequired("client-id", "OIDC client ID: ", interactive)
		if err != nil {
			return err
		}
		s.clientID = v
	}
	// Client secret is optional for public (PKCE-only) clients.
	if s.secret == "" && interactive {
		v, err := promptSecret(s.out, s.stdin, "OIDC client secret (blank=public): ", s.isTerminal, s.readPassword)
		if err != nil {
			return err
		}
		s.secret = v
	}
	if s.issuerURL == "" || s.clientID == "" {
		return errors.New("--issuer-url and --client-id are required for oidc")
	}
	cfg := OIDCLoginConfig{
		IssuerURL:    s.issuerURL,
		ClientID:     s.clientID,
		ClientSecret: s.secret,
		RedirectURI:  s.redirectURI,
		NoBrowser:    s.noBrowser,
		OpenBrowser:  s.openBrowser,
	}
	res, err := loginOIDC(context.Background(), cfg, s.out)
	if err != nil {
		return err
	}
	s.token = res.IDToken
	s.authMethod = loginMethodOIDC
	return saveOIDCLoginConfig(s, s.endpoint, s.acp, res)
}

// promptRequired reads a labelled value interactively; when not a terminal it
// returns an error telling the user to supply the matching flag.
func (s *settings) promptRequired(flag, label string, interactive bool) (string, error) {
	if !interactive {
		return "", fmt.Errorf("--%s is required (or run `aoctl login` in a terminal to be prompted)", flag)
	}
	return promptLine(s.out, s.stdin, label)
}

// promptRequiredSecret is the no-echo variant of promptRequired. When a terminal
// is attached it reads the secret without echo; otherwise it errors with a hint
// (so non-TTY callers must pass the flag).
func (s *settings) promptRequiredSecret(flag, label string, interactive bool) (string, error) {
	if !interactive {
		return "", fmt.Errorf("--%s is required (or run `aoctl login` in a terminal to be prompted)", flag)
	}
	return promptSecret(s.out, s.stdin, label, s.isTerminal, s.readPassword)
}

// refreshOIDCIfNeeded refreshes an expired OIDC id_token using the cached refresh
// token. It is best-effort: a failure returns an error (logged by the caller)
// but never panics. Returns (newToken, refreshed, err).
func (s *settings) refreshOIDCIfNeeded(cfg *Config) (string, bool, error) {
	// Decide whether the id_token is expired (with grace).
	expStr := cfg.IDTokenExpiry
	var exp time.Time
	if expStr != "" {
		if e, err := time.Parse(time.RFC3339, expStr); err == nil {
			exp = e
		}
	} else if e, err := jwtExpiry(cfg.Token); err == nil {
		exp = e
	}
	if !exp.IsZero() && time.Now().Before(exp.Add(-oidcLoginGracePeriod)) {
		return "", false, nil // still valid
	}
	if cfg.RefreshToken == "" {
		return "", false, nil // nothing to refresh with
	}
	redirectURI := cfg.RedirectURI
	if redirectURI == "" {
		redirectURI = defaultOIDCRedirectURI
	}
	provider, err := oidc.NewProvider(context.Background(), oidc.Config{
		IssuerURL:    cfg.IssuerURL,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURI:  redirectURI,
	})
	if err != nil {
		return "", false, fmt.Errorf("discovering OIDC provider: %w", err)
	}
	newID, newRefresh, err := provider.Refresh(context.Background(), cfg.RefreshToken)
	if err != nil {
		return "", false, err
	}
	newExp, _ := jwtExpiry(newID)
	updated := *cfg
	updated.Token = newID
	updated.RefreshToken = newRefresh
	if !newExp.IsZero() {
		updated.IDTokenExpiry = newExp.UTC().Format(time.RFC3339)
	}
	if err := saveConfig(&updated); err != nil {
		// Non-fatal: the in-memory token is still returned to the caller.
		_, _ = fmt.Fprintf(s.errw, "warning: could not persist refreshed OIDC token: %v\n", err)
	}
	return newID, true, nil
}

// saveLoginConfig persists an OAuth login (token only).
func saveLoginConfig(endpoint, acp, token, authMethod string) error {
	return saveConfig(&Config{
		Endpoint:   endpoint,
		ACP:        acp,
		Token:      token,
		AuthMethod: authMethod,
	})
}

// saveOIDCLoginConfig persists an OIDC login (token + refresh material).
func saveOIDCLoginConfig(s *settings, endpoint, acp string, res *OIDCLoginResult) error {
	cfg := &Config{
		Endpoint:     endpoint,
		ACP:          acp,
		Token:        res.IDToken,
		AuthMethod:   loginMethodOIDC,
		IssuerURL:    s.issuerURL,
		ClientID:     s.clientID,
		ClientSecret: s.secret,
		RedirectURI:  s.redirectURI,
		RefreshToken: res.RefreshToken,
	}
	if !res.ExpiresAt.IsZero() {
		cfg.IDTokenExpiry = res.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return saveConfig(cfg)
}

// newRootCmd builds the command tree and returns it. Split out so tests can
// construct it with an isolated config dir.
func newRootCmd() (*cobra.Command, *settings) { //nolint:gocyclo

	s := &settings{}

	root := &cobra.Command{
		Use:   appName,
		Short: "CLI for agent-orca's external APIs (tasks + agent discovery)",
		Long:  `aoctl submits tasks to agent-orca and streams results. Run ` + "`aoctl login`" + ` first.`,
	}
	s.out = os.Stdout
	s.errw = os.Stderr
	// Interactive-login seams. Tests override these; production uses the real
	// terminal detector / cross-platform browser opener / os.Stdin.
	s.stdin = bufio.NewReader(os.Stdin)
	s.isTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	s.readPassword = term.ReadPassword
	s.openBrowser = defaultOpenBrowser
	root.PersistentFlags().StringVar(&s.endpoint, "endpoint", "", "External Task API base URL (default: $AOCTL_ENDPOINT or http://localhost:8084)") //nolint:lll

	root.PersistentFlags().StringVar(&s.acp, "acp-endpoint", "", "ACP API base URL (default: http://localhost:8000)")
	root.PersistentFlags().StringVar(&s.token, "token", "", "Bearer token (default: saved config)")
	root.PersistentFlags().BoolVar(&s.insecure, "insecure", false, "skip TLS verification (local dev only)")
	root.PersistentFlags().DurationVar(&s.timeout, "timeout", defaultTimeout, "HTTP timeout")
	root.PersistentFlags().BoolVar(&s.jsonOut, "json", false, "emit machine-readable JSON instead of human-readable tables (env: AOCTL_OUTPUT_FORMAT=json)") //nolint:lll
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// Resolve endpoint/acp-token from env or saved config when not given on the flag.
		if s.endpoint == "" {
			s.endpoint = os.Getenv("AOCTL_ENDPOINT")
		}
		if s.acp == "" {
			s.acp = os.Getenv("AOCTL_ACP_ENDPOINT")
		}
		if os.Getenv("AOCTL_OUTPUT_FORMAT") != "" && !root.PersistentFlags().Changed("json") {
			s.jsonOut = strings.EqualFold(os.Getenv("AOCTL_OUTPUT_FORMAT"), "json") ||
				strings.EqualFold(os.Getenv("AOCTL_OUTPUT_FORMAT"), "true")
		}
		tokenFromFlag := s.token != ""
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if s.endpoint == "" {
			s.endpoint = cfg.Endpoint
		}
		if s.acp == "" {
			s.acp = cfg.ACP
		}
		if s.token == "" {
			s.token = cfg.Token
		}
		// Best-effort OIDC session refresh: when the cached token is an OIDC
		// id_token that is expired (or close to it) and a refresh token is
		// available, mint a fresh one. Skipped for `login` (which establishes a
		// new session) and when a token was supplied via --token (untouched).
		// Refresh failures are non-fatal — the API call surfaces a clear 401.
		// NOTE: we read cfg.AuthMethod here (NOT s.authMethod) so a --auth-method
		// flag on the current command is never clobbered by the saved config.
		if cmd.Name() != loginCmd && !tokenFromFlag && s.token != "" &&
			cfg.AuthMethod == loginMethodOIDC && cfg.RefreshToken != "" {
			if fresh, refreshed, rerr := s.refreshOIDCIfNeeded(cfg); rerr != nil {
				_, _ = fmt.Fprintf(s.errw, "warning: OIDC token refresh failed: %v\n", rerr)
			} else if refreshed {
				s.token = fresh
			}
		}
		return nil
	}

	// login
	login := &cobra.Command{
		Use:   "login",
		Short: "Authenticate (OAuth client_credentials or OIDC authorization code) and cache a bearer token",
		Long: `Authenticate to agent-orca and cache a bearer token for subsequent commands.

Two methods are supported:
  oauth — OAuth2 client_credentials exchange (POST /oauth/token). Supplied with
          --client-id + --client-secret, or prompted for interactively.
  oidc  — OIDC authorization-code flow: the CLI opens your browser at the IdP,
          catches the loopback redirect, exchanges the code for an id_token, and
          uses it directly as a bearer token. The id_token is refreshed behind
          the scenes using the refresh token so the session survives its short
          lifetime.

With no --auth-method (and no credentials) aoctl presents an interactive
selection menu — the CLI analogue of the UI's /oauth/login tenant picker.
`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if s.endpoint == "" {
				s.endpoint = defaultEndpoint
			}
			if s.acp == "" {
				s.acp = defaultACP
			}
			return s.runLogin()
		},
	}
	login.Flags().StringVar(&s.authMethod, "auth-method", "", "auth method: 'oauth' (client_credentials) or 'oidc' (authorization code); empty = interactive picker") //nolint:lll
	login.Flags().StringVar(&s.clientID, "client-id", "", "OAuth2 client id (oauth) or OIDC client id (oidc)")
	login.Flags().StringVar(&s.secret, "client-secret", "", "OAuth2 client secret (oauth) or OIDC client secret (oidc); empty is allowed for public clients") //nolint:lll
	login.Flags().StringVar(&s.issuerURL, "issuer-url", "", "OIDC issuer URL (required for --auth-method=oidc)")
	login.Flags().StringVar(&s.redirectURI, "redirect-uri", defaultOIDCRedirectURI, "OIDC loopback callback URL the CLI listens on") //nolint:lll
	login.Flags().BoolVar(&s.noBrowser, "no-browser", false, "print the authorization URL instead of opening a browser")

	// tasks
	tasks := &cobra.Command{Use: "tasks", Short: "Manage agent tasks"}
	submit := &cobra.Command{
		Use:   "submit",
		Short: "Submit a task (POST /v1/tasks)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if s.agent == "" || s.input == "" {
				return errors.New("--agent and --input are required")
			}
			c := s.client()
			tr, code, err := c.SubmitTask(context.Background(), TaskSubmission{
				Agent:   s.agent,
				Input:   s.input,
				Timeout: s.timeoutStr,
			})
			if err != nil {
				return fmt.Errorf("submit task: %s (HTTP %d)", err, code)
			}
			if s.stream && tr.Links != nil && tr.Links.Stream != "" {
				return streamAndPrint(context.Background(), c, tr.ID, s.out, s.errw)
			}
			return printTask(s.out, tr)
		},
	}
	submit.Flags().StringVar(&s.agent, "agent", "", "agent name to invoke (required)")
	submit.Flags().StringVar(&s.input, "input", "", "task input (required)")
	submit.Flags().BoolVar(&s.stream, "stream", false, "stream SSE events after submitting")
	submit.Flags().StringVar(&s.timeoutStr, "timeout", "", "task timeout (e.g. 5m)")

	list := &cobra.Command{
		Use:   "ls",
		Short: "List tasks",
		RunE: func(_ *cobra.Command, _ []string) error {
			c := s.client()
			tasks, err := c.ListTasks(context.Background(), s.agent, s.status)
			if err != nil {
				return err
			}
			return s.render(tasks, func(w io.Writer) error {
				if len(tasks) == 0 {
					_, _ = fmt.Fprintln(w, "No tasks found.")
					return nil
				}
				tw := newTableWriter().header("ID", "AGENT", "STATUS")
				for _, t := range tasks {
					tw.row(t.ID, t.Agent, t.Status)
				}
				return tw.render(w)
			})
		},
	}
	list.Flags().StringVar(&s.agent, "agent", "", "filter by agent")
	list.Flags().StringVar(&s.status, "status", "", "filter by status")

	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a task by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := s.client()
			tr, err := c.GetTask(context.Background(), args[0])
			if err != nil {
				return err
			}
			return printTask(s.out, tr)
		},
	}

	cancel := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := s.client()
			if err := c.CancelTask(context.Background(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(s.out, "cancelled")
			return nil
		},
	}
	wait := &cobra.Command{
		Use:   "wait <id>",
		Short: "Stream a task to completion",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return streamAndPrint(context.Background(), s.client(), args[0], s.out, s.errw)
		},
	}

	tasks.AddCommand(submit, list, get, cancel, wait)

	// agents
	agents := &cobra.Command{Use: "agents", Short: "Discover agents (ACP API)"}
	agents.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List agents available to your tenant",
		RunE: func(_ *cobra.Command, _ []string) error {
			agentList, err := s.client().ListAgents(context.Background())
			if err != nil {
				return err
			}
			return s.render(agentList, func(w io.Writer) error {
				if len(agentList) == 0 {
					_, _ = fmt.Fprintln(w, "No agents found.")
					return nil
				}
				tw := newTableWriter().header("NAME", "NAMESPACE")
				for _, a := range agentList {
					tw.row(a.Name, a.Namespace)
				}
				return tw.render(w)
			})
		},
	})

	agents.AddCommand(&cobra.Command{
		Use:   "describe <name>",
		Short: "Show the manifest (schema, tools, KBs) for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manifest, err := s.client().GetAgentManifest(context.Background(), args[0])
			if err != nil {
				return err
			}
			return s.render(manifest, func(w io.Writer) error {
				return printAgentManifestHuman(w, manifest)
			})
		},
	})

	agentRun := &cobra.Command{
		Use:   "run <name>",
		Short: "Run an agent (POST /agents/{name}/run)",
		Long: `Launch a run against an ACP agent and get a run ID back.

The agent name is the first positional argument.  The task input can be
supplied three ways (checked in this order):

  1. --input <text> … inline text.                  (most common)
  2. --file <path>  … read input from a file.
  3. stdin         … when --input is omitted and stdin is NOT a terminal,
                       the command reads piped content.

The input is wrapped as an ACP "user" message with a single text part whose
content-type defaults to text/plain (override with --content-type).  The run is
created asynchronously: the server returns a run_id immediately that you can
poll with GET /runs/{run_id} or stream with GET /runs/{run_id}/events.

For conversation continuity, pass the same --session-id across successive
calls so context chains.

Examples:

  # Simple one-liner
  aoctl agents run support-bot --input "How do I reset my password?"

  # Read the prompt from a file
  aoctl agents run support-bot --file prompt.txt

  # Pipe input from another command
  cat alert.txt | aoctl agents run soc-enricher-agent

  # Chain a conversation
  aoctl agents run support-bot --input "Hello, I'm Matthew" --session-id chat-1
  aoctl agents run support-bot --input "Follow up please"  --session-id chat-1

  # Machine-readable output
  aoctl agents run support-bot --input "hi" --json
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentName := args[0]

			inputText, err := resolveRunInput(s, cmd)
			if err != nil {
				return err
			}
			if inputText == "" {
				return fmt.Errorf("--input, --file, or piped stdin is required for `aoctl agents run %s`", agentName) //nolint:lll
			}

			req := ACPRunRequest{
				Input: []ACPMessage{{
					Role: "user",
					Parts: []ACPMessagePart{{
						ContentType: s.contentType,
						Content:     inputText,
					}},
				}},
			}
			if s.sessionID != "" {
				req.SessionID = s.sessionID
			}

			resp, err := s.client().CreateAgentRun(context.Background(), agentName, req)
			if err != nil {
				return err
			}
			return s.render(resp, func(w io.Writer) error {
				return printAgentRunHuman(w, resp)
			})
		},
	}
	agentRun.Flags().StringVar(&s.input, "input", "", "task input (inline text)")
	agentRun.Flags().StringVar(&s.file, "file", "", "read input from a file (alternative to --input)")
	agentRun.Flags().StringVar(&s.sessionID, "session-id", "", "session ID for conversation continuity")
	agentRun.Flags().StringVar(&s.contentType, "content-type", "text/plain", "MIME type of the input (e.g. text/plain, application/json)") //nolint:lll
	agents.AddCommand(agentRun)

	root.AddCommand(login, tasks, agents)

	// admin — tenant lifecycle management (requires a K8s SA token, not OAuth2).
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Admin operations (requires a Kubernetes ServiceAccount token)",
		Long: `Admin operations target the /admin/* surface on the External Task API.
These require a Kubernetes ServiceAccount bearer token (not an OAuth2 client_credentials JWT).
Obtain one via 'kubectl create token agentorca-admin -n agent-orca-system' or the
--admin-bootstrap-token operator flag.`,
	}
	adminTenants := &cobra.Command{Use: "tenants", Short: "Manage tenants"}

	adminList := &cobra.Command{
		Use:   "list",
		Short: "List all tenants (GET /admin/tenants)",
		RunE: func(_ *cobra.Command, _ []string) error {
			c := s.client()
			tenants, err := c.ListTenants(context.Background())
			if err != nil {
				return err
			}
			return s.render(tenants, func(w io.Writer) error {
				if len(tenants) == 0 {
					_, _ = fmt.Fprintln(w, "No tenants found.")
					return nil
				}
				tw := newTableWriter().header("NAME", "CLIENT ID", "NAMESPACE")
				for _, t := range tenants {
					tw.row(t.Name, t.ClientID, t.TargetNamespace)
				}
				return tw.render(w)
			})
		},
	}

	adminGet := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a tenant by name (GET /admin/tenants/{name})",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := s.client()
			t, err := c.GetTenant(context.Background(), args[0])
			if err != nil {
				return err
			}
			return printJSON(s.out, t)
		},
	}

	adminCreate := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a tenant (POST /admin/tenants)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if s.targetNamespace == "" || s.clientID == "" {
				return errors.New("--namespace and --client-id are required for create")
			}
			req := AdminTenantCreateRequest{
				Name:            args[0],
				TargetNamespace: s.targetNamespace,
				ClientID:        s.clientID,
			}
			if s.allowedAgents != nil {
				req.AllowedAgents = s.allowedAgents
			}
			if s.budgetPerDay != "" {
				req.BudgetPerDayUSD = s.budgetPerDay
			}
			if s.rpm > 0 || s.concurrent > 0 {
				req.RateLimit = &AdminRateLimit{
					RequestsPerMinute: s.rpm,
					ConcurrentRuns:    s.concurrent,
				}
			}
			c := s.client()
			resp, err := c.CreateTenant(context.Background(), req)
			if err != nil {
				return err
			}
			return printJSON(s.out, resp)
		},
	}
	adminCreate.Flags().StringVar(&s.targetNamespace, "namespace", "", "target namespace for the tenant's agents")
	adminCreate.Flags().StringVar(&s.clientID, "client-id", "", "OAuth2 client ID")
	adminCreate.Flags().StringSliceVar(&s.allowedAgents, "allowed-agents", nil, "comma-separated list of allowed agent names") //nolint:lll

	adminCreate.Flags().IntVar(&s.rpm, "rpm", 0, "max task submissions per minute (0 = unlimited)")
	adminCreate.Flags().IntVar(&s.concurrent, "concurrent", 0, "max concurrent runs (0 = unlimited)")
	adminCreate.Flags().StringVar(&s.budgetPerDay, "budget", "", "daily budget in USD (e.g. 100.00)")

	adminRotate := &cobra.Command{
		Use:   "rotate-secret <name>",
		Short: "Rotate a tenant's client secret (POST /admin/tenants/{name}/rotate-secret)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := s.client()
			secret, err := c.RotateTenantSecret(context.Background(), args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(s.out, secret)
			return nil
		},
	}

	adminDelete := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a tenant (DELETE /admin/tenants/{name})",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := s.client()
			if err := c.DeleteTenant(context.Background(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(s.out, "deleted")
			return nil
		},
	}

	adminTenants.AddCommand(adminList, adminGet, adminCreate, adminRotate, adminDelete)
	admin.AddCommand(adminTenants)
	root.AddCommand(admin)

	// ACP bridge: expose agent-orca agents to editors (Zed/ACP) over stdio.
	root.AddCommand(newACPCommand(s))

	return root, s
}

func streamAndPrint(ctx context.Context, c *Client, id string, out, errw io.Writer) error {
	ch, err := c.StreamTask(ctx, id)
	if err != nil {
		return fmt.Errorf("streaming: %w", err)
	}
	for ev := range ch {
		switch ev.Type {
		case "token":
			_, _ = fmt.Fprint(out, ev.Data)
		case "complete":
			var tr TaskResponse
			if json.Unmarshal([]byte(ev.Data), &tr) == nil {
				_ = printTask(out, tr)
			}
		case "status":
			var m map[string]string
			if json.Unmarshal([]byte(ev.Data), &m) == nil {
				if p, ok := m["phase"]; ok {
					_, _ = fmt.Fprintf(errw, "\rphase: %-12s", p)
				}
			}
		case "error":
			return errors.New("stream error: " + ev.Data)
		}
	}
	_, _ = fmt.Fprintln(errw)
	return nil
}

func printTask(w io.Writer, tr TaskResponse) error {
	out, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(w, string(out))
	return nil
}

// resolveRunInput determines the task input text from --input, --file, or piped
// stdin.  It returns an error when the combination is ambiguous or when stdin
// is a terminal (i.e. the user forgot to supply input interactively).
func resolveRunInput(s *settings, cmd *cobra.Command) (string, error) {
	inputSet := cmd.Flags().Changed("input")
	fileSet := cmd.Flags().Changed("file")

	if inputSet && fileSet {
		return "", errors.New("--input and --file are mutually exclusive; use one or the other")
	}

	if s.input != "" {
		return s.input, nil
	}

	if s.file != "" {
		b, err := os.ReadFile(s.file)
		if err != nil {
			return "", fmt.Errorf("reading input file %q: %w", s.file, err)
		}
		return string(b), nil
	}

	// Neither flag was set — try stdin (works for `echo ... | aoctl agents run ...`).
	if s.stdin != nil && !s.isTerminal() {
		data, err := io.ReadAll(s.stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		text := strings.TrimSpace(string(data))
		if text != "" {
			return text, nil
		}
	}

	// Fall through: no input anywhere.
	return "", nil
}

// printAgentRunHuman renders an agent-run response in a friendly, multi-line
// format rather than raw JSON.
func printAgentRunHuman(w io.Writer, resp ACPRunResponse) error {
	_, _ = fmt.Fprintln(w, "✓ Run created")
	_, _ = fmt.Fprintf(w, "  Agent:     %s\n", resp.AgentName)
	_, _ = fmt.Fprintf(w, "  Run ID:    %s\n", resp.RunID)
	_, _ = fmt.Fprintf(w, "  Status:    %s\n", resp.Status)
	_, _ = fmt.Fprintf(w, "  Created:   %s\n", resp.CreatedAt)
	if resp.SessionID != "" {
		_, _ = fmt.Fprintf(w, "  Session:   %s\n", resp.SessionID)
	}
	if resp.FinishedAt != "" {
		_, _ = fmt.Fprintf(w, "  Finished:  %s\n", resp.FinishedAt)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "  Poll:  aoctl tasks get %s  (External Task API)\n", resp.RunID)
	_, _ = fmt.Fprintf(w, "  Or query: GET /runs/%s  (ACP API on --acp-endpoint)\n", resp.RunID)
	return nil
}

// printAgentManifestHuman renders an ACP agent manifest in a human-readable form:
// a header (name + namespace) followed by labelled sections (content types,
// tools, knowledge bases, guardrails, clarify) and pretty-printed schemas.
func printAgentManifestHuman(w io.Writer, m ACPAgentManifest) error {
	_, _ = fmt.Fprintf(w, "Name:        %s\n", m.Name)
	if m.Namespace != "" {
		_, _ = fmt.Fprintf(w, "Namespace:   %s\n", m.Namespace)
	}
	_, _ = fmt.Fprintf(w, "Description: %s\n", m.Description)
	_, _ = fmt.Fprintln(w)

	if len(m.InputContentTypes) > 0 || len(m.OutputContentTypes) > 0 {
		_, _ = fmt.Fprintf(w, "Input content types:  %s\n", strings.Join(m.InputContentTypes, ", "))
		_, _ = fmt.Fprintf(w, "Output content types: %s\n\n", strings.Join(m.OutputContentTypes, ", "))
	}

	if len(m.AllowedTools) > 0 {
		_, _ = fmt.Fprintln(w, "Tools:")
		tw := newTableWriter().header("NAME", "DESCRIPTION")
		for _, t := range m.AllowedTools {
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			tw.row(t.Name, desc)
		}
		if err := tw.render(w); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w)
	}

	if len(m.KnowledgeBases) > 0 {
		_, _ = fmt.Fprintf(w, "Knowledge bases: %s\n\n", strings.Join(m.KnowledgeBases, ", "))
	}
	if m.GuardrailPolicy != "" {
		_, _ = fmt.Fprintf(w, "Guardrail policy: %s\n\n", m.GuardrailPolicy)
	}
	_, _ = fmt.Fprintf(w, "Clarify available: %t\n", m.ClarifyAvailable)

	if err := printSchema(w, "Input schema", m.InputSchema); err != nil {
		return err
	}
	if err := printSchema(w, "Output schema", m.OutputSchema); err != nil {
		return err
	}
	return nil
}

// printSchema pretty-prints a JSON schema map under a label, skipping empty maps.
func printSchema(w io.Writer, label string, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "%s:\n%s\n", label, string(b))
	return nil
}

// printJSON marshals v as indented JSON and writes it to w.
func printJSON(w io.Writer, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(w, string(out))
	return nil
}

// render prints v as JSON when --json is requested, otherwise delegates to the
// human-readable formatter. It is the single switch every list/describe command
// uses to support machine-readable output (note 3).
func (s *settings) render(v any, human func(io.Writer) error) error {
	if s.jsonOut {
		return printJSON(s.out, v)
	}
	return human(s.out)
}

// Execute runs the root command.
func Execute() error {
	c, _ := newRootCmd()
	return c.Execute()
}

func bodyReader(b []byte) io.Reader { return strings.NewReader(string(b)) }

// insecureTransport is a minimal TLS-insecure round tripper for local dev.
func insecureTransport() http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig.InsecureSkipVerify = true
	return base
}

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
