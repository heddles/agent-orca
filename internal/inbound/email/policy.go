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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/mail"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode legacy charsets to UTF-8
	"golang.org/x/net/html"

	"github.com/heddles/agent-orca/internal/inbound"
)

// doc is a message reduced to what the policy needs, whichever way it arrived.
type doc struct {
	// headers are the top-level header fields, values in the order they appear
	// (the topmost first).
	headers map[string][]string
	text    string
	html    string
	atts    []attachment
	// size is the size of the whole message as it arrived.
	size int
}

type attachment struct {
	name, ctype string
	size        int
	data        []byte // kept only up to MaxTextAttachmentBytes
}

func (d *doc) get(k string) string {
	if v := d.headers[canon(k)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (d *doc) has(k string) bool { return len(d.headers[canon(k)]) > 0 }

func canon(k string) string { return strings.ToLower(k) }

// ── parsing a raw message ────────────────────────────────────────────────────

// decodeRFC822 reads a raw message. Transfer encodings and charsets are decoded
// to UTF-8; nothing is fetched and nothing is executed.
func decodeRFC822(raw []byte) (*doc, error) {
	e, err := gomessage.Read(bytes.NewReader(raw))
	if e == nil || (err != nil && !gomessage.IsUnknownCharset(err) && !gomessage.IsUnknownEncoding(err)) {
		return nil, errors.New("the message is not readable")
	}
	d := &doc{headers: map[string][]string{}, size: len(raw)}
	for f := e.Header.Fields(); f.Next(); {
		k := canon(f.Key())
		d.headers[k] = append(d.headers[k], decodeHeaderValue(k, f.Value()))
	}
	parts := 0
	walk(e, 0, &parts, d)
	return d, nil
}

// decodeHeaderValue decodes RFC 2047 words in the free-text headers.
func decodeHeaderValue(key, v string) string {
	if key == "subject" || key == "from" || key == "to" {
		if dec, err := (&mime.WordDecoder{}).DecodeHeader(v); err == nil {
			return dec
		}
	}
	return v
}

// walk collects the text parts and the attachment descriptors of an entity.
func walk(e *gomessage.Entity, depth int, parts *int, d *doc) {
	*parts++
	if *parts > maxParts || depth > maxDepth {
		return
	}
	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if err != nil {
				return
			}
			walk(p, depth+1, parts, d)
			if *parts > maxParts {
				return
			}
		}
	}
	ctype, _, _ := e.Header.ContentType()
	if ctype == "" {
		ctype = "text/plain"
	}
	disp, dparams, _ := e.Header.ContentDisposition()
	name := dparams["filename"]
	if name == "" {
		_, cparams, _ := e.Header.ContentType()
		name = cparams["name"]
	}
	isText := strings.HasPrefix(ctype, "text/plain") || strings.HasPrefix(ctype, "text/html")
	if isText && disp != "attachment" && name == "" {
		body, _ := io.ReadAll(io.LimitReader(e.Body, MaxMessageBytes))
		switch {
		case strings.HasPrefix(ctype, "text/html"):
			if d.html == "" {
				d.html = string(body)
			}
		default:
			if d.text == "" {
				d.text = string(body)
			}
		}
		return
	}
	// An attachment (or any part that is not the body): keep a little of the
	// content only where the policy could use it, and count the rest.
	keep := int64(0)
	if looksTextual(ctype, name) {
		keep = MaxTextAttachmentBytes + 1
	}
	var data []byte
	n := 0
	if keep > 0 {
		data, _ = io.ReadAll(io.LimitReader(e.Body, keep))
		n = len(data)
		rest, _ := io.Copy(io.Discard, io.LimitReader(e.Body, MaxMessageBytes))
		n += int(rest)
	} else {
		c, _ := io.Copy(io.Discard, io.LimitReader(e.Body, MaxMessageBytes))
		n = int(c)
	}
	if len(d.atts) < maxListedAttachments {
		d.atts = append(d.atts, attachment{name: name, ctype: ctype, size: n, data: data})
	}
}

func looksTextual(ctype, name string) bool {
	return textTypes[strings.ToLower(ctype)] || textExts[strings.ToLower(path.Ext(name))]
}

// ── the policy ───────────────────────────────────────────────────────────────

// Options are the settings of one evaluation.
type Options struct {
	// Own is the connection's own address: mail from it is never answered.
	Own                   string
	RequireAuth           bool
	TrustedAuthservID     string
	AcceptTextAttachments bool
	// Domain is the domain of Own, used for the Message-ID of replies and to
	// recognise replies to ours.
	Domain string
}

// Evaluate turns a message into the core's Message, or a *inbound.Rejection. The
// Message returned with a rejection carries the identity fields so it can be
// recorded.
func (d *doc) evaluate(o Options) (inbound.Message, error) {
	m := inbound.Message{Size: d.size}
	msgID := firstID(d.get("Message-ID"))
	if msgID == "" {
		// No id to de-duplicate on: a digest of the message stands in for it.
		sum := sha256.Sum256([]byte(strings.Join(d.headers["date"], "") + d.get("From") + d.get("Subject") + d.text + d.html))
		msgID = "synthetic:" + hex.EncodeToString(sum[:16])
	}
	m.MessageID = msgID

	if d.size > MaxMessageBytes {
		return m, &inbound.Rejection{Reason: "too_large"}
	}
	from, why := singleFrom(d)
	if why != "" {
		return m, &inbound.Rejection{Reason: why}
	}
	m.Sender = inbound.Sender{ID: from.addr, Group: from.domain, Display: from.display}

	if reason := automated(d, o.Own, from.addr); reason != "" {
		return m, &inbound.Rejection{Reason: reason, Ignore: true}
	}
	if o.RequireAuth {
		if ok, reason := AuthPassed(d.headers["authentication-results"], from.domain, o.TrustedAuthservID); !ok {
			return m, &inbound.Rejection{Reason: reason}
		}
	}

	body := d.text
	if strings.TrimSpace(body) == "" && d.html != "" {
		body = HTMLToText(d.html)
	}
	body = StripQuoted(cleanText(body), MaxBodyChars)
	subject := oneLineText(d.get("Subject"), 200)
	m.Title = subject
	m.Text = body

	for i, a := range d.atts {
		info := inbound.AttachmentInfo{Name: oneLineText(a.name, 100), ContentType: oneLineText(a.ctype, 60), Size: a.size}
		if info.Name == "" {
			info.Name = "attachment-" + string(rune('1'+i%9))
		}
		if o.AcceptTextAttachments && acceptableAttachment(a) && accepted(m.Attachments) < MaxAcceptedAttachments {
			info.Text = cleanText(string(a.data))
		}
		m.Attachments = append(m.Attachments, info)
	}
	if strings.TrimSpace(m.Text) == "" && accepted(m.Attachments) == 0 {
		return m, &inbound.Rejection{Reason: "empty", Ignore: true}
	}

	refs := idList(d.get("References"))
	inReplyTo := firstID(d.get("In-Reply-To"))
	m.ThreadID = threadRoot(refs, inReplyTo, msgID, o.Domain)
	target, _ := marshalTarget(replyTarget{
		To: from.addr, Subject: subject, MessageID: msgID, References: lastN(append(refs, msgID), 10), Root: m.ThreadID,
	})
	m.Reply = target
	return m, nil
}

func accepted(a []inbound.AttachmentInfo) int {
	n := 0
	for _, x := range a {
		if x.Text != "" {
			n++
		}
	}
	return n
}

type sender struct{ addr, domain, display string }

// singleFrom returns the one From address, or why the message is refused.
func singleFrom(d *doc) (sender, string) {
	vals := d.headers["from"]
	if len(vals) != 1 {
		return sender{}, reasonBadFrom
	}
	list, err := mail.ParseAddressList(vals[0])
	if err != nil || len(list) != 1 {
		return sender{}, reasonBadFrom
	}
	a := strings.ToLower(strings.TrimSpace(list[0].Address))
	at := strings.LastIndexByte(a, '@')
	if at < 1 || at == len(a)-1 || strings.ContainsAny(a, "\r\n \t,;<>\"") {
		return sender{}, reasonBadFrom
	}
	return sender{addr: a, domain: a[at+1:], display: oneLineText(list[0].Name, 100)}, ""
}

const (
	reasonBadFrom = "bad_from"
	resultPass    = "pass"
)

var noReplyLocal = regexp.MustCompile(`^(mailer-daemon|postmaster|no[-_.]?reply|do[-_.]?not[-_.]?reply|bounces?([-_.+].*)?|abuse|root|daemon|nobody|autoreply|auto[-_.]?reply)$`)

