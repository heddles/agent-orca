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

package teams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
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
	appID    = "11111111-2222-3333-4444-555555555555"
	appPass  = "s3cr3t-client-secret~value"
	tenant   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	botID    = "28:" + appID
	userAAD  = "99999999-8888-7777-6666-555555555555"
	convDM   = "a:1abcDM"
	convChan = "19:chan@thread.tacv2"
)

var clock = time.Unix(1_700_000_000, 0)

// world is the fake of everything outside: the Bot Framework key set, the login
// service and the Bot Connector (service URL).
type world struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	keys []jose.JSONWebKey
	// endorse is what the key set says each key is endorsed for.
	endorse []string
	keyHits atomic.Int32
	tokHits atomic.Int32

	connCalls []connCall
	// script answers Bot Connector calls by index; nil entry means 200.
	script []func(w http.ResponseWriter)
	// loginStatus, when set, makes the login service fail.
	loginFail func(w http.ResponseWriter)
	expiresIn int
}

type connCall struct {
	method, path, auth string
	body               map[string]any
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, endorse: []string{"msteams", "webchat"}, expiresIn: 3600}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *world) serve(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/keys":
		w.keyHits.Add(1)
		w.mu.Lock()
		ks := make([]map[string]any, 0, len(w.keys))
		for _, k := range w.keys {
			raw, _ := json.Marshal(k)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			m["endorsements"] = w.endorse
			ks = append(ks, m)
		}
		w.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]any{"keys": ks})
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		w.tokHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		f, _ := url.ParseQuery(string(b))
		if w.loginFail != nil {
			w.loginFail(rw)
			return
		}
		if f.Get("grant_type") != "client_credentials" || f.Get("scope") != "https://api.botframework.com/.default" ||
			f.Get("client_id") != appID || f.Get("client_secret") != appPass {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = rw.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided. ` + appPass + `"}`))
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"token_type": "Bearer", "expires_in": w.expiresIn, "access_token": "access-" + r.URL.Path})
	case strings.HasPrefix(r.URL.Path, "/connector/"):
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		w.mu.Lock()
		w.connCalls = append(w.connCalls, connCall{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), body})
		i := len(w.connCalls) - 1
		w.mu.Unlock()
		if i < len(w.script) && w.script[i] != nil {
			w.script[i](rw)
			return
		}
		_, _ = rw.Write([]byte(`{"id":"reply-1"}`))
	default:
		rw.WriteHeader(http.StatusNotFound)
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

func (w *world) serviceURL() string { return w.srv.URL + "/connector/" }

func (w *world) adapter() *Adapter {
	return &Adapter{
		KeysURL: w.srv.URL + "/keys", AuthBase: w.srv.URL, Insecure: true, Now: func() time.Time { return clock },
		Client: &chat.Client{AllowPrivate: true, Wait: func(context.Context, time.Duration) error { return nil }},
	}
}

func conn(t *testing.T, settings string, tenantKey bool) inbound.Connection {
	t.Helper()
	s, err := inbound.ParseSettings([]byte(settings))
	if err != nil {
		t.Fatal(err)
	}
	sec := inbound.Secret{KeyAppID: []byte(appID), KeyAppPassword: []byte(appPass)}
	if tenantKey {
		sec[KeyTenantID] = []byte(tenant)
	}
	return inbound.Connection{ID: "inb_" + strings.Repeat("c", 32), Kind: Kind, Settings: s, Config: json.RawMessage(`{"typingIndicator":true}`), Secret: sec}
}

const defaultSettings = `{"allowSenders":["` + userAAD + `"],"allowGroups":["` + tenant + `"]}`

