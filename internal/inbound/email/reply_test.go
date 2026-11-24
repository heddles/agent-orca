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

package email

import (
	"context"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/inbound"
)

func replyConn(s *fakeSMTP) inbound.Connection {
	return connFor(secretFor(nil, s, map[string]string{KeySMTPUsername: "mailer", KeySMTPPassword: "smtp-password-1234"}), `{"mode":"webhook"}`)
}

func target(t *testing.T, tt replyTarget) inbound.ReplyTarget {
	t.Helper()
	b, err := marshalTarget(tt)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parts returns the text/plain and text/html bodies of a sent reply, decoded.
func parts(t *testing.T, sent string) (msg *mail.Message, plain, html string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(sent))
	if err != nil {
		t.Fatalf("the reply is not a message: %v\n%s", err, sent)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type = %q, %v", m.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		r := quotedprintable.NewReader(p)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		switch {
		case strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain"):
			plain = b.String()
		case strings.HasPrefix(p.Header.Get("Content-Type"), "text/html"):
			html = b.String()
		}
	}
	return m, plain, html
}

func TestReplyKeepsTheThreadAndIsPlainText(t *testing.T) {
	smtp, sender := startSMTP(t, true)
	a := &Adapter{SMTP: sender}
	tt := replyTarget{To: "alice@example.com", Subject: "Re: RE: Printer is on fire", MessageID: "m2@example.com", References: []string{"m1@example.com", "ours.orca@agents.example", "m2@example.com"}, Root: "m1@example.com"}
	err := a.Reply(context.Background(), replyConn(smtp), target(t, tt), inbound.Result{Text: "Try turning it off.\n\nThen on again: <b>done</b> & <script>alert(1)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	sent := smtp.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	if !strings.Contains(smtp.auth, "AUTH PLAIN") || len(smtp.rcpts) != 1 || !strings.Contains(smtp.rcpts[0], "alice@example.com") {
		t.Errorf("auth %q rcpts %v", smtp.auth, smtp.rcpts)
	}
	m, plain, html := parts(t, sent[0])
	h := m.Header
	if h.Get("From") != ownAddr || h.Get("To") != "alice@example.com" || h.Get("Subject") != "Re: Printer is on fire" {
		t.Errorf("From %q To %q Subject %q", h.Get("From"), h.Get("To"), h.Get("Subject"))
	}
	if h.Get("In-Reply-To") != "<m2@example.com>" || h.Get("References") != "<m1@example.com> <ours.orca@agents.example> <m2@example.com>" {
		t.Errorf("In-Reply-To %q References %q", h.Get("In-Reply-To"), h.Get("References"))
	}
	if !strings.HasSuffix(h.Get("Message-ID"), ".orca@agents.example>") {
		t.Errorf("Message-ID = %q", h.Get("Message-ID"))
	}
	if id, ok := decodeOurID(strings.Trim(h.Get("Message-ID"), "<>"), "agents.example"); !ok || id != "m1@example.com" {
		t.Errorf("our id decodes to %q, %v: a client that sets only In-Reply-To must point back at the thread", id, ok)
	}
	if h.Get("Auto-Submitted") != "auto-replied" || h.Get("X-Auto-Response-Suppress") != "All" {
		t.Errorf("loop prevention headers: %v", h)
	}
	if !strings.Contains(plain, "<b>done</b> & <script>alert(1)</script>") {
		t.Errorf("plain part = %q", plain)
	}
	// The HTML twin is escaped: nothing the model wrote is markup.
	if strings.Contains(html, "<script>") || strings.Contains(html, "<b>") || !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "&amp;") {
		t.Errorf("html part = %q", html)
	}
	if strings.Contains(html, "href") || strings.Contains(html, "<style") {
		t.Errorf("html part has links or styles: %q", html)
	}
	// The reply is itself ignored by an agent-orca connection that receives it.
	d, err := decodeRFC822([]byte(sent[0]))
	if err != nil {
		t.Fatal(err)
	}
	if reason := automated(d, "other@example.com", ownAddr); reason != "auto_submitted" {
		t.Errorf("our own reply is not recognised as automatic: %q", reason)
	}
}

