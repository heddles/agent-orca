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

package googlechat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

const (
	project   = "123456789012"
	endpoint  = "https://orca.example.com/inbound/googlechat/inb_x"
	userName  = "users/111122223333444455556"
	spaceName = "spaces/AAAAspace1"
	threadNm  = "spaces/AAAAspace1/threads/TTT111"
	saEmail   = "orca-chat@my-project.iam.gserviceaccount.com"
)

var clock = time.Unix(1_700_000_000, 0)

type apiCall struct {
	path, query, auth string
	body              map[string]any
}

type world struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	keys    []jose.JSONWebKey
	keyHits atomic.Int32
	tokHits atomic.Int32
	saKey   *rsa.PrivateKey
	calls   []apiCall
	script  []func(w http.ResponseWriter)
	expires int
	// assertionErr records what was wrong with the last assertion, if anything.
	assertionErr string
}

func newWorld(t *testing.T) *world {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, saKey: k, expires: 3600}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *world) serve(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/keys":
		w.keyHits.Add(1)
		w.mu.Lock()
		set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), w.keys...)}
		w.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(set)
	case r.URL.Path == "/token":
		w.tokHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		f, _ := url.ParseQuery(string(b))
		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(f.Get("assertion"), claims, func(*jwt.Token) (any, error) { return &w.saKey.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return clock }))
		exp, _ := claims.GetExpirationTime()
		iat, _ := claims.GetIssuedAt()
		switch {
		case f.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer":
			w.assertionErr = "grant"
		case err != nil:
			w.assertionErr = "signature"
		case claims["iss"] != saEmail || claims["scope"] != chatScope || claims["aud"] != w.srv.URL+"/token":
			w.assertionErr = "claims"
		case exp == nil || iat == nil || exp.Sub(iat.Time) > time.Hour:
			w.assertionErr = "lifetime"
		}
		if w.assertionErr != "" {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = rw.Write([]byte(`{"error":"invalid_grant","error_description":"` + w.assertionErr + `"}`))
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "ya29.token-" + strings.Repeat("x", 5), "expires_in": w.expires, "token_type": "Bearer"})
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		w.mu.Lock()
		w.calls = append(w.calls, apiCall{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), body})
		i := len(w.calls) - 1
		w.mu.Unlock()
		if i < len(w.script) && w.script[i] != nil {
			w.script[i](rw)
			return
		}
		_, _ = rw.Write([]byte(`{"name":"` + spaceName + `/messages/1"}`))
	default:
		rw.WriteHeader(404)
	}
}

func (w *world) addKey(kid string) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		w.t.Fatal(err)
	}
	w.mu.Lock()
	w.keys = append(w.keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
	w.mu.Unlock()
	return k
}

func (w *world) keyJSON() string {
	der, _ := x509.MarshalPKCS8PrivateKey(w.saKey)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	b, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": saEmail, "private_key": pemKey, "private_key_id": "kid-1", "project_id": "my-project", "token_uri": "https://evil.example/token"})
	return string(b)
}

func (w *world) adapter() *Adapter {
	return &Adapter{
		APIBase: w.srv.URL, TokenURL: w.srv.URL + "/token", KeysURL: w.srv.URL + "/keys", Insecure: true, Now: func() time.Time { return clock },
		Client: &chat.Client{AllowPrivate: true, Wait: func(context.Context, time.Duration) error { return nil }},
	}
}

func (w *world) conn(t *testing.T, settings string, adapterCfg string) inbound.Connection {
	t.Helper()
	s, err := inbound.ParseSettings([]byte(settings))
	if err != nil {
		t.Fatal(err)
	}
	if adapterCfg == "" {
		adapterCfg = `{"audienceType":"projectNumber","audience":"` + project + `"}`
	}
	return inbound.Connection{ID: "inb_" + strings.Repeat("c", 32), Kind: Kind, Settings: s, Config: json.RawMessage(adapterCfg),
		Secret: inbound.Secret{KeyServiceAccount: []byte(w.keyJSON())}}
}

const defaultSettings = `{"allowSenders":["` + userName + `"],"allowGroups":["example.com"]}`

func claims(mut ...func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{"iss": chatIssuer, "aud": project, "exp": clock.Add(time.Hour).Unix(), "iat": clock.Unix()}
	for _, m := range mut {
		m(c)
	}
	return c
}

