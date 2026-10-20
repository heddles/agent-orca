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

// Package acpfs implements the relay that lets an agent pod drive the user's
// workstation filesystem through an ACP bridge ("pod drives the workstation",
// issue #34).
//
// The flow is a request/response mailbox scoped to "<namespace>:<run>":
//
//  1. The model-router sidecar executes the built-in tools fs_read_text_file /
//     fs_write_text_file by POSTing an fs method call ("fs/read_text_file" or
//     "fs/write_text_file", exactly as defined by ACP v1's agent→client
//     filesystem surface) to the operator's internal API, which Enqueues it.
//  2. The `aoctl acp serve` bridge on the workstation polls the ACP API for
//     Pending requests for the run it is watching, forwards each one to the
//     ACP client (e.g. Zed) over the stdio JSON-RPC channel, and Responds with
//     whatever the client returned.
//  3. The router takes the response (Take) and returns it as the tool result.
//
// The relay carries only ephemeral request/response state. Workspace
// confinement and write approval are enforced in the bridge (the editor
// remains the arbiter of access), while whether the tools exist at all is
// decided in-cluster by Agent.spec.filesystem.
package acpfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Relayable method names. fs/read_text_file and fs/write_text_file are the
// ACP v1 agent→client filesystem methods and are forwarded to the editor.
// fs/list_dir is an agent-orca extension: ACP v1 defines no directory-listing
// method, so the bridge answers it locally (confined to the same workspace
// roots) instead of forwarding it.
const (
	MethodReadTextFile  = "fs/read_text_file"
	MethodWriteTextFile = "fs/write_text_file"
	MethodListDir       = "fs/list_dir"
)

// Request is a single fs/* method call from an agent pod awaiting a
// workstation-side (bridge) response.
type Request struct {
	// ID is the relay-generated request identifier, unique per scope.
	ID string `json:"id"`
	// Method is the ACP agent→client method name.
	Method string `json:"method"`
	// Params are the method parameters exactly as defined by the ACP v1
	// schema (e.g. {"path": "...", "line": 1, "limit": 100} for
	// fs/read_text_file, {"path": "...", "content": "..."} for
	// fs/write_text_file).
	Params json.RawMessage `json:"params"`
	// CreatedAt is when the request was enqueued.
	CreatedAt time.Time `json:"createdAt"`
}

// ResponseError mirrors the JSON-RPC error object an ACP client can return.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Response is the bridge's answer for a given request: at most one of Result
// and Error is set (both empty means the call returned no result, which is
// valid for fs/write_text_file).
type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ResponseError  `json:"error,omitempty"`
}

// EnqueueRequest is the payload the model-router POSTs to enqueue an fs call.
type EnqueueRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ErrUnavailable is returned by relay operations when no relay is configured
// (e.g. the deployment runs without a shared state store). API handlers
// surface it as a 503 so callers fail fast into a clear error.
var ErrUnavailable = errors.New("filesystem relay not configured on this deployment")

// Relay carries fs/* requests from agent pods to the workstation-side bridge
// and their responses back. All operations are scoped per "<ns>:<run>".
type Relay interface {
	// Enqueue registers a pending flow request and returns it with a fresh ID.
	Enqueue(ctx context.Context, scope string, req EnqueueRequest) (*Request, error)
	// Pending lists all requests still awaiting a response for the scope.
	Pending(ctx context.Context, scope string) ([]Request, error)
	// Respond stores the response for a request and clears the pending entry.
	// Returns an error if the request no longer exists (expired).
	Respond(ctx context.Context, scope, requestID string, resp *Response) error
	// Take atomically fetches and removes the response for a request.
	// Returns (nil, nil) while the response has not yet arrived but the
	// request still exists. Returns an error once the request itself has
	// disappeared (expired) so callers give up instead of polling forever.
	Take(ctx context.Context, scope, requestID string) (*Response, error)
}

// RelayTTL bounds every relay key. It must comfortably exceed the longest fs
// tool wait (bridge polling plus possible user approval in the editor) while
// ensuring abandoned requests self-expire. 30 minutes is generous next to the
// router's default fs tool timeout (5 minutes) and runs shorter than that.
const RelayTTL = 30 * time.Minute

