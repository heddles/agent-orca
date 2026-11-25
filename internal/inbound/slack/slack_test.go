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

package slack

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

const (
	ts1     = "1700.01"
	signing = "8f742231b10e8888abcd99yyyzzz85a5"
	botTok  = "xoxb-0000-secret-token"
	team    = "T0TEAM"
)

var clock = time.Unix(1_700_000_000, 0)

func conn(t *testing.T, settings string) inbound.Connection {
	t.Helper()
	s, err := inbound.ParseSettings([]byte(settings))
	if err != nil {
		t.Fatal(err)
	}
	return inbound.Connection{
		ID: "inb_" + strings.Repeat("c", 32), Kind: Kind, Settings: s, Config: json.RawMessage(`{"teamId":"` + team + `","workingReaction":true}`),
		Secret: inbound.Secret{KeyBotToken: []byte(botTok), KeySigningSecret: []byte(signing)},
	}
}

const defaultSettings = `{"allowSenders":["U1"],"allowChannels":["C1","D1"]}`

func adapter(api string) *Adapter {
	return &Adapter{
		APIBase: api, Now: func() time.Time { return clock },
		Client: &chat.Client{AllowPrivate: true, Wait: func(context.Context, time.Duration) error { return nil }},
	}
}

func signedReq(body, contentType string, mutate ...func(*http.Request)) *http.Request {
	ts := strconv.FormatInt(clock.Unix(), 10)
	sig := "v0=" + hex.EncodeToString(verify.HMACSHA256([]byte(signing), []byte("v0:"+ts+":"+body)))
	r := httptest.NewRequest(http.MethodPost, "/inbound/slack/x", strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", sig)
	for _, m := range mutate {
		m(r)
	}
	return r
}

func eventBody(ev map[string]any, extra map[string]any) string {
	m := map[string]any{"type": "event_callback", "team_id": team, "event_id": "Ev1", "event": ev}
	maps.Copy(m, extra)
	b, _ := json.Marshal(m)
	return string(b)
}

func receive(t *testing.T, a *Adapter, c inbound.Connection, body string) (inbound.Message, error) {
	t.Helper()
	return a.Receive(context.Background(), c, signedReq(body, "application/json"))
}

func TestSignature(t *testing.T) {
	a, c := adapter(""), conn(t, defaultSettings)
	body := eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1.1", "text": "<@UBOT> hi"}, nil)
	if _, err := receive(t, a, c, body); err != nil {
		t.Fatalf("a signed request: %v", err)
	}
	var auth *inbound.AuthError
	for name, r := range map[string]*http.Request{
		"wrong signature": signedReq(body, "application/json", func(r *http.Request) { r.Header.Set("X-Slack-Signature", "v0="+strings.Repeat("0", 64)) }),
		"no signature":    signedReq(body, "application/json", func(r *http.Request) { r.Header.Del("X-Slack-Signature") }),
		"no v0 prefix": signedReq(body, "application/json", func(r *http.Request) {
			r.Header.Set("X-Slack-Signature", strings.TrimPrefix(r.Header.Get("X-Slack-Signature"), "v0="))
		}),
		"old timestamp": signedReq(body, "application/json", func(r *http.Request) {
			r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(clock.Unix()-301, 10))
		}),
		"future": signedReq(body, "application/json", func(r *http.Request) {
			r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(clock.Unix()+301, 10))
		}),
		"no timestamp": signedReq(body, "application/json", func(r *http.Request) { r.Header.Del("X-Slack-Request-Timestamp") }),
		"tampered body": func() *http.Request {
			r := signedReq(body, "application/json")
			r.Body = io.NopCloser(strings.NewReader(body + " "))
			return r
		}(),
	} {
		if _, err := a.Receive(context.Background(), c, r); !errors.As(err, &auth) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Four minutes old is inside the window.
	r := signedReq(body, "application/json", func(r *http.Request) {
		ts := strconv.FormatInt(clock.Unix()-240, 10)
		r.Header.Set("X-Slack-Request-Timestamp", ts)
		r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(verify.HMACSHA256([]byte(signing), []byte("v0:"+ts+":"+body))))
	})
	if _, err := a.Receive(context.Background(), c, r); err != nil {
		t.Errorf("inside the window: %v", err)
	}
	c.Secret = inbound.Secret{}
	if _, err := a.Receive(context.Background(), c, signedReq(body, "application/json")); !errors.As(err, &auth) {
		t.Errorf("no secret: %v", err)
	}
}