func (w *world) claims(mut ...func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{"iss": issuer, "aud": appID, "exp": clock.Add(time.Hour).Unix(), "nbf": clock.Add(-time.Minute).Unix(), "serviceurl": w.serviceURL()}
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

func (w *world) act(mut ...func(map[string]any)) map[string]any {
	a := map[string]any{
		"type": "message", "id": "1700000000001", "channelId": "msteams", "serviceUrl": w.serviceURL(), "text": "<at>Orca</at> summarise &amp; go",
		"from":         map[string]any{"id": "29:user", "name": "Ann", "aadObjectId": userAAD},
		"recipient":    map[string]any{"id": botID, "name": "Orca"},
		"conversation": map[string]any{"id": convChan + ";messageid=1700000000001", "conversationType": "channel", "tenantId": tenant},
		"entities":     []any{map[string]any{"type": "mention", "text": "<at>Orca</at>", "mentioned": map[string]any{"id": botID, "name": "Orca"}}},
		"channelData":  map[string]any{"tenant": map[string]any{"id": tenant}},
	}
	for _, m := range mut {
		m(a)
	}
	return a
}

func request(token string, act map[string]any) *http.Request {
	b, _ := json.Marshal(act)
	r := httptest.NewRequest(http.MethodPost, "/inbound/teams/x", strings.NewReader(string(b)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func receive(t *testing.T, a *Adapter, c inbound.Connection, token string, act map[string]any) (inbound.Message, error) {
	t.Helper()
	return a.Receive(context.Background(), c, request(token, act))
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
	if strings.Contains(ae.Msg, "eyJ") || strings.Contains(ae.Msg, appPass) {
		t.Errorf("%s: the error holds a token: %q", name, ae.Msg)
	}
}

func TestTokenVerification(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), conn(t, defaultSettings, true)
	good := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	if _, err := receive(t, a, c, good, w.act()); err != nil {
		t.Fatalf("a valid token: %v", err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := map[string]struct {
		token, want string
	}{
		"no token":                             {"", "no bearer"},
		"expired":                              {mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { c["exp"] = clock.Add(-2 * time.Minute).Unix() })), "expired"},
		"not yet valid":                        {mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { c["nbf"] = clock.Add(5 * time.Minute).Unix() })), "not valid yet"},
		"no exp":                               {mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { delete(c, "exp") })), "claim"},
		"wrong audience":                       {mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { c["aud"] = "someone-else" })), "audience"},
		"wrong issuer":                         {mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { c["iss"] = "https://sts.windows.net/" + tenant + "/" })), "issuer"},
		"wrong alg":                            {mint(t, jwt.SigningMethodRS384, priv, "k1", w.claims()), ""},
		"alg none":                             {mint(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "k1", w.claims()), ""},
		"hmac with a public key as the secret": {mint(t, jwt.SigningMethodHS256, []byte("x"), "k1", w.claims()), ""},
		"signed by another key":                {mint(t, jwt.SigningMethodRS256, other, "k1", w.claims()), "signature"},
		"no kid":                               {mint(t, jwt.SigningMethodRS256, priv, "", w.claims()), ""},
		"garbage":                              {"a.b.c", ""},
	}
	for name, tc := range cases {
		_, err := receive(t, a, c, tc.token, w.act())
		authFail(t, name, err, tc.want)
	}
	// Leeway is small: a token one minute past exp passes, two minutes does not (above).
	late := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { c["exp"] = clock.Add(-30 * time.Second).Unix() }))
	if _, err := receive(t, a, c, late, w.act()); err != nil {
		t.Errorf("inside the leeway: %v", err)
	}
	// Not "Bearer".
	r := request(good, w.act())
	r.Header.Set("Authorization", "Basic "+good)
	if _, err := a.Receive(context.Background(), c, r); err == nil {
		t.Error("a non-bearer scheme was accepted")
	}
	// A connection without an app id verifies nothing.
	c2 := conn(t, defaultSettings, false)
	c2.Secret = inbound.Secret{}
	_, err := receive(t, a, c2, good, w.act())
	authFail(t, "no app id", err, "app id")
}

