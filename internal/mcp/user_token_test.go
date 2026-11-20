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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type countingSource struct {
	mu     sync.Mutex
	calls  int
	expiry time.Duration
}

func (c *countingSource) MCPToken(_ context.Context, server string) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return Token{Value: "tok-" + server + "-" + string(rune('0'+c.calls)), ExpiresAt: time.Now().Add(c.expiry)}, nil
}

func TestUserTokenIsCachedUntilItExpiresAndDroppedOn401(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	rejectFirst := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64
			Method string
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		reject := rejectFirst && req.Method == "tools/list"
		if reject {
			rejectFirst = false
		}
		mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{}}})
	}))
	defer srv.Close()

	src := &countingSource{expiry: time.Hour}
	c, err := connect(context.Background(), ServerConfig{Name: "t", Transport: TransportHTTP, URL: srv.URL, User: &UserConnection{Server: "github", Tokens: src}})
	if err != nil {
		t.Fatal(err)
	}
	// initialize + initialized shared one token.
	if src.calls != 1 {
		t.Fatalf("token fetched %d times for connect", src.calls)
	}
	// The server answers 401 once: the cached token is dropped, and the next request asks again.
	_, _ = c.listTools(context.Background())
	if _, err := c.listTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 {
		t.Errorf("token fetched %d times, want 2 (once refetched after the 401)", src.calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if auths[0] != "Bearer tok-github-1" || auths[len(auths)-1] != "Bearer tok-github-2" {
		t.Errorf("Authorization headers = %v", auths)
	}
}

func TestUserTokenNearExpiryIsReplaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID int64 }
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
	}))
	defer srv.Close()
	src := &countingSource{expiry: 5 * time.Second} // inside the 30s skew
	c, err := connect(context.Background(), ServerConfig{Name: "t", Transport: TransportHTTP, URL: srv.URL, User: &UserConnection{Server: "github", Tokens: src}})
	if err != nil {
		t.Fatal(err)
	}
	if src.calls < 2 {
		t.Errorf("a token inside the skew was reused: %d fetches", src.calls)
	}
	_ = c
}
