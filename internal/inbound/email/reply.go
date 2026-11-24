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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/notify"
)

// replyTarget is what the adapter keeps, while a reply is pending, to answer one
// message. It is stored by the core and cleared once the reply is sent.
type replyTarget struct {
	To         string   `json:"to"`
	Subject    string   `json:"subject"`
	MessageID  string   `json:"messageId"`
	References []string `json:"references"`
	Root       string   `json:"root"`
}

func marshalTarget(t replyTarget) (inbound.ReplyTarget, error) {
	b, err := json.Marshal(t)
	return inbound.ReplyTarget(b), err
}

// Reply mails the result to the sender of the message. The recipient is the From
// address of the message (never a Reply-To or any address inside the text); the
// subject is "Re: " and theirs, and In-Reply-To and References keep the mail
// client's thread together. The text goes out as plain text with a minimal escaped
// HTML twin: nothing the model wrote is ever sent as markup.
func (a *Adapter) Reply(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, res inbound.Result) error {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return err
	}
	cr, err := ParseCredentials(cfg.Mode, c.Secret)
	if err != nil {
		return err
	}
	var t replyTarget
	if err := json.Unmarshal(to, &t); err != nil {
		return errors.New("the reply target is not readable")
	}
	rcpt, err := safeAddress(t.To)
	if err != nil {
		return err
	}
	raw, err := buildReply(cr.From, rcpt, t, res.Text, time.Now())
	if err != nil {
		return err
	}
	s := a.SMTP
	if s == nil {
		s = &notify.Sender{}
	}
	return s.SendMail(ctx, cr.smtpChannel(rcpt), raw)
}

// safeAddress accepts one plain address and nothing that could add a header or a
// second recipient.
func safeAddress(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00,;<>\" \t") {
		return "", errors.New("the reply address is not a plain address")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return "", errors.New("the reply address is not a plain address")
	}
	return s, nil
}

// headerValue makes s safe as one header value: no line break survives.
func headerValue(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(s)
	return strings.TrimSpace(s)
}

func encodeSubject(s string) string {
	s = headerValue(s)
	return mime.QEncoding.Encode("utf-8", s)
}

func replySubject(orig string) string {
	orig = strings.TrimSpace(orig)
	for {
		l := strings.ToLower(orig)
		if strings.HasPrefix(l, "re:") {
			orig = strings.TrimSpace(orig[3:])
			continue
		}
		break
	}
	if orig == "" {
		return "Re: your message"
	}
	return "Re: " + orig
}

func domainOfAddr(a string) string {
	if i := strings.LastIndexByte(a, '@'); i >= 0 {
		return a[i+1:]
	}
	return "localhost"
}

// buildReply writes the RFC 5322 message. Every value that came from outside goes
// through headerValue or is encoded.
func buildReply(from, to string, t replyTarget, text string, now time.Time) (string, error) {
	if from == "" || to == "" {
		return "", errors.New("a reply needs a sender and a recipient")
	}
	domain := domainOfAddr(from)
	root := t.Root
	if root == "" {
		root = t.MessageID
	}
	var refs []string
	for _, r := range t.References {
		if r = headerValue(r); r != "" && !strings.ContainsAny(r, "<> \t") {
			refs = append(refs, "<"+r+">")
		}
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part := func(ctype string, content string) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype+"; charset=utf-8")
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		w, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(content)); err != nil {
			return err
		}
		return qp.Close()
	}
	if err := part("text/plain", strings.ReplaceAll(text, "\n", "\r\n")); err != nil {
		return "", err
	}
	if err := part("text/html", escapedHTML(text)); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, headerValue(v)) }
	line("From", from)
	line("To", to)
	b.WriteString("Subject: " + encodeSubject(replySubject(t.Subject)) + "\r\n")
	line("Date", now.UTC().Format(time.RFC1123Z))
	line("Message-ID", "<"+replyMessageID(root, domain)+">")
	if t.MessageID != "" && !strings.ContainsAny(t.MessageID, "<> \t\r\n") {
		line("In-Reply-To", "<"+t.MessageID+">")
	}
	if len(refs) > 0 {
		line("References", strings.Join(refs, " "))
	}
	// An automatic answer: other systems (and other agent-orca connections) must not answer it.
	line("Auto-Submitted", "auto-replied")
	line("X-Auto-Response-Suppress", "All")
	line("MIME-Version", "1.0")
	line("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.String(), nil
}

// escapedHTML is the minimal HTML twin of a plain-text reply: paragraphs and line
// breaks, every character escaped, no link, no style.
func escapedHTML(text string) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><body>")
	for para := range strings.SplitSeq(strings.TrimSpace(text), "\n\n") {
		b.WriteString("<p>")
		b.WriteString(strings.ReplaceAll(html.EscapeString(para), "\n", "<br>\r\n"))
		b.WriteString("</p>\r\n")
	}
	b.WriteString("</body></html>\r\n")
	return b.String()
}
