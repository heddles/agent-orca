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

package webex

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

const (
	botTok   = "NjQ0-bot-token-secret"
	hookSec  = "webhook-secret-value"
	botID    = "Y2lzY29zcGFyazovL3VzL1BFT1BMRS9ib3Q"
	userID   = "Y2lzY29zcGFyazovL3VzL1BFT1BMRS91c2Vy"
	roomDM   = "Y2lzY29zcGFyazovL3VzL1JPT00vZG0"
	roomGrp  = "Y2lzY29zcGFyazovL3VzL1JPT00vZ3JvdXA"
	msgID    = "Y2lzY29zcGFyazovL3VzL01FU1NBR0UvMQ"
	parentID = "Y2lzY29zcGFyazovL3VzL01FU1NBR0UvcGFyZW50"
	email    = "ann@example.com"
)

var clock = time.Unix(1_700_000_000, 0)

type apiCall struct {
	method, path, auth string
	body               map[string]any
}

type fakeWebex struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []apiCall
	messages map[string]map[string]any // GET /v1/messages/{id}
	meBody   string
	script   []func(w http.ResponseWriter) // by call index; nil = default
}

func newFake(t *testing.T) *fakeWebex {
	f := &fakeWebex{t: t, messages: map[string]map[string]any{},
		meBody: `{"id":"` + botID + `","displayName":"Orca Bot","nickName":"Orca","type":"bot","emails":["orca@webex.bot"]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		i := len(f.calls) - 1
		f.mu.Unlock()
		if i < len(f.script) && f.script[i] != nil {
			f.script[i](w)
			return
		}
		switch {
		case r.URL.Path == "/v1/people/me":
			_, _ = w.Write([]byte(f.meBody))
		case strings.HasPrefix(r.URL.Path, "/v1/messages/") && r.Method == http.MethodGet:
			if m, ok := f.messages[strings.TrimPrefix(r.URL.Path, "/v1/messages/")]; ok {
				_ = json.NewEncoder(w).Encode(m)
				return
			}
			w.WriteHeader(404)
		case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"new"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWebex) adapter() *Adapter {
	return &Adapter{APIBase: f.srv.URL, Now: func() time.Time { return clock },
		Client: &chat.Client{AllowPrivate: true, Wait: func(context.Context, time.Duration) error { return nil }}}
}

func (f *fakeWebex) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.path, prefix) {
			n++
		}
	}
	return n
}

func conn(t *testing.T, settings string) inbound.Connection {
	t.Helper()
	s, err := inbound.ParseSettings([]byte(settings))
	if err != nil {
		t.Fatal(err)
	}
	return inbound.Connection{ID: "inb_" + strings.Repeat("c", 32), Kind: Kind, Settings: s, Config: json.RawMessage(`{}`),
		Secret: inbound.Secret{KeyBotToken: []byte(botTok), KeyWebhookSecret: []byte(hookSec)}}
}

const defaultSettings = `{"allowSenders":["` + email + `"]}`

func hook(mut ...func(map[string]any)) string {
	d := map[string]any{"id": msgID, "roomId": roomGrp, "roomType": "group", "personId": userID, "personEmail": "Ann@Example.com", "created": clock.Add(-time.Second).UTC().Format(time.RFC3339)}
	w := map[string]any{"id": "hook1", "name": "orca", "resource": "messages", "event": "created", "orgId": "org", "data": d}
	for _, m := range mut {
		m(d)
	}
	b, _ := json.Marshal(w)
	return string(b)
}

func signed(body string, mutate ...func(*http.Request)) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/inbound/webex/x", strings.NewReader(body))
	r.Header.Set("X-Spark-Signature", hex.EncodeToString(verify.HMACSHA1([]byte(hookSec), []byte(body))))
	for _, m := range mutate {
		m(r)
	}
	return r
}

func receive(t *testing.T, a *Adapter, c inbound.Connection, body string) (inbound.Message, error) {
	t.Helper()
	return a.Receive(context.Background(), c, signed(body))
}

func (f *fakeWebex) addMessage(id string, mut ...func(map[string]any)) {
	m := map[string]any{"id": id, "roomId": roomGrp, "personId": userID, "personEmail": email, "text": "Orca summarise this", "mentionedPeople": []string{botID}, "created": "x"}
	for _, f := range mut {
		f(m)
	}
	f.messages[id] = m
}

func TestSignature(t *testing.T) {
	a, c := bare(), conn(t, defaultSettings)
	body := hook()
	if _, err := receive(t, a, c, body); err != nil {
		t.Fatalf("a signed webhook: %v", err)
	}
	var auth *inbound.AuthError
	for name, r := range map[string]*http.Request{
		"wrong signature": signed(body, func(r *http.Request) { r.Header.Set("X-Spark-Signature", strings.Repeat("0", 40)) }),
		"no signature":    signed(body, func(r *http.Request) { r.Header.Del("X-Spark-Signature") }),
		"not hex":         signed(body, func(r *http.Request) { r.Header.Set("X-Spark-Signature", "sha1="+strings.Repeat("a", 40)) }),
		"sha256 value": signed(body, func(r *http.Request) {
			r.Header.Set("X-Spark-Signature", hex.EncodeToString(verify.HMACSHA256([]byte(hookSec), []byte(body))))
		}),
		"another secret": signed(body, func(r *http.Request) {
			r.Header.Set("X-Spark-Signature", hex.EncodeToString(verify.HMACSHA1([]byte("other"), []byte(body))))
		}),
		"tampered body": func() *http.Request {
			r := signed(body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(body, "Ann", "Eve", 1)))
			return r
		}(),
	} {
		if _, err := a.Receive(context.Background(), c, r); !errors.As(err, &auth) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(auth.Msg, hookSec) {
			t.Errorf("%s: the error holds the secret", name)
		}
	}
	c.Secret = inbound.Secret{KeyBotToken: []byte(botTok)}
	if _, err := receive(t, a, c, body); !errors.As(err, &auth) {
		t.Errorf("no secret: %v", err)
	}
}

func TestStaleAndFutureWebhooksAreNotRun(t *testing.T) {
	a, c := bare(), conn(t, defaultSettings)
	for name, created := range map[string]time.Time{"old": clock.Add(-2 * time.Hour), "future": clock.Add(10 * time.Minute)} {
		_, err := receive(t, a, c, hook(func(d map[string]any) { d["created"] = created.UTC().Format(time.RFC3339) }))
		var rej *inbound.Rejection
		if !errors.As(err, &rej) || !rej.Ignore || rej.Reason != "stale" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := receive(t, a, c, hook(func(d map[string]any) { d["created"] = clock.Add(-50 * time.Minute).UTC().Format(time.RFC3339) })); err != nil {
		t.Errorf("inside the window: %v", err)
	}
}

func TestOtherResourcesAreAcknowledgedAndIgnored(t *testing.T) {
	a, c := bare(), conn(t, defaultSettings)
	for _, body := range []string{
		`{"resource":"memberships","event":"created","data":{}}`, `{"resource":"messages","event":"deleted","data":{}}`, `not json at all`,
	} {
		_, err := receive(t, a, c, body)
		var resp *inbound.Response
		if !errors.As(err, &resp) || resp.Status != 200 {
			t.Errorf("%q: %v", body, err)
		}
	}
	for name, mut := range map[string]func(map[string]any){
		"bad id":     func(d map[string]any) { d["id"] = "../../people/me" },
		"bad room":   func(d map[string]any) { d["roomId"] = "x y" },
		"unknown rt": func(d map[string]any) { d["roomType"] = "meeting" },
		"no sender":  func(d map[string]any) { d["personEmail"] = ""; d["personId"] = "" },
	} {
		_, err := receive(t, a, c, hook(mut))
		var rej *inbound.Rejection
		if !errors.As(err, &rej) || !rej.Ignore {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDirectMessageIsFetchedAndOneFlatConversation(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	f.addMessage(msgID, func(m map[string]any) { m["roomId"] = roomDM; m["text"] = "hello there"; m["mentionedPeople"] = nil })
	m, err := receive(t, a, c, hook(func(d map[string]any) { d["roomType"] = "direct"; d["roomId"] = roomDM }))
	if err != nil || m.Mode != inbound.ModeDirect || m.Text != "" || m.MessageID != msgID {
		t.Fatalf("receive = %+v %v", m, err)
	}
	if m.Sender.ID != email || m.Sender.Group != "example.com" || m.Channel != roomDM || m.ThreadID != roomDM+"/dm" {
		t.Errorf("identity = %+v %q", m.Sender, m.ThreadID)
	}
	if f.count("/") != 0 {
		t.Error("Receive called the API: that belongs after the acknowledgement")
	}
	a.Enrich(context.Background(), c, &m)
	if m.FromBot || m.Text != "hello there" || m.Mode != inbound.ModeDirect {
		t.Errorf("enriched = %+v", m)
	}
	for _, call := range f.calls {
		if call.auth != "Bearer "+botTok {
			t.Errorf("call without the bot token: %+v", call)
		}
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Room != roomDM || to.Parent != "" {
		t.Errorf("target = %+v", to)
	}
}

func TestGroupMentionStripsTheBotNameAndJoinsTheThread(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	f.addMessage(msgID, func(m map[string]any) { m["text"] = "Orca Bot summarise this"; m["parentId"] = parentID })
	m, err := receive(t, a, c, hook())
	if err != nil || m.Mode != inbound.ModeMention || m.ThreadID != roomGrp+"/"+msgID {
		t.Fatalf("receive = %+v %v", m, err)
	}
	a.Enrich(context.Background(), c, &m)
	if m.FromBot || m.Text != "summarise this" || m.Mode != inbound.ModeMention || m.ThreadID != roomGrp+"/"+parentID {
		t.Errorf("enriched = %+v", m)
	}
	var to target
	_ = json.Unmarshal(m.Reply, &to)
	if to.Room != roomGrp || to.Parent != parentID {
		t.Errorf("target = %+v", to)
	}
	// A top-level message: the answer opens a thread under it, and replies in it share the conversation.
	f.addMessage("Y2lzY29zcGFyazovL3VzL01FU1NBR0UvMg", func(m map[string]any) { m["text"] = "Orca again" })
	m2, _ := receive(t, a, c, hook(func(d map[string]any) { d["id"] = "Y2lzY29zcGFyazovL3VzL01FU1NBR0UvMg" }))
	a.Enrich(context.Background(), c, &m2)
	_ = json.Unmarshal(m2.Reply, &to)
	if to.Parent != "Y2lzY29zcGFyazovL3VzL01FU1NBR0UvMg" || m2.ThreadID == m.ThreadID || m2.Text != "again" {
		t.Errorf("second = %+v %+v", m2, to)
	}
	// The nickname is stripped too.
	f.addMessage("Y2lzY29zcGFyazovL3VzL01FU1NBR0UvMw", func(m map[string]any) { m["text"] = "orca: hello" })
	m3, _ := receive(t, a, c, hook(func(d map[string]any) { d["id"] = "Y2lzY29zcGFyazovL3VzL01FU1NBR0UvMw" }))
	a.Enrich(context.Background(), c, &m3)
	if m3.Text != "hello" {
		t.Errorf("nickname text = %q", m3.Text)
	}
	// The bot's id was fetched once for all of these.
	if n := f.count("/v1/people/me"); n != 1 {
		t.Errorf("people/me calls = %d", n)
	}
}

func TestAGroupMessageThatDoesNotMentionTheBotIsRefused(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	f.addMessage(msgID, func(m map[string]any) { m["mentionedPeople"] = []string{"someone-else"}; m["text"] = "everyone look" })
	m, _ := receive(t, a, c, hook())
	a.Enrich(context.Background(), c, &m)
	if why := c.Settings.Refusal(a.Capabilities(), m); why != "mode_not_enabled" {
		t.Errorf("refusal = %q (mode %s)", why, m.Mode)
	}
}

func TestBotsAndOwnMessagesAreIgnored(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	// Another bot, known from its address, needs no lookup at all.
	m, err := receive(t, a, c, hook(func(d map[string]any) { d["personEmail"] = "other@webex.bot" }))
	if err != nil || !m.FromBot || !strings.HasPrefix(m.MessageID, "ignored:") || f.count("/") != 0 {
		t.Errorf("other bot: %+v %v", m, err)
	}
	// Our own message (Webex sends the bot its own posts): ignored after the account is known.
	f.addMessage(msgID, func(m map[string]any) { m["personId"] = botID; m["personEmail"] = "orca@webex.bot" })
	m, _ = receive(t, a, c, hook(func(d map[string]any) { d["personId"] = botID; d["personEmail"] = "" }))
	a.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Errorf("own message through enrich: %+v", m)
	}
	// Once learned, Receive itself recognises the bot's id.
	m, _ = receive(t, a, c, hook(func(d map[string]any) { d["personId"] = botID; d["personEmail"] = "" }))
	if !m.FromBot || !strings.HasPrefix(m.MessageID, "ignored:") {
		t.Errorf("own message in receive: %+v", m)
	}
	// An API message whose author is the bot although the webhook said otherwise.
	f.addMessage(msgID, func(m map[string]any) { m["personId"] = botID })
	m, _ = receive(t, a, c, hook())
	a.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Error("a message authored by the bot was kept")
	}
}

func TestEnrichDropsWhatItCannotTrust(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	// The message is gone / unreadable.
	m, _ := receive(t, a, c, hook())
	a.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Error("a message that could not be fetched was kept")
	}
	// The API says another room than the webhook.
	f.addMessage(msgID, func(m map[string]any) { m["roomId"] = "Y2lzY29zcGFyazovL3VzL1JPT00vb3RoZXI" })
	m, _ = receive(t, a, c, hook())
	a.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Error("a message from another room than its webhook was kept")
	}
	// The bot account cannot be read.
	f2 := newFake(t)
	f2.meBody = `{}`
	f2.addMessage(msgID)
	a2 := f2.adapter()
	m, _ = receive(t, a2, c, hook())
	a2.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Error("kept although the bot account is unknown")
	}
	// A bare mention has nothing to run.
	f3 := newFake(t)
	f3.addMessage(msgID, func(m map[string]any) { m["text"] = "Orca Bot" })
	a3 := f3.adapter()
	m, _ = receive(t, a3, c, hook())
	a3.Enrich(context.Background(), c, &m)
	if !m.FromBot {
		t.Error("an empty message was kept")
	}
}

func TestDuplicateDeliveryAndAllowLists(t *testing.T) {
	a := bare()
	c := conn(t, `{"allowSenders":["`+email+`"],"allowGroups":["corp.example"],"allowChannels":["`+roomGrp+`"]}`)
	m1, _ := receive(t, a, c, hook())
	m2, _ := receive(t, a, c, hook()) // Webex delivers again
	if m1.MessageID == "" || m1.MessageID != m2.MessageID {
		t.Errorf("keys differ: %q %q", m1.MessageID, m2.MessageID)
	}
	caps := a.Capabilities()
	if why := c.Settings.Refusal(caps, m1); why != "" {
		t.Errorf("allowed: %s", why)
	}
	m1.Channel = "Y2lzY29zcGFyazovL3VzL1JPT00vb3RoZXI"
	if why := c.Settings.Refusal(caps, m1); why != "channel_not_allowed" {
		t.Errorf("other room: %q", why)
	}
	m1.Channel, m1.Sender.ID, m1.Sender.Group = roomGrp, "eve@evil.example", "evil.example"
	if why := c.Settings.Refusal(caps, m1); why != "sender_not_allowed" {
		t.Errorf("other user: %q", why)
	}
	m1.Sender.Group = "corp.example"
	if why := c.Settings.Refusal(caps, m1); why != "" {
		t.Errorf("allowed domain: %q", why)
	}
}

// ── outbound ────────────────────────────────────────────────────────────────

func rt(room, parent string) json.RawMessage {
	b, _ := json.Marshal(target{Room: room, Parent: parent})
	return b
}

func TestReplyIsThreadedMarkdownAndCannotPing(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	text := "**Done** <@all> <@personEmail:a@b.c> @all @everyone <at>x</at> <users/all> see [docs](https://e.example/a?b=1&c=2)\n\n- one\n- two"
	if err := a.Reply(context.Background(), c, rt(roomGrp, parentID), inbound.Result{Text: text}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	got := f.calls[0]
	if got.method != "POST" || got.path != "/v1/messages" || got.auth != "Bearer "+botTok || got.body["roomId"] != roomGrp || got.body["parentId"] != parentID {
		t.Errorf("call = %+v", got)
	}
	if _, has := got.body["text"]; has {
		t.Error("the plain text field is set next to the markdown")
	}
	msg := got.body["markdown"].(string)
	for _, bad := range []string{"<@all", "<@personEmail", "@all", "@everyone", "<at>", "<users/all"} {
		if strings.Contains(msg, bad) {
			t.Errorf("the message still holds %q: %q", bad, msg)
		}
	}
	for _, want := range []string{"**Done**", "[docs](https://e.example/a?b=1&c=2)", "- one"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	if err := a.Reply(context.Background(), c, rt(roomDM, ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, has := f.calls[1].body["parentId"]; has {
		t.Error("parentId set for a flat reply")
	}
}

func TestLongRepliesStayWithin7439Bytes(t *testing.T) {
	for name, para := range map[string]string{"ascii": strings.Repeat("sentence of words. ", 80), "cjk": strings.Repeat("日本語のテキスト。", 90)} {
		f := newFake(t)
		a, c := f.adapter(), conn(t, defaultSettings)
		long := strings.Repeat(para+"\n\n", 40)
		if err := a.Reply(context.Background(), c, rt(roomGrp, parentID), inbound.Result{Text: long}); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) < 2 || len(f.calls) > maxReplyParts {
			t.Fatalf("%s: messages = %d", name, len(f.calls))
		}
		for _, cl := range f.calls {
			s := cl.body["markdown"].(string)
			if len(s) > 7439 || !utf8.ValidString(s) {
				t.Errorf("%s: a message of %d bytes", name, len(s))
			}
			if cl.body["parentId"] != parentID {
				t.Errorf("%s: a part left the thread", name)
			}
		}
		if !strings.Contains(f.calls[len(f.calls)-1].body["markdown"].(string), "shortened") {
			t.Errorf("%s: the cut was not announced", name)
		}
	}
}

func TestRateLimitRetryAfter(t *testing.T) {
	f := newFake(t)
	f.script = []func(http.ResponseWriter){func(w http.ResponseWriter) { w.Header().Set("Retry-After", "3"); w.WriteHeader(429) }}
	var waited []time.Duration
	a, c := f.adapter(), conn(t, defaultSettings)
	a.Client.Wait = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	if err := a.Reply(context.Background(), c, rt(roomGrp, ""), inbound.Result{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 3*time.Second || len(f.calls) != 2 {
		t.Errorf("waited=%v calls=%d", waited, len(f.calls))
	}
	f2 := newFake(t)
	f2.script = []func(http.ResponseWriter){func(w http.ResponseWriter) { w.Header().Set("Retry-After", "900"); w.WriteHeader(429) }}
	err := f2.adapter().Reply(context.Background(), c, rt(roomGrp, ""), inbound.Result{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "wait") || len(f2.calls) != 1 {
		t.Errorf("long retry-after: %v (%d calls)", err, len(f2.calls))
	}
}

func TestErrorsNeverHoldTokenSecretOrText(t *testing.T) {
	f := newFake(t)
	f.script = []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"` + botTok + ` confidential answer","trackingId":"x"}`))
	}}
	a, c := f.adapter(), conn(t, defaultSettings)
	err := a.Reply(context.Background(), c, rt(roomGrp, ""), inbound.Result{Text: "confidential answer"})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), botTok) || strings.Contains(err.Error(), "confidential") {
		t.Errorf("error = %v", err)
	}
	// An unreachable API: the URL (and with it the host) is not in the error either.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	a2 := &Adapter{APIBase: dead.URL, Client: &chat.Client{AllowPrivate: true, Attempts: 1}}
	err = a2.Reply(context.Background(), c, rt(roomGrp, ""), inbound.Result{Text: "x"})
	if err == nil || strings.Contains(err.Error(), dead.URL) || strings.Contains(err.Error(), botTok) {
		t.Errorf("transport error = %v", err)
	}
	for _, bad := range []string{`{}`, `{"r":"../x"}`, `{"r":"` + roomGrp + `","p":"a b"}`, `nope`} {
		n := len(f.calls)
		if err := a.Reply(context.Background(), c, json.RawMessage(bad), inbound.Result{Text: "x"}); err == nil || len(f.calls) != n {
			t.Errorf("target %s: %v", bad, err)
		}
	}
	// The SSRF guard is on by default: a loopback API base is refused.
	guard := &Adapter{APIBase: f.srv.URL}
	if err := guard.Reply(context.Background(), c, rt(roomGrp, ""), inbound.Result{Text: "x"}); err == nil {
		t.Error("the default client reached a loopback address")
	}
}