// StateConfig is the subset of internal/state.Config the relay needs, kept
// local so the relay package stays independent of the state store.
type StateConfig struct {
	Backend  string
	RedisURL string
}

// New builds a Relay from the state-store configuration. A redis backend
// shares relay state across apiserver replicas and across restarts; the empty
// backend returns (nil, nil) meaning "filesystem relay unavailable", which
// API handlers report as a 503.
func New(cfg StateConfig) (Relay, error) {
	switch cfg.Backend {
	case "redis":
		url := cfg.RedisURL
		if url == "" {
			url = "redis://localhost:6379"
		}
		opts, err := redis.ParseURL(url)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %w", err)
		}
		client := redis.NewClient(opts)
		if err := client.Ping(context.Background()).Err(); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("connecting to redis for filesystem relay: %w", err)
		}
		return NewRedisRelay(client), nil
	default:
		return nil, nil
	}
}

// redisRelay implements Relay on top of Redis. Every key carries RelayTTL so
// abandoned requests/responses self-expire.
type redisRelay struct {
	client *redis.Client
}

// NewRedisRelay wraps an existing redis client so deployments can share the
// state store's connection pool.
func NewRedisRelay(client *redis.Client) Relay {
	return &redisRelay{client: client}
}

const (
	redisPendingPrefix  = "acpfs:req:"  // acpfs:req:<scope>:<id>
	redisResponsePrefix = "acpfs:resp:" // acpfs:resp:<scope>:<id>
)

func pendingKey(scope, id string) string  { return redisPendingPrefix + scope + ":" + id }
func responseKey(scope, id string) string { return redisResponsePrefix + scope + ":" + id }

func (r *redisRelay) Enqueue(ctx context.Context, scope string, in EnqueueRequest) (*Request, error) {
	if r.client == nil {
		return nil, ErrUnavailable
	}
	req := Request{
		ID:        uuid.NewString(),
		Method:    in.Method,
		Params:    in.Params,
		CreatedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling fs request: %w", err)
	}
	if err := r.client.Set(ctx, pendingKey(scope, req.ID), body, RelayTTL).Err(); err != nil {
		return nil, fmt.Errorf("storing fs request: %w", err)
	}
	return &req, nil
}

func (r *redisRelay) Pending(ctx context.Context, scope string) ([]Request, error) {
	if r.client == nil {
		return nil, ErrUnavailable
	}
	pattern := redisPendingPrefix + scope + ":*"
	// The working set is one (or a few racing) open fs tool calls per run, so
	// KEYS cost is proportional to live requests rather than total history.
	raws, err := r.client.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("listing fs requests: %w", err)
	}
	out := make([]Request, 0, len(raws))
	for _, raw := range raws {
		body, err := r.client.Get(ctx, raw).Bytes()
		if err != nil {
			continue // expired between KEYS and GET
		}
		var req Request
		if err := json.Unmarshal(body, &req); err != nil {
			continue // corrupt entry: skip rather than fail the poll
		}
		out = append(out, req)
	}
	return out, nil
}

func (r *redisRelay) Respond(ctx context.Context, scope, requestID string, resp *Response) error {
	if r.client == nil {
		return ErrUnavailable
	}
	// Persist the response only while the request still exists: an expired
	// request must not accumulate an orphaned response key.
	exists, err := r.client.Exists(ctx, pendingKey(scope, requestID)).Result()
	if err != nil {
		return fmt.Errorf("checking fs request: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("fs request %q not found (expired)", requestID)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshalling fs response: %w", err)
	}
	tx := r.client.TxPipeline()
	tx.Set(ctx, responseKey(scope, requestID), body, RelayTTL)
	tx.Del(ctx, pendingKey(scope, requestID))
	if _, err := tx.Exec(ctx); err != nil {
		return fmt.Errorf("storing fs response: %w", err)
	}
	return nil
}