func TestKeyRotationRefetchesOnceAndUnknownKeysAreRefused(t *testing.T) {
	w := newWorld(t)
	k1 := w.addKey("k1")
	now := clock
	a, c := w.adapter(), conn(t, defaultSettings, false)
	a.Now = func() time.Time { return now }
	if _, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, k1, "k1", w.claims()), w.act()); err != nil {
		t.Fatal(err)
	}
	// The service starts signing with a new key: the unknown kid triggers a refetch.
	now = now.Add(2 * time.Minute) // past the minimum refetch interval
	k2 := w.addKey("k2")
	hits := w.keyHits.Load()
	tok2 := mint(t, jwt.SigningMethodRS256, k2, "k2", w.claims(func(c jwt.MapClaims) { c["exp"] = now.Add(time.Hour).Unix() }))
	if _, err := receive(t, a, c, tok2, w.act()); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if w.keyHits.Load() != hits+1 {
		t.Errorf("key fetches = %d, want one more", w.keyHits.Load()-hits)
	}
	// A flood of unknown kids does not hammer the key endpoint.
	hits = w.keyHits.Load()
	rogue, _ := rsa.GenerateKey(rand.Reader, 2048)
	for range 20 {
		_, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, rogue, "nope", w.claims(func(c jwt.MapClaims) { c["exp"] = now.Add(time.Hour).Unix() })), w.act())
		authFail(t, "unknown kid", err, "unknown signing key")
	}
	if got := w.keyHits.Load() - hits; got > 1 {
		t.Errorf("%d fetches for a flood of unknown keys", got)
	}
}

func TestKeyMustBeEndorsedForTeams(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	w.endorse = []string{"webchat", "directline"}
	a, c := w.adapter(), conn(t, defaultSettings, false)
	_, err := receive(t, a, c, mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims()), w.act())
	authFail(t, "not endorsed", err, "endorsed")
}

func TestServiceURLIsBoundToTheToken(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), conn(t, defaultSettings, false)
	good := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	// The activity names another service URL than the token vouches for.
	_, err := receive(t, a, c, good, w.act(func(m map[string]any) { m["serviceUrl"] = "https://evil.example/" }))
	authFail(t, "mismatch", err, "service URL")
	// A token without the claim.
	noClaim := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { delete(c, "serviceurl") }))
	_, err = receive(t, a, c, noClaim, w.act())
	authFail(t, "no claim", err, "service URL")
	// Trailing slash is not a difference.
	if _, err := receive(t, a, c, good, w.act(func(m map[string]any) { m["serviceUrl"] = strings.TrimRight(w.serviceURL(), "/") })); err != nil {
		t.Errorf("trailing slash: %v", err)
	}
}

