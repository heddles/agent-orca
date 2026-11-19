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

// Package connections implements per-user OAuth connections to MCP servers
// (docs/mcp-oauth.md): envelope encryption of the stored tokens, the
// authorization-code flow with PKCE and a stored single-use state, and the
// short-lived access token the operator hands to a run's router.
package connections

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Secret is a credential that must not leak through formatting, JSON or logs.
// Every way of printing it yields "[REDACTED]"; Reveal is the one deliberate way out.
type Secret string

const redacted = "[REDACTED]"

// Reveal returns the secret value. Call it only where the value is sent to the
// party it belongs to (the authorization server, or the MCP server's request header).
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter so %v, %+v, %s, %q and %x all redact.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Key-related errors.
var (
	// ErrNoKeys means no key-encryption key is configured: the feature is off.
	ErrNoKeys = errors.New("connections: no key-encryption key configured")
	// ErrUnknownKey means a row names a key id the keyring does not hold.
	ErrUnknownKey = errors.New("connections: unknown key id")
	// ErrDecrypt means a blob did not decrypt: wrong key, tampering, or a row
	// copied to another tenant, user or server (the AAD no longer matches).
	ErrDecrypt = errors.New("connections: cannot decrypt")
)

const (
	blobVersion = 1
	keyLen      = 32
	nonceLen    = 12
	// wrappedLen is the data key (32) plus the GCM tag (16).
	wrappedLen = keyLen + 16
)

// Keyring holds key-encryption keys (KEKs) by id. New data is sealed under the
// active key; any held key opens old rows, so two keys are valid while a rotation
// re-wraps the rows. The Kubernetes Secret behind it is the only place the KEKs live.
type Keyring struct {
	active string
	keys   map[string][]byte
}

// NewKeyring builds a keyring. Every key must be 32 bytes and active must be one of them.
func NewKeyring(active string, keys map[string][]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	k := &Keyring{active: active, keys: make(map[string][]byte, len(keys))}
	for id, key := range keys {
		if id == "" || len(key) != keyLen {
			return nil, fmt.Errorf("connections: key %q must be %d bytes", id, keyLen)
		}
		k.keys[id] = append([]byte(nil), key...)
	}
	if _, ok := k.keys[active]; !ok {
		return nil, fmt.Errorf("connections: active key %q is not in the keyring", active)
	}
	return k, nil
}

// ActiveID is the id new data is sealed under.
func (k *Keyring) ActiveID() string { return k.active }

// LoadKeyringDir reads a keyring from a directory mounted from a Kubernetes
// Secret: one file per key, named by key id, holding the 32-byte key as base64
// or hex; and a file named "active" holding the id of the key to seal with
// (when there is exactly one key the file may be absent).
func LoadKeyringDir(dir string) (*Keyring, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the connection key directory: %w", err)
	}
	keys := map[string][]byte{}
	active := ""
	for _, e := range entries {
		name := e.Name()
		// Skip the ..data symlink machinery of mounted Secrets and sub-directories.
		if e.IsDir() || strings.HasPrefix(name, "..") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		val := strings.TrimSpace(string(raw))
		if name == "active" {
			active = val
			continue
		}
		key, err := decodeKey(val)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", name, err)
		}
		keys[name] = key
	}
	if active == "" && len(keys) == 1 {
		for id := range keys {
			active = id
		}
	}
	return NewKeyring(active, keys)
}

func decodeKey(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil && len(b) == keyLen {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == keyLen {
			return b, nil
		}
	}
	return nil, fmt.Errorf("must be %d bytes as base64 or hex", keyLen)
}

// AAD builds the additional authenticated data that binds a ciphertext to its
// owner: the purpose of the field and the parts that identify the row (tenant,
// user, server UID). A row copied to another tenant or user fails to decrypt.
// Parts are length-prefixed so ("a|b","c") and ("a","b|c") differ.
func AAD(purpose string, parts ...string) []byte {
	b := make([]byte, 0, 64)
	for _, p := range append([]string{purpose}, parts...) {
		b = append(b, byte(len(p)>>8), byte(len(p)))
		b = append(b, p...)
	}
	return b
}

// Seal encrypts plaintext with a fresh AES-256-GCM data key and wraps that key
// under the active KEK. It returns the key id to store with the row and a
// self-contained blob: version | wrap nonce | wrapped data key | nonce | ciphertext.
func (k *Keyring) Seal(plaintext, aad []byte) (keyID string, blob []byte, err error) {
	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		return "", nil, err
	}
	wrapNonce, wrapped, err := gcmSeal(k.keys[k.active], dek, wrapAAD(k.active, aad))
	if err != nil {
		return "", nil, err
	}
	nonce, ct, err := gcmSeal(dek, plaintext, aad)
	if err != nil {
		return "", nil, err
	}
	blob = append(blob, blobVersion)
	blob = append(blob, wrapNonce...)
	blob = append(blob, wrapped...)
	blob = append(blob, nonce...)
	blob = append(blob, ct...)
	return k.active, blob, nil
}

// Open decrypts a blob sealed under keyID.
func (k *Keyring) Open(keyID string, blob, aad []byte) ([]byte, error) {
	kek, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, keyID)
	}
	min := 1 + nonceLen + wrappedLen + nonceLen
	if len(blob) < min || blob[0] != blobVersion {
		return nil, ErrDecrypt
	}
	wrapNonce := blob[1 : 1+nonceLen]
	wrapped := blob[1+nonceLen : 1+nonceLen+wrappedLen]
	rest := blob[1+nonceLen+wrappedLen:]
	dek, err := gcmOpen(kek, wrapNonce, wrapped, wrapAAD(keyID, aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	pt, err := gcmOpen(dek, rest[:nonceLen], rest[nonceLen:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// Rewrap re-encrypts a blob under the active key (rotation). The plaintext is
// only in memory for the call.
func (k *Keyring) Rewrap(keyID string, blob, aad []byte) (string, []byte, error) {
	pt, err := k.Open(keyID, blob, aad)
	if err != nil {
		return "", nil, err
	}
	return k.Seal(pt, aad)
}

func wrapAAD(keyID string, aad []byte) []byte {
	return append(append([]byte("wrap:"+keyID+":"), aad...), 0)
}

func gcmSeal(key, plaintext, aad []byte) (nonce, ct []byte, err error) {
	g, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, plaintext, aad), nil
}

func gcmOpen(key, nonce, ct, aad []byte) ([]byte, error) {
	g, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
