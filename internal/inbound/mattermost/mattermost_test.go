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

package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

const (
	botTok  = "bot-access-token-secret"
	hookTok = "webhook-token-1"
	teamID  = "teamteamteamteamteamteam12"
	chanID  = "chanchanchanchanchanchan12"
	userID  = "useruseruseruseruseruser12"
	botID   = "botbotbotbotbotbotbotbot12"
)

func noWait(context.Context, time.Duration) error { return nil }

func adapter() *Adapter {
	return &Adapter{Client: &chat.Client{AllowPrivate: true, Wait: noWait}}
}

func conn(t *testing.T, base, settings string) inbound.Connection {
	t.Helper()
	s, err := inbound.ParseSettings([]byte(settings))
	if err != nil {
		t.Fatal(err)
	}
	return inbound.Connection{
		ID: "inb_" + strings.Repeat("d", 32), Kind: Kind, Settings: s, Config: json.RawMessage(`{}`),
		Secret: inbound.Secret{KeyBaseURL: []byte(base), KeyBotToken: []byte(botTok), KeyTokens: []byte("other-token\n" + hookTok), KeyInsecure: []byte("true")},
	}
}

const settingsDefault = `{"allowSenders":["` + userID + `"],"allowChannels":["` + chanID + `"]}`

func form(v url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/inbound/mattermost/x", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func hook(extra url.Values) url.Values {
	v := url.Values{"token": {hookTok}, "team_id": {teamID}, "channel_id": {chanID}, "user_id": {userID}, "user_name": {"ann"}, "post_id": {"postpostpostpostpostpost12"},
		"text": {"@agent what is up"}, "trigger_word": {"@agent"}}
	maps.Copy(v, extra)
	return v
}

type fakeMM struct {
	mu     sync.Mutex
	calls  []call
	script map[int]func(w http.ResponseWriter)
	post   string // JSON of GET /posts/{id}
	srv    *httptest.Server
}

type call struct {
	method, path, auth string
	body               map[string]any
}