func (r *redisRelay) Take(ctx context.Context, scope, requestID string) (*Response, error) {
	if r.client == nil {
		return nil, ErrUnavailable
	}
	body, err := r.client.GetDel(ctx, responseKey(scope, requestID)).Bytes()
	switch {
	case err == nil:
		return decodeResponse(body, requestID)
	case err != redis.Nil:
		return nil, fmt.Errorf("reading fs response: %w", err)
	}
	// No response yet. Verify the request itself still exists; if not, check
	// once more for a response that raced in between poll and expiry so a
	// finished call is never lost to the tightest interleaving.
	exists, err := r.client.Exists(ctx, pendingKey(scope, requestID)).Result()
	if err != nil {
		return nil, fmt.Errorf("checking fs request: %w", err)
	}
	if exists > 0 {
		return nil, nil // still pending
	}
	if body, err := r.client.GetDel(ctx, responseKey(scope, requestID)).Bytes(); err == nil {
		return decodeResponse(body, requestID)
	}
	return nil, fmt.Errorf("fs request %q not found (expired)", requestID)
}

// decodeResponse unmarshals a stored response and stamps the request ID so
// callers correlate even across scopes.
func decodeResponse(body []byte, requestID string) (*Response, error) {
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding fs response: %w", err)
	}
	resp.ID = requestID
	return &resp, nil
}

// memRelay is an in-memory Relay used in unit tests and in builds where the
// operator was started without a redis state store but with an explicit
// in-memory filesystem relay (single-instance deployments; local dev).
type memRelay struct {
	mu        sync.Mutex
	pending   map[string]Request   // key "<scope>:<id>"
	responses map[string]Response  // key "<scope>:<id>"
	expiry    map[string]time.Time // applies to both maps, same key
	clock     func() time.Time
}

// NewMemRelay returns an in-memory Relay. State lives only in this process:
// use it where a single apiserver instance serves routers and bridges.
func NewMemRelay() Relay {
	return &memRelay{
		pending:   map[string]Request{},
		responses: map[string]Response{},
		expiry:    map[string]time.Time{},
		clock:     time.Now,
	}
}

// NewMemRelayWithClock builds an in-memory Relay with an injectable clock,
// letting tests drive TTL expiry deterministically.
func NewMemRelayWithClock(now func() time.Time) Relay {
	r := NewMemRelay().(*memRelay)
	r.clock = now
	return r
}

func memKey(scope, id string) string { return scope + ":" + id }

func (m *memRelay) Enqueue(_ context.Context, scope string, in EnqueueRequest) (*Request, error) {
	req := Request{
		ID:        uuid.NewString(),
		Method:    in.Method,
		Params:    in.Params,
		CreatedAt: m.clock().UTC(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[memKey(scope, req.ID)] = req
	m.expiry[memKey(scope, req.ID)] = m.clock().Add(RelayTTL)
	return &req, nil
}

func (m *memRelay) Pending(_ context.Context, scope string) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	prefix := scope + ":"
	var out []Request
	for key, req := range m.pending {
		if exp, ok := m.expiry[key]; ok && now.After(exp) {
			delete(m.pending, key)
			delete(m.responses, key)
			delete(m.expiry, key)
			continue
		}
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			out = append(out, req)
		}
	}
	return out, nil
}

func (m *memRelay) Respond(_ context.Context, scope, requestID string, resp *Response) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(scope, requestID)
	if _, ok := m.pending[key]; !ok {
		return fmt.Errorf("fs request %q not found (expired)", requestID)
	}
	delete(m.pending, key)
	resp.ID = requestID
	m.responses[key] = *resp
	m.expiry[key] = m.clock().Add(RelayTTL)
	return nil
}

func (m *memRelay) Take(_ context.Context, scope, requestID string) (*Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(scope, requestID)
	now := m.clock()
	if exp, ok := m.expiry[key]; ok && now.After(exp) {
		delete(m.pending, key)
		delete(m.responses, key)
		delete(m.expiry, key)
	}
	if resp, ok := m.responses[key]; ok {
		delete(m.responses, key)
		delete(m.expiry, key)
		resp.ID = requestID
		return &resp, nil
	}
	if _, ok := m.pending[key]; !ok {
		return nil, fmt.Errorf("fs request %q not found (expired)", requestID)
	}
	return nil, nil // still pending
}
