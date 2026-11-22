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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyHMAC_Vectors(t *testing.T) {
	// The documented example of a versioned signature: v0:<ts>:<body>.
	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	base := []byte("v0:1531420618:token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J")
	sig := "v0=" + hex.EncodeToString(HMACSHA256(secret, base))
	if err := VerifyHMAC(Sig{Secret: secret, Base: base, Got: sig, Prefix: "v0="}); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	for name, s := range map[string]Sig{
		"wrong secret": {Secret: []byte("other"), Base: base, Got: sig, Prefix: "v0="},
		"wrong base":   {Secret: secret, Base: append([]byte("x"), base...), Got: sig, Prefix: "v0="},
		"no prefix":    {Secret: secret, Base: base, Got: sig[3:], Prefix: "v0="},
		"not hex":      {Secret: secret, Base: base, Got: "v0=zz", Prefix: "v0="},
		"empty":        {Secret: secret, Base: base, Got: "v0=", Prefix: "v0="},
		"no secret":    {Base: base, Got: sig, Prefix: "v0="},
	} {
		if err := VerifyHMAC(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	b64 := base64.StdEncoding.EncodeToString(HMACSHA256(secret, base))
	if err := VerifyHMAC(Sig{Secret: secret, Base: base, Got: b64, Encoding: Base64}); err != nil {
		t.Errorf("base64 signature: %v", err)
	}
}

func TestTimestampWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for ts, want := range map[string]error{
		"1700000000": nil, "1699999800": nil, "1700000300": nil,
		"1699999699": ErrStaleTimestamp, "1700000301": ErrStaleTimestamp,
		"": ErrBadTimestamp, "abc": ErrBadTimestamp, "-5": ErrBadTimestamp, "0": ErrBadTimestamp,
	} {
		if _, err := Timestamp(ts, now, 5*time.Minute); !errors.Is(err, want) && (err != nil || want != nil) {
			t.Errorf("Timestamp(%q) = %v, want %v", ts, err, want)
		}
	}
}

func TestTokenCompare(t *testing.T) {
	if !TokenEqual("abc", "abc") || TokenEqual("abc", "abd") || TokenEqual("", "") || TokenEqual("a", "") || TokenEqual("abc", "abcd") {
		t.Error("TokenEqual")
	}
	if !TokenAny("b", []string{"a", "b"}) || TokenAny("c", []string{"a", "b"}) || TokenAny("x", nil) {
		t.Error("TokenAny")
	}
	if got := SplitTokens("a, b\nc  d"); len(got) != 4 {
		t.Errorf("SplitTokens = %v", got)
	}
}

// ── JWT ──────────────────────────────────────────────────────────────────────

type jwks struct {
	srv   *httptest.Server
	keys  atomic.Value // []jose.JSONWebKey
	hits  atomic.Int32
	clock atomic.Int64
}

func newJWKS(t *testing.T, keys ...jose.JSONWebKey) *jwks {
	t.Helper()
	j := &jwks{}
	j.keys.Store(keys)
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		j.hits.Add(1)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: j.keys.Load().([]jose.JSONWebKey)})
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func rsaKey(t *testing.T, kid string) (*rsa.PrivateKey, jose.JSONWebKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"}
}

var epoch = time.Unix(1_700_000_000, 0)

func (j *jwks) verifier(t *testing.T, mutate ...func(*JWTConfig)) *JWTVerifier {
	t.Helper()
	cfg := JWTConfig{
		JWKSURL: j.srv.URL, Issuers: []string{"https://issuer.example"}, Audiences: []string{"app-1"},
		AllowPrivate: true, AllowInsecureHTTP: true, Now: func() time.Time { return time.Unix(j.clock.Load(), 0) },
	}
	j.clock.Store(epoch.Unix())
	for _, m := range mutate {
		m(&cfg)
	}
	v, err := NewJWTVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func goodClaims() jwt.MapClaims {
	return jwt.MapClaims{"iss": "https://issuer.example", "aud": "app-1", "exp": epoch.Add(time.Hour).Unix(), "sub": "u"}
}

func TestJWT_AcceptsAValidToken(t *testing.T) {
	priv, pub := rsaKey(t, "k1")
	j := newJWKS(t, pub)
	v := j.verifier(t)
	claims, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, priv, "k1", goodClaims()))
	if err != nil || claims["sub"] != "u" {
		t.Fatalf("Verify = %v, %v", claims, err)
	}
	// The key set is cached: a second token costs no fetch.
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, priv, "k1", goodClaims())); err != nil || j.hits.Load() != 1 {
		t.Errorf("second token: err=%v hits=%d", err, j.hits.Load())
	}
}