func mint(t *testing.T, m jwt.SigningMethod, key any, kid string, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(m, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func botMention(text string) []any {
	i := strings.Index(text, "@Orca")
	return []any{map[string]any{"type": "USER_MENTION", "startIndex": i, "length": 5, "userMention": map[string]any{"type": "MENTION", "user": map[string]any{"name": "users/bot", "type": userTypeBot, "displayName": "Orca"}}}}
}

func messageEvent(mut ...func(map[string]any)) map[string]any {
	text := "@Orca summarise this"
	ev := map[string]any{
		"type": "MESSAGE", "eventTime": "2023-11-14T22:13:20Z",
		"user":  map[string]any{"name": userName, "displayName": "Ann", "type": "HUMAN", "email": "Ann@Example.com", "domainId": "C0domain"},
		"space": map[string]any{"name": spaceName, "type": "ROOM", "spaceType": "SPACE"},
		"message": map[string]any{
			"name": spaceName + "/messages/m1.m1", "text": text, "argumentText": " summarise this", "annotations": botMention(text),
			"sender": map[string]any{"name": userName, "type": "HUMAN"},
			"thread": map[string]any{"name": threadNm}, "space": map[string]any{"name": spaceName, "type": "ROOM"},
		},
	}
	for _, m := range mut {
		m(ev)
	}
	return ev
}

func msgOf(ev map[string]any) map[string]any { return ev["message"].(map[string]any) }

func request(token string, ev map[string]any) *http.Request {
	b, _ := json.Marshal(ev)
	r := httptest.NewRequest(http.MethodPost, "/inbound/googlechat/x", strings.NewReader(string(b)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func receive(t *testing.T, a *Adapter, c inbound.Connection, token string, ev map[string]any) (inbound.Message, error) {
	t.Helper()
	return a.Receive(context.Background(), c, request(token, ev))
}

func authFail(t *testing.T, name string, err error, want string) {
	t.Helper()
	var ae *inbound.AuthError
	if !errors.As(err, &ae) {
		t.Errorf("%s: %v is not an auth error", name, err)
		return
	}
	if want != "" && !strings.Contains(ae.Msg, want) {
		t.Errorf("%s: %q does not say %q", name, ae.Msg, want)
	}
}

func TestProjectNumberTokenVerification(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	if _, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, priv, "k1", claims()), messageEvent()); err != nil {
		t.Fatalf("a valid token: %v", err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, tc := range map[string]struct{ token, want string }{
		"no token":       {"", "no bearer"},
		"expired":        {mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { c["exp"] = clock.Add(-2 * time.Minute).Unix() })), "expired"},
		"wrong audience": {mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { c["aud"] = "999999999" })), "audience"},
		"url audience":   {mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { c["aud"] = endpoint })), "audience"},
		"wrong issuer":   {mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { c["iss"] = "https://accounts.google.com" })), "issuer"},
		"wrong alg":      {mint(t, jwt.SigningMethodRS512, priv, "k1", claims()), ""},
		"alg none":       {mint(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "k1", claims()), ""},
		"hs256":          {mint(t, jwt.SigningMethodHS256, []byte("x"), "k1", claims()), ""},
		"other key":      {mint(t, jwt.SigningMethodRS256, other, "k1", claims()), "signature"},
		"no exp":         {mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { delete(c, "exp") })), "claim"},
	} {
		_, err := receive(t, a, c, tc.token, messageEvent())
		authFail(t, name, err, tc.want)
	}
	// One minute of leeway, no more.
	if _, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { c["exp"] = clock.Add(-30 * time.Second).Unix() })), messageEvent()); err != nil {
		t.Errorf("inside the leeway: %v", err)
	}
}