func TestTestShowsTheBotAndRefusesAUserToken(t *testing.T) {
	f := newFake(t)
	a, c := f.adapter(), conn(t, defaultSettings)
	rep := a.Test(context.Background(), c)
	if !rep.OK || rep.Details["bot"] != "Orca Bot" {
		t.Fatalf("report = %+v", rep)
	}
	if dump, _ := json.Marshal(rep); strings.Contains(string(dump), botTok) || strings.Contains(string(dump), hookSec) || strings.Contains(string(dump), botID) {
		t.Error("the report holds a secret or an id")
	}
	// Learned for loop prevention.
	m, _ := receive(t, a, c, hook(func(d map[string]any) { d["personId"] = botID }))
	if !m.FromBot {
		t.Error("the bot's own id from people/me was not used")
	}
	f2 := newFake(t)
	f2.meBody = `{"id":"x","displayName":"Ann","type":"person"}`
	if rep := f2.adapter().Test(context.Background(), c); rep.OK || !strings.Contains(rep.Error, "bot") {
		t.Errorf("a person token passed: %+v", rep)
	}
	f3 := newFake(t)
	f3.script = []func(http.ResponseWriter){func(w http.ResponseWriter) { w.WriteHeader(401) }}
	if rep := f3.adapter().Test(context.Background(), c); rep.OK || !strings.Contains(rep.Error, "401") || strings.Contains(rep.Error, botTok) {
		t.Errorf("bad token = %+v", rep)
	}
	c2 := conn(t, defaultSettings)
	c2.Secret = inbound.Secret{KeyBotToken: []byte(botTok)}
	if rep := a.Test(context.Background(), c2); rep.OK {
		t.Error("a connection without a webhook secret passed")
	}
}

