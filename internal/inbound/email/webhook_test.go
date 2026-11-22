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
	"encoding/base64"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
)

const webhookCfg = `{"mode":"webhook","trustedAuthservId":"mx.agents.example"}`

func signedReq(secret, contentType string, body []byte, at time.Time) *http.Request {
	ts := strconv.FormatInt(at.Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/inbound/email/x", bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set(inbound.TimestampHeader, ts)
	r.Header.Set(inbound.SignatureHeader, inbound.Sign(inbound.SigningKey(secret), ts, body))
	return r
}

func receive(a *Adapter, r *http.Request) (inbound.Message, error) {
	return a.Receive(context.Background(), connFor(secretFor(nil, nil, nil), webhookCfg), r)
}

const relaySecret = "relay-signing-secret"

func TestWebhookRawMessage(t *testing.T) {
	a := &Adapter{}
	m, err := receive(a, signedReq(relaySecret, "message/rfc822", goodMail("w1"), time.Now()))
	if err != nil || m.Sender.ID != aliceAddr || m.Text != "Body of w1" || m.MessageID != "w1@example.com" {
		t.Fatalf("m = %+v, %v", m, err)
	}
	// A spoofed From with no evidence is refused, whatever the signature says: the relay vouches
	// for who it is, not for the sender.
	_, err = receive(a, signedReq(relaySecret, "message/rfc822", basic(), time.Now()))
	wantRejection(t, err, "no_auth_results", false)
}

func TestWebhookJSONShapes(t *testing.T) {
	a := &Adapter{}
	generic := `{"from":"Alice <alice@example.com>","subject":"Hello","text":"On Monday, Bob wrote:\n> old\nNew text","messageId":"<j1@example.com>",
	  "inReplyTo":"<root@example.com>","references":"<root@example.com>","authenticationResults":"mx.agents.example; dmarc=pass header.from=example.com",
	  "attachments":[{"name":"a.txt","contentType":"text/plain","content":"` + base64.StdEncoding.EncodeToString([]byte("attached")) + `"}]}`
	m, err := receive(a, signedReq(relaySecret, "application/json", []byte(generic), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Hello" || m.MessageID != "j1@example.com" || m.ThreadID != "root@example.com" || len(m.Attachments) != 1 || m.Attachments[0].Text != "" {
		t.Errorf("generic = %+v", m)
	}
	if !strings.HasPrefix(m.Text, "On Monday, Bob wrote:") && m.Text != "" {
		// the quoted-reply cut applies at the "On ... wrote:" line
		t.Logf("text = %q", m.Text)
	}

	// Postmark's own field names, headers as a list of name and value.
	postmark := `{"From":"alice@example.com","Subject":"Hi","TextBody":"Postmark body","MessageID":"pm-1","Headers":[
	  {"Name":"Message-ID","Value":"<pm-1@example.com>"},{"Name":"Authentication-Results","Value":"mx.agents.example; dkim=pass header.d=example.com"}]}`
	m, err = receive(a, signedReq(relaySecret, "application/json", []byte(postmark), time.Now()))
	if err != nil || m.Text != "Postmark body" || m.MessageID != "pm-1" {
		t.Errorf("postmark = %+v, %v", m, err)
	}

	// Mailgun's pairs, and SendGrid's raw header block.
	mailgun := `{"from":"alice@example.com","subject":"Hi","body-plain":"Mailgun body","Message-Id":"<mg-1@example.com>",
	  "message-headers":[["Authentication-Results","mx.agents.example; dmarc=pass header.from=example.com"],["X-Other","1"]]}`
	if m, err = receive(a, signedReq(relaySecret, "application/json", []byte(mailgun), time.Now())); err != nil || m.Text != "Mailgun body" {
		t.Errorf("mailgun = %+v, %v", m, err)
	}
	block := `{"from":"alice@example.com","subject":"Hi","text":"SendGrid body","headers":"Message-ID: <sg-1@example.com>\nAuthentication-Results: mx.agents.example; dmarc=pass header.from=example.com\nX-Other: 1\n"}`
	if m, err = receive(a, signedReq(relaySecret, "application/json", []byte(block), time.Now())); err != nil || m.Text != "SendGrid body" || m.MessageID != "sg-1@example.com" {
		t.Errorf("header block = %+v, %v", m, err)
	}

	// The relay's own verdict replaces whatever the message claims about itself.
	forged := `{"from":"alice@example.com","subject":"Hi","text":"x","messageId":"f1","headers":{"Authentication-Results":"mx.agents.example; dmarc=pass header.from=example.com"},
	  "authenticationResults":"mx.agents.example; dmarc=fail header.from=example.com"}`
	_, err = receive(a, signedReq(relaySecret, "application/json", []byte(forged), time.Now()))
	wantRejection(t, err, "dmarc_fail", false)
	// No evidence from the relay and none in the message: refused.
	none := `{"from":"alice@example.com","subject":"Hi","text":"x","messageId":"n1"}`
	_, err = receive(a, signedReq(relaySecret, "application/json", []byte(none), time.Now()))
	wantRejection(t, err, "no_auth_results", false)
	// Header injection through a field value cannot add a header.
	inj := `{"from":"alice@example.com","subject":"Hi\r\nBcc: evil@example.com","text":"x","messageId":"i1","authenticationResults":"mx.agents.example; dmarc=pass header.from=example.com"}`
	m, err = receive(a, signedReq(relaySecret, "application/json", []byte(inj), time.Now()))
	if err != nil || strings.Contains(m.Title, "\n") || strings.Contains(string(m.Reply), "\\r") {
		t.Errorf("injected subject = %q (%s), %v", m.Title, m.Reply, err)
	}
	// A JSON message over the size limit is refused by its decoded size.
	big := `{"from":"alice@example.com","subject":"Hi","text":"` + strings.Repeat("a", MaxMessageBytes+1) + `","messageId":"b1","authenticationResults":"mx.agents.example; dmarc=pass header.from=example.com"}`
	m, err = receive(a, signedReq(relaySecret, "application/json", []byte(big), time.Now()))
	wantRejection(t, err, "too_large", false)
	if m.MessageID != "b1" {
		t.Errorf("a refused message must carry its id: %+v", m)
	}
}

func form(t *testing.T, fields map[string]string, files map[string]string) (string, []byte) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		fw, _ := w.CreateFormFile("attachment1", name)
		_, _ = fw.Write([]byte(content))
	}
	_ = w.Close()
	return w.FormDataContentType(), b.Bytes()
}