func TestTrustedServiceURL(t *testing.T) {
	a := &Adapter{}
	for _, ok := range []string{"https://smba.trafficmanager.net/emea/", "https://smba.infra.gcc.teams.microsoft.com/teams", "https://directline.botframework.com/", "https://smba.trafficmanager.net:443/x"} {
		if !a.trustedServiceURL(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"http://smba.trafficmanager.net/", "https://evil.example/", "https://smba.trafficmanager.net.evil.example/", "https://169.254.169.254/",
		"https://127.0.0.1/", "https://user:pw@smba.trafficmanager.net/", "https://smba.trafficmanager.net:8443/", "ftp://x.botframework.com", "", "notaurl", "https://notmicrosoft.com/"} {
		if a.trustedServiceURL(bad) {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestPersonalChatIsADirectFlatConversation(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), conn(t, defaultSettings, false)
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	m, err := receive(t, a, c, tok, w.act(func(m map[string]any) {
		m["conversation"] = map[string]any{"id": convDM, "conversationType": "personal", "tenantId": tenant}
		m["text"] = "hello there"
		m["entities"] = nil
	}))
	if err != nil || m.Mode != inbound.ModeDirect || m.Text != "hello there" || m.Channel != convDM || m.ThreadID != tenant+"/"+convDM+"/dm" {
		t.Fatalf("dm = %+v %v", m, err)
	}
	if m.Sender.ID != userAAD || m.Sender.Group != tenant || m.Sender.Display != "Ann" || m.MessageID != tenant+":"+convDM+":1700000000001" {
		t.Errorf("identity = %+v %q", m.Sender, m.MessageID)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Activity != "" || to.Conversation != convDM || to.ServiceURL != w.serviceURL() || to.Bot != botID {
		t.Errorf("target = %+v", to)
	}
}

func TestChannelMentionStripsTheBotAndKeepsThreads(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), conn(t, defaultSettings, false)
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	m, err := receive(t, a, c, tok, w.act(func(m map[string]any) {
		m["text"] = "<at>Orca</at> ask <at>Bob</at> about <b>x</b> &lt;ok&gt;&nbsp;now"
		m["entities"] = []any{
			map[string]any{"type": "mention", "text": "<at>Orca</at>", "mentioned": map[string]any{"id": botID}},
			map[string]any{"type": "mention", "text": "<at>Bob</at>", "mentioned": map[string]any{"id": "29:bob"}},
		}
	}))
	if err != nil || m.Mode != inbound.ModeMention {
		t.Fatalf("mention = %+v %v", m, err)
	}
	if m.Text != "ask @Bob about x <ok> now" {
		t.Errorf("text = %q", m.Text)
	}
	if m.Channel != convChan || m.ThreadID != tenant+"/"+convChan+";messageid=1700000000001" {
		t.Errorf("channel/thread = %q %q", m.Channel, m.ThreadID)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Conversation != convChan+";messageid=1700000000001" || to.Activity != "1700000000001" {
		t.Errorf("target = %+v", to)
	}
	// A reply in the same thread (same conversation id) continues the conversation; another channel does not.
	m2, _ := receive(t, a, c, tok, w.act(func(m map[string]any) { m["id"] = "1700000000009" }))
	if m2.ThreadID != m.ThreadID || m2.MessageID == m.MessageID {
		t.Errorf("follow-up = %+v", m2)
	}
	m3, _ := receive(t, a, c, tok, w.act(func(m map[string]any) {
		m["conversation"] = map[string]any{"id": "19:other@thread.tacv2;messageid=1700000000001", "conversationType": "channel", "tenantId": tenant}
	}))
	if m3.ThreadID == m.ThreadID {
		t.Error("two channels share a conversation")
	}
}

func TestUnmentionedChannelMessagesNeedTheChannelMode(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a := w.adapter()
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	plain := func(m map[string]any) { m["text"] = "just talking"; m["entities"] = nil }
	m, err := receive(t, a, conn(t, defaultSettings, false), tok, w.act(plain))
	var rej *inbound.Rejection
	if !errors.As(err, &rej) || !rej.Ignore || !strings.HasPrefix(m.MessageID, "ignored:") {
		t.Fatalf("channel mode off: %+v %v", m, err)
	}
	on := conn(t, `{"allowChannels":["`+convChan+`"],"modes":["mention","channel"]}`, false)
	m, err = receive(t, a, on, tok, w.act(plain))
	if err != nil || m.Mode != inbound.ModeChannel {
		t.Fatalf("channel mode on: %+v %v", m, err)
	}
	if why := on.Settings.Refusal(a.Capabilities(), m); why != "" {
		t.Errorf("listed channel: %s", why)
	}
	m.Channel = "19:other@thread.tacv2"
	if why := on.Settings.Refusal(a.Capabilities(), m); why != "channel_not_allowed" {
		t.Errorf("other channel: %q", why)
	}
}

func TestBotsOwnMessagesAndOtherActivitiesAreIgnored(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), conn(t, defaultSettings, false)
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	for name, mut := range map[string]func(map[string]any){
		"role bot": func(m map[string]any) {
			m["from"] = map[string]any{"id": "29:x", "aadObjectId": userAAD, "role": "bot"}
		},
		"bot id":      func(m map[string]any) { m["from"] = map[string]any{"id": "28:other-app"} },
		"own id":      func(m map[string]any) { m["from"] = map[string]any{"id": botID} },
		"own via aad": func(m map[string]any) { m["from"] = map[string]any{"id": botID, "aadObjectId": appID} },
	} {
		m, err := receive(t, a, c, tok, w.act(mut))
		if err != nil || !m.FromBot || !strings.HasPrefix(m.MessageID, "ignored:") {
			t.Errorf("%s: %+v %v", name, m, err)
		}
	}
	for name, mut := range map[string]func(map[string]any){
		"other channel": func(m map[string]any) { m["channelId"] = "webchat" },
		"empty":         func(m map[string]any) { m["text"] = "<at>Orca</at>" },
		"no id":         func(m map[string]any) { delete(m, "id") },
		"unknown conv":  func(m map[string]any) { m["conversation"] = map[string]any{"id": "x", "conversationType": "weird"} },
	} {
		_, err := receive(t, a, c, tok, w.act(mut))
		var rej *inbound.Rejection
		if !errors.As(err, &rej) || !rej.Ignore {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, typ := range []string{"conversationUpdate", "typing", "messageUpdate", "invoke"} {
		_, err := receive(t, a, c, tok, w.act(func(m map[string]any) { m["type"] = typ }))
		var resp *inbound.Response
		if !errors.As(err, &resp) || resp.Status != 200 {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

func TestDuplicateDeliveryHasTheSameKey(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a, c := w.adapter(), conn(t, defaultSettings, false)
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	m1, _ := receive(t, a, c, tok, w.act())
	// Teams retries a delivery it got no answer for, with a fresh token.
	tok2 := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims(func(c jwt.MapClaims) { c["exp"] = clock.Add(2 * time.Hour).Unix() }))
	m2, _ := receive(t, a, c, tok2, w.act())
	if m1.MessageID == "" || m1.MessageID != m2.MessageID {
		t.Errorf("keys differ: %q %q", m1.MessageID, m2.MessageID)
	}
}

func TestAllowListsThroughTheCoreRules(t *testing.T) {
	w := newWorld(t)
	priv := w.addKey("k1")
	a := w.adapter()
	c := conn(t, `{"allowSenders":["`+userAAD+`"],"allowGroups":["`+tenant+`"],"allowChannels":["`+convChan+`"]}`, false)
	tok := mint(t, jwt.SigningMethodRS256, priv, "k1", w.claims())
	m, _ := receive(t, a, c, tok, w.act())
	caps := a.Capabilities()
	if why := c.Settings.Refusal(caps, m); why != "" {
		t.Errorf("allowed: %s", why)
	}
	m.Sender.ID = "other-user"
	if why := c.Settings.Refusal(caps, m); why != "" {
		t.Errorf("member of an allowed tenant: %q", why)
	}
	m.Sender.Group = "some-other-tenant"
	if why := c.Settings.Refusal(caps, m); why != "sender_not_allowed" {
		t.Errorf("other tenant: %q", why)
	}
}

// ── outbound ────────────────────────────────────────────────────────────────

func replyTarget(w *world, conv, act string) json.RawMessage {
	b, _ := json.Marshal(target{ServiceURL: w.serviceURL(), Conversation: conv, Activity: act, Bot: botID})
	return b
}

func TestReplyIsThreadedMarkdownAndCannotPing(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), conn(t, defaultSettings, true)
	to := replyTarget(w, convChan+";messageid=77", "77")
	text := "**Done** <at>Everyone</at> @everyone @all <users/all> see [docs](https://e.example/a?b=1&c=2)\n\n- one\n- two"
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: text}); err != nil {
		t.Fatal(err)
	}
	if len(w.connCalls) != 1 {
		t.Fatalf("calls = %d", len(w.connCalls))
	}
	got := w.connCalls[0]
	wantPath := "/connector/v3/conversations/" + url.PathEscape(convChan+";messageid=77") + "/activities/77"
	if got.method != "POST" || got.path != wantPath || !strings.HasPrefix(got.auth, "Bearer access-") {
		t.Errorf("call = %+v (want path %s)", got, wantPath)
	}
	if got.body["type"] != "message" || got.body["textFormat"] != "markdown" || got.body["replyToId"] != "77" {
		t.Errorf("body = %+v", got.body)
	}
	if conv, _ := got.body["conversation"].(map[string]any); conv["id"] != convChan+";messageid=77" {
		t.Errorf("conversation = %+v", got.body["conversation"])
	}
	msg := got.body["text"].(string)
	for _, bad := range []string{"<at>", "@everyone", "@all", "<users/all"} {
		if strings.Contains(msg, bad) {
			t.Errorf("the message still holds %q: %q", bad, msg)
		}
	}
	for _, want := range []string{"**Done**", "[docs](https://e.example/a?b=1&c=2)", "- one"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	if _, has := got.body["entities"]; has {
		t.Error("the reply carries entities (mentions)")
	}
	// A personal chat reply has no replyToId.
	to = replyTarget(w, convDM, "")
	_ = a.Reply(context.Background(), c, to, inbound.Result{Text: "hi"})
	if _, has := w.connCalls[1].body["replyToId"]; has || strings.Contains(w.connCalls[1].path, "/activities/") {
		t.Errorf("flat reply = %+v", w.connCalls[1])
	}
}

func TestLongRepliesSplitWithinTheByteLimit(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), conn(t, defaultSettings, false)
	to := replyTarget(w, convDM, "")
	para := strings.Repeat("日本語のテキスト。", 80)
	long := strings.Repeat(para+"\n\n", 120)
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: long}); err != nil {
		t.Fatal(err)
	}
	if n := len(w.connCalls); n < 2 || n > maxReplyParts {
		t.Fatalf("messages = %d", n)
	}
	for _, cl := range w.connCalls {
		if n := len(cl.body["text"].(string)); n > 28*1024 {
			t.Errorf("a message of %d bytes", n)
		}
	}
	if !strings.Contains(w.connCalls[len(w.connCalls)-1].body["text"].(string), "shortened") {
		t.Error("the cut was not announced")
	}
}

func TestTokenIsCachedPerConnectionAndRefreshedBeforeExpiry(t *testing.T) {
	w := newWorld(t)
	now := clock
	a, c := w.adapter(), conn(t, defaultSettings, true)
	a.Now = func() time.Time { return now }
	a.tokens.Now = a.Now
	to := replyTarget(w, convDM, "")
	send := func() {
		t.Helper()
		if err := a.Reply(context.Background(), c, to, inbound.Result{Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	send()
	send()
	if w.tokHits.Load() != 1 {
		t.Errorf("token requests = %d, want 1", w.tokHits.Load())
	}
	// The single-tenant authority is the tenant's.
	// (the fake keeps the path in the token)
	if got := w.connCalls[0].auth; got != "Bearer access-/"+tenant+"/oauth2/v2.0/token" {
		t.Errorf("auth = %q", got)
	}
	now = now.Add(56 * time.Minute) // inside the margin of a one hour token
	send()
	if w.tokHits.Load() != 2 {
		t.Errorf("token requests = %d, want 2 after the margin", w.tokHits.Load())
	}
	// Another connection has its own token; a multi-tenant app uses botframework.com.
	c2 := conn(t, defaultSettings, false)
	c2.ID = "inb_" + strings.Repeat("d", 32)
	if err := a.Reply(context.Background(), c2, to, inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if w.tokHits.Load() != 3 || w.connCalls[len(w.connCalls)-1].auth != "Bearer access-/botframework.com/oauth2/v2.0/token" {
		t.Errorf("second connection: hits=%d auth=%q", w.tokHits.Load(), w.connCalls[len(w.connCalls)-1].auth)
	}
}

func TestA401DropsTheTokenAndRetriesOnce(t *testing.T) {
	w := newWorld(t)
	w.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) { rw.WriteHeader(http.StatusUnauthorized) }}
	a, c := w.adapter(), conn(t, defaultSettings, false)
	if err := a.Reply(context.Background(), c, replyTarget(w, convDM, ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if w.tokHits.Load() != 2 || len(w.connCalls) != 2 {
		t.Errorf("tokens=%d calls=%d", w.tokHits.Load(), len(w.connCalls))
	}
	// Refused again: the error says so and stops.
	w2 := newWorld(t)
	w2.script = []func(http.ResponseWriter){
		func(rw http.ResponseWriter) { rw.WriteHeader(401) }, func(rw http.ResponseWriter) { rw.WriteHeader(401) },
	}
	err := w.adapter().Reply(context.Background(), c, replyTarget(w2, convDM, ""), inbound.Result{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "401") || len(w2.connCalls) != 2 {
		t.Errorf("refused twice: err=%v calls=%d", err, len(w2.connCalls))
	}
}

func TestRateLimitRetryAfterThenSuccessAndLongWaitIsHandedBack(t *testing.T) {
	w := newWorld(t)
	w.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) { rw.Header().Set("Retry-After", "3"); rw.WriteHeader(429) }}
	var waited []time.Duration
	a, c := w.adapter(), conn(t, defaultSettings, false)
	a.Client.Wait = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	if err := a.Reply(context.Background(), c, replyTarget(w, convDM, ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 3*time.Second || len(w.connCalls) != 2 {
		t.Errorf("waited=%v calls=%d", waited, len(w.connCalls))
	}
	w2 := newWorld(t)
	w2.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) { rw.Header().Set("Retry-After", "900"); rw.WriteHeader(429) }}
	err := w.adapter().Reply(context.Background(), c, replyTarget(w2, convDM, ""), inbound.Result{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "wait") || len(w2.connCalls) != 1 {
		t.Errorf("long retry-after: err=%v calls=%d", err, len(w2.connCalls))
	}
}

func TestErrorsNeverHoldSecretsTokensOrText(t *testing.T) {
	w := newWorld(t)
	w.script = []func(http.ResponseWriter){func(rw http.ResponseWriter) {
		rw.WriteHeader(403)
		_, _ = rw.Write([]byte(`{"error":{"code":"BotNotInConversationRoster","message":"` + appPass + ` confidential answer"}}`))
	}}
	a, c := w.adapter(), conn(t, defaultSettings, false)
	err := a.Reply(context.Background(), c, replyTarget(w, convDM, ""), inbound.Result{Text: "confidential answer"})
	if err == nil || !strings.Contains(err.Error(), "BotNotInConversationRoster") || strings.Contains(err.Error(), appPass) || strings.Contains(err.Error(), "confidential") || strings.Contains(err.Error(), "access-") {
		t.Errorf("error = %v", err)
	}
	// A wrong secret: the login service's description echoes it back; only its code survives.
	c.Secret[KeyAppPassword] = []byte("wrong")
	a2 := w.adapter()
	err = a2.Reply(context.Background(), c, replyTarget(w, convDM, ""), inbound.Result{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), "AADSTS7000215") || strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), appPass) {
		t.Errorf("login error = %v", err)
	}
	// Bad targets are refused before any call.
	for _, bad := range []string{`{}`, `{"u":"https://evil.example/","c":"x"}`, `{"u":"` + w.serviceURL() + `"}`, `nope`} {
		n := len(w.connCalls)
		if err := a.Reply(context.Background(), conn(t, defaultSettings, false), json.RawMessage(bad), inbound.Result{Text: "x"}); err == nil || len(w.connCalls) != n {
			t.Errorf("target %s: err=%v", bad, err)
		}
	}
}

func TestWorkingIndicatorSendsTypingOnlyWhenAsked(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), conn(t, defaultSettings, false)
	if err := a.Working(context.Background(), c, replyTarget(w, convChan+";messageid=5", "5")); err != nil {
		t.Fatal(err)
	}
	cl := w.connCalls[0]
	if cl.body["type"] != "typing" || strings.Contains(cl.path, "/activities/5") {
		t.Errorf("typing = %+v", cl)
	}
	c.Config = json.RawMessage(`{}`)
	n := len(w.connCalls)
	_ = a.Working(context.Background(), c, replyTarget(w, convDM, ""))
	if len(w.connCalls) != n {
		t.Error("typing sent although not asked for")
	}
}

func TestTestProvesCredentialsWithoutLeakingThem(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), conn(t, defaultSettings, true)
	rep := a.Test(context.Background(), c)
	if !rep.OK || rep.Details["appId"] != appID || rep.Details["registration"] != "single-tenant" {
		t.Fatalf("report = %+v", rep)
	}
	if dump, _ := json.Marshal(rep); strings.Contains(string(dump), appPass) || strings.Contains(string(dump), "access-") {
		t.Error("the report holds a secret")
	}
	c.Secret[KeyAppPassword] = []byte("wrong")
	rep = a.Test(context.Background(), c)
	if rep.OK || !strings.Contains(rep.Error, "AADSTS7000215") || strings.Contains(rep.Error, "wrong") {
		t.Errorf("bad secret = %+v", rep)
	}
	c3 := conn(t, defaultSettings, false)
	c3.Secret = inbound.Secret{KeyAppID: []byte(appID)}
	if rep := a.Test(context.Background(), c3); rep.OK {
		t.Error("a connection without a password passed")
	}
	w.loginFail = func(rw http.ResponseWriter) { rw.Header().Set("Retry-After", "900"); rw.WriteHeader(429) }
	c.Secret[KeyAppPassword] = []byte(appPass)
	if rep := a.Test(context.Background(), c); rep.OK || !strings.Contains(rep.Error, "wait") {
		t.Errorf("throttled login = %+v", rep)
	}
}

func TestTokenValueNeverPrints(t *testing.T) {
	w := newWorld(t)
	a, c := w.adapter(), conn(t, defaultSettings, false)
	tok, err := a.token(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.Join([]string{tok.String()}, ""); strings.Contains(s, "access-") {
		t.Errorf("token prints: %s", s)
	}
}

func TestConfigValidationAndDescriptor(t *testing.T) {
	a := &Adapter{}
	for _, bad := range []string{`{"unknown":1}`, `{"typingIndicator":"yes"}`} {
		if a.ValidateConfig(json.RawMessage(bad)) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{``, `{}`, `{"typingIndicator":true}`} {
		if err := a.ValidateConfig(json.RawMessage(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	d := a.Describe()
	if d.Title != "Microsoft Teams" || len(d.Setup) == 0 || !strings.Contains(strings.Join(d.Setup, " "), "{{url}}") || len(d.SecretKeys) != 3 {
		t.Errorf("descriptor = %+v", d)
	}
	if err := inbound.CheckSecret(a, map[string][]byte{KeyAppID: []byte("x")}); err == nil || strings.Contains(err.Error(), "x\"") {
		t.Errorf("secret check = %v", err)
	}
	caps := a.Capabilities()
	if !caps.AckFirst || caps.Dialect != inbound.DialectMarkdown || !caps.Threads || len(caps.Modes) != 3 {
		t.Errorf("caps = %+v", caps)
	}
	s, _ := inbound.ParseSettings([]byte(`{"allowSenders":["u"],"modes":["command"]}`))
	if inbound.CheckSettings(a, s) == nil {
		t.Error("a command mode was accepted")
	}
	s, _ = inbound.ParseSettings([]byte(`{"allowSenders":["u"],"modes":["channel"]}`))
	if inbound.CheckSettings(a, s) == nil {
		t.Error("channel mode without channels passed")
	}
	if got := a.AckResponse(inbound.Message{}, inbound.OutcomeRejected); got == nil || got.Status != 200 || len(got.Body) != 0 {
		t.Errorf("ack = %+v", got)
	}
}
