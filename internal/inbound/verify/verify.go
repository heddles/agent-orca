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

// Package verify is the toolkit a receiving adapter authenticates a platform's
// request with. Platforms differ in the shape (a signature over a base string
// they define, a shared token in the body, a JWT signed with keys published at
// a URL) but not in the rules, and the rules are the part that must not be
// written twice: constant-time comparison, a bounded clock window, only the
// algorithms the adapter names, no `none`, no key confusion.
//
// Nothing here knows a platform. An adapter builds its platform's base string
// and calls in. Every failure is an error that says which kind of check failed
// and never includes a secret or a token.
package verify

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // see VerifyHMACSHA1: a platform mandates it
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Errors an adapter may map to its own wording. All of them mean "not authentic".
var (
	ErrNoSecret       = errors.New("no secret is configured")
	ErrBadTimestamp   = errors.New("missing or malformed timestamp")
	ErrStaleTimestamp = errors.New("timestamp outside the allowed window")
	ErrBadSignature   = errors.New("invalid signature")
	ErrBadToken       = errors.New("invalid token")
)

// Encoding is how a signature is written on the wire.
type Encoding int

// Encodings.
const (
	Hex Encoding = iota
	Base64
)

// HMACSHA256 is the raw HMAC-SHA256 of base under secret.
func HMACSHA256(secret, base []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(base)
	return m.Sum(nil)
}

// Sig describes a signature check.
type Sig struct {
	// Secret is the signing secret.
	Secret []byte
	// Base is exactly the string the platform signs, built by the adapter
	// (for example "v0:<timestamp>:<body>").
	Base []byte
	// Got is the signature header as received.
	Got string
	// Prefix is what precedes the signature in the header ("v0=", "sha256="); it
	// must be present.
	Prefix string
	// Encoding is Hex (default) or Base64.
	Encoding Encoding
}

// VerifyHMAC checks an HMAC-SHA256 signature in constant time.
func VerifyHMAC(s Sig) error {
	return verifyHMAC(s, func(secret, base []byte) []byte { return HMACSHA256(secret, base) })
}

// HMACSHA1 is the raw HMAC-SHA1 of base under secret.
func HMACSHA1(secret, base []byte) []byte {
	m := hmac.New(sha1.New, secret)
	m.Write(base)
	return m.Sum(nil)
}

// VerifyHMACSHA1 checks an HMAC-SHA1 signature in constant time. SHA-1 is weak
// as a plain hash, but HMAC does not depend on collision resistance, and this
// exists only because one chat platform signs its webhooks with HMAC-SHA1 and
// offers nothing else. An adapter must use VerifyHMAC wherever the platform
// allows a choice.
func VerifyHMACSHA1(s Sig) error {
	return verifyHMAC(s, HMACSHA1)
}

func verifyHMAC(s Sig, mac func(secret, base []byte) []byte) error {
	if len(s.Secret) == 0 {
		return ErrNoSecret
	}
	if !strings.HasPrefix(s.Got, s.Prefix) {
		return ErrBadSignature
	}
	raw := strings.TrimPrefix(s.Got, s.Prefix)
	var got []byte
	var err error
	if s.Encoding == Base64 {
		got, err = base64.StdEncoding.DecodeString(raw)
	} else {
		got, err = hex.DecodeString(raw)
	}
	if err != nil || len(got) == 0 {
		return ErrBadSignature
	}
	if !hmac.Equal(got, mac(s.Secret, s.Base)) {
		return ErrBadSignature
	}
	return nil
}

// Timestamp parses a Unix-seconds timestamp header and checks that it is within
// window of now, in either direction. It returns the time.
func Timestamp(ts string, now time.Time, window time.Duration) (time.Time, error) {
	sec, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, ErrBadTimestamp
	}
	t := time.Unix(sec, 0)
	if d := now.Sub(t); d > window || d < -window {
		return time.Time{}, ErrStaleTimestamp
	}
	return t, nil
}

// TokenEqual compares two secrets in constant time, whatever their lengths (both
// are hashed first, so the length does not leak either). Empty never matches.
func TokenEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// TokenAny reports whether got equals any of wants, comparing all of them (the
// time does not tell which one matched).
func TokenAny(got string, wants []string) bool {
	ok := false
	for _, w := range wants {
		if TokenEqual(got, w) {
			ok = true
		}
	}
	return ok
}

// SplitTokens reads a Secret value that holds several tokens, separated by
// commas, spaces or newlines.
func SplitTokens(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
}
