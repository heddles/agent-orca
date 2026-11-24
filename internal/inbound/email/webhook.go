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
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/heddles/agent-orca/internal/inbound"
)

// webhookMaxBody bounds a request: a message of MaxMessageBytes, with room for
// the base64 of an attachment in a JSON body.
const webhookMaxBody = 2*MaxMessageBytes + 64<<10

// Receive takes one message from a mail provider's inbound parse, through a relay
// that signs the request like the trigger webhooks do (internal/inbound/signed.go).
// The body is one of
//
//   - message/rfc822 (or text/plain): the raw message, headers included;
//   - application/json: {from, subject, text, html, messageId, inReplyTo,
//     references, headers, authenticationResults, attachments:[{name,
//     contentType, content (base64)}]}, with the field names of Postmark and
//     Mailgun accepted too;
//   - multipart/form-data: the same fields (SendGrid, Mailgun), file parts as
//     attachments, or one field "email" (or "raw") holding the raw message.
//
// Whatever the shape, the message then goes through the same policy as a polled one.
func (a *Adapter) Receive(_ context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.Message{}, err
	}
	if cfg.Mode != ModeWebhook {
		return inbound.Message{}, inbound.ErrNotReceiving
	}
	cr, err := ParseCredentials(cfg.Mode, c.Secret)
	if err != nil {
		return inbound.Message{}, err
	}
	body, err := inbound.ReadSigned(r, cr.SigningSecret, webhookMaxBody, a.now(), &a.replay)
	if err != nil {
		return inbound.Message{}, err
	}
	d, err := decodeWebhook(r.Header.Get("Content-Type"), body)
	if err != nil {
		return inbound.Message{}, errors.New("the message could not be read")
	}
	return d.evaluate(cfg.options(cr))
}

func decodeWebhook(contentType string, body []byte) (*doc, error) {
	mt, params, _ := mime.ParseMediaType(contentType)
	switch mt {
	case "message/rfc822", "text/plain", "":
		return decodeRFC822(body)
	case "application/json":
		return decodeJSON(body)
	case "multipart/form-data":
		return decodeForm(body, params["boundary"])
	}
	return nil, errors.New("unsupported content type")
}

// ── JSON ─────────────────────────────────────────────────────────────────────

type jsonAttachment struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// fields is a case-insensitive view of the payload's keys.
type fields map[string]json.RawMessage