func newFake(t *testing.T) *fakeMM {
	f := &fakeMM{post: `{"id":"p","root_id":"","type":"","props":{}}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		i := len(f.calls) - 1
		f.mu.Unlock()
		if fn := f.script[i]; fn != nil {
			fn(w)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/chat") // a sub-path install
		switch {
		case path == "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"` + botID + `","username":"orca-bot","is_bot":true}`))
		case strings.HasPrefix(path, "/api/v4/teams/"):
			_, _ = w.Write([]byte(`{"display_name":"Acme Team"}`))
		case strings.HasPrefix(path, "/api/v4/posts/"):
			_, _ = w.Write([]byte(f.post))
		case path == "/api/v4/posts":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"new"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestTokenIsCheckedInConstantTimeAgainstTheSecret(t *testing.T) {
	a, c := adapter(), conn(t, "https://mm.example", settingsDefault)
	if _, err := a.Receive(context.Background(), c, form(hook(nil))); err != nil {
		t.Fatalf("a valid webhook: %v", err)
	}
	// The second of two configured tokens is as good as the first.
	if _, err := a.Receive(context.Background(), c, form(hook(url.Values{"token": {"other-token"}}))); err != nil {
		t.Errorf("first token: %v", err)
	}
	var auth *inbound.AuthError
	for name, tok := range map[string]string{"wrong": "nope", "empty": "", "prefix of a token": hookTok[:5], "token plus": hookTok + "x"} {
		if _, err := a.Receive(context.Background(), c, form(hook(url.Values{"token": {tok}}))); !errors.As(err, &auth) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c.Secret[KeyTokens] = nil
	if _, err := a.Receive(context.Background(), c, form(hook(nil))); !errors.As(err, &auth) {
		t.Errorf("no tokens configured: %v", err)
	}
	// JSON bodies work too.
	c = conn(t, "https://mm.example", settingsDefault)
	b, _ := json.Marshal(map[string]string{"token": hookTok, "team_id": teamID, "channel_id": chanID, "user_id": userID, "post_id": "pp", "text": "@a hi", "trigger_word": "@a"})
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	if m, err := a.Receive(context.Background(), c, r); err != nil || m.Mode != inbound.ModeMention {
		t.Errorf("json: %+v %v", m, err)
	}
}

func TestWebhookBecomesAMentionMessage(t *testing.T) {
	a, c := adapter(), conn(t, "https://mm.example", settingsDefault)
	m, err := a.Receive(context.Background(), c, form(hook(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != inbound.ModeMention || m.Text != "what is up" || m.Sender.ID != userID || m.Sender.Group != teamID || m.Channel != chanID ||
		m.MessageID != "postpostpostpostpostpost12" || m.Sender.Display != "ann" {
		t.Errorf("message = %+v", m)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Channel != chanID || to.Root != "postpostpostpostpostpost12" {
		t.Errorf("target = %+v", to)
	}
	// A retry of the same post has the same key.
	m2, _ := a.Receive(context.Background(), c, form(hook(nil)))
	if m2.MessageID != m.MessageID {
		t.Error("a redelivery has another key")
	}
}

func TestModesTeamAndEmpty(t *testing.T) {
	a, c := adapter(), conn(t, "https://mm.example", settingsDefault)
	var rej *inbound.Rejection
	// No trigger word: every post of the channel; off unless the connection says so.
	plain := hook(url.Values{"trigger_word": {""}})
	if _, err := a.Receive(context.Background(), c, form(plain)); !errors.As(err, &rej) || !rej.Ignore {
		t.Errorf("channel mode off: %v", err)
	}
	on := conn(t, "https://mm.example", `{"allowChannels":["`+chanID+`"],"modes":["channel"]}`)
	if m, err := a.Receive(context.Background(), on, form(plain)); err != nil || m.Mode != inbound.ModeChannel {
		t.Errorf("channel mode on: %+v %v", m, err)
	}
	// Slash command.
	cmd := form(url.Values{"token": {hookTok}, "team_id": {teamID}, "channel_id": {chanID}, "user_id": {userID}, "command": {"/agent"}, "text": {"do it"}, "trigger_id": {"trig1"}})
	m, err := a.Receive(context.Background(), c, cmd)
	if err != nil || m.Mode != inbound.ModeCommand || m.MessageID != "cmd:trig1" || m.Text != "do it" {
		t.Fatalf("command = %+v %v", m, err)
	}
	if ack := a.AckResponse(m, inbound.OutcomeStarted); !strings.Contains(string(ack.Body), "ephemeral") {
		t.Errorf("ack = %+v", ack)
	}
	if ack := a.AckResponse(m, inbound.OutcomeRejected); len(ack.Body) != 0 {
		t.Errorf("a refused command was told something: %s", ack.Body)
	}
	// A configured team: others are ignored.
	c.Config = json.RawMessage(`{"teamId":"` + teamID + `"}`)
	if _, err := a.Receive(context.Background(), c, form(hook(url.Values{"team_id": {"otherotherotherotherother12"}}))); !errors.As(err, &rej) {
		t.Errorf("other team: %v", err)
	}
	// Nothing after the trigger word.
	if _, err := a.Receive(context.Background(), c, form(hook(url.Values{"text": {"@agent"}}))); !errors.As(err, &rej) {
		t.Errorf("empty: %v", err)
	}
	// Allow-lists through the core rules.
	caps := a.Capabilities()
	good, _ := a.Receive(context.Background(), c, form(hook(nil)))
	if why := c.Settings.Refusal(caps, good); why != "" {
		t.Errorf("allowed: %s", why)
	}
	good.Channel = "elsewhereelsewhereelsewh12"
	if why := c.Settings.Refusal(caps, good); why != "channel_not_allowed" {
		t.Errorf("other channel: %s", why)
	}
}

func TestEnrichFindsTheThreadAndBotsAndOwnBot(t *testing.T) {
	f := newFake(t)
	a, c := adapter(), conn(t, f.srv.URL, settingsDefault)
	recv := func() inbound.Message {
		m, err := a.Receive(context.Background(), c, form(hook(nil)))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// A post inside a thread: the conversation is the root's.
	f.post = `{"id":"p","root_id":"rootrootrootrootrootroot12","type":"","props":{}}`
	m := recv()
	a.Enrich(context.Background(), c, &m)
	if m.ThreadID != teamID+"/"+chanID+"/rootrootrootrootrootroot12" || m.FromBot {
		t.Errorf("thread = %q bot=%v", m.ThreadID, m.FromBot)
	}
	if f.calls[0].auth != "Bearer "+botTok {
		t.Error("the lookup did not use the bot token")
	}
	// A post by another integration, a system post, and the bot's own user.
	for name, post := range map[string]string{
		"integration": `{"root_id":"","type":"","props":{"from_webhook":"true"}}`,
		"bot":         `{"root_id":"","type":"","props":{"from_bot":"true"}}`,
		"system":      `{"root_id":"","type":"system_join_channel","props":{}}`,
	} {
		f.post = post
		m := recv()
		a.Enrich(context.Background(), c, &m)
		if !m.FromBot {
			t.Errorf("%s post was not marked as a bot", name)
		}
	}
	own := hook(url.Values{"user_id": {botID}})
	m, _ = a.Receive(context.Background(), c, form(own))
	a.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Error("the bot's own post was not marked")
	}
	// A failed lookup leaves the message alone.
	f.srv.Close()
	m = recv()
	m.FromBot = false
	a.Enrich(context.Background(), c, &m)
	if m.FromBot || m.ThreadID != teamID+"/"+chanID+"/postpostpostpostpostpost12" {
		t.Errorf("after a failed lookup: %+v", m)
	}
}

func TestReplyPostsMarkdownUnderTheMessageAndCannotPing(t *testing.T) {
	f := newFake(t)
	a, c := adapter(), conn(t, f.srv.URL+"/chat/", settingsDefault) // a sub-path install
	to, _ := json.Marshal(target{Channel: chanID, Root: "rootid"})
	text := "**Done** @all @channel @here @ann and [x](https://e.example/@here) `@here`\n\n- a"
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: text}); err != nil {
		t.Fatal(err)
	}
	got := f.calls[0]
	if got.method != "POST" || got.path != "/chat/api/v4/posts" || got.auth != "Bearer "+botTok || got.body["channel_id"] != chanID || got.body["root_id"] != "rootid" {
		t.Fatalf("call = %+v", got)
	}
	msg := got.body["message"].(string)
	for _, bad := range []string{" @all", " @channel", " @here ", " @ann"} {
		if strings.Contains(msg, bad) {
			t.Errorf("%q still pings in %q", bad, msg)
		}
	}
	if !strings.Contains(msg, "**Done**") || !strings.Contains(msg, "https://e.example/@here") {
		t.Errorf("markdown or link damaged: %q", msg)
	}
	// A slash command's answer has no root: it goes to the channel.
	to, _ = json.Marshal(target{Channel: chanID})
	_ = a.Reply(context.Background(), c, to, inbound.Result{Text: "hi"})
	if _, has := f.calls[1].body["root_id"]; has {
		t.Error("root_id set for a command's answer")
	}
}

func TestLongRepliesSplitRateLimitsAndErrors(t *testing.T) {
	f := newFake(t)
	var waited []time.Duration
	a, c := adapter(), conn(t, f.srv.URL, settingsDefault)
	a.Client.Wait = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	to, _ := json.Marshal(target{Channel: chanID, Root: "r"})
	f.script = map[int]func(http.ResponseWriter){0: func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}}
	long := strings.Repeat(strings.Repeat("word ", 150)+"\n\n", 40) // ~30000 characters
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: long}); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 2*time.Second {
		t.Errorf("waited = %v", waited)
	}
	if posts := len(f.calls) - 1; posts != maxReplyParts {
		t.Errorf("posts = %d, want the cap %d", posts, maxReplyParts)
	}
	for _, cl := range f.calls[1:] {
		if n := len([]rune(cl.body["message"].(string))); n > maxReplyChars {
			t.Errorf("a post of %d characters", n)
		}
	}
	// An error answer names the status only.
	f2 := newFake(t)
	f2.script = map[int]func(http.ResponseWriter){0: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(botTok + " confidential"))
	}}
	c2 := conn(t, f2.srv.URL, settingsDefault)
	err := a.Reply(context.Background(), c2, to, inbound.Result{Text: "confidential answer"})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), botTok) || strings.Contains(err.Error(), "confidential") {
		t.Errorf("error = %v", err)
	}
}

