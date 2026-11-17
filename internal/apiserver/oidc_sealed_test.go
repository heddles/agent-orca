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

package apiserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/security/oidc"
)

const (
	sealTestState  = "state-1"
	sealTestTenant = "acme"
	sealTestKeyA   = "key-a"
	sealTestKeyB   = "key-b"
)

func testSealer(t *testing.T, secret string) *loginSealer {
	t.Helper()
	s, err := newLoginSealer([]byte(secret))
	if err != nil {
		t.Fatalf("newLoginSealer: %v", err)
	}
	return s
}

func TestLoginSealer_SealOpen(t *testing.T) {
	pending := loginState{Tenant: sealTestTenant, Nonce: "n", CodeVerifier: "v", State: sealTestState}

	type opened struct {
		opener *loginSealer
		value  string
		state  string
	}
	tests := []struct {
		name    string
		mutate  func(t *testing.T, sealed string) opened
		wantErr bool
	}{
		{"round trip", func(t *testing.T, sealed string) opened {
			return opened{testSealer(t, sealTestKeyA), sealed, sealTestState}
		}, false},
		{"tampered ciphertext", func(t *testing.T, sealed string) opened {
			b := []byte(sealed)
			i := len(b) - 3
			if b[i] == 'A' {
				b[i] = 'B'
			} else {
				b[i] = 'A'
			}
			return opened{testSealer(t, sealTestKeyA), string(b), sealTestState}
		}, true},
		{"truncated", func(t *testing.T, sealed string) opened {
			return opened{testSealer(t, sealTestKeyA), sealed[:10], sealTestState}
		}, true},
		{"not base64", func(t *testing.T, _ string) opened {
			return opened{testSealer(t, sealTestKeyA), "!!!", sealTestState}
		}, true},
		{"wrong key", func(t *testing.T, sealed string) opened {
			return opened{testSealer(t, sealTestKeyB), sealed, sealTestState}
		}, true},
		{"expired", func(t *testing.T, sealed string) opened {
			s := testSealer(t, sealTestKeyA)
			s.now = func() time.Time { return time.Now().Add(oidcLoginStateTTL + time.Second) }
			return opened{s, sealed, sealTestState}
		}, true},
		{"wrong state", func(t *testing.T, sealed string) opened {
			return opened{testSealer(t, sealTestKeyA), sealed, "other"}
		}, true},
		{"empty state", func(t *testing.T, sealed string) opened {
			return opened{testSealer(t, sealTestKeyA), sealed, ""}
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sealed, err := testSealer(t, sealTestKeyA).seal(pending)
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			o := tc.mutate(t, sealed)
			got, err := o.opener.open(o.value, o.state)
			if (err != nil) != tc.wantErr {
				t.Fatalf("open err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (got.Tenant != sealTestTenant || got.Nonce != "n" || got.CodeVerifier != "v") {
				t.Fatalf("payload mismatch: %+v", got)
			}
		})
	}
}

func TestLoginSealer_SizeAndNoPlaintext(t *testing.T) {
	long := strings.Repeat("x", 43)
	s := testSealer(t, sealTestKeyA)
	sealed, err := s.seal(loginState{Tenant: "a-tenant", Nonce: long, CodeVerifier: long, State: long})
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) > 1024 {
		t.Fatalf("cookie is %d bytes", len(sealed))
	}
	if strings.Contains(sealed, "a-tenant") {
		t.Fatal("payload must be encrypted")
	}
	if _, err := s.seal(loginState{Tenant: strings.Repeat("t", 4000)}); err == nil {
		t.Fatal("oversized payload must be rejected")
	}
}

func TestLoginSealer_EmptyKey(t *testing.T) {
	if _, err := newLoginSealer(nil); err == nil {
		t.Fatal("empty key material must be rejected")
	}
}

// Login on one replica, callback on another, both built from the same key.
func TestOIDCLogin_MultiReplica(t *testing.T) {
	key := testRSAKey(t)
	principal := &oidc.IDTokenPrincipal{
		Issuer: "https://idp.example.com", UserID: "sub-1",
		Claims: map[string]any{"email_verified": true},
	}
	a := newHandler(testAuth(t, key, loginTenant(sealTestTenant, "", "")), principal)
	b := newHandler(testAuth(t, key, loginTenant(sealTestTenant, "", "")), principal)

	state, sealed := loginFlow(t, a, sealTestTenant)
	callback := func(h *OIDCLoginHandler, sealed string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/oauth/callback?code=x&state="+state, nil)
		req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
		req.AddCookie(&http.Cookie{Name: oidcLoginCookie, Value: sealed})
		h.handleCallback(rec, req)
		return rec
	}

	rec := callback(b, sealed)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback on second replica = %d, want 302", rec.Code)
	}
	if getCookie(rec, oidcSessionCookie) == "" {
		t.Fatal("expected session cookie")
	}
	for _, name := range []string{oidcLoginCookie, oidcStateCookie} {
		cleared := false
		for _, c := range rec.Result().Cookies() {
			if c.Name == name && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Errorf("cookie %s not cleared", name)
		}
	}

	// A replica with a different key cannot open the cookie.
	other := newHandler(testAuth(t, testRSAKey(t), loginTenant(sealTestTenant, "", "")), principal)
	if rec := callback(other, sealed); rec.Code != http.StatusBadRequest {
		t.Fatalf("callback with different key = %d, want 400", rec.Code)
	}
	// State must match the sealed payload even when the plain cookie agrees.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state=forged", nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: "forged"})
	req.AddCookie(&http.Cookie{Name: oidcLoginCookie, Value: sealed})
	b.handleCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d, want 400", rec.Code)
	}
	// Missing sealed cookie.
	if rec := callback(b, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing sealed cookie = %d, want 400", rec.Code)
	}
}

func TestOIDCLogin_FallbackStoreWithoutKey(t *testing.T) {
	// A handler built without a signing key falls back to the in-process store.
	keyless := newHandler(testAuth(t, nil, loginTenant(sealTestTenant, "", "")), nil)
	if keyless.sealer != nil || keyless.sessions == nil {
		t.Fatal("expected in-process fallback without a signing key")
	}
	// Minting a session still needs a key, so exercise the fallback path of the
	// callback on a handler that has one but has its sealer removed.
	auth := testAuth(t, testRSAKey(t), loginTenant(sealTestTenant, "", ""))
	h := newHandler(auth, &oidc.IDTokenPrincipal{Issuer: "https://idp.example.com", Claims: map[string]any{}})
	h.sealer, h.sessions = nil, newLoginStateStore()
	state, sealed := loginFlow(t, h, sealTestTenant)
	if sealed != "" {
		t.Fatal("no sealed cookie expected in fallback mode")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	h.handleCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("fallback callback = %d, want 302", rec.Code)
	}
}