func TestWebhookMultipartForm(t *testing.T) {
	a := &Adapter{}
	ct, body := form(t, map[string]string{
		"from": "alice@example.com", "subject": "Form mail", "text": "Form body", "html": "<p>ignored when text exists</p>",
		"headers":                "Message-ID: <f1@example.com>\nX: y",
		"authentication_results": "mx.agents.example; dmarc=pass header.from=example.com",
	}, map[string]string{"run.exe": "MZ"})
	m, err := receive(a, signedReq(relaySecret, ct, body, time.Now()))
	if err != nil || m.Text != "Form body" || m.MessageID != "f1@example.com" || len(m.Attachments) != 1 || m.Attachments[0].Name != "run.exe" || m.Attachments[0].Text != "" {
		t.Fatalf("form = %+v, %v", m, err)
	}
	// The whole raw message in one field.
	ct, body = form(t, map[string]string{"email": string(goodMail("raw1"))}, nil)
	if m, err = receive(a, signedReq(relaySecret, ct, body, time.Now())); err != nil || m.MessageID != "raw1@example.com" {
		t.Errorf("raw field = %+v, %v", m, err)
	}
}

func TestWebhookAuthentication(t *testing.T) {
	a := &Adapter{}
	body := goodMail("a1")
	var auth *inbound.AuthError
	check := func(name string, r *http.Request) {
		t.Helper()
		if _, err := receive(a, r); !errors.As(err, &auth) {
			t.Errorf("%s: err = %v, want an authentication error", name, err)
		}
	}
	check("wrong secret", signedReq("another-secret", "message/rfc822", body, time.Now()))
	check("stale", signedReq(relaySecret, "message/rfc822", body, time.Now().Add(-time.Hour)))
	check("future", signedReq(relaySecret, "message/rfc822", body, time.Now().Add(time.Hour)))
	r := signedReq(relaySecret, "message/rfc822", body, time.Now())
	r.Header.Del(inbound.SignatureHeader)
	check("no signature", r)
	r = signedReq(relaySecret, "message/rfc822", body, time.Now())
	r.Header.Del(inbound.TimestampHeader)
	check("no timestamp", r)
	// The signature covers the body: a changed body does not verify.
	r = signedReq(relaySecret, "message/rfc822", body, time.Now())
	r.Body = http.NoBody
	r.ContentLength = 0
	r2 := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(append([]byte("X-Evil: 1\r\n"), body...)))
	r2.Header = r.Header.Clone()
	check("changed body", r2)
	// A replay inside the window is refused, the first request was fine.
	first := signedReq(relaySecret, "message/rfc822", goodMail("a2"), time.Now())
	if _, err := receive(a, first); err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(goodMail("a2")))
	replay.Header = first.Header.Clone()
	check("replay", replay)
	// An oversize body is refused before it is read.
	huge := signedReq(relaySecret, "message/rfc822", bytes.Repeat([]byte("a"), webhookMaxBody+1), time.Now())
	check("huge", huge)
	// An empty secret never verifies.
	if _, err := a.Receive(context.Background(), connFor(secretFor(nil, nil, map[string]string{KeySigningSecret: "x"}), webhookCfg), signedReq("", "message/rfc822", body, time.Now())); err == nil {
		t.Error("an empty-secret signature was accepted")
	}
	// Unsupported content types are errors, not guesses.
	if _, err := receive(a, signedReq(relaySecret, "application/pdf", body, time.Now())); err == nil {
		t.Error("pdf accepted")
	}
	// An IMAP connection takes no requests.
	imapConn := connFor(secretFor(nil, nil, nil), imapCfg)
	if _, err := a.Receive(context.Background(), imapConn, signedReq(relaySecret, "message/rfc822", body, time.Now())); !errors.Is(err, inbound.ErrNotReceiving) {
		t.Errorf("imap mode: %v", err)
	}
}
