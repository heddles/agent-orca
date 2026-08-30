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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// This file holds the ACP JSON-RPC wire types that Zed (and other ACP clients)
// send over stdio, the conversions between ACP content blocks and agent-orca's
// ACPMessage part model, and the Client methods that hit agent-orca's ACP HTTP
// surface for run status / event streaming / cancellation.
//
// The ACP protocol types here mirror https://agentclientprotocol.com/protocol/v1/.
// Capability blocks are kept loose (json.RawMessage where the spec is still
// evolving) so a small mismatch doesn't prevent the core prompt flow, which is
// stable.

// errNotStreaming is returned when the ACP HTTP API does not yield an SSE
// stream (e.g. the run is not in-progress yet, or no state store is configured).
// Callers fall back to polling GetACPRun.
var errNotStreaming = errors.New("not streaming")

// ACP run-status string constants — mirrors the server's ACPRunStatus enum
// (internal/apiserver/acp_api.go). Used by the bridge to interpret poll results.
const (
	acpStatusCreated    = "created" //nolint:unused
	acpStatusInProgress = "in-progress"
	acpStatusAwaiting   = "awaiting"
	acpStatusCancelling = "cancelling" //nolint:unused
	acpStatusCancelled  = "cancelled"
	acpStatusCompleted  = "completed"
	acpStatusFailed     = "failed"
)

// ACP JSON-RPC protocol version string used in every message envelope.
const acpJSONRPCVersion = "2.0"

// ACP stop-reason constants returned in session/prompt (PromptResponse)
// results. Per the ACP spec
// (https://agentclientprotocol.com/protocol/v1/prompt-turn#stop-reasons)
// the stopReason field MUST be one of the StopReason enum:
//
//	end_turn | max_tokens | max_turn_requests | refusal | cancelled
//
// agent-orca run statuses that have no direct StopReason equivalent —
// "awaiting" (waiting for the user, e.g. via _clarify) and "failed" — are
// mapped to end_turn. The awaiting state is additionally surfaced to the
// client through a session/update notification (see acpStatusAwaiting) so the
// client can prompt the user for input even though the turn itself has ended.
const (
	acpStopReasonEndTurn    = "end_turn"
	acpStopReasonUserCancel = "cancelled"
)

// ACP JSON-RPC notification method names.
const (
	acpMethodSessionUpdate = "session/update"
)

// ----------------------------------------------------------------------------
// ACP JSON-RPC content blocks (client -> server)
// ----------------------------------------------------------------------------

// acpContentBlock is one element of a session/prompt "prompt" array.
type acpContentBlock struct {
	Type     string `json:"type"` // "text" | "image" | "resource" | "audio"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64 for image/audio
	URI      string `json:"uri,omitempty"`  // content_url / resource uri
	MimeType string `json:"mimeType,omitempty"`
}

// acpPromptParams is the params of a session/prompt request.
type acpPromptParams struct {
	SessionID string            `json:"sessionId"`
	Prompt    []acpContentBlock `json:"prompt"`
}

// acpPromptResponse is the result of a session/prompt request.
type acpPromptResponse struct {
	StopReason string `json:"stopReason"`
}

// acpSessionNewParams is the params of a session/new request.
type acpSessionNewParams struct {
	SessionID    string `json:"sessionId,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// acpSessionNewResponse is the result of a session/new request.
type acpSessionNewResponse struct {
	SessionID string `json:"sessionId"`
}

// acpInitializeParams is the params of an initialize request.
type acpInitializeParams struct {
	ProtocolVersion    uint16          `json:"protocolVersion"`
	ClientInfo         json.RawMessage `json:"clientInfo,omitempty"`
	ClientCapabilities json.RawMessage `json:"clientCapabilities,omitempty"`
}

// acpInitializeResponse negotiates the connection and advertises capabilities.
// Field names follow ACP v1 (agentCapabilities / agentInfo / authMethods).
// ProtocolVersion is a uint16 per the ACP spec — a single integer identifying a
// MAJOR protocol version (currently 1), NOT a semver string.
type acpInitializeResponse struct {
	ProtocolVersion   uint16            `json:"protocolVersion"`
	AgentCapabilities acpAgentCaps      `json:"agentCapabilities"`
	AgentInfo         acpImplementation `json:"agentInfo,omitempty"`
	AuthMethods       []any             `json:"authMethods,omitempty"` // empty => pre-authenticated
}

type acpAgentCaps struct {
	LoadSession         bool           `json:"loadSession"`
	PromptCapabilities  acpPromptCaps  `json:"promptCapabilities"`
	MCPCapabilities     acpMCPCaps     `json:"mcpCapabilities"`
	SessionCapabilities map[string]any `json:"sessionCapabilities"`
	Auth                map[string]any `json:"auth"`
}

type acpPromptCaps struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type acpMCPCaps struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type acpImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// acpUpdateParams is a server->client session/update notification.
type acpUpdateParams struct {
	SessionID string    `json:"sessionId"`
	Update    acpUpdate `json:"update"`
}

type acpUpdate struct {
	SessionUpdate string           `json:"sessionUpdate"` // "plan" | "agent_message_chunk" | "tool_call" | ...
	MessageID     string           `json:"messageId,omitempty"`
	Content       *acpContentBlock `json:"content,omitempty"`
	PlanEntries   []acpPlanEntry   `json:"entries,omitempty"` // used when SessionUpdate == "plan"
}

type acpPlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"` // "low" | "medium" | "high"
	Status   string `json:"status,omitempty"`   // "pending" | "in-progress" | "completed"
}