func TestKeyRotationRefetchesAndUnknownKeysDoNotHammer(t *testing.T) {
	w := newWorld(t)
	k1 := w.addKey("k1")
	now := clock
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	a.Now = func() time.Time { return now }
	if _, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, k1, "k1", claims()), messageEvent()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	k2 := w.addKey("k2")
	hits := w.keyHits.Load()
	tok := mint(t, jwt.SigningMethodRS256, k2, "k2", claims(func(c jwt.MapClaims) { c["exp"] = now.Add(time.Hour).Unix() }))
	if _, err := receive(t, a, c, tok, messageEvent()); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if w.keyHits.Load() != hits+1 {
		t.Errorf("fetches = %d", w.keyHits.Load()-hits)
	}
	hits = w.keyHits.Load()
	rogue, _ := rsa.GenerateKey(rand.Reader, 2048)
	for range 15 {
		_, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, rogue, "nope", claims(func(c jwt.MapClaims) { c["exp"] = now.Add(time.Hour).Unix() })), messageEvent())
		authFail(t, "unknown kid", err, "unknown signing key")
	}
	if w.keyHits.Load()-hits > 1 {
		t.Errorf("%d fetches for unknown kids", w.keyHits.Load()-hits)
	}
}

func TestAppURLAudienceNeedsTheChatServiceAccountEmail(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a := w.adapter()
	c := w.conn(t, defaultSettings, `{"audienceType":"appUrl","audience":"`+endpoint+`"}`)
	idToken := func(mut ...func(jwt.MapClaims)) string {
		c := jwt.MapClaims{
			"iss": "https://accounts.google.com", "aud": endpoint, "exp": clock.Add(time.Hour).Unix(), "email": chatIssuer, "email_verified": true,
		}
		for _, f := range mut {
			f(c)
		}
		return mint(t, jwt.SigningMethodRS256, priv, "k1", c)
	}
	if _, err := receive(t, a, c, idToken(), messageEvent()); err != nil {
		t.Fatalf("a valid ID token: %v", err)
	}
	for name, tok := range map[string]string{
		"another account":  idToken(func(c jwt.MapClaims) { c["email"] = "someone@example.com" }),
		"unverified email": idToken(func(c jwt.MapClaims) { c["email_verified"] = false }),
		"no email":         idToken(func(c jwt.MapClaims) { delete(c, "email") }),
		"wrong audience":   idToken(func(c jwt.MapClaims) { c["aud"] = "https://other.example/" }),
		"project issuer":   idToken(func(c jwt.MapClaims) { c["iss"] = chatIssuer }),
	} {
		_, err := receive(t, a, c, tok, messageEvent())
		authFail(t, name, err, "")
	}
	// The project-number token is not accepted by an appUrl connection, nor the reverse.
	if _, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, priv, "k1", claims()), messageEvent()); err == nil {
		t.Error("a project number token passed an appUrl connection")
	}
}

func TestMessageInASpaceIsAMentionInItsThread(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", claims())
	m, err := receive(t, a, c, tok, messageEvent())
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != inbound.ModeMention || m.Text != "summarise this" || m.Channel != spaceName || m.MessageID != spaceName+"/messages/m1.m1" || m.ThreadID != threadNm {
		t.Errorf("message = %+v", m)
	}
	if m.Sender.ID != userName || m.Sender.Group != "example.com" || m.Sender.Display != "Ann" {
		t.Errorf("sender = %+v", m.Sender)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Space != spaceName || to.Thread != threadNm {
		t.Errorf("target = %+v", to)
	}
	// A message in another thread of the same space is another conversation.
	m2, _ := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		msgOf(ev)["thread"] = map[string]any{"name": spaceName + "/threads/OTHER"}
		msgOf(ev)["name"] = spaceName + "/messages/m2.m2"
	}))
	if m2.ThreadID == m.ThreadID {
		t.Error("two threads share a conversation")
	}
	// A mention in the middle of the text, after other mentions and non-ASCII text.
	text := "héllo 😀 @Bob and @Orca do it"
	m3, err := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		msgOf(ev)["text"] = text
		msgOf(ev)["annotations"] = []any{
			map[string]any{"type": "USER_MENTION", "startIndex": len([]rune("héllo 😀 ")), "length": 4, "userMention": map[string]any{"user": map[string]any{"name": "users/bob", "type": "HUMAN"}}},
			map[string]any{"type": "USER_MENTION", "startIndex": len([]rune("héllo 😀 @Bob and ")), "length": 5, "userMention": map[string]any{"user": map[string]any{"type": userTypeBot}}},
		}
	}))
	if err != nil || m3.Text != "héllo 😀 @Bob and do it" {
		t.Errorf("text = %q %v", m3.Text, err)
	}
	// Annotation indices counted in UTF-16 units (the emoji is two) are understood too.
	m4, _ := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		msgOf(ev)["text"] = "😀 @Orca hi"
		msgOf(ev)["annotations"] = []any{map[string]any{"type": "USER_MENTION", "startIndex": 3, "length": 5, "userMention": map[string]any{"user": map[string]any{"type": userTypeBot}}}}
	}))
	if m4.Text != "😀 hi" {
		t.Errorf("utf-16 text = %q", m4.Text)
	}
	// Indices that do not point at an @ are not trusted: argumentText is used.
	m5, _ := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		msgOf(ev)["text"] = "@Orca real question"
		msgOf(ev)["argumentText"] = " real question"
		msgOf(ev)["annotations"] = []any{map[string]any{"type": "USER_MENTION", "startIndex": 40, "length": 5, "userMention": map[string]any{"user": map[string]any{"type": userTypeBot}}}}
	}))
	if m5.Text != "real question" {
		t.Errorf("fallback text = %q", m5.Text)
	}
}