func TestJWT_RefusesWhatIsNotExactlyRight(t *testing.T) {
	priv, pub := rsaKey(t, "k1")
	other, _ := rsaKey(t, "k1")
	j := newJWKS(t, pub)
	v := j.verifier(t)
	with := func(f func(c jwt.MapClaims)) string {
		c := goodClaims()
		f(c)
		return sign(t, jwt.SigningMethodRS256, priv, "k1", c)
	}
	pubDER, _ := json.Marshal(pub) // any bytes the attacker can know

	cases := map[string]string{
		"expired":                             with(func(c jwt.MapClaims) { c["exp"] = epoch.Add(-time.Hour).Unix() }),
		"no exp":                              with(func(c jwt.MapClaims) { delete(c, "exp") }),
		"not yet valid":                       with(func(c jwt.MapClaims) { c["nbf"] = epoch.Add(time.Hour).Unix() }),
		"wrong issuer":                        with(func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }),
		"no issuer":                           with(func(c jwt.MapClaims) { delete(c, "iss") }),
		"wrong audience":                      with(func(c jwt.MapClaims) { c["aud"] = "app-2" }),
		"no audience":                         with(func(c jwt.MapClaims) { delete(c, "aud") }),
		"other key":                           sign(t, jwt.SigningMethodRS256, other, "k1", goodClaims()),
		"no kid":                              sign(t, jwt.SigningMethodRS256, priv, "", goodClaims()),
		"alg none":                            sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "k1", goodClaims()),
		"hs256 with the public key as secret": sign(t, jwt.SigningMethodHS256, pubDER, "k1", goodClaims()),
		"garbage":                             "a.b.c",
	}
	for name, tok := range cases {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := v.Verify(context.Background(), ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty = %v", err)
	}
	// Several accepted issuers and audiences: any one of each.
	v2 := j.verifier(t, func(c *JWTConfig) {
		c.Issuers = []string{"x", "https://issuer.example"}
		c.Audiences = []string{"y", "app-1"}
	})
	if _, err := v2.Verify(context.Background(), with(func(c jwt.MapClaims) { c["aud"] = []string{"zzz", "app-1"} })); err != nil {
		t.Errorf("list audience: %v", err)
	}
}

func TestJWT_LeewayAndTime(t *testing.T) {
	priv, pub := rsaKey(t, "k1")
	j := newJWKS(t, pub)
	v := j.verifier(t)
	c := goodClaims()
	c["exp"] = epoch.Add(-30 * time.Second).Unix() // inside the 1 minute leeway
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, priv, "k1", c)); err != nil {
		t.Errorf("inside leeway: %v", err)
	}
	c["exp"] = epoch.Add(-2 * time.Minute).Unix()
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, priv, "k1", c)); err == nil {
		t.Error("outside leeway accepted")
	}
}

func TestJWT_KeyRotation(t *testing.T) {
	priv1, pub1 := rsaKey(t, "k1")
	priv2, pub2 := rsaKey(t, "k2")
	j := newJWKS(t, pub1)
	v := j.verifier(t)
	ctx := context.Background()
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, priv1, "k1", goodClaims())); err != nil {
		t.Fatal(err)
	}
	// The provider rotates. The first token with the new id is inside MinRefetch of
	// the first fetch, so it is refused without a fetch.
	j.keys.Store([]jose.JSONWebKey{pub1, pub2})
	tok2 := sign(t, jwt.SigningMethodRS256, priv2, "k2", goodClaims())
	if _, err := v.Verify(ctx, tok2); !errors.Is(err, ErrUnknownKey) || j.hits.Load() != 1 {
		t.Fatalf("inside the refetch interval: err=%v hits=%d", err, j.hits.Load())
	}
	// A minute later the unknown id triggers a refresh and the token passes.
	j.clock.Add(61)
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, priv2, "k2", func() jwt.MapClaims {
		c := goodClaims()
		c["exp"] = epoch.Add(time.Hour).Unix()
		return c
	}())); err != nil || j.hits.Load() != 2 {
		t.Fatalf("after rotation: err=%v hits=%d", err, j.hits.Load())
	}
	// An id that never existed does not make us fetch again within the interval.
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, priv1, "nope", goodClaims())); !errors.Is(err, ErrUnknownKey) || j.hits.Load() != 2 {
		t.Errorf("unknown id: err=%v hits=%d", err, j.hits.Load())
	}
}

func TestJWT_AlgPinningAndECDSA(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub := rsaKey(t, "r1")
	j := newJWKS(t, pub, jose.JSONWebKey{Key: &ec.PublicKey, KeyID: "e1", Algorithm: "ES256", Use: "sig"})
	// Default RS256 only: an ES256 token is refused even with a good key.
	v := j.verifier(t)
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodES256, ec, "e1", goodClaims())); err == nil {
		t.Error("ES256 accepted by an RS256 verifier")
	}
	v = j.verifier(t, func(c *JWTConfig) { c.Algs = []string{"RS256", "ES256"} })
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodES256, ec, "e1", goodClaims())); err != nil {
		t.Errorf("ES256: %v", err)
	}
	// A key that is for RS256 may not verify an RS384 token.
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS384, priv, "r1", goodClaims())); err == nil {
		t.Error("an RS256 key verified an RS384 token")
	}
	if _, err := NewJWTVerifier(JWTConfig{JWKSURL: "https://x.example/k", Issuers: []string{"i"}, Audiences: []string{"a"}, Algs: []string{"HS256"}}); err == nil {
		t.Error("HS256 configured")
	}
	if _, err := NewJWTVerifier(JWTConfig{JWKSURL: "https://x.example/k", Issuers: []string{"i"}, Audiences: []string{"a"}, Algs: []string{"none"}}); err == nil {
		t.Error("none configured")
	}
}