// automated says why a message must not be answered: an automatic message, a
// list, a bounce, or this mailbox's own. Two such systems answering each other
// is how mail loops start.
func automated(d *doc, own, from string) string {
	if v := strings.ToLower(strings.TrimSpace(d.get("Auto-Submitted"))); v != "" && v != "no" {
		return "auto_submitted"
	}
	switch strings.ToLower(strings.TrimSpace(d.get("Precedence"))) {
	case "bulk", "junk", "list", "auto_reply", "auto-reply":
		return "precedence"
	}
	if slices.ContainsFunc([]string{"X-Autoreply", "X-Autorespond", "X-Auto-Response-Suppress", "List-Id", "List-Unsubscribe", "X-Loop"}, d.has) {
		return "automated_header"
	}
	if rp := strings.TrimSpace(d.get("Return-Path")); rp == "<>" {
		return "bounce"
	}
	if strings.HasPrefix(strings.ToLower(d.get("Content-Type")), "multipart/report") {
		return "report"
	}
	if own != "" && from == strings.ToLower(own) {
		return "own_address"
	}
	if local, _, ok := strings.Cut(from, "@"); ok && noReplyLocal.MatchString(local) {
		return "noreply_sender"
	}
	return ""
}

// ── Authentication-Results ───────────────────────────────────────────────────

// AuthPassed reports whether the receiving server vouches for the From domain.
//
// The evidence is the Authentication-Results header (RFC 8601) that your own
// receiving mail server adds. Anything else in a message is the sender's own
// claim. With trusted set, only headers whose authserv-id is that host name
// count: a sender cannot forge one, because a server strips or renames incoming
// headers that claim its identity. Without it, only the topmost header counts
// (the last server to touch the message adds its header on top): that is sound
// only if your server adds one to every message, so set trusted.
//
// A header vouches when it says dmarc=pass, or dkim=pass for a domain aligned
// with the From domain, or spf=pass for an aligned envelope domain. A dmarc
// failure or any header that says dmarc=fail vouches for nothing.
func AuthPassed(headers []string, fromDomain, trusted string) (bool, string) {
	if len(headers) == 0 {
		return false, "no_auth_results"
	}
	candidates := headers
	if trusted != "" {
		candidates = nil
		for _, h := range headers {
			if id, _ := splitAuthserv(h); strings.EqualFold(id, trusted) {
				candidates = append(candidates, h)
			}
		}
		if len(candidates) == 0 {
			return false, "no_trusted_auth_results"
		}
	}
	h := candidates[0] // the topmost of those that count
	_, results := splitAuthserv(h)
	var dmarc, dkim, spf bool
	for _, r := range results {
		switch r.method {
		case "dmarc":
			if r.result == resultPass {
				dmarc = true
			} else {
				return false, "dmarc_" + shortResult(r.result)
			}
		case "dkim":
			if r.result == resultPass && aligned(r.props["header.d"], fromDomain) {
				dkim = true
			}
		case "spf":
			if r.result == resultPass && aligned(domainOf(r.props["smtp.mailfrom"]), fromDomain) {
				spf = true
			}
		}
	}
	if dmarc || dkim || spf {
		return true, ""
	}
	return false, "auth_not_aligned"
}

func shortResult(s string) string {
	if s == "" || len(s) > 12 {
		return "fail"
	}
	return s
}

