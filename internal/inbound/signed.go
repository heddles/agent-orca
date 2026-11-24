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

package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A receiving adapter authenticates a request the way the trigger webhooks do
// (docs/integrations.md): the sender signs "<timestamp>.<body>" with HMAC-SHA256
// and sends "sha256=<hex>" and the Unix time in two headers. The key is
// hex(sha256(secret)), where the secret is the connection's Secret value, so
// the secret itself never travels.
const (
	SignatureHeader = "X-AgentOrca-Signature"
	TimestampHeader = "X-AgentOrca-Timestamp"
	// ReplayWindow is how far a timestamp may be from the clock.
	ReplayWindow = 5 * time.Minute
)

// SigningKey is the key a sender signs with for a secret.
func SigningKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Sign returns the signature header value, as a sender computes it.
func Sign(key, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ReplayCache remembers the signatures accepted inside the replay window so an
// intercepted request cannot be sent again while its timestamp is fresh. It is
// per process.
type ReplayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (c *ReplayCache) add(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	for k, exp := range c.seen {
		if now.After(exp) {
			delete(c.seen, k)
		}
	}
	if _, dup := c.seen[key]; dup {
		return false
	}
	c.seen[key] = now.Add(2 * ReplayWindow)
	return true
}

// ReadSigned reads the body of r (at most max bytes) and verifies its signature
// against the connection's secret. Any failure is an *AuthError that says only
// what kind of check failed.
func ReadSigned(r *http.Request, secret string, max int64, now time.Time, replay *ReplayCache) ([]byte, error) {
	if secret == "" {
		return nil, &AuthError{Msg: "the connection has no signing secret"}
	}
	if r.ContentLength > max {
		return nil, &AuthError{Msg: "the body is too large"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, &AuthError{Msg: "the body is unreadable"}
	}
	if int64(len(body)) > max {
		return nil, &AuthError{Msg: "the body is too large"}
	}
	ts := r.Header.Get(TimestampHeader)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, &AuthError{Msg: "missing or malformed " + TimestampHeader}
	}
	if d := now.Sub(time.Unix(sec, 0)); d > ReplayWindow || d < -ReplayWindow {
		return nil, &AuthError{Msg: "timestamp outside the replay window"}
	}
	sig := r.Header.Get(SignatureHeader)
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil || !strings.HasPrefix(sig, "sha256=") {
		return nil, &AuthError{Msg: "invalid signature"}
	}
	want, _ := hex.DecodeString(strings.TrimPrefix(Sign(SigningKey(secret), ts, body), "sha256="))
	if !hmac.Equal(got, want) {
		return nil, &AuthError{Msg: "invalid signature"}
	}
	if replay != nil && !replay.add(sig, now) {
		return nil, &AuthError{Msg: "this request was already received"}
	}
	return body, nil
}