func TestReplyCannotBeHeaderInjected(t *testing.T) {
	smtp, sender := startSMTP(t, true)
	a := &Adapter{SMTP: sender}
	c := replyConn(smtp)
	// A subject carrying a line break cannot add a header.
	tt := replyTarget{To: "alice@example.com", Subject: "hello\r\nBcc: evil@example.com\r\nX-Evil: 1", MessageID: "m@example.com\r\nBcc: evil2@example.com", References: []string{"a@x\r\nBcc: evil3@example.com"}}
	if err := a.Reply(context.Background(), c, target(t, tt), inbound.Result{Text: "ok"}); err != nil {
		t.Fatal(err)
	}
	msg := smtp.sent()[0]
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, bad := range []string{"\r\nBcc:", "\r\nX-Evil:"} {
		if strings.Contains(head, bad) {
			t.Errorf("a header was injected (%q):\n%s", bad, head)
		}
	}
	// The recipient is one plain address: nothing that adds a second one is mailed.
	for _, to := range []string{"alice@example.com\r\nBcc: evil@example.com", "a@example.com,b@example.com", "Alice <a@example.com>", "a@example.com;b@example.com", "a b@example.com", ""} {
		before := len(smtp.sent())
		err := a.Reply(context.Background(), c, target(t, replyTarget{To: to, Subject: "x"}), inbound.Result{Text: "ok"})
		if err == nil || len(smtp.sent()) != before {
			t.Errorf("recipient %q: err %v, mails %d -> %d", to, err, before, len(smtp.sent()))
		}
	}
	// Unreadable targets are refused.
	if err := a.Reply(context.Background(), c, inbound.ReplyTarget(`not json`), inbound.Result{Text: "x"}); err == nil {
		t.Error("an unreadable target was accepted")
	}
	// A non-ASCII subject is encoded, not sent raw.
	tt = replyTarget{To: "alice@example.com", Subject: "Ärger"}
	if err := a.Reply(context.Background(), c, target(t, tt), inbound.Result{Text: "ok"}); err != nil {
		t.Fatal(err)
	}
	m, _, _ := parts(t, smtp.sent()[len(smtp.sent())-1])
	if dec, _ := (&mime.WordDecoder{}).DecodeHeader(m.Header.Get("Subject")); dec != "Re: Ärger" || strings.ContainsRune(m.Header.Get("Subject"), 'Ä') {
		t.Errorf("subject = %q", m.Header.Get("Subject"))
	}
}

func TestReplyNeedsSTARTTLSUnlessInsecure(t *testing.T) {
	smtp, sender := startSMTP(t, false)
	a := &Adapter{SMTP: sender}
	tt := target(t, replyTarget{To: "alice@example.com", Subject: "x"})
	if err := a.Reply(context.Background(), replyConn(smtp), tt, inbound.Result{Text: "ok"}); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("a server without STARTTLS: %v", err)
	}
	if len(smtp.sent()) != 0 || smtp.auth != "" {
		t.Error("something was sent in clear")
	}
	c := connFor(secretFor(nil, smtp, map[string]string{KeyInsecure: "true"}), `{"mode":"webhook"}`)
	if err := a.Reply(context.Background(), c, tt, inbound.Result{Text: "ok"}); err != nil || len(smtp.sent()) != 1 {
		t.Errorf("insecure: %v", err)
	}
}

func TestReplySubjectAndHTMLShapes(t *testing.T) {
	for in, want := range map[string]string{"hi": "Re: hi", "Re: hi": "Re: hi", "RE: re: Hi": "Re: Hi", "": "Re: your message", "  ": "Re: your message"} {
		if got := replySubject(in); got != want {
			t.Errorf("replySubject(%q) = %q, want %q", in, got, want)
		}
	}
	h := escapedHTML("one\ntwo\n\nthree <i>x</i>")
	if !strings.Contains(h, "<p>one<br>") || !strings.Contains(h, "three &lt;i&gt;x&lt;/i&gt;") {
		t.Errorf("html = %q", h)
	}
	// Long ids are hashed, not sent whole.
	long := strings.Repeat("a", 500)
	if id := replyMessageID(long, "agents.example"); len(id) > 120 {
		t.Errorf("id length = %d", len(id))
	}
}