func TestURLVerificationChallenge(t *testing.T) {
	a, c := adapter(""), conn(t, defaultSettings)
	_, err := receive(t, a, c, `{"type":"url_verification","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","token":"x"}`)
	var resp *inbound.Response
	if !errors.As(err, &resp) || string(resp.Body) != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" || resp.Status != 200 {
		t.Fatalf("challenge = %v", err)
	}
	// An unsigned challenge is not answered: it would prove nothing.
	var auth *inbound.AuthError
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"type":"url_verification","challenge":"c"}`))
	if _, err := a.Receive(context.Background(), c, r); !errors.As(err, &auth) {
		t.Errorf("unsigned challenge: %v", err)
	}
}

func TestMentionBecomesAMessageInAThread(t *testing.T) {
	a, c := adapter(""), conn(t, defaultSettings)
	m, err := receive(t, a, c, eventBody(map[string]any{
		"type": "app_mention", "user": "U1", "channel": "C1", "ts": ts1, "text": "<@UBOT> summarise <https://e.example/x|the doc> &amp; <!here> for <@U2>",
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != inbound.ModeMention || m.Sender.ID != "U1" || m.Sender.Group != team || m.Channel != "C1" || m.MessageID != team+":C1:1700.01" {
		t.Errorf("message = %+v", m)
	}
	if m.Text != "summarise the doc (https://e.example/x) & @here for @U2" {
		t.Errorf("text = %q", m.Text)
	}
	if m.ThreadID != team+"/C1/1700.01" {
		t.Errorf("thread = %q", m.ThreadID)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Channel != "C1" || to.ThreadTS != ts1 {
		t.Errorf("target = %+v", to)
	}
	// A mention inside a thread continues that thread's conversation.
	m2, _ := receive(t, a, c, eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1700.09", "thread_ts": ts1, "text": "<@UBOT> and more"}, map[string]any{"event_id": "Ev2"}))
	if m2.ThreadID != m.ThreadID {
		t.Errorf("thread of the follow-up = %q", m2.ThreadID)
	}
	// The same channel and thread id in another channel is another conversation.
	m3, _ := receive(t, a, c, eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C2", "ts": "1700.09", "thread_ts": ts1, "text": "<@UBOT> x"}, nil))
	if m3.ThreadID == m.ThreadID {
		t.Error("two channels share a conversation")
	}
}

func TestDirectMessageIsOneFlatConversation(t *testing.T) {
	a, c := adapter(""), conn(t, defaultSettings)
	m, err := receive(t, a, c, eventBody(map[string]any{"type": "message", "channel_type": "im", "user": "U1", "channel": "D1", "ts": "5.5", "text": "hello"}, nil))
	if err != nil || m.Mode != inbound.ModeDirect || m.ThreadID != team+"/D1/dm" {
		t.Fatalf("dm = %+v %v", m, err)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.ThreadTS != "" {
		t.Errorf("a direct message was answered in a thread: %+v", to)
	}
}

func TestBotsAndOwnMessagesAreIgnored(t *testing.T) {
	a, c := adapter(""), conn(t, defaultSettings)
	cases := map[string]map[string]any{
		"bot_id":      {"type": "message", "channel_type": "im", "user": "U1", "bot_id": "B1", "channel": "D1", "ts": "1.1", "text": "x"},
		"subtype":     {"type": "message", "channel_type": "im", "user": "U1", "subtype": "message_changed", "channel": "D1", "ts": "1.2", "text": "x"},
		"no user":     {"type": "message", "channel_type": "im", "channel": "D1", "ts": "1.3", "text": "x"},
		"bot subtype": {"type": "app_mention", "user": "U1", "subtype": "bot_message", "channel": "C1", "ts": "1.4", "text": "x"},
	}
	for name, ev := range cases {
		m, err := receive(t, a, c, eventBody(ev, nil))
		if err != nil || !m.FromBot || !strings.HasPrefix(m.MessageID, "ignored:") {
			t.Errorf("%s: %+v %v", name, m, err)
		}
	}
	// Our own user id, known from the event's authorizations, is never answered.
	body := eventBody(map[string]any{"type": "app_mention", "user": "UBOT", "channel": "C1", "ts": "2.1", "text": "<@UBOT> x"},
		map[string]any{"authorizations": []map[string]any{{"user_id": "UBOT", "is_bot": true}}})
	if m, err := receive(t, a, c, body); err != nil || !m.FromBot {
		t.Errorf("own message: %+v %v", m, err)
	}
	// Unhandled events and other workspaces are ignored quietly.
	for name, body := range map[string]string{
		"other event":     eventBody(map[string]any{"type": "reaction_added", "user": "U1"}, nil),
		"other workspace": eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1.1", "text": "x"}, map[string]any{"team_id": "T0OTHER"}),
		"group dm":        eventBody(map[string]any{"type": "message", "channel_type": "mpim", "user": "U1", "channel": "G1", "ts": "1.1", "text": "x"}, nil),
		"empty mention":   eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1.1", "text": "<@UBOT>"}, nil),
	} {
		_, err := receive(t, a, c, body)
		var rej *inbound.Rejection
		if !errors.As(err, &rej) || !rej.Ignore {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestChannelMessagesOnlyWhenTheModeIsOnAndDeduplicateWithTheMention(t *testing.T) {
	a := adapter("")
	off := conn(t, defaultSettings)
	ev := map[string]any{"type": "message", "channel_type": "channel", "user": "U1", "channel": "C1", "ts": "9.9", "text": "<@UBOT> hi"}
	m, err := receive(t, a, off, eventBody(ev, nil))
	var rej *inbound.Rejection
	if !errors.As(err, &rej) || !rej.Ignore || m.MessageID == team+":C1:9.9" {
		t.Fatalf("channel mode off: %+v %v", m, err)
	}
	on := conn(t, `{"allowChannels":["C1"],"modes":["mention","channel"]}`)
	chanMsg, err := receive(t, a, on, eventBody(ev, nil))
	if err != nil || chanMsg.Mode != inbound.ModeChannel {
		t.Fatalf("channel mode on: %+v %v", chanMsg, err)
	}
	// The same message also arrives as app_mention with another event id: same key, one run.
	mention, _ := receive(t, a, on, eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "9.9", "text": "<@UBOT> hi"}, map[string]any{"event_id": "Ev99"}))
	if mention.MessageID != chanMsg.MessageID {
		t.Errorf("keys differ: %q %q", mention.MessageID, chanMsg.MessageID)
	}
	// A Slack retry (same event, retry headers) has the same key too.
	retry, _ := a.Receive(context.Background(), on, signedReq(eventBody(ev, nil), "application/json", func(r *http.Request) {
		r.Header.Set("X-Slack-Retry-Num", "1")
		r.Header.Set("X-Slack-Retry-Reason", "http_timeout")
	}))
	if retry.MessageID != chanMsg.MessageID {
		t.Errorf("a retry has another key: %q", retry.MessageID)
	}
}

func TestAllowListsThroughTheCoreRules(t *testing.T) {
	a := adapter("")
	c := conn(t, `{"allowSenders":["U1"],"allowGroups":["T0OTHERALLOWED"],"allowChannels":["C1"]}`)
	caps := a.Capabilities()
	m, _ := receive(t, a, c, eventBody(map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1.1", "text": "<@B> x"}, nil))
	if why := c.Settings.Refusal(caps, m); why != "" {
		t.Errorf("allowed user and channel: %s", why)
	}
	m.Channel = "C2"
	if why := c.Settings.Refusal(caps, m); why != "channel_not_allowed" {
		t.Errorf("other channel: %q", why)
	}
	m.Channel, m.Sender.ID = "C1", "U9"
	if why := c.Settings.Refusal(caps, m); why != "sender_not_allowed" {
		t.Errorf("other user: %q", why)
	}
	m.Sender.Group = "T0OTHERALLOWED"
	if why := c.Settings.Refusal(caps, m); why != "" {
		t.Errorf("allowed workspace: %q", why)
	}
}

func TestSlashCommand(t *testing.T) {
	a, c := adapter(""), conn(t, defaultSettings)
	form := url.Values{"team_id": {team}, "channel_id": {"C1"}, "user_id": {"U1"}, "user_name": {"ann"}, "command": {"/agent"}, "text": {"what &amp; why"}, "trigger_id": {"trig.1"}}.Encode()
	m, err := a.Receive(context.Background(), c, signedReq(form, "application/x-www-form-urlencoded"))
	if err != nil || m.Mode != inbound.ModeCommand || m.MessageID != "cmd:"+team+":trig.1" || m.Text != "what & why" || m.Sender.Display != "ann" {
		t.Fatalf("command = %+v %v", m, err)
	}
	ack := a.AckResponse(m, inbound.OutcomeStarted)
	if ack == nil || !strings.Contains(string(ack.Body), "ephemeral") {
		t.Errorf("ack = %+v", ack)
	}
	if ack := a.AckResponse(m, inbound.OutcomeRejected); ack == nil || len(ack.Body) != 0 {
		t.Errorf("a refused command was told something: %+v", ack)
	}
	if a.AckResponse(inbound.Message{Mode: inbound.ModeMention}, inbound.OutcomeStarted) != nil {
		t.Error("an event got a command ack")
	}
	other := strings.Replace(form, team, "T0OTHER", 1)
	if _, err := a.Receive(context.Background(), c, signedReq(other, "application/x-www-form-urlencoded")); err == nil {
		t.Error("a command of another workspace was accepted")
	}
}

// ── the Web API ──────────────────────────────────────────────────────────────

type fakeSlack struct {
	mu    sync.Mutex
	calls []call
	// script answers per call index; the default is {"ok":true}
	script []func(w http.ResponseWriter)
	srv    *httptest.Server
}

type call struct {
	method string
	auth   string
	body   map[string]any
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{method: strings.TrimPrefix(r.URL.Path, "/"), auth: r.Header.Get("Authorization"), body: body})
		i := len(f.calls) - 1
		f.mu.Unlock()
		if i < len(f.script) && f.script[i] != nil {
			f.script[i](w)
			return
		}
		if strings.HasSuffix(r.URL.Path, "auth.test") {
			w.Header().Set("X-OAuth-Scopes", "chat:write,app_mentions:read,im:history")
			_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","team_id":"` + team + `","user":"orca","user_id":"UBOT","bot_id":"B1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestReplyIsThreadedFormattedAndCannotPing(t *testing.T) {
	f := newFakeSlack(t)
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	to, _ := json.Marshal(target{Channel: "C1", ThreadTS: ts1})
	text := "**Done** <!channel> @here <@U2> see [docs](https://e.example/a?b=1&c=2)\n\n- one\n- two"
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: text}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	got := f.calls[0]
	if got.method != "chat.postMessage" || got.auth != "Bearer "+botTok || got.body["channel"] != "C1" || got.body["thread_ts"] != ts1 ||
		got.body["unfurl_links"] != false || got.body["link_names"] != false || got.body["parse"] != "none" {
		t.Errorf("call = %+v", got)
	}
	msg := got.body["text"].(string)
	for _, bad := range []string{"<!channel>", "@here", "<@U2>"} {
		if strings.Contains(msg, bad) {
			t.Errorf("the message still holds %q: %q", bad, msg)
		}
	}
	for _, want := range []string{"*Done*", "<https://e.example/a?b=1&amp;c=2|docs>", "• one"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	// No thread: no thread_ts.
	to, _ = json.Marshal(target{Channel: "D1"})
	_ = a.Reply(context.Background(), c, to, inbound.Result{Text: "hi"})
	if _, has := f.calls[1].body["thread_ts"]; has {
		t.Error("thread_ts set for a flat reply")
	}
}

func TestLongReplySplitsIntoOrderedMessagesUpToTheCap(t *testing.T) {
	f := newFakeSlack(t)
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	to, _ := json.Marshal(target{Channel: "C1", ThreadTS: "1.1"})
	para := strings.Repeat("sentence of words. ", 60) // ~1100 chars
	long := strings.Repeat(para+"\n\n", 30)           // ~13000 chars
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: long}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != maxReplyParts {
		t.Fatalf("messages = %d, want the cap of %d", len(f.calls), maxReplyParts)
	}
	for _, cl := range f.calls {
		if n := len([]rune(cl.body["text"].(string))); n > maxReplyChars {
			t.Errorf("a message of %d characters", n)
		}
		if cl.body["thread_ts"] != "1.1" {
			t.Error("a part left the thread")
		}
	}
	if !strings.Contains(f.calls[len(f.calls)-1].body["text"].(string), "shortened") {
		t.Error("the cut was not announced")
	}
	// A reply that fits is one message.
	f.calls = nil
	_ = a.Reply(context.Background(), c, to, inbound.Result{Text: "short"})
	if len(f.calls) != 1 {
		t.Errorf("short reply = %d messages", len(f.calls))
	}
}

func TestRateLimitRetryAfterIsHonouredThenSucceeds(t *testing.T) {
	f := newFakeSlack(t)
	f.script = []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}}
	var waited []time.Duration
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	a.Client.Wait = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	to, _ := json.Marshal(target{Channel: "C1"})
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || len(waited) != 1 || waited[0] != 3*time.Second {
		t.Errorf("calls=%d waited=%v", len(f.calls), waited)
	}
	// A long Retry-After is handed back to the core, whose delivery retries later.
	f2 := newFakeSlack(t)
	f2.script = []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
	}}
	a2 := adapter(f2.srv.URL)
	err := a2.Reply(context.Background(), c, to, inbound.Result{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "wait") || len(f2.calls) != 1 {
		t.Errorf("long retry-after: err=%v calls=%d", err, len(f2.calls))
	}
}

func TestApiErrorsNameTheCodeNeverTheTokenOrText(t *testing.T) {
	f := newFakeSlack(t)
	f.script = []func(http.ResponseWriter){func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel","detail":"` + botTok + ` secret text"}`))
	}}
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	to, _ := json.Marshal(target{Channel: "C1"})
	err := a.Reply(context.Background(), c, to, inbound.Result{Text: "confidential answer"})
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") || strings.Contains(err.Error(), botTok) || strings.Contains(err.Error(), "confidential") {
		t.Errorf("error = %v", err)
	}
	if err := a.Reply(context.Background(), c, json.RawMessage(`{}`), inbound.Result{Text: "x"}); err == nil {
		t.Error("a reply without a channel was sent")
	}
}