func TestConfigValidationAndDescriptor(t *testing.T) {
	a := &Adapter{}
	if a.ValidateConfig(json.RawMessage(`{"x":1}`)) == nil {
		t.Error("an unknown setting was accepted")
	}
	for _, ok := range []string{``, `{}`} {
		if err := a.ValidateConfig(json.RawMessage(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	d := a.Describe()
	if d.Title != "Webex" || !strings.Contains(strings.Join(d.Setup, " "), "{{url}}") || len(d.SecretKeys) != 2 {
		t.Errorf("descriptor = %+v", d)
	}
	caps := a.Capabilities()
	if !caps.AckFirst || caps.Dialect != inbound.DialectMarkdown || !caps.Threads || len(caps.Modes) != 2 || caps.WorkingIndicator {
		t.Errorf("caps = %+v", caps)
	}
	s, _ := inbound.ParseSettings([]byte(`{"allowSenders":["u"],"modes":["command"]}`))
	if inbound.CheckSettings(a, s) == nil {
		t.Error("a command mode was accepted")
	}
	if err := inbound.CheckSecret(a, map[string][]byte{KeyBotToken: []byte("t")}); err == nil || !strings.Contains(err.Error(), KeyWebhookSecret) {
		t.Errorf("secret check = %v", err)
	}
}

// bare is an adapter that must not call the API.
func bare() *Adapter { return &Adapter{Now: func() time.Time { return clock }} }
