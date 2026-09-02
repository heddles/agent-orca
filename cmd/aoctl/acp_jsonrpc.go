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
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// This file implements the ACP JSON-RPC 2.0 transport over stdio.
//
// The Agent Communication Protocol (ACP) runs agents as local subprocesses:
// the editor sends newline-delimited JSON-RPC messages on stdin and the agent
// replies (responses + server->client notifications) on stdout. Logs go to
// stderr. This is the transport Zed uses for "External Agents" (the same model
// as `pool acp`, Claude Code, Codex, etc.), which is why aoctl can be that
// subprocess and bridge over to agent-orca's HTTP ACP API.
//
// Spec reference: https://agentclientprotocol.com/protocol/v1/transports

// jsonrpcRequest is an incoming JSON-RPC 2.0 message from the editor. ID is nil
// (or absent) for notifications; otherwise it carries the request id.
type jsonrpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// jsonrpcError is the JSON-RPC 2.0 error object.
type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonrpcMessage is the envelope we write back to the editor: a response
// (result/error with id), or a notification (method+params, no id).
type jsonrpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *jsonrpcError    `json:"error,omitempty"`
}

// acpRequestHandler is implemented by the bridge; it owns method dispatch.
// Each request/notification is handed off in its own goroutine by the transport
// loop so a long-running session/prompt never blocks an incoming session/cancel.
type acpRequestHandler interface {
	Dispatch(ctx context.Context, req jsonrpcRequest)
}

// acpStdioServer is the ACP-over-stdio transport. It reads JSON-RPC requests
// from stdin, hands each to the handler, and serializes all stdout writes (and
// stderr logging) for thread safety. It also supports agent→client requests
// (e.g. elicitation/create): when the handler calls SendRequest, the reply is
// matched by ID on stdin and routed back to the caller.
type acpStdioServer struct {
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	mu      sync.Mutex
	handler acpRequestHandler
	wg      sync.WaitGroup // tracks in-flight Dispatch goroutines

	// Fields for agent→client request/response correlation.
	seq       atomic.Int64                   // request id sequence
	pendingMu sync.Mutex                     // guards pending
	pending   map[string]chan jsonrpcMessage // requestID → response channel
}

func newACPStdioServer(handler acpRequestHandler, stdin io.Reader, stdout, stderr io.Writer) *acpStdioServer {
	return &acpStdioServer{
		handler: handler,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: map[string]chan jsonrpcMessage{},
	}
}

// Serve reads newline-delimited JSON-RPC requests until stdin closes (editor
// terminates the process) or ctx is cancelled.
//
// Each request is dispatched in its own goroutine so a long-running
// session/prompt never blocks an incoming session/cancel. When stdin reaches
// EOF, Serve waits for all in-flight goroutines to finish so their responses
// are flushed to stdout before the process exits. The parent ctx (signal-
// based) governs forced shutdown: when it fires, in-flight HTTP calls are
// cancelled and the goroutines observe ctx.Done().
func (s *acpStdioServer) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.stdin)
	// Token chunks from agent-orca can be large; allow up to 1 MiB per line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		default:
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				_, _ = fmt.Fprintln(s.stderr, "stdin scan error:", err)
				s.wg.Wait()
				return err
			}
			// EOF: editor closed the transport. Wait for in-flight handlers to
			// flush their responses to stdout before returning. We do NOT cancel
			// ctx here — the handlers may need it to complete HTTP calls for the
			// current turn. The parent signal context terminates forced shutdowns.
			s.wg.Wait()
			return nil
		}
		line := scanner.Text()
		if line == "" {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			_, _ = fmt.Fprintln(s.stderr, "acp: decode error:", err)
			continue
		}
		if msg.JSONRPC != acpJSONRPCVersion {
			_, _ = fmt.Fprintf(s.stderr, "acp: ignoring non-jsonrpc message: %s\n", line)
			continue
		}
		// If this is a response to a pending agent→client request (has an id,
		// no method), route it to the waiting caller instead of dispatching.
		if msg.ID != nil && msg.Method == "" {
			idStr := string(*msg.ID)
			if ch := s.takePending(idStr); ch != nil {
				ch <- msg
				continue
			}
		}
		req := jsonrpcRequest{
			JSONRPC: msg.JSONRPC,
			ID:      msg.ID,
			Method:  msg.Method,
			Params:  msg.Params,
		}
		s.wg.Add(1)
		go func(r jsonrpcRequest) {
			defer s.wg.Done()
			s.handler.Dispatch(ctx, r)
		}(req)
	}
}

// writeMessage serializes one JSON-RPC envelope + trailing newline under the
// stdout lock.
func (s *acpStdioServer) writeMessage(msg jsonrpcMessage) {
	b, err := json.Marshal(msg)
	if err != nil {
		_, _ = fmt.Fprintln(s.stderr, "acp: encode error:", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintln(s.stdout, string(b))
}

// respond sends a request-scoped result, echoing the caller's id.
func (s *acpStdioServer) respond(id *json.RawMessage, result any) {
	s.writeMessage(jsonrpcMessage{JSONRPC: acpJSONRPCVersion, ID: id, Result: result})
}

// respondErr sends a request-scoped error.
func (s *acpStdioServer) respondErr(id *json.RawMessage, code int, message string) {
	s.writeMessage(jsonrpcMessage{JSONRPC: acpJSONRPCVersion, ID: id, Error: &jsonrpcError{Code: code, Message: message}})
}

// notify sends a server->client notification (no id). params is marshaled.
// The method parameter is intentionally generic for future ACP notifications.
func (s *acpStdioServer) notify(method string, params any) { //nolint:unparam // method is always session/update today
	if params == nil {
		s.writeMessage(jsonrpcMessage{JSONRPC: acpJSONRPCVersion, Method: method})
		return
	}
	raw, err := json.Marshal(params)
	if err != nil {
		_, _ = fmt.Fprintln(s.stderr, "acp: encode notification params:", err)
		return
	}
	s.writeMessage(jsonrpcMessage{JSONRPC: acpJSONRPCVersion, Method: method, Params: raw})
}

func (s *acpStdioServer) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.stderr, format, args...)
}

// takePending atomically removes and returns the response channel registered
// for the given request id, if any. Used by Serve to route client replies and
// by SendRequest for cleanup.
func (s *acpStdioServer) takePending(id string) chan jsonrpcMessage {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	ch, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	return ch
}

// sendRequest sends an agent→client JSON-RPC request (e.g. elicitation/create)
// on stdout and blocks until the matching response arrives, the context is
// cancelled, or the client returns an error. This lets the bridge ask the
// editor for user input mid-turn.
func (s *acpStdioServer) sendRequest(ctx context.Context, method string, params any) (*jsonrpcMessage, error) {
	// Prefix with "elicit_" so the id can never collide with a client→agent
	// request id (which may be a bare number like "1").
	id := "elicit_" + strconv.FormatInt(s.seq.Add(1), 10)
	rawID := json.RawMessage(`"` + id + `"`)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshalling %s params: %w", method, err)
		}
		rawParams = b
	}

	ch := make(chan jsonrpcMessage, 1)
	s.pendingMu.Lock()
	s.pending[string(rawID)] = ch
	s.pendingMu.Unlock()

	defer s.takePending(string(rawID))

	s.writeMessage(jsonrpcMessage{
		JSONRPC: acpJSONRPCVersion,
		ID:      &rawID,
		Method:  method,
		Params:  rawParams,
	})

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("acp: %s request failed: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
