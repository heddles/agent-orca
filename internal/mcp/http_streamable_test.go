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

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// MCP method names, which the fixtures and the assertions both speak.
const (
	methodInitialize               = "initialize"
	methodNotificationsInitialized = "notifications/initialized"
	methodToolsList                = "tools/list"
)

// streamableServer is a minimal Streamable HTTP MCP server: it demands the Accept
// header the spec requires, opens a session on initialize, answers initialize with
// a one-shot SSE stream that starts with a notification, and answers everything
// after that with plain JSON until the client DELETEs the session.
type streamableServer struct {
	session string
	tools   string

	mu         sync.Mutex
	deletes    int
	initHeader string // MCP-Protocol-Version seen on initialize (want empty)
	seenMethod []string
}

func (s *streamableServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if got := r.Header.Get(mcpSessionHeader); got != s.session {
				t.Errorf("DELETE %s = %q, want %q", mcpSessionHeader, got, s.session)
			}
			s.mu.Lock()
			s.deletes++
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// The check every Streamable HTTP server makes first; without it a real
		// SDK server answers 406 and the client never gets past initialize.
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Not Acceptable: Client must accept both application/json and text/event-stream"}}`))
			return
		}

		var msg struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decoding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.seenMethod = append(s.seenMethod, msg.Method)
		s.mu.Unlock()

		switch msg.Method {
		case methodInitialize:
			s.mu.Lock()
			s.initHeader = r.Header.Get(mcpProtocolVersionHeader)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set(mcpSessionHeader, s.session)
			w.WriteHeader(http.StatusOK)
			// A notification ahead of the result is what SDK servers do; the
			// client must skip it and read on.
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"starting\"}}\n\n")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"serverInfo\":{\"name\":\"test\",\"version\":\"1\"}}}\n\n", msg.ID)
		case methodNotificationsInitialized:
			if got := r.Header.Get(mcpSessionHeader); got != s.session {
				t.Errorf("initialized %s = %q, want %q", mcpSessionHeader, got, s.session)
			}
			w.WriteHeader(http.StatusAccepted)
		case methodToolsList:
			if got := r.Header.Get(mcpSessionHeader); got != s.session {
				t.Errorf("tools/list %s = %q, want %q", mcpSessionHeader, got, s.session)
			}
			if got := r.Header.Get(mcpProtocolVersionHeader); got != "2025-06-18" {
				t.Errorf("tools/list %s = %q, want the negotiated 2025-06-18", mcpProtocolVersionHeader, got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":%s}}`, msg.ID, s.tools)
		default:
			t.Errorf("unexpected method %q", msg.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

func TestStreamableHTTPConnectListAndClose(t *testing.T) {
	srv := &streamableServer{session: "sess-abc", tools: `[{"name":"echo","description":"echo it","inputSchema":{"type":"object"}}]`}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	conn, err := connect(context.Background(), ServerConfig{Name: "stream", Transport: TransportHTTP, URL: ts.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	tools, err := conn.listTools(context.Background())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want the one echo tool", tools)
	}
	if err := conn.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.deletes != 1 {
		t.Errorf("DELETE requests = %d, want 1", srv.deletes)
	}
	// The version header carries a version negotiated at initialize; there is no
	// negotiated version yet when the initialize request itself goes out.
	if srv.initHeader != "" {
		t.Errorf("%s on initialize = %q, want none", mcpProtocolVersionHeader, srv.initHeader)
	}
	if want := []string{methodInitialize, methodNotificationsInitialized, methodToolsList}; strings.Join(srv.seenMethod, ",") != strings.Join(want, ",") {
		t.Errorf("methods = %v, want %v", srv.seenMethod, want)
	}
}

func TestStreamableHTTPStatusErrorQuotesServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Not Acceptable: Client must accept both application/json and text/event-stream"}}`))
	}))
	defer ts.Close()

	_, err := connect(context.Background(), ServerConfig{Name: "bad", Transport: TransportHTTP, URL: ts.URL})
	if err == nil {
		t.Fatal("expected connect to fail")
	}
	if !strings.Contains(err.Error(), "406") || !strings.Contains(err.Error(), "Not Acceptable") {
		t.Fatalf("error = %v, want the status and the server's reason", err)
	}
	if strings.Contains(err.Error(), "parsing MCP HTTP response") {
		t.Fatalf("a 406 was reported as a JSON parse failure: %v", err)
	}
}

func TestReadSSEResponseSkipsNotificationsAndServerRequests(t *testing.T) {
	stream := strings.Join([]string{
		"event: message",
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`,
		"",
		"event: message",
		`data: {"jsonrpc":"2.0","id":7,"method":"ping"}`,
		"",
		"event: message",
		`data: {"jsonrpc":"2.0","id":7,"result":{"ok":true}}`,
		"",
	}, "\n")

	got, err := (&serverConn{name: "test"}).readSSEResponse(strings.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("readSSEResponse: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("result = %s, want {\"ok\":true}", got)
	}
}

func TestReadSSEResponseErrorsWhenTheStreamEndsEarly(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"
	_, err := (&serverConn{name: "test"}).readSSEResponse(strings.NewReader(stream), 3)
	if err == nil || !strings.Contains(err.Error(), "before the response") {
		t.Fatalf("error = %v, want an early-close error naming the request", err)
	}
}

func TestReadSSEResponseErrorsOnJSONRPCError(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-32601,\"message\":\"Method not found\"}}\n\n"
	_, err := (&serverConn{name: "test"}).readSSEResponse(strings.NewReader(stream), 2)
	if err == nil || !strings.Contains(err.Error(), "Method not found") {
		t.Fatalf("error = %v, want the JSON-RPC error", err)
	}
}

// A server that assigns no session and answers with plain JSON must keep
// working: nothing in the Streamable HTTP path may require a session id.
func TestStreamableHTTPWithoutSessionStaysStateless(t *testing.T) {
	var mu sync.Mutex
	deletes := 0
	version := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "application/json")
		switch msg.Method {
		case methodInitialize:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`, msg.ID)
		case methodNotificationsInitialized:
			w.WriteHeader(http.StatusAccepted)
		case methodToolsList:
			// No session id, but the negotiated version must still be sent: the
			// two are independent, and a server rejects a version it cannot speak.
			mu.Lock()
			version = r.Header.Get(mcpProtocolVersionHeader)
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, msg.ID)
		}
	}))
	defer ts.Close()

	conn, err := connect(context.Background(), ServerConfig{Name: "stateless", Transport: TransportHTTP, URL: ts.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := conn.listTools(context.Background()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if err := conn.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 0 {
		t.Errorf("DELETE requests = %d, want none without a session", deletes)
	}
	if version != "2025-03-26" {
		t.Errorf("%s on tools/list = %q, want 2025-03-26 without a session", mcpProtocolVersionHeader, version)
	}
}

// A response tagged with a different id is not this request's result; using it
// would hand the caller another call's payload.
func TestStreamableHTTPRejectsAMismatchedResponseID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "application/json")
		if msg.Method == methodInitialize {
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`, msg.ID)
			return
		}
		// tools/list answered with somebody else's id.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":99,"result":{"tools":[]}}`))
	}))
	defer ts.Close()

	conn, err := connect(context.Background(), ServerConfig{Name: "mismatch", Transport: TransportHTTP, URL: ts.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	_, err = conn.listTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match request") {
		t.Fatalf("error = %v, want a mismatched-id error", err)
	}
}