func TestSSRFGuardAndAllowPrivateLiveInTheSecret(t *testing.T) {
	f := newFake(t) // on 127.0.0.1
	to, _ := json.Marshal(target{Channel: chanID})
	a := &Adapter{} // the default, guarded client
	c := conn(t, f.srv.URL, settingsDefault)
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: "x"}); err == nil || len(f.calls) != 0 {
		t.Errorf("loopback reached without allowPrivateAddresses: err=%v calls=%d", err, len(f.calls))
	}
	c.Secret[KeyAllowPrivate] = []byte("true")
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: "x"}); err != nil || len(f.calls) != 1 {
		t.Errorf("with allowPrivateAddresses: err=%v calls=%d", err, len(f.calls))
	}
	// The address cannot come from the builder's settings: it is not a setting.
	if (&Adapter{}).ValidateConfig(json.RawMessage(`{"baseUrl":"https://evil.example"}`)) == nil {
		t.Error("a baseUrl in the settings was accepted")
	}
}

func TestCredentials(t *testing.T) {
	ok := inbound.Secret{KeyBaseURL: []byte("https://mm.example/sub/"), KeyBotToken: []byte("t"), KeyTokens: []byte("a,b")}
	cr, err := ParseCredentials(ok)
	if err != nil || cr.BaseURL != "https://mm.example/sub" || len(cr.Tokens) != 2 || cr.AllowPrivate {
		t.Errorf("creds = %+v %v", cr, err)
	}
	for name, s := range map[string]inbound.Secret{
		"no url":     {KeyBotToken: []byte("t")},
		"http":       {KeyBaseURL: []byte("http://mm.example"), KeyBotToken: []byte("t")},
		"userinfo":   {KeyBaseURL: []byte("https://u:p@mm.example"), KeyBotToken: []byte("t")},
		"no bot":     {KeyBaseURL: []byte("https://mm.example")},
		"javascript": {KeyBaseURL: []byte("javascript:alert(1)"), KeyBotToken: []byte("t")},
	} {
		if _, err := ParseCredentials(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseCredentials(inbound.Secret{KeyBaseURL: []byte("http://mm.example"), KeyBotToken: []byte("t"), KeyInsecure: []byte("true")}); err != nil {
		t.Errorf("insecure http: %v", err)
	}
}

func TestTestShowsBotAndTeamWithoutSecrets(t *testing.T) {
	f := newFake(t)
	a, c := adapter(), conn(t, f.srv.URL, settingsDefault)
	c.Config = json.RawMessage(`{"teamId":"` + teamID + `"}`)
	rep := a.Test(context.Background(), c)
	if !rep.OK || rep.Details["bot"] != "orca-bot" || rep.Details["team"] != "Acme Team" || rep.Details["tokens"] != "2 configured" {
		t.Fatalf("report = %+v", rep)
	}
	dump, _ := json.Marshal(rep)
	for _, s := range []string{botTok, hookTok, "other-token"} {
		if strings.Contains(string(dump), s) {
			t.Errorf("the report holds %q", s)
		}
	}
	f.script = map[int]func(http.ResponseWriter){len(f.calls): func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }}
	if rep := a.Test(context.Background(), c); rep.OK || !strings.Contains(rep.Error, "401") {
		t.Errorf("bad token = %+v", rep)
	}
	// The bot's id is remembered for loop prevention.
	m, _ := a.Receive(context.Background(), c, form(hook(url.Values{"user_id": {botID}})))
	if !m.FromBot {
		t.Error("the bot's own id from the test was not used")
	}
}

func TestConfigAndDescriptor(t *testing.T) {
	a := &Adapter{}
	for _, bad := range []string{`{"teamId":"short"}`, `{"x":1}`} {
		if a.ValidateConfig(json.RawMessage(bad)) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, good := range []string{``, `{}`, `{"teamId":"` + teamID + `"}`} {
		if err := a.ValidateConfig(json.RawMessage(good)); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	d := a.Describe()
	if d.Title != "Mattermost" || len(d.SecretKeys) < 3 || !strings.Contains(strings.Join(d.Setup, " "), "{{url}}") {
		t.Errorf("descriptor = %+v", d)
	}
	if caps := a.Capabilities(); !caps.AckFirst || caps.Dialect != inbound.DialectMarkdown {
		t.Errorf("caps = %+v", caps)
	}
}
