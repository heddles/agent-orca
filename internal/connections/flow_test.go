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

package connections

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/oauth"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// fakeRefreshToken is the refresh token the fake authorization server mints.
const fakeRefreshToken = "refresh-1"

// memStore is an in-memory Store. It models what the real store guarantees at
// the level the flow depends on: single-use flows, one connection per user and server.
type memStore struct {
	mu    sync.Mutex
	flows map[string]postgresql.MCPOAuthFlow
	apps  map[string]postgresql.MCPOAuthApp
	conns map[string]postgresql.MCPConnection
}

func newMemStore() *memStore {
	return &memStore{flows: map[string]postgresql.MCPOAuthFlow{}, apps: map[string]postgresql.MCPOAuthApp{}, conns: map[string]postgresql.MCPConnection{}}
}

func (m *memStore) CreateOAuthFlow(_ context.Context, h string, f postgresql.MCPOAuthFlow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flows[h] = f
	return nil
}

func (m *memStore) ConsumeOAuthFlow(_ context.Context, h string) (postgresql.MCPOAuthFlow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.flows[h]
	delete(m.flows, h)
	if !ok || time.Now().After(f.ExpiresAt) {
		return postgresql.MCPOAuthFlow{}, postgresql.ErrConnectionNotFound
	}
	return f, nil
}

func (m *memStore) GetOAuthApp(_ context.Context, ns, ref string) (postgresql.MCPOAuthApp, error) {
	a, ok := m.apps[ns+"/"+ref]
	if !ok {
		return postgresql.MCPOAuthApp{}, postgresql.ErrConnectionNotFound
	}
	return a, nil
}

func (m *memStore) UpsertConnection(_ context.Context, c postgresql.MCPConnection) (postgresql.MCPConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.Status = postgresql.ConnectionActive
	m.conns[c.UserID+"/"+c.ServerNS+"/"+c.ServerName] = c
	return c, nil
}

func (m *memStore) GetConnection(_ context.Context, u, ns, n string) (postgresql.MCPConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[u+"/"+ns+"/"+n]
	if !ok {
		return postgresql.MCPConnection{}, postgresql.ErrConnectionNotFound
	}
	return c, nil
}

func (m *memStore) DeleteConnection(_ context.Context, u, ns, n string) (postgresql.MCPConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[u+"/"+ns+"/"+n]
	if !ok {
		return postgresql.MCPConnection{}, postgresql.ErrConnectionNotFound
	}
	delete(m.conns, u+"/"+ns+"/"+n)
	return c, nil
}

func (m *memStore) SetConnectionStatus(_ context.Context, u, ns, n, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[u+"/"+ns+"/"+n]
	if !ok {
		return postgresql.ErrConnectionNotFound
	}
	c.Status = status
	m.conns[u+"/"+ns+"/"+n] = c
	return nil
}

func (m *memStore) WithConnectionLock(_ context.Context, u, ns, n string, fn func(postgresql.MCPConnection) (*postgresql.MCPConnection, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := u + "/" + ns + "/" + n
	c, ok := m.conns[key]
	if !ok {
		return postgresql.ErrConnectionNotFound
	}
	next, err := fn(c)
	if err != nil {
		return err
	}
	if next != nil {
		m.conns[key] = *next
	}
	return nil
}

// fakeAS is a fake authorization server and MCP server in one httptest server.
type fakeAS struct {
	*httptest.Server
	mu sync.Mutex
	// What the last requests carried.
	authQuery    url.Values
	tokenForm    url.Values
	revoked      []string
	challenges   map[string]string // code -> challenge
	tokenScope   string
	noRefresh    bool
	issParam     bool
	pkceMethods  []string
	refreshCalls int
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	f := &fakeAS{challenges: map[string]string{}, pkceMethods: []string{"S256"}}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{f.URL}, "resource": f.URL + "/mcp"})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"revocation_endpoint": f.URL + "/revoke", "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": f.pkceMethods, "authorization_response_iss_parameter_supported": f.issParam,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tokenForm = r.PostForm
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			// PKCE: the verifier must hash to the challenge sent at /authorize.
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if f.challenges[r.PostForm.Get("code")] != base64.RawURLEncoding.EncodeToString(sum[:]) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			resp := map[string]any{"access_token": "access-1", "expires_in": 3600, "scope": f.tokenScope}
			if !f.noRefresh {
				resp["refresh_token"] = fakeRefreshToken
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "refresh_token":
			f.refreshCalls++
			if r.PostForm.Get("refresh_token") != fakeRefreshToken {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-2", "expires_in": 3600})
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm.Get("token"))
		f.mu.Unlock()
	})
	return f
}