func TestDirectMessageIsOneFlatConversation(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", claims())
	m, err := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		ev["space"] = map[string]any{"name": "spaces/DMspace", "type": "DM", "singleUserBotDm": true}
		msgOf(ev)["space"] = map[string]any{"name": "spaces/DMspace", "type": "DM"}
		msgOf(ev)["text"] = "hello"
		msgOf(ev)["annotations"] = nil
		msgOf(ev)["thread"] = map[string]any{"name": "spaces/DMspace/threads/T1"}
	}))
	if err != nil || m.Mode != inbound.ModeDirect || m.Text != "hello" || m.ThreadID != "spaces/DMspace/dm" || m.Channel != "spaces/DMspace" {
		t.Fatalf("dm = %+v %v", m, err)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Thread != "" {
		t.Errorf("a direct message is answered in a thread: %+v", to)
	}
}

func TestSlashCommandUsesTheArguments(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", claims())
	m, err := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		msgOf(ev)["text"] = "/agent what is up"
		msgOf(ev)["argumentText"] = " what is up"
		msgOf(ev)["annotations"] = nil
		msgOf(ev)["slashCommand"] = map[string]any{"commandId": 1}
	}))
	if err != nil || m.Mode != inbound.ModeCommand || m.Text != "what is up" {
		t.Fatalf("command = %+v %v", m, err)
	}
	if why := c.Settings.Refusal(a.Capabilities(), m); why != "" {
		t.Errorf("command refused: %s", why)
	}
	off := w.conn(t, `{"allowSenders":["`+userName+`"],"modes":["direct"]}`, "")
	if why := off.Settings.Refusal(a.Capabilities(), m); why != "mode_not_enabled" {
		t.Errorf("command mode off: %q", why)
	}
}