// ----------------------------------------------------------------------------
// agent-orca server-mirror types (for GET /runs/{id})
// ----------------------------------------------------------------------------

// acpRun mirrors internal/apiserver.ACPRun for the GET /runs/{run_id} response.
type acpRun struct {
	AgentName  string       `json:"agent_name"`
	SessionID  string       `json:"session_id,omitempty"`
	RunID      string       `json:"run_id"`
	Status     string       `json:"status"`
	Output     []ACPMessage `json:"output,omitempty"`
	Error      *acpErr      `json:"error,omitempty"`
	CreatedAt  string       `json:"created_at"`
	FinishedAt string       `json:"finished_at,omitempty"`
}

type acpErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ----------------------------------------------------------------------------
// Conversions
// ----------------------------------------------------------------------------

// toAgentOrcaInput converts ACP prompt content blocks into agent-orca's input
// message model: a single user message whose parts carry content_type + content.
func toAgentOrcaInput(blocks []acpContentBlock) []ACPMessage {
	if len(blocks) == 0 {
		return []ACPMessage{{Role: "user", Parts: []ACPMessagePart{{ContentType: "text/plain", Content: ""}}}}
	}
	parts := make([]ACPMessagePart, 0, len(blocks))
	for _, b := range blocks {
		ct := b.MimeType
		if ct == "" {
			ct = "text/plain"
		}
		switch b.Type {
		case "image", "audio", "resource":
			// Best-effort: pass content/url through. agent-orca collapses parts to
			// text for the model-router, so non-text blocks arrive as a string.
			parts = append(parts, ACPMessagePart{ContentType: ct, Content: b.Data, ContentURL: b.URI})
		default: // "text" or unknown -> treat as text
			parts = append(parts, ACPMessagePart{ContentType: "text/plain", Content: b.Text})
		}
	}
	return []ACPMessage{{Role: "user", Parts: parts}}
}

// runStatusToStopReason maps an agent-orca ACP run status to a valid ACP
// StopReason. The ACP StopReason enum (end_turn | max_tokens |
// max_turn_requests | refusal | cancelled) has no entry for "awaiting" or
// "failed", so both map to end_turn. The awaiting state is signalled to the
// client separately via a session/update notification; failed runs surface
// their diagnostics through message chunks.
func runStatusToStopReason(status string) string {
	switch status {
	case acpStatusCompleted, acpStatusFailed, acpStatusAwaiting:
		return acpStopReasonEndTurn
	case acpStatusCancelled, acpStatusCancelling:
		return acpStopReasonUserCancel
	default:
		return acpStopReasonEndTurn
	}
}

// ----------------------------------------------------------------------------
// Client methods: GET /runs/{id}, SSE stream, and cancel
// ----------------------------------------------------------------------------

// GetACPRun fetches a run's current status (and output, once terminal).
func (c *Client) GetACPRun(ctx context.Context, runID string) (acpRun, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/runs/%s", strings.TrimRight(c.ACP, "/"), url.PathEscape(runID)), nil)
	if err != nil {
		return acpRun{}, err
	}
	req.Header.Set("Accept", "application/json")
	c.tokenAuth(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return acpRun{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var run acpRun
	if err := decodeJSON(resp, &run, http.StatusOK); err != nil {
		return acpRun{}, err
	}
	return run, nil
}

// StreamRunEvents streams the live ACP run events as SSE. Only valid while the
// run is in the "in-progress" phase (the server returns JSON otherwise, and
// 503s when no state store is configured). Mirrors StreamTask's scanner.
func (c *Client) StreamRunEvents(ctx context.Context, runID string) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/runs/%s", strings.TrimRight(c.ACP, "/"), url.PathEscape(runID)), nil)
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
		return nil, fmt.Errorf("stream run events failed (HTTP %d)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		_ = resp.Body.Close()
		// Server handed back JSON (run not running yet, or no state store).
		// Caller falls back to polling GetACPRun.
		return nil, errNotStreaming
	}

	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
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

// CancelACPRun cancels a run via POST /runs/{run_id}/cancel.
func (c *Client) CancelACPRun(ctx context.Context, runID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/runs/%s/cancel", strings.TrimRight(c.ACP, "/"), url.PathEscape(runID)), nil)
	if err != nil {
		return err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel run %q failed (HTTP %d)", runID, resp.StatusCode)
	}
	return nil
}
