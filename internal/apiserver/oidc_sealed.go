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
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// oidcLoginCookie carries the sealed pending login (see loginSealer).
	oidcLoginCookie = "agentorca_oidc_login"

	// oidcLoginHKDFInfo is the HKDF context string that separates the pending-login
	// key from every other use of the signing key material.
	oidcLoginHKDFInfo = "agent-orca/oidc-login-cookie/v1"

	// oidcLoginCookieMax is a hard ceiling on the encoded cookie value. Browsers
	// allow about 4 KB per cookie including attributes; a real payload is under
	// 500 bytes, so this only trips if something is badly wrong.
	oidcLoginCookieMax = 2048
)

// errLoginInvalid is returned by open for every failure (bad encoding, tamper,
// wrong key, expiry, state mismatch) so callers cannot distinguish them.
var errLoginInvalid = errors.New("invalid or expired login")

// loginState holds the tenant + nonce + PKCE verifier bound to a login state.
// The tenant is carried so the callback uses the right tenant's IdP/secret; this
// is what keeps the flow multi-tenant.
type loginState struct {
	Tenant       string `json:"t"`
	Nonce        string `json:"n"`
	CodeVerifier string `json:"v"`
	State        string `json:"s"`
	ExpiresAt    int64  `json:"e"` // unix seconds
}

// loginSealer seals a pending login into an opaque cookie value with AES-256-GCM,
// so any replica holding the same key can open it. There is no server-side state.
//
// Replay: the payload is stateless, so a callback replayed within its lifetime
// is not rejected here. It is limited by the IdP, whose authorization code is
// single use, and by the short expiry.
type loginSealer struct {
	aead cipher.AEAD
	now  func() time.Time
}

// newLoginSealer builds a sealer from key material shared by all replicas. The
// AES key is derived with HKDF-SHA256 under a dedicated context string.
func newLoginSealer(secret []byte) (*loginSealer, error) {
	if len(secret) == 0 {
		return nil, errors.New("empty key material")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, oidcLoginHKDFInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving login key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &loginSealer{aead: aead, now: time.Now}, nil
}

// seal encrypts a pending login. The cookie name is bound as additional data.
func (s *loginSealer) seal(st loginState) (string, error) {
	st.ExpiresAt = s.now().Add(oidcLoginStateTTL).Unix()
	plain, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, []byte(oidcLoginCookie)))
	if len(out) > oidcLoginCookieMax {
		return "", fmt.Errorf("sealed login cookie is %d bytes, limit %d", len(out), oidcLoginCookieMax)
	}
	return out, nil
}

// open decrypts a cookie value and checks expiry and that the payload's state
// equals the state the IdP returned.
func (s *loginSealer) open(value, state string) (loginState, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return loginState{}, errLoginInvalid
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(oidcLoginCookie))
	if err != nil {
		return loginState{}, errLoginInvalid
	}
	var st loginState
	if err := json.Unmarshal(plain, &st); err != nil {
		return loginState{}, errLoginInvalid
	}
	if s.now().Unix() >= st.ExpiresAt || state == "" || st.State != state {
		return loginState{}, errLoginInvalid
	}
	return st, nil
}

// loginKeyMaterial returns the bytes every replica shares: the DER form of the
// JWT signing key, which is persisted in a Kubernetes Secret and loaded by all
// replicas. It is only an HKDF input; the signing key itself is never used as
// a cipher key. Returns nil when no key is configured.
func (a *ExternalAuth) loginKeyMaterial() []byte {
	if a == nil || a.signingKey == nil {
		return nil
	}
	der, err := x509.MarshalPKCS8PrivateKey(a.signingKey)
	if err != nil {
		return nil
	}
	return der
}

// loginStateStore is the single-process fallback used only when no shared key
// is available. It does not work across replicas.
type loginStateStore struct {
	mu      sync.Mutex
	entries map[string]loginState
}

func newLoginStateStore() *loginStateStore {
	return &loginStateStore{entries: make(map[string]loginState)}
}

func (s *loginStateStore) Set(st loginState) {
	st.ExpiresAt = time.Now().Add(oidcLoginStateTTL).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[st.State] = st
}

func (s *loginStateStore) Take(state string) (loginState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.entries[state]
	delete(s.entries, state)
	if !ok || time.Now().Unix() >= st.ExpiresAt {
		return loginState{}, false
	}
	return st, true
}