func domainOf(s string) string {
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// aligned is relaxed alignment: the authenticated domain is the From domain or a
// parent of it, never a sibling or a child.
func aligned(authenticated, from string) bool {
	a, f := strings.ToLower(strings.Trim(authenticated, ". ")), strings.ToLower(from)
	return a != "" && (f == a || strings.HasSuffix(f, "."+a))
}

type authResult struct {
	method, result string
	props          map[string]string
}

var parenComment = regexp.MustCompile(`\([^()]*\)`)

// splitAuthserv parses one Authentication-Results value into its authserv-id (empty
// if the server wrote none, as some do) and its results.
func splitAuthserv(v string) (string, []authResult) {
	for parenComment.MatchString(v) {
		v = parenComment.ReplaceAllString(v, " ")
	}
	segs := strings.Split(v, ";")
	id := ""
	first := strings.Fields(segs[0])
	if len(first) > 0 && !strings.Contains(first[0], "=") {
		id = strings.ToLower(first[0])
		segs = segs[1:]
	}
	var out []authResult
	for _, seg := range segs {
		f := strings.Fields(seg)
		if len(f) == 0 {
			continue
		}
		method, result, ok := strings.Cut(f[0], "=")
		if !ok {
			continue
		}
		if i := strings.IndexByte(method, '/'); i >= 0 {
			method = method[:i] // dkim/1
		}
		r := authResult{method: strings.ToLower(method), result: strings.ToLower(result), props: map[string]string{}}
		for _, p := range f[1:] {
			if k, val, ok := strings.Cut(p, "="); ok {
				r.props[strings.ToLower(k)] = strings.Trim(val, `"`)
			}
		}
		out = append(out, r)
	}
	return id, out
}

// ── attachments ──────────────────────────────────────────────────────────────

var textTypes = map[string]bool{"text/plain": true, "text/csv": true, "text/markdown": true, "text/x-markdown": true}
var textExts = map[string]bool{".txt": true, ".csv": true, ".md": true, ".markdown": true}

// neverExts are file types that are never passed on, whatever the content type says.
var neverExts = map[string]bool{
	".exe": true, ".dll": true, ".bat": true, ".cmd": true, ".com": true, ".scr": true, ".msi": true, ".js": true, ".jse": true,
	".vbs": true, ".vbe": true, ".wsf": true, ".ps1": true, ".sh": true, ".jar": true, ".apk": true, ".app": true, ".lnk": true,
	".hta": true, ".py": true, ".pl": true, ".rb": true, ".php": true, ".html": true, ".htm": true, ".svg": true, ".docm": true,
	".xlsm": true, ".iso": true, ".img": true, ".reg": true,
}

// acceptableAttachment is the policy for the one kind of attachment that may reach
// the agent: a small plain-text file. The content type and the name must both say
// text, the name must not be an executable type, and the bytes must be valid UTF-8
// without control characters other than line breaks and tabs.
func acceptableAttachment(a attachment) bool {
	ext := strings.ToLower(path.Ext(a.name))
	if neverExts[ext] || a.size > MaxTextAttachmentBytes || len(a.data) == 0 {
		return false
	}
	if !textTypes[strings.ToLower(a.ctype)] && (!strings.EqualFold(a.ctype, "application/octet-stream") || !textExts[ext]) {
		return false
	}
	if !textExts[ext] && a.name != "" {
		return false
	}
	if !utf8.Valid(a.data) {
		return false
	}
	for _, r := range string(a.data) {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}

// ── text ─────────────────────────────────────────────────────────────────────

// cleanText removes what has no business in a prompt: control characters, and the
// invisible and direction-changing characters used to hide text from a reader.
func cleanText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
			return -1
		case r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
}

func oneLineText(s string, n int) string {
	s = cleanText(s)
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

var skipElements = map[string]bool{"script": true, "style": true, "head": true, "title": true, "noscript": true, "template": true, "iframe": true, "object": true, "embed": true, "svg": true, "math": true}
var blockElements = map[string]bool{"p": true, "div": true, "br": true, "tr": true, "li": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "blockquote": true, "pre": true, "table": true, "ul": true, "ol": true, "hr": true, "section": true, "article": true}

// HTMLToText reduces HTML to its visible text. Scripts, styles and embedded
// objects are dropped with their content, as is anything marked hidden. Links
// contribute their text only: no address is shown, none is fetched. Nothing in
// this function touches the network.
func HTMLToText(src string) string {
	if len(src) > MaxMessageBytes {
		src = src[:MaxMessageBytes]
	}
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	var skipStack []string
	hidden := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			isHidden := false
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				val := strings.ToLower(string(v))
				switch string(k) {
				case "hidden":
					isHidden = true
				case "style":
					val = strings.ReplaceAll(val, " ", "")
					isHidden = isHidden || strings.Contains(val, "display:none") || strings.Contains(val, "visibility:hidden") || strings.Contains(val, "font-size:0")
				}
			}
			if tt == html.StartTagToken && (skipElements[tag] || isHidden) && !voidElement(tag) {
				skipStack = append(skipStack, tag)
				hidden++
				continue
			}
			if hidden == 0 && blockElements[tag] {
				b.WriteString("\n")
				if tag == "li" {
					b.WriteString("- ")
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if n := len(skipStack); n > 0 && skipStack[n-1] == tag {
				skipStack = skipStack[:n-1]
				hidden--
				continue
			}
			if hidden == 0 && blockElements[tag] {
				b.WriteString("\n")
			}
		case html.TextToken:
			if hidden == 0 {
				b.Write(z.Text())
			}
		}
		if b.Len() > 4*MaxBodyChars {
			break
		}
	}
	// Collapse runs of spaces, keep paragraph breaks.
	lines := strings.Split(strings.ReplaceAll(b.String(), " ", " "), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			if blank++; blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func voidElement(tag string) bool {
	switch tag {
	case "br", "hr", "img", "input", "meta", "link", "area", "base", "col", "source", "wbr":
		return true
	}
	return false
}

var (
	onWrote   = regexp.MustCompile(`(?i)^\s*(on|am|le|el|il|op|den)\b.{5,200}\b(wrote|schrieb|a écrit|escribió|ha scritto|skrev|schreef)\b.{0,120}:\s*$`)
	origLine  = regexp.MustCompile(`(?i)^\s*(-{2,}\s*(original message|forwarded message|ursprüngliche nachricht|message d'origine)\s*-{2,}|_{10,}|-{10,}\s*$)`)
	hdrFrom   = regexp.MustCompile(`(?i)^\s*(from|von|de|da)\s*:\s*\S`)
	hdrSent   = regexp.MustCompile(`(?i)^\s*(sent|date|gesendet|envoyé|enviado|datum)\s*:\s*\S`)
	sigDelim  = regexp.MustCompile(`^--\s?$`)
	quoteLine = regexp.MustCompile(`^\s*>`)
)

// StripQuoted keeps the new text of a reply: it cuts at the first sign of quoted
// history ("On ... wrote:", "-----Original Message-----", an Outlook header
// block, a line of ">") and drops the signature after "-- ". The result is at
// most max characters. If nothing is left (the reply was written between the
// quoted lines) the whole text, bounded, is kept.
func StripQuoted(text string, max int) string {
	lines := strings.Split(text, "\n")
	var kept []string
	for i := range lines {
		l := lines[i]
		cut := onWrote.MatchString(l) || origLine.MatchString(l) || sigDelim.MatchString(l) ||
			(hdrFrom.MatchString(l) && i+1 < len(lines) && (hdrSent.MatchString(lines[i+1]) || (i+2 < len(lines) && hdrSent.MatchString(lines[i+2])))) ||
			// "On Mon, Jan 1, Alice <a@b> wrote:" wrapped over two lines
			(i+1 < len(lines) && onWrote.MatchString(l+" "+strings.TrimSpace(lines[i+1])) && !quoteLine.MatchString(l))
		if cut {
			break
		}
		if quoteLine.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	out := strings.TrimSpace(strings.Join(kept, "\n"))
	if out == "" {
		out = strings.TrimSpace(text)
	}
	if utf8.RuneCountInString(out) > max {
		out = string([]rune(out)[:max])
	}
	return out
}

// ── ids and threads ──────────────────────────────────────────────────────────

var idRe = regexp.MustCompile(`<([^<>\s]{1,300})>`)

func firstID(v string) string {
	if m := idRe.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	// Providers that hand over a bare id (Postmark's MessageID).
	if v = strings.TrimSpace(v); v != "" && len(v) <= 300 && !strings.ContainsAny(v, " \t\r\n<>") {
		return v
	}
	return ""
}

func idList(v string) []string {
	all := idRe.FindAllStringSubmatch(v, 50)
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m[1])
	}
	return out
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// ourSuffix marks the Message-ID of a reply this adapter sent: the id is the
// thread's root id, encoded, then ".orca@" and the mailbox's domain. A client
// that sets only In-Reply-To (no References) still points back at the thread.
const ourSuffix = ".orca@"

func replyMessageID(root, domain string) string {
	if len(root) > 200 {
		sum := sha256.Sum256([]byte(root))
		root = "h-" + hex.EncodeToString(sum[:16])
	}
	return base64.RawURLEncoding.EncodeToString([]byte(root)) + ourSuffix + domain
}

func decodeOurID(id, domain string) (string, bool) {
	enc, ok := strings.CutSuffix(id, ourSuffix+domain)
	if !ok || enc == "" {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

// threadRoot picks the id that names a conversation: the root of a reply to one
// of ours, else the first id of References, else In-Reply-To, else the message's own.
func threadRoot(refs []string, inReplyTo, own, domain string) string {
	for _, id := range append(append([]string{}, refs...), inReplyTo) {
		if root, ok := decodeOurID(id, domain); ok {
			return root
		}
	}
	switch {
	case len(refs) > 0:
		return refs[0]
	case inReplyTo != "":
		return inReplyTo
	}
	return own
}
