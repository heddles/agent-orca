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
// for agent discovery. Both accept the same bearer token (issued via the
// /oauth/token client_credentials exchange), so a single `aoctl login` covers
// both surfaces.
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
		Endpoint: strings.TrimRight(endpoint, "/"),
		ACP:      strings.TrimRight(acp, "/"),
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
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return TaskResponse{}, resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(respBody)))
	}
	var tr TaskResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return TaskResponse{}, resp.StatusCode, fmt.Errorf("decoding task response: %w", err)
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
	if resp.StatusCode != http.StatusOK {
		return TaskResponse{}, fmt.Errorf("get task failed (HTTP %d)", resp.StatusCode)
	}
	var tr TaskResponse
	return tr, json.NewDecoder(resp.Body).Decode(&tr)
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list tasks failed (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Tasks []TaskResponse `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding task list: %w", err)
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list agents failed (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Agents []ACPAgentManifest `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding agents list: %w", err)
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
	if resp.StatusCode != http.StatusOK {
		return ACPAgentManifest{}, fmt.Errorf("get agent manifest failed (HTTP %d)", resp.StatusCode)
	}
	var m ACPAgentManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return ACPAgentManifest{}, fmt.Errorf("decoding manifest: %w", err)
	}
	return m, nil
}

// ACPRunRequest is the body for POST /agents/{name}/run.
type ACPRunRequest struct {
	Input     []ACPMessagePart `json:"input"`
	SessionID string           `json:"session_id,omitempty"`
}

// ACPMessagePart is a single part of an ACP message.
type ACPMessagePart struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ACPRunResponse is the response from POST /agents/{name}/run.
type ACPRunResponse struct {
	AgentName string `json:"agent_name"`
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
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
		respBody, _ := io.ReadAll(resp.Body)
		return ACPRunResponse{}, fmt.Errorf("create agent run failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody))) //nolint:lll

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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list tenants failed (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Tenants []AdminTenantResponse `json:"tenants"`
		Count   int                   `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding tenants list: %w", err)
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
	if resp.StatusCode != http.StatusOK {
		return AdminTenantResponse{}, fmt.Errorf("get tenant failed (HTTP %d)", resp.StatusCode)
	}
	var out AdminTenantResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AdminTenantResponse{}, fmt.Errorf("decoding get tenant response: %w", err)
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
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rotate secret failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decoding rotate response: %w", err)
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
func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{Endpoint: defaultEndpoint, ACP: defaultACP}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.ACP == "" {
		cfg.ACP = defaultACP
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
	out, errw       io.Writer
}

func (s *settings) client() *Client {
	return newClient(s.endpoint, s.acp, s.token, s.timeout, s.insecure)
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
	root.PersistentFlags().StringVar(&s.endpoint, "endpoint", "", "External Task API base URL (default: $AOCTL_ENDPOINT or http://localhost:8084)") //nolint:lll

	root.PersistentFlags().StringVar(&s.acp, "acp-endpoint", "", "ACP API base URL (default: http://localhost:8000)")
	root.PersistentFlags().StringVar(&s.token, "token", "", "Bearer token (default: saved config)")
	root.PersistentFlags().BoolVar(&s.insecure, "insecure", false, "skip TLS verification (local dev only)")
	root.PersistentFlags().DurationVar(&s.timeout, "timeout", defaultTimeout, "HTTP timeout")
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		// Resolve endpoint/acp-token from env or saved config when not given on the flag.
		if s.endpoint == "" {
			s.endpoint = os.Getenv("AOCTL_ENDPOINT")
		}
		if s.acp == "" {
			s.acp = os.Getenv("AOCTL_ACP_ENDPOINT")
		}
		if s.endpoint == "" || s.token == "" || s.acp == "" {
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
		}
		return nil
	}

	// login
	login := &cobra.Command{
		Use:   "login",
		Short: "Exchange client credentials for a bearer token and cache it",
		RunE: func(_ *cobra.Command, _ []string) error {
			if s.clientID == "" || s.secret == "" {
				return errors.New("--client-id and --client-secret are required")
			}
			if s.endpoint == "" {
				s.endpoint = defaultEndpoint
			}
			c := newClient(s.endpoint, s.acp, "", s.timeout, s.insecure)
			tok, err := c.Login(context.Background(), s.clientID, s.secret)
			if err != nil {
				return err
			}
			s.token = tok
			if s.acp == "" {
				s.acp = defaultACP
			}
			if err := saveConfig(&Config{Endpoint: s.endpoint, ACP: s.acp, Token: tok}); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(s.out, "logged in to", s.endpoint)
			return nil
		},
	}
	login.Flags().StringVar(&s.clientID, "client-id", "", "OAuth2 client id")
	login.Flags().StringVar(&s.secret, "client-secret", "", "OAuth2 client secret")

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
			for _, t := range tasks {
				_ = printTask(s.out, t)
			}
			return nil
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
			for _, a := range agentList {
				_, _ = fmt.Fprintf(s.out, "%-30s %s\n", a.Name, a.Description)
			}
			return nil
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
			return printJSON(s.out, manifest)
		},
	})

	agentRun := &cobra.Command{
		Use:   "run <name>",
		Short: "Run an agent (POST /agents/{name}/run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if s.input == "" {
				return errors.New("--input is required")
			}
			req := ACPRunRequest{
				Input: []ACPMessagePart{{Role: "user", Content: s.input}},
			}
			if s.sessionID != "" {
				req.SessionID = s.sessionID
			}
			resp, err := s.client().CreateAgentRun(context.Background(), args[0], req)
			if err != nil {
				return err
			}
			return printJSON(s.out, resp)
		},
	}
	agentRun.Flags().StringVar(&s.input, "input", "", "task input (required)")
	agentRun.Flags().StringVar(&s.sessionID, "session-id", "", "session ID for conversation continuity")
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
			for _, t := range tenants {
				_, _ = fmt.Fprintf(s.out, "%-30s %-20s %s\n", t.Name, t.ClientID, t.TargetNamespace)
			}
			return nil
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

// printJSON marshals v as indented JSON and writes it to w.
func printJSON(w io.Writer, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(w, string(out))
	return nil
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