// authorize plays the browser: it reads the authorization URL, remembers the
// PKCE challenge for a fresh code, and returns the state and code.
func (f *fakeAS) authorize(t *testing.T, authURL string) (state, code string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authQuery = q
	code = "code-" + q.Get("state")[:6]
	f.challenges[code] = q.Get("code_challenge")
	return q.Get("state"), code
}

type harness struct {
	svc   *Service
	store *memStore
	as    *fakeAS
	ref   ServerRef
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	as := newFakeAS(t)
	store := newMemStore()
	keys := testKeyring(t)
	svc := &Service{Store: store, Keys: keys, OAuth: oauth.New(as.Client(), nil), RedirectURI: "https://ui.example/api/connections/callback", AllowInsecure: true}
	keyID, enc, err := svc.SealClientSecret("tnt_a", "team", "github", "client-secret-xyz")
	if err != nil {
		t.Fatal(err)
	}
	store.apps["team/github"] = postgresql.MCPOAuthApp{TenantID: "tnt_a", ServerNS: "team", AppRef: "github", ClientID: "client-1",
		ClientSecretEnc: enc, KeyID: keyID, ScopesAllowed: []string{"read", "write"}}
	ref := ServerRef{Namespace: "team", Name: "github", UID: "uid-1", URL: as.URL + "/mcp", AppRef: "github"}
	return &harness{svc: svc, store: store, as: as, ref: ref}
}

func (h *harness) resolve(ref ServerRef) func(context.Context, string, string) (ServerRef, error) {
	return func(context.Context, string, string) (ServerRef, error) { return ref, nil }
}

