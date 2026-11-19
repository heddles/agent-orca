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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	k, err := NewKeyring("k1", map[string][]byte{"k1": testKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenRoundTripAndBinding(t *testing.T) {
	k := testKeyring(t)
	aad := AAD("refresh", "tnt_a", "alice", "uid-1")
	id, blob, err := k.Seal([]byte("super-secret-refresh"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("super-secret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if pt, err := k.Open(id, blob, aad); err != nil || string(pt) != "super-secret-refresh" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	// A row copied to another tenant, user, server or purpose does not decrypt.
	for name, other := range map[string][]byte{
		"tenant":  AAD("refresh", "tnt_b", "alice", "uid-1"),
		"user":    AAD("refresh", "tnt_a", "bob", "uid-1"),
		"server":  AAD("refresh", "tnt_a", "alice", "uid-2"),
		"purpose": AAD("access", "tnt_a", "alice", "uid-1"),
		"shifted": AAD("refresh", "tnt_a", "alic", "euid-1"),
	} {
		if _, err := k.Open(id, blob, other); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: want ErrDecrypt, got %v", name, err)
		}
	}
	// Tampering is detected.
	bad := append([]byte(nil), blob...)
	bad[len(bad)-1] ^= 1
	if _, err := k.Open(id, bad, aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("tampered blob = %v", err)
	}
	if _, err := k.Open("nope", blob, aad); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key = %v", err)
	}
	// Two seals of the same plaintext differ (fresh data key and nonce).
	_, blob2, _ := k.Seal([]byte("super-secret-refresh"), aad)
	if bytes.Equal(blob, blob2) {
		t.Error("seals are deterministic")
	}
}

func TestKeyRotation(t *testing.T) {
	old := testKeyring(t)
	aad := AAD("refresh", "t", "u", "s")
	id, blob, _ := old.Seal([]byte("tok"), aad)

	// Both keys are valid during the change: new data uses k2, old rows still open.
	rotated, err := NewKeyring("k2", map[string][]byte{"k1": testKey(1), "k2": testKey(2)})
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := rotated.Open(id, blob, aad); err != nil || string(pt) != "tok" {
		t.Fatalf("old row under rotated ring: %q %v", pt, err)
	}
	newID, newBlob, err := rotated.Rewrap(id, blob, aad)
	if err != nil || newID != "k2" {
		t.Fatalf("rewrap = %q, %v", newID, err)
	}
	// After the old key is dropped the re-wrapped row still opens; the old blob does not.
	only2, _ := NewKeyring("k2", map[string][]byte{"k2": testKey(2)})
	if pt, err := only2.Open(newID, newBlob, aad); err != nil || string(pt) != "tok" {
		t.Fatalf("rewrapped row: %q %v", pt, err)
	}
	if _, err := only2.Open(id, blob, aad); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("old row after dropping k1 = %v", err)
	}
}

func TestNewKeyringValidation(t *testing.T) {
	if _, err := NewKeyring("a", nil); !errors.Is(err, ErrNoKeys) {
		t.Errorf("no keys = %v", err)
	}
	if _, err := NewKeyring("a", map[string][]byte{"a": []byte("short")}); err == nil {
		t.Error("short key accepted")
	}
	if _, err := NewKeyring("missing", map[string][]byte{"a": testKey(1)}); err == nil {
		t.Error("active key not in ring accepted")
	}
}

func TestLoadKeyringDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, val string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-01", base64.StdEncoding.EncodeToString(testKey(7)))
	k, err := LoadKeyringDir(dir)
	if err != nil || k.ActiveID() != "2026-01" {
		t.Fatalf("single key = %v, %v", k, err)
	}
	write("2026-02", fmt.Sprintf("%x", testKey(8)))
	if _, err := LoadKeyringDir(dir); err == nil {
		t.Error("two keys and no active file must fail")
	}
	write("active", "2026-02")
	if k, err = LoadKeyringDir(dir); err != nil || k.ActiveID() != "2026-02" {
		t.Fatalf("two keys = %v, %v", k, err)
	}
	if _, err := LoadKeyringDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing dir accepted")
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := Secret("tok-12345")
	var out strings.Builder
	for _, v := range []string{fmt.Sprint(s), fmt.Sprintf("%v|%+v|%s|%q|%x|%#v", s, s, s, s, s, s)} {
		out.WriteString(v)
	}
	b, _ := json.Marshal(map[string]any{"t": s, "p": &s})
	out.Write(b)
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "token", s, slog.Any("t2", s))
	out.Write(logs.Bytes())
	if strings.Contains(out.String(), "tok-12345") {
		t.Fatalf("secret leaked: %s", out.String())
	}
	if s.Reveal() != "tok-12345" {
		t.Error("Reveal")
	}
}
