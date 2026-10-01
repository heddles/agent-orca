/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// oauthMCPServer serves the OAuth metadata endpoints, a token endpoint (refresh grant),
// and a minimal MCP JSON-RPC server at /mcp. It records the Authorization header it
// receives on the initialize request.
type oauthMCPServer struct {
	*httptest.Server
	mu       sync.Mutex
	initAuth string
}

func newOAuthMCPServer(t *testing.T, access string) *oauthMCPServer {
	t.Helper()
	o := &oauthMCPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_servers":    []string{o.URL},
			"bearer_methods_supported": []string{"header"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint":                o.URL + "/oauth/authorize",
			"token_endpoint":                        o.URL + "/oauth/token",
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unsupported_grant_type"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "token_type": "user", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var req struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int64  `json:"id"`
			Method  string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			o.mu.Lock()
			o.initAuth = r.Header.Get("Authorization")
			o.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
				},
			})
			return
		}
		if req.Method == "tools/list" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{{"name": "fake_tool", "description": "d", "inputSchema": map[string]any{}}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
	})
	o.Server = httptest.NewServer(mux)
	return o
}

func (o *oauthMCPServer) initAuthHeader() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.initAuth
}

func TestClientOAuthExchangesRefreshAndSendsBearer(t *testing.T) {
	srv := newOAuthMCPServer(t, "xoxp-IN-MEMORY-TOKEN")
	defer srv.Close()

	creds := t.TempDir()
	writeCreds(t, creds, "client_id", "test-client")
	writeCreds(t, creds, "client_secret", "test-secret")
	writeCreds(t, creds, "refresh_token", "seed-refresh-token")

	cfg := ServerConfig{
		Name:      "slack-mcp",
		Transport: TransportHTTP,
		URL:       srv.URL + "/mcp",
		OAuth: &OAuthConfig{
			CredentialsDir: creds,
			Scopes:         []string{"search:read.public"},
		},
	}
	c := New(context.Background(), []ServerConfig{cfg})
	if len(c.Tools()) != 1 {
		t.Fatalf("expected 1 discovered tool, got %d", len(c.Tools()))
	}
	got := srv.initAuthHeader()
	if got != "Bearer xoxp-IN-MEMORY-TOKEN" {
		t.Errorf("initialize Authorization = %q, want Bearer xoxp-IN-MEMORY-TOKEN", got)
	}
}

func writeCreds(t *testing.T, dir, key, val string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, key), []byte(val), 0600); err != nil {
		t.Fatal(err)
	}
}
