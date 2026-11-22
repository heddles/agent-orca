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

package chat

import (
	"context"
	"sync"
	"time"
)

// Token is an access token an adapter obtained for one connection. It never
// prints: String and GoString hide the value, so a stray %v in a log line or an
// error does not leak it.
type Token struct{ value string }

// NewToken wraps a token value.
func NewToken(v string) Token { return Token{value: v} }

// Value is the token, for the Authorization header and nowhere else.
func (Token) String() string   { return "[token]" }
func (Token) GoString() string { return "[token]" }

// Value returns the token text.
func (t Token) Value() string { return t.value }

// TokenCache keeps the short-lived outbound tokens (OAuth client credentials, a
// service account's access token) of the connections, one entry per connection.
// An entry is reused until shortly before it expires, then fetched again; one
// fetch at a time per connection. An entry belongs to the credentials it was
// fetched with: when the Secret changes (a rotation) the fingerprint differs
// and the entry is dropped. The zero value is ready to use.
type TokenCache struct {
	// Margin is how long before expiry a token is replaced (default 5 minutes).
	// It is halved down to what a short-lived token allows.
	Margin time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu sync.Mutex
	m  map[string]*tokenEntry
}

type tokenEntry struct {
	mu          sync.Mutex
	fingerprint string
	tok         Token
	expires     time.Time
	margin      time.Duration
}

// Fetch gets a new token and says how long it lives.
type Fetch func(ctx context.Context) (Token, time.Duration, error)

func (c *TokenCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get returns the connection's token, fetching one when there is none, it is
// about to expire or the credentials changed. A failed fetch is not cached.
func (c *TokenCache) Get(ctx context.Context, conn, fingerprint string, fetch Fetch) (Token, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*tokenEntry{}
	}
	e := c.m[conn]
	if e == nil {
		e = &tokenEntry{}
		c.m[conn] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fingerprint == fingerprint && e.tok.value != "" && c.now().Before(e.expires.Add(-e.margin)) {
		return e.tok, nil
	}
	tok, ttl, err := fetch(ctx)
	if err != nil {
		e.tok = Token{}
		return Token{}, err
	}
	margin := c.Margin
	if margin <= 0 {
		margin = 5 * time.Minute
	}
	for margin > 0 && ttl <= 2*margin {
		margin /= 2
	}
	e.fingerprint, e.tok, e.expires, e.margin = fingerprint, tok, c.now().Add(ttl), margin
	return tok, nil
}

// Forget drops a connection's token, after the platform refused it.
func (c *TokenCache) Forget(conn string) {
	c.mu.Lock()
	delete(c.m, conn)
	c.mu.Unlock()
}