func (f fields) str(keys ...string) string {
	for _, k := range keys {
		if raw, ok := f[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

func decodeJSON(body []byte) (*doc, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	f := fields{}
	for k, v := range top {
		f[strings.ToLower(strings.ReplaceAll(k, "_", "-"))] = v
	}
	d := &doc{headers: map[string][]string{}}
	if raw, ok := f["headers"]; ok {
		parseHeaders(raw, d)
	} else if raw, ok := f["message-headers"]; ok {
		parseHeaders(raw, d)
	}
	set := func(h string, v string) {
		if v != "" {
			d.headers[canon(h)] = []string{headerValue(v)}
		}
	}
	set("From", f.str("from"))
	set("Subject", f.str("subject"))
	set("Message-ID", f.str("messageid", "message-id"))
	set("In-Reply-To", f.str("inreplyto", "in-reply-to"))
	set("References", f.str("references"))
	// The relay vouches for these: they go on top, as the receiving server's own would.
	var ar []string
	for _, k := range []string{"authenticationresults", "authentication-results"} {
		if raw, ok := f[k]; ok {
			var one string
			var many []string
			if json.Unmarshal(raw, &one) == nil && one != "" {
				ar = append(ar, headerValue(one))
			} else if json.Unmarshal(raw, &many) == nil {
				for _, m := range many {
					ar = append(ar, headerValue(m))
				}
			}
		}
	}
	if len(ar) > 0 {
		d.headers["authentication-results"] = ar
	}
	d.text = f.str("text", "textbody", "text-body", "body-plain", "plain")
	d.html = f.str("html", "htmlbody", "html-body", "body-html")
	size := len(d.text) + len(d.html)
	var atts []jsonAttachment
	if raw, ok := f["attachments"]; ok {
		_ = json.Unmarshal(raw, &atts)
	}
	for _, at := range atts {
		if len(d.atts) >= maxListedAttachments {
			break
		}
		n := base64.StdEncoding.DecodedLen(len(at.Content))
		var data []byte
		if n <= MaxTextAttachmentBytes+4 {
			data, _ = base64.StdEncoding.DecodeString(at.Content)
			n = len(data)
		}
		size += n
		d.atts = append(d.atts, attachment{name: at.Name, ctype: strings.ToLower(strings.TrimSpace(at.ContentType)), size: n, data: data})
	}
	d.size = size + headerBytes(d.headers)
	return d, nil
}

func headerBytes(h map[string][]string) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}

// parseHeaders reads the headers in any of the shapes providers use: an object
// of name to value(s), a list of {name, value}, a list of [name, value] pairs, or
// a raw header block as a string.
func parseHeaders(raw json.RawMessage, d *doc) {
	add := func(k, v string) {
		if k = strings.TrimSpace(k); k != "" && !strings.ContainsAny(k, "\r\n:") {
			d.headers[canon(k)] = append(d.headers[canon(k)], headerValue(v))
		}
	}
	var block string
	if json.Unmarshal(raw, &block) == nil {
		parseBlock(block, d)
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			var one string
			var many []string
			if json.Unmarshal(v, &one) == nil {
				add(k, one)
			} else if json.Unmarshal(v, &many) == nil {
				for _, m := range many {
					add(k, m)
				}
			}
		}
		return
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	for _, item := range list {
		var kv struct{ Name, Value string }
		var pair []string
		if json.Unmarshal(item, &kv) == nil && kv.Name != "" {
			add(kv.Name, kv.Value)
		} else if json.Unmarshal(item, &pair) == nil && len(pair) == 2 {
			add(pair[0], pair[1])
		}
	}
}

func parseBlock(block string, d *doc) {
	h, _ := textproto.NewReader(bufioReader(strings.TrimSpace(block) + "\r\n\r\n")).ReadMIMEHeader()
	for k, vs := range h {
		for _, v := range vs {
			d.headers[canon(k)] = append(d.headers[canon(k)], headerValue(v))
		}
	}
}

// ── multipart/form-data ──────────────────────────────────────────────────────

func decodeForm(body []byte, boundary string) (*doc, error) {
	if boundary == "" {
		return nil, errors.New("no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	top := map[string]json.RawMessage{}
	var atts []attachment
	var rawMail []byte
	var ar []string
	for range maxParts {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		name := strings.ToLower(strings.ReplaceAll(p.FormName(), "_", "-"))
		data, _ := io.ReadAll(io.LimitReader(p, MaxMessageBytes+1))
		switch {
		case name == "email" || name == "raw":
			rawMail = data
		case p.FileName() != "":
			keep := data
			if len(keep) > MaxTextAttachmentBytes+1 {
				keep = keep[:0]
			}
			atts = append(atts, attachment{name: p.FileName(), ctype: strings.ToLower(strings.TrimSpace(strings.SplitN(p.Header.Get("Content-Type"), ";", 2)[0])), size: len(data), data: keep})
		case name == "authenticationresults" || name == "authentication-results":
			ar = append(ar, headerValue(string(data)))
		default:
			b, _ := json.Marshal(string(data))
			top[name] = b
		}
	}
	if rawMail != nil {
		d, err := decodeRFC822(rawMail)
		if err != nil {
			return nil, err
		}
		if len(ar) > 0 {
			d.headers["authentication-results"] = append(ar, d.headers["authentication-results"]...)
		}
		return d, nil
	}
	b, _ := json.Marshal(top)
	d, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	d.atts = append(d.atts, atts...)
	for _, a := range atts {
		d.size += a.size
	}
	if len(ar) > 0 {
		d.headers["authentication-results"] = ar
	}
	return d, nil
}

func bufioReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }
