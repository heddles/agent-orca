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

package verify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"

	"github.com/heddles/agent-orca/internal/notify"
)

// Errors of the JWT check.
var (
	ErrNoToken    = errors.New("no bearer token")
	ErrBadJWT     = errors.New("the token is not valid")
	ErrUnknownKey = errors.New("the token is signed with an unknown key")
	ErrKeyFetch   = errors.New("the signing keys could not be fetched")
)

// JWTConfig configures a verifier of tokens signed with the keys published at a
// JWKS URL.
type JWTConfig struct {
	// JWKSURL is where the signing keys are published. Must be https.
	JWKSURL string
	// Issuers are the accepted values of iss; the token's must equal one. Required.
	Issuers []string
	// Audiences are the accepted values of aud; the token must carry one. Required.
	Audiences []string
	// Algs are the accepted signature algorithms (default RS256). The token's
	// header must name one of them. "none" and the HMAC algorithms are refused
	// whatever is listed here: the keys are public, so an HMAC "secret" would be
	// known to everyone.
	Algs []string
	// Leeway is the clock skew allowed on exp and nbf (default 1 minute).
	Leeway time.Duration
	// CacheTTL is how long a fetched key set is used (default 1 hour).
	CacheTTL time.Duration
	// MinRefetch is the shortest time between two fetches, so a flood of tokens
	// with unknown key ids cannot make us hammer the key endpoint (default 1 minute).
	MinRefetch time.Duration
	// Timeout bounds a fetch (default 10s).
	Timeout time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// AllowPrivate lets the key URL resolve to a private address (an on-premises
	// identity provider). Off by default: the fetch uses the SSRF-guarded dialer.
	AllowPrivate bool
	// AllowInsecureHTTP permits an http key URL. For tests only.
	AllowInsecureHTTP bool
	// TLS overrides the TLS settings of the fetch (tests).
	TLS *tls.Config
}

// JWTVerifier verifies bearer tokens. Safe for concurrent use; keep one per
// (key URL, issuer, audience) and share it, so the cache works.
type JWTVerifier struct {
	cfg JWTConfig

	mu        sync.Mutex
	keys      map[string]jose.JSONWebKey
	fetchedAt time.Time
	lastTry   time.Time
}

// NewJWTVerifier checks the configuration and returns a verifier.
func NewJWTVerifier(cfg JWTConfig) (*JWTVerifier, error) {
	u, err := url.Parse(cfg.JWKSURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !cfg.AllowInsecureHTTP)) {
		return nil, errors.New("the key URL must be an https URL")
	}
	if len(cfg.Issuers) == 0 || len(cfg.Audiences) == 0 {
		return nil, errors.New("issuer and audience are required: a token must be checked against both")
	}
	if len(cfg.Algs) == 0 {
		cfg.Algs = []string{"RS256"}
	}
	for _, a := range cfg.Algs {
		if !slices.Contains(asymmetric, a) {
			return nil, fmt.Errorf("algorithm %q is not accepted: only public-key algorithms are", a)
		}
	}
	return &JWTVerifier{cfg: cfg}, nil
}

var asymmetric = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}

func (v *JWTVerifier) now() time.Time {
	if v.cfg.Now != nil {
		return v.cfg.Now()
	}
	return time.Now()
}

func dur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// BearerToken extracts the token of an "Authorization: Bearer <token>" header.
func BearerToken(header string) (string, error) {
	const p = "Bearer "
	if len(header) <= len(p) || !strings.EqualFold(header[:len(p)], p) {
		return "", ErrNoToken
	}
	return header[len(p):], nil
}

// Verify checks the token's signature against the published keys and its claims
// (iss, aud, exp required; nbf if present) and returns the claims. The error
// never holds the token.
func (v *JWTVerifier) Verify(ctx context.Context, token string) (jwt.MapClaims, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("the token names no key id")
		}
		k, err := v.key(ctx, kid, t.Method.Alg())
		if err != nil {
			return nil, err
		}
		return k.Key, nil
	},
		jwt.WithValidMethods(v.cfg.Algs),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(dur(v.cfg.Leeway, time.Minute)),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownKey):
			return nil, ErrUnknownKey
		case errors.Is(err, ErrKeyFetch):
			return nil, ErrKeyFetch
		}
		return nil, fmt.Errorf("%w: %s", ErrBadJWT, reason(err))
	}
	if !parsed.Valid {
		return nil, ErrBadJWT
	}
	iss, _ := claims.GetIssuer()
	if !slices.Contains(v.cfg.Issuers, iss) {
		return nil, fmt.Errorf("%w: wrong issuer", ErrBadJWT)
	}
	aud, _ := claims.GetAudience()
	okAud := false
	for _, a := range aud {
		if slices.Contains(v.cfg.Audiences, a) {
			okAud = true
		}
	}
	if !okAud {
		return nil, fmt.Errorf("%w: wrong audience", ErrBadJWT)
	}
	return claims, nil
}

// reason names the failed check without echoing token content.
func reason(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return "not valid yet"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "bad signature"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return "a required claim is missing"
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "malformed"
	default:
		return "rejected"
	}
}

// key returns the key with the id, refreshing the cache when it is stale or the
// id is unknown (a rotation), at most once per MinRefetch.
func (v *JWTVerifier) key(ctx context.Context, kid, alg string) (jose.JSONWebKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	stale := v.keys == nil || now.Sub(v.fetchedAt) > dur(v.cfg.CacheTTL, time.Hour)
	k, known := v.keys[kid]
	if stale || !known {
		if v.keys == nil || now.Sub(v.lastTry) >= dur(v.cfg.MinRefetch, time.Minute) {
			v.lastTry = now
			keys, err := v.fetch(ctx)
			if err != nil {
				if stale {
					return jose.JSONWebKey{}, ErrKeyFetch
				}
			} else {
				v.keys, v.fetchedAt = keys, now
				k, known = keys[kid]
			}
		}
	}
	if !known {
		return jose.JSONWebKey{}, ErrUnknownKey
	}
	if k.Algorithm != "" && k.Algorithm != alg {
		return jose.JSONWebKey{}, fmt.Errorf("%w: the key is for another algorithm", ErrBadJWT)
	}
	if k.Use != "" && k.Use != "sig" {
		return jose.JSONWebKey{}, fmt.Errorf("%w: the key is not a signing key", ErrBadJWT)
	}
	return k, nil
}

const maxJWKSBytes = 512 << 10

func (v *JWTVerifier) fetch(ctx context.Context) (map[string]jose.JSONWebKey, error) {
	timeout := dur(v.cfg.Timeout, 10*time.Second)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil, // a proxy would dial past the address guard
			DialContext:         notify.GuardedDialer(v.cfg.AllowPrivate, timeout).DialContext,
			TLSClientConfig:     v.cfg.TLS,
			TLSHandshakeTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the key endpoint answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil || len(body) > maxJWKSBytes {
		return nil, errors.New("the key set is unreadable or too large")
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("the key set is not valid")
	}
	out := map[string]jose.JSONWebKey{}
	for _, k := range set.Keys {
		if k.KeyID != "" && k.Valid() && k.IsPublic() {
			out[k.KeyID] = k
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the key set holds no usable key")
	}
	return out, nil
}