func TestJWT_ConfigAndFetchGuards(t *testing.T) {
	if _, err := NewJWTVerifier(JWTConfig{JWKSURL: "http://x.example/k", Issuers: []string{"i"}, Audiences: []string{"a"}}); err == nil {
		t.Error("http key URL accepted")
	}
	if _, err := NewJWTVerifier(JWTConfig{JWKSURL: "https://x.example/k", Audiences: []string{"a"}}); err == nil {
		t.Error("no issuer accepted")
	}
	if _, err := NewJWTVerifier(JWTConfig{JWKSURL: "https://x.example/k", Issuers: []string{"i"}}); err == nil {
		t.Error("no audience accepted")
	}
	// The key URL is fetched through the SSRF guard: a loopback server is refused
	// unless the configuration allows private addresses.
	priv, pub := rsaKey(t, "k1")
	j := newJWKS(t, pub)
	v := j.verifier(t, func(c *JWTConfig) { c.AllowPrivate = false })
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, priv, "k1", goodClaims())); !errors.Is(err, ErrKeyFetch) || j.hits.Load() != 0 {
		t.Errorf("loopback fetch: err=%v hits=%d", err, j.hits.Load())
	}
}

func TestBearerToken(t *testing.T) {
	if tok, err := BearerToken("Bearer abc"); err != nil || tok != "abc" {
		t.Errorf("%q %v", tok, err)
	}
	if tok, err := BearerToken("bearer abc"); err != nil || tok != "abc" {
		t.Errorf("%q %v", tok, err)
	}
	for _, h := range []string{"", "Bearer ", "Basic abc", "abc"} {
		if _, err := BearerToken(h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
}

func TestVerifyHMACSHA1(t *testing.T) {
	secret, body := []byte("hook-secret"), []byte(`{"data":{"id":"m1"}}`)
	sig := hex.EncodeToString(HMACSHA1(secret, body))
	if err := VerifyHMACSHA1(Sig{Secret: secret, Base: body, Got: sig}); err != nil {
		t.Fatalf("a valid signature: %v", err)
	}
	for name, s := range map[string]Sig{
		"wrong secret": {Secret: []byte("other"), Base: body, Got: sig},
		"tampered":     {Secret: secret, Base: append([]byte(" "), body...), Got: sig},
		"empty":        {Secret: secret, Base: body, Got: ""},
		"not hex":      {Secret: secret, Base: body, Got: "zz"},
		"no secret":    {Secret: nil, Base: body, Got: sig},
		// A SHA-256 signature is not accepted for the SHA-1 check, nor the reverse.
		"sha256 value": {Secret: secret, Base: body, Got: hex.EncodeToString(HMACSHA256(secret, body))},
	} {
		if err := VerifyHMACSHA1(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := VerifyHMAC(Sig{Secret: secret, Base: body, Got: sig}); err == nil {
		t.Error("a SHA-1 signature passed the SHA-256 check")
	}
}

func TestJWT_Endorsements(t *testing.T) {
	priv, pub := rsaKey(t, "k1")
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body.Load().([]byte)) }))
	defer srv.Close()
	set := func(endorsements ...string) {
		raw, _ := json.Marshal(pub)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["endorsements"] = endorsements
		b, _ := json.Marshal(map[string]any{"keys": []any{m}})
		body.Store(b)
	}
	v, err := NewJWTVerifier(JWTConfig{JWKSURL: srv.URL, Issuers: []string{"https://issuer.example"}, Audiences: []string{"app-1"},
		AllowPrivate: true, AllowInsecureHTTP: true, Now: func() time.Time { return epoch }})
	if err != nil {
		t.Fatal(err)
	}
	tok := sign(t, jwt.SigningMethodRS256, priv, "k1", goodClaims())
	set("webchat", "chan-a")
	if _, err := v.VerifyEndorsed(context.Background(), tok, "chan-a"); err != nil {
		t.Fatalf("endorsed: %v", err)
	}
	if _, err := v.VerifyEndorsed(context.Background(), tok, "chan-b"); !errors.Is(err, ErrNotEndorsed) {
		t.Errorf("not endorsed: %v", err)
	}
	// A key set without endorsements endorses nothing.
	v2, _ := NewJWTVerifier(JWTConfig{JWKSURL: srv.URL, Issuers: []string{"https://issuer.example"}, Audiences: []string{"app-1"},
		AllowPrivate: true, AllowInsecureHTTP: true, Now: func() time.Time { return epoch }})
	set()
	if _, err := v2.VerifyEndorsed(context.Background(), tok, "chan-a"); !errors.Is(err, ErrNotEndorsed) {
		t.Errorf("no endorsements: %v", err)
	}
	// Plain Verify is unaffected.
	if _, err := v2.Verify(context.Background(), tok); err != nil {
		t.Errorf("Verify: %v", err)
	}
}