func (h *harness) start(t *testing.T) (state, code string) {
	t.Helper()
	st, err := h.svc.Start(context.Background(), StartRequest{TenantID: "tnt_a", UserID: "alice", Server: h.ref, Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	return h.as.authorize(t, st.AuthorizeURL)
}

func (h *harness) complete(state, code, cookie, tenant, user, iss string) (postgresql.MCPConnection, error) {
	return h.svc.Complete(context.Background(), CompleteRequest{TenantID: tenant, UserID: user, State: state, CookieState: cookie,
		Code: code, Iss: iss, Resolve: h.resolve(h.ref), AccountLabel: user})
}

func TestStartAndCompleteRoundTrip(t *testing.T) {
	h := newHarness(t)
	state, code := h.start(t)
	q := h.as.authQuery
	if q.Get("client_id") != "client-1" || q.Get("redirect_uri") != h.svc.RedirectURI || q.Get("code_challenge_method") != "S256" ||
		q.Get("resource") != h.as.URL+"/mcp" || q.Get("scope") != "read" || len(state) < 40 {
		t.Fatalf("authorize query = %v", q)
	}
	conn, err := h.complete(state, code, state, "tnt_a", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if conn.UserID != "alice" || conn.ServerUID != "uid-1" || conn.Status != "active" || len(conn.Scopes) != 1 || conn.Scopes[0] != "read" {
		t.Fatalf("connection = %+v", conn)
	}
	// The token endpoint got the client secret (decrypted), the resource and the verifier.
	if f := h.as.tokenForm; f.Get("client_secret") != "client-secret-xyz" || f.Get("resource") != h.as.URL+"/mcp" || f.Get("redirect_uri") != h.svc.RedirectURI {
		t.Errorf("token form = %v", f)
	}
	// What is stored is ciphertext bound to the owner.
	if strings.Contains(string(conn.RefreshTokenEnc), fakeRefreshToken) || strings.Contains(string(conn.AccessTokenEnc), "access-1") {
		t.Error("tokens stored in the clear")
	}
	rt, err := h.svc.Keys.Open(conn.KeyID, conn.RefreshTokenEnc, AAD("refresh", "tnt_a", "alice", "uid-1"))
	if err != nil || string(rt) != fakeRefreshToken {
		t.Errorf("refresh token = %q, %v", rt, err)
	}
	if _, err := h.svc.Keys.Open(conn.KeyID, conn.RefreshTokenEnc, AAD("refresh", "tnt_a", "bob", "uid-1")); err == nil {
		t.Error("a row for alice opened for bob")
	}
}

func TestCallbackRejections(t *testing.T) {
	cases := []struct {
		name string
		run  func(h *harness, state, code string) error
		want error
	}{
		{"replay", func(h *harness, s, c string) error {
			_, _ = h.complete(s, c, s, "tnt_a", "alice", "")
			_, err := h.complete(s, c, s, "tnt_a", "alice", "")
			return err
		}, ErrBadState},
		{"unknown state", func(h *harness, s, c string) error {
			_, err := h.complete("nope", c, "nope", "tnt_a", "alice", "")
			return err
		}, ErrBadState},
		{"missing cookie", func(h *harness, s, c string) error {
			_, err := h.complete(s, c, "", "tnt_a", "alice", "")
			return err
		}, ErrStateMismatch},
		{"cookie of another flow", func(h *harness, s, c string) error {
			_, err := h.complete(s, c, "some-other-state", "tnt_a", "alice", "")
			return err
		}, ErrStateMismatch},
		{"state of another user", func(h *harness, s, c string) error {
			_, err := h.complete(s, c, s, "tnt_a", "bob", "")
			return err
		}, ErrStateMismatch},
		{"state of another tenant", func(h *harness, s, c string) error {
			_, err := h.complete(s, c, s, "tnt_b", "alice", "")
			return err
		}, ErrStateMismatch},
		{"mismatched iss", func(h *harness, s, c string) error {
			_, err := h.complete(s, c, s, "tnt_a", "alice", "https://evil.example")
			return err
		}, ErrIssuerMismatch},
		{"missing iss when the server announces it", func(h *harness, s, c string) error {
			h.as.mu.Lock()
			h.as.issParam = true
			h.as.mu.Unlock()
			_, err := h.complete(s, c, s, "tnt_a", "alice", "")
			return err
		}, ErrIssuerMismatch},
		{"user denied", func(h *harness, s, c string) error {
			_, err := h.svc.Complete(context.Background(), CompleteRequest{TenantID: "tnt_a", UserID: "alice", State: s, CookieState: s,
				ProviderError: "access_denied", Resolve: h.resolve(h.ref)})
			return err
		}, ErrProviderDenied},
		{"server recreated (new uid)", func(h *harness, s, c string) error {
			other := h.ref
			other.UID = "uid-2"
			_, err := h.svc.Complete(context.Background(), CompleteRequest{TenantID: "tnt_a", UserID: "alice", State: s, CookieState: s,
				Code: c, Resolve: h.resolve(other)})
			return err
		}, ErrBadState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			state, code := h.start(t)
			if err := tc.run(h, state, code); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.name == "replay" || tc.name == "unknown state" {
				return // the first use of a replayed state legitimately connects
			}
			if len(h.store.conns) != 0 {
				t.Error("a connection was stored")
			}
			// A rejected state is burned: the right session cannot use it afterwards.
			if _, err := h.complete(state, code, state, "tnt_a", "alice", ""); err == nil {
				t.Error("a state that failed a check was still usable")
			}
		})
	}
}

func TestExpiredFlow(t *testing.T) {
	h := newHarness(t)
	state, code := h.start(t)
	h.store.mu.Lock()
	for k, f := range h.store.flows {
		f.ExpiresAt = time.Now().Add(-time.Second)
		h.store.flows[k] = f
	}
	h.store.mu.Unlock()
	if _, err := h.complete(state, code, state, "tnt_a", "alice", ""); !errors.Is(err, ErrBadState) {
		t.Fatalf("expired = %v", err)
	}
}

func TestStartRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.as.mu.Lock()
	h.as.pkceMethods = []string{"plain"}
	h.as.mu.Unlock()
	if _, err := h.svc.Start(ctx, StartRequest{TenantID: "tnt_a", UserID: "alice", Server: h.ref}); !errors.Is(err, ErrNoPKCE) {
		t.Errorf("no S256 = %v", err)
	}
	h.as.mu.Lock()
	h.as.pkceMethods = []string{"S256"}
	h.as.mu.Unlock()
	noApp := h.ref
	noApp.AppRef = "missing"
	if _, err := h.svc.Start(ctx, StartRequest{TenantID: "tnt_a", UserID: "alice", Server: noApp}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no app = %v", err)
	}
	if _, err := h.svc.Start(ctx, StartRequest{TenantID: "tnt_a", UserID: "alice", Server: h.ref, Scopes: []string{"admin"}}); !errors.Is(err, ErrScopesExceeded) {
		t.Errorf("scope outside the app = %v", err)
	}
	bounded := h.ref
	bounded.UserScopes = []string{"read"}
	if _, err := h.svc.Start(ctx, StartRequest{TenantID: "tnt_a", UserID: "alice", Server: bounded, Scopes: []string{"write"}}); !errors.Is(err, ErrScopesExceeded) {
		t.Errorf("scope outside userScopes = %v", err)
	}
	h.svc.AllowInsecure = false
	if _, err := h.svc.Start(ctx, StartRequest{TenantID: "tnt_a", UserID: "alice", Server: h.ref}); !errors.Is(err, ErrInsecureOAuth) {
		t.Errorf("http endpoints = %v", err)
	}
	h.svc.Keys = nil
	if _, err := h.svc.Start(ctx, StartRequest{TenantID: "tnt_a", UserID: "alice", Server: h.ref}); !errors.Is(err, ErrNoKeys) {
		t.Errorf("no keys = %v", err)
	}
}

func TestCompleteRefusesWithoutRefreshTokenOrTooMuchScope(t *testing.T) {
	h := newHarness(t)
	h.as.noRefresh = true
	state, code := h.start(t)
	if _, err := h.complete(state, code, state, "tnt_a", "alice", ""); !errors.Is(err, ErrNoRefreshToken) {
		t.Errorf("no refresh token = %v", err)
	}
	if len(h.as.revoked) != 1 {
		t.Errorf("the refused grant was not revoked at the provider: %v", h.as.revoked)
	}

	h = newHarness(t)
	h.as.tokenScope = "read admin"
	state, code = h.start(t)
	if _, err := h.complete(state, code, state, "tnt_a", "alice", ""); !errors.Is(err, ErrScopesExceeded) {
		t.Errorf("provider granted more = %v", err)
	}
	if len(h.store.conns) != 0 {
		t.Error("connection stored")
	}
}

func TestPKCEMismatchIsRejectedByProvider(t *testing.T) {
	h := newHarness(t)
	state, code := h.start(t)
	h.as.mu.Lock()
	h.as.challenges[code] = "different-challenge"
	h.as.mu.Unlock()
	if _, err := h.complete(state, code, state, "tnt_a", "alice", ""); !oauth.IsInvalidGrant(err) {
		t.Fatalf("want invalid_grant, got %v", err)
	}
}

func TestDisconnectRevokesAtProvider(t *testing.T) {
	h := newHarness(t)
	state, code := h.start(t)
	if _, err := h.complete(state, code, state, "tnt_a", "alice", ""); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.Disconnect(context.Background(), "tnt_a", "alice", h.ref)
	if err != nil || !res.Revoked {
		t.Fatalf("disconnect = %+v, %v", res, err)
	}
	if len(h.as.revoked) != 1 || h.as.revoked[0] != fakeRefreshToken {
		t.Errorf("revoked = %v", h.as.revoked)
	}
	if _, err := h.svc.Disconnect(context.Background(), "tnt_a", "alice", h.ref); !errors.Is(err, postgresql.ErrConnectionNotFound) {
		t.Errorf("second disconnect = %v", err)
	}
}

func TestResourceIndicator(t *testing.T) {
	for in, want := range map[string]string{
		"https://MCP.Example.com/mcp":      "https://mcp.example.com/mcp",
		"https://mcp.example.com/":         "https://mcp.example.com",
		"https://mcp.example.com:8443/a#f": "https://mcp.example.com:8443/a",
	} {
		if got := ResourceIndicator(in); got != want {
			t.Errorf("ResourceIndicator(%q) = %q, want %q", in, got, want)
		}
	}
}