func TestOtherEventsBotsAndEmptyMessagesAreIgnored(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", claims())
	for _, typ := range []string{"ADDED_TO_SPACE", "REMOVED_FROM_SPACE", "CARD_CLICKED", "APP_HOME", ""} {
		_, err := receive(t, a, c, tok, messageEvent(func(ev map[string]any) { ev["type"] = typ }))
		var resp *inbound.Response
		if !errors.As(err, &resp) || resp.Status != 200 || string(resp.Body) != "{}" {
			t.Errorf("%q: %v", typ, err)
		}
	}
	m, err := receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		ev["user"] = map[string]any{"name": "users/otherbot", "type": userTypeBot}
	}))
	if err != nil || !m.FromBot || !strings.HasPrefix(m.MessageID, "ignored:") {
		t.Errorf("bot: %+v %v", m, err)
	}
	m, err = receive(t, a, c, tok, messageEvent(func(ev map[string]any) { msgOf(ev)["sender"] = map[string]any{"name": userName, "type": userTypeBot} }))
	if err != nil || !m.FromBot {
		t.Errorf("bot sender: %+v %v", m, err)
	}
	for name, mut := range map[string]func(map[string]any){
		"empty": func(ev map[string]any) {
			msgOf(ev)["text"] = "@Orca"
			msgOf(ev)["argumentText"] = ""
			msgOf(ev)["annotations"] = botMention("@Orca")
		},
		"no name": func(ev map[string]any) { delete(msgOf(ev), "name") },
		"bad space": func(ev map[string]any) {
			ev["space"] = map[string]any{"name": "spaces/../../x"}
			msgOf(ev)["space"] = map[string]any{"name": "spaces/../../x"}
		},
		"no sender": func(ev map[string]any) { ev["user"] = map[string]any{}; msgOf(ev)["sender"] = map[string]any{} },
	} {
		_, err := receive(t, a, c, tok, messageEvent(mut))
		var rej *inbound.Rejection
		if !errors.As(err, &rej) || !rej.Ignore {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A thread name that is not a thread name is not used for a reply.
	m, _ = receive(t, a, c, tok, messageEvent(func(ev map[string]any) {
		msgOf(ev)["thread"] = map[string]any{"name": "spaces/AAAAspace1/threads/x?y=z"}
	}))
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Thread != "" {
		t.Errorf("a malformed thread was kept: %+v", to)
	}
}

func TestDuplicateDeliveryAndAllowLists(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a := w.adapter()
	c := w.conn(t, `{"allowSenders":["`+userName+`"],"allowGroups":["corp.example"],"allowChannels":["`+spaceName+`"]}`, "")
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", claims())
	m1, _ := receive(t, a, c, tok, messageEvent())
	m2, _ := receive(t, a, c, mint(t, jwt.SigningMethodRS256, priv, "k1", claims(func(c jwt.MapClaims) { c["exp"] = clock.Add(2 * time.Hour).Unix() })), messageEvent())
	if m1.MessageID == "" || m1.MessageID != m2.MessageID {
		t.Errorf("keys differ: %q %q", m1.MessageID, m2.MessageID)
	}
	caps := a.Capabilities()
	if why := c.Settings.Refusal(caps, m1); why != "" {
		t.Errorf("allowed: %s", why)
	}
	m1.Channel = "spaces/AAAAother"
	if why := c.Settings.Refusal(caps, m1); why != "channel_not_allowed" {
		t.Errorf("other space: %q", why)
	}
	m1.Channel, m1.Sender.ID, m1.Sender.Group = spaceName, "users/9", "evil.example"
	if why := c.Settings.Refusal(caps, m1); why != "sender_not_allowed" {
		t.Errorf("other user: %q", why)
	}
	m1.Sender.Group = "corp.example"
	if why := c.Settings.Refusal(caps, m1); why != "" {
		t.Errorf("allowed domain: %q", why)
	}
}

// ── outbound ────────────────────────────────────────────────────────────────

func target2(space, thread string) json.RawMessage {
	b, _ := json.Marshal(target{Space: space, Thread: thread})
	return b
}

func TestReplyIsThreadedFormattedAndCannotPing(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	text := "**Done** <users/all> <users/123> @all @everyone see [docs](https://e.example/a?b=1&c=2) 1 < 2\n\n- one\n- two\n\n# Head"
	if err := a.Reply(context.Background(), c, target2(spaceName, threadNm), inbound.Result{Text: text}); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || w.assertionErr != "" {
		t.Fatalf("calls=%d assertion=%q", len(w.calls), w.assertionErr)
	}
	got := w.calls[0]
	if got.path != "/v1/"+spaceName+"/messages" || got.query != "messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" || !strings.HasPrefix(got.auth, "Bearer ya29.") {
		t.Errorf("call = %+v", got)
	}
	if th, _ := got.body["thread"].(map[string]any); th["name"] != threadNm {
		t.Errorf("thread = %+v", got.body["thread"])
	}
	msg := got.body["text"].(string)
	for _, bad := range []string{"<users/all", "<users/123", "@all", "@everyone"} {
		if strings.Contains(msg, bad) {
			t.Errorf("the message still holds %q: %q", bad, msg)
		}
	}
	for _, want := range []string{"*Done*", "<https://e.example/a?b=1&c=2|docs>", "• one", "1 < 2", "*Head*"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	// A direct conversation: no thread, no reply option.
	if err := a.Reply(context.Background(), c, target2("spaces/DMspace", ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, has := w.calls[1].body["thread"]; has || w.calls[1].query != "" {
		t.Errorf("flat reply = %+v", w.calls[1])
	}
}

func TestLongRepliesSplitAt4096OrLess(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	para := strings.Repeat("sentence of words. ", 100)
	long := strings.Repeat(para+"\n\n", 30)
	if err := a.Reply(context.Background(), c, target2(spaceName, threadNm), inbound.Result{Text: long}); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != maxReplyParts {
		t.Fatalf("messages = %d", len(w.calls))
	}
	for _, cl := range w.calls {
		if n := len([]rune(cl.body["text"].(string))); n > 4096 {
			t.Errorf("a message of %d characters", n)
		}
		if th, _ := cl.body["thread"].(map[string]any); th["name"] != threadNm {
			t.Error("a part left the thread")
		}
	}
	if !strings.Contains(w.calls[len(w.calls)-1].body["text"].(string), "shortened") {
		t.Error("the cut was not announced")
	}
}

func TestTokenCachedPerConnectionRefreshedAndRefetchedOn401(t *testing.T) {
	w := newWorld(t)
	now := clock
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	a.Now = func() time.Time { return now }
	a.tokens.Now = a.Now
	send := func(c inbound.Connection) {
		t.Helper()
		if err := a.Reply(context.Background(), c, target2(spaceName, ""), inbound.Result{Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	send(c)
	send(c)
	if w.tokHits.Load() != 1 {
		t.Errorf("token requests = %d", w.tokHits.Load())
	}
	now = now.Add(56 * time.Minute)
	send(c)
	if w.tokHits.Load() != 2 {
		t.Errorf("after the margin: %d", w.tokHits.Load())
	}
	c2 := w.conn(t, defaultSettings, "")
	c2.ID = "inb_" + strings.Repeat("d", 32)
	send(c2)
	if w.tokHits.Load() != 3 {
		t.Errorf("other connection: %d", w.tokHits.Load())
	}
	// A 401 from the API: the token is dropped and one retry is made with a new one.
	w.mu.Lock()
	n := len(w.calls)
	w.script = make([]func(http.ResponseWriter), n)
	w.script = append(w.script, func(rw http.ResponseWriter) { rw.WriteHeader(401) })
	w.mu.Unlock()
	send(c)
	if w.tokHits.Load() != 4 || len(w.calls) != n+2 {
		t.Errorf("401 handling: tokens=%d calls=%d", w.tokHits.Load(), len(w.calls)-n)
	}
}

func TestRateLimitErrorsAndSecretsNeverLeak(t *testing.T) {
	w := newWorld(t)
	w.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) { rw.Header().Set("Retry-After", "2"); rw.WriteHeader(429) }}
	var waited []time.Duration
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	a.Client.Wait = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	if err := a.Reply(context.Background(), c, target2(spaceName, ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 2*time.Second {
		t.Errorf("waited = %v", waited)
	}
	w2 := newWorld(t)
	w2.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) { rw.Header().Set("Retry-After", "900"); rw.WriteHeader(429) }}
	a2 := w2.adapter()
	err := a2.Reply(context.Background(), w2.conn(t, defaultSettings, ""), target2(spaceName, ""), inbound.Result{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "wait") || len(w2.calls) != 1 {
		t.Errorf("long retry-after: %v (%d calls)", err, len(w2.calls))
	}
	// API errors: only the status code word.
	w3 := newWorld(t)
	w3.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) {
		rw.WriteHeader(403)
		_, _ = rw.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"confidential answer ya29.token-xxxxx private_key"}}`))
	}}
	c3 := w3.conn(t, defaultSettings, "")
	err = w3.adapter().Reply(context.Background(), c3, target2(spaceName, ""), inbound.Result{Text: "confidential answer"})
	if err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Fatalf("error = %v", err)
	}
	for _, leak := range []string{"confidential", "ya29", "PRIVATE KEY", "private_key", "kid-1", saEmail} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error holds %q: %v", leak, err)
		}
	}
	// A token endpoint failure names the code only.
	w4 := newWorld(t)
	w4.saKey, _ = rsa.GenerateKey(rand.Reader, 2048) // the server expects another key than the Secret holds? no: Secret is made from the new key, so break the server side
	c4 := w4.conn(t, defaultSettings, "")
	w4.saKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	err = w4.adapter().Reply(context.Background(), c4, target2(spaceName, ""), inbound.Result{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "PRIVATE") {
		t.Errorf("token error = %v", err)
	}
	// Bad targets are refused before any call.
	for _, bad := range []string{`{}`, `{"s":"spaces/../x"}`, `{"s":"spaces/A","t":"spaces/B/threads/x"}`, `{"s":"spaces/A?x=1"}`, `nope`} {
		n := len(w.calls)
		if err := a.Reply(context.Background(), c, json.RawMessage(bad), inbound.Result{Text: "x"}); err == nil || len(w.calls) != n {
			t.Errorf("target %s: %v", bad, err)
		}
	}
}

func TestKeyFileIsPinnedAndNeverEchoed(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	// The key file's token_uri (evil.example) is ignored: the call goes to the configured endpoint.
	if err := a.Reply(context.Background(), c, target2(spaceName, ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatalf("the pinned token endpoint was not used: %v", err)
	}
	for name, raw := range map[string]string{
		"empty":      "",
		"not json":   "-----BEGIN PRIVATE KEY----- hunter2",
		"wrong type": `{"type":"authorized_user","client_email":"a@b","private_key":"x"}`,
		"bad pem":    `{"type":"service_account","client_email":"a@b","private_key":"hunter2-not-pem"}`,
	} {
		c.Secret[KeyServiceAccount] = []byte(raw)
		rep := a.Test(context.Background(), c)
		if rep.OK || rep.Error == "" || strings.Contains(rep.Error, "hunter2") {
			t.Errorf("%s: %+v", name, rep)
		}
	}
}

func TestTestReportsTheServiceAccountWithoutSecrets(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), w.conn(t, defaultSettings, "")
	rep := a.Test(context.Background(), c)
	if !rep.OK || rep.Details["serviceAccount"] != saEmail || rep.Details["project"] != "my-project" || rep.Details["audienceType"] != AudienceProjectNumber {
		t.Fatalf("report = %+v", rep)
	}
	dump, _ := json.Marshal(rep)
	for _, leak := range []string{"PRIVATE KEY", "ya29", "kid-1"} {
		if strings.Contains(string(dump), leak) {
			t.Errorf("the report holds %q", leak)
		}
	}
	if rep := a.Test(context.Background(), inbound.Connection{Config: c.Config, Secret: inbound.Secret{}}); rep.OK {
		t.Error("a connection without a key passed")
	}
	if rep := a.Test(context.Background(), inbound.Connection{Config: json.RawMessage(`{}`), Secret: c.Secret}); rep.OK {
		t.Error("a connection without settings passed")
	}
}

func TestConfigValidationAndDescriptor(t *testing.T) {
	a := &Adapter{}
	for _, bad := range []string{``, `{}`, `{"audienceType":"projectNumber"}`, `{"audienceType":"projectNumber","audience":"my-project-id"}`,
		`{"audienceType":"appUrl","audience":"http://x.example/"}`, `{"audienceType":"appUrl","audience":"notaurl"}`, `{"audienceType":"other","audience":"1234"}`,
		`{"audienceType":"projectNumber","audience":"123456","extra":1}`} {
		if a.ValidateConfig(json.RawMessage(bad)) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{`{"audienceType":"projectNumber","audience":"123456789012"}`, `{"audienceType":"appUrl","audience":"https://orca.example.com/inbound/googlechat/x"}`} {
		if err := a.ValidateConfig(json.RawMessage(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	d := a.Describe()
	if d.Title != "Google Chat" || !strings.Contains(strings.Join(d.Setup, " "), "{{url}}") || len(d.SecretKeys) != 1 || len(d.Fields) != 2 {
		t.Errorf("descriptor = %+v", d)
	}
	caps := a.Capabilities()
	if !caps.AckFirst || caps.Dialect != inbound.DialectMrkdwnRaw || caps.MaxReplyChars > 4096 || len(caps.Modes) != 3 || caps.WorkingIndicator {
		t.Errorf("caps = %+v", caps)
	}
	if got := a.AckResponse(inbound.Message{}, inbound.OutcomeRejected); got == nil || string(got.Body) != "{}" {
		t.Errorf("ack = %+v", got)
	}
	s, _ := inbound.ParseSettings([]byte(`{"allowSenders":["u"],"modes":["channel"]}`))
	if inbound.CheckSettings(a, s) == nil {
		t.Error("channel mode accepted although Google Chat cannot deliver it")
	}
}