func TestWorkingReaction(t *testing.T) {
	f := newFakeSlack(t)
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	to, _ := json.Marshal(target{Channel: "C1", TS: ts1})
	if err := a.Working(context.Background(), c, to); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].method != "reactions.add" || f.calls[0].body["name"] != "eyes" || f.calls[0].body["timestamp"] != ts1 {
		t.Errorf("call = %+v", f.calls[0])
	}
	c.Config = json.RawMessage(`{"teamId":"` + team + `"}`)
	f.calls = nil
	_ = a.Working(context.Background(), c, to)
	if len(f.calls) != 0 {
		t.Error("a reaction was added although the connection did not ask for it")
	}
}

func TestTestReportsBotAndWorkspaceWithoutSecrets(t *testing.T) {
	f := newFakeSlack(t)
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	rep := a.Test(context.Background(), c)
	if !rep.OK || rep.Details["bot"] != "orca" || rep.Details["workspace"] != "Acme" || rep.Details["workspaceId"] != team || !strings.Contains(rep.Details["scopes"], "chat:write") {
		t.Fatalf("report = %+v", rep)
	}
	if dump, _ := json.Marshal(rep); strings.Contains(string(dump), botTok) || strings.Contains(string(dump), signing) {
		t.Error("the report holds a secret")
	}
	// Learned for loop prevention: the bot's own user id.
	m, _ := receive(t, a, c, eventBody(map[string]any{"type": "app_mention", "user": "UBOT", "channel": "C1", "ts": "3.3", "text": "<@UBOT> x"}, nil))
	if !m.FromBot {
		t.Error("the bot's own id from auth.test was not used")
	}
	// A token of another workspace is a failure.
	c2 := conn(t, defaultSettings)
	c2.Config = json.RawMessage(`{"teamId":"T0WRONG"}`)
	if rep := a.Test(context.Background(), c2); rep.OK || !strings.Contains(rep.Error, "T0WRONG") {
		t.Errorf("wrong workspace = %+v", rep)
	}
	f.script = append(make([]func(http.ResponseWriter), len(f.calls)), func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`)) })
	if rep := a.Test(context.Background(), c); rep.OK || !strings.Contains(rep.Error, "invalid_auth") {
		t.Errorf("bad token = %+v", rep)
	}
	c3 := conn(t, defaultSettings)
	c3.Secret = inbound.Secret{KeyBotToken: []byte(botTok)}
	if rep := a.Test(context.Background(), c3); rep.OK {
		t.Error("a connection without a signing secret passed the test")
	}
}

func TestConfigValidationAndDescriptor(t *testing.T) {
	a := &Adapter{}
	for _, bad := range []string{`{"teamId":"x"}`, `{"teamId":"T0TEAM","unknown":1}`, `{"teamId":"t0team"}`} {
		if a.ValidateConfig(json.RawMessage(bad)) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The id may be absent: it is looked up once there are credentials.
	for _, ok := range []string{``, `{}`, `{"teamId":"T0TEAM"}`} {
		if err := a.ValidateConfig(json.RawMessage(ok)); err != nil {
			t.Error(ok, err)
		}
	}
	d := a.Describe()
	if d.Title != "Slack" || len(d.Setup) == 0 || !strings.Contains(strings.Join(d.Setup, " "), "{{url}}") || len(d.SecretKeys) != 2 {
		t.Errorf("descriptor = %+v", d)
	}
	caps := a.Capabilities()
	if !caps.AckFirst || caps.Dialect != inbound.DialectMrkdwn || len(caps.Modes) != 4 {
		t.Errorf("caps = %+v", caps)
	}
	// The core accepts the settings the adapter's modes allow, and no more.
	s, _ := inbound.ParseSettings([]byte(`{"allowSenders":["U1"],"modes":["channel"]}`))
	if inbound.CheckSettings(a, s) == nil {
		t.Error("channel mode without channels passed")
	}
}

func TestReplySwapsTheWorkingReactionForDoneOrFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  inbound.Result
		want string
	}{
		{"answered", inbound.Result{Text: "ok"}, "white_check_mark"},
		{"failed", inbound.Result{Text: "Sorry, that did not work.", Failed: true, Notice: true, Reason: "model error"}, "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSlack(t)
			a, c := adapter(f.srv.URL), conn(t, defaultSettings)
			to, _ := json.Marshal(target{Channel: "C1", ThreadTS: ts1, TS: ts1})
			if err := a.Reply(context.Background(), c, to, tc.res); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 3 {
				t.Fatalf("calls = %d, want the post, the end reaction and the removal", len(f.calls))
			}
			if f.calls[1].method != "reactions.add" || f.calls[1].body["name"] != tc.want || f.calls[1].body["timestamp"] != ts1 {
				t.Errorf("end reaction = %+v", f.calls[1])
			}
			if f.calls[2].method != "reactions.remove" || f.calls[2].body["name"] != "eyes" {
				t.Errorf("removal = %+v", f.calls[2])
			}
		})
	}
}

func TestReplyReactionsAreBestEffortAndOptional(t *testing.T) {
	// A missing scope never fails the reply.
	f := newFakeSlack(t)
	f.script = []func(http.ResponseWriter){nil, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`)) }}
	a, c := adapter(f.srv.URL), conn(t, defaultSettings)
	to, _ := json.Marshal(target{Channel: "C1", TS: ts1})
	if err := a.Reply(context.Background(), c, to, inbound.Result{Text: "ok"}); err != nil {
		t.Fatalf("a failed reaction failed the reply: %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("after a failed add the working reaction must stay: calls = %d", len(f.calls))
	}
	// Not asked for: no reaction calls.
	f = newFakeSlack(t)
	a = adapter(f.srv.URL)
	c.Config = json.RawMessage(`{"teamId":"` + team + `"}`)
	_ = a.Reply(context.Background(), c, to, inbound.Result{Text: "ok"})
	if len(f.calls) != 1 {
		t.Errorf("reactions without workingReaction: calls = %d", len(f.calls))
	}
	// A notice from the core never started a run: nothing to swap.
	f = newFakeSlack(t)
	a, c = adapter(f.srv.URL), conn(t, defaultSettings)
	_ = a.Reply(context.Background(), c, to, inbound.Result{Text: "busy", Notice: true})
	if len(f.calls) != 1 {
		t.Errorf("reactions on a core notice: calls = %d", len(f.calls))
	}
}
