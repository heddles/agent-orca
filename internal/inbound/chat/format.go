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

package chat

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/heddles/agent-orca/internal/inbound"
)

// zwsp is a zero-width space. Inserted after an "@" it keeps a platform from
// reading the word that follows as a mention; it does not show.
const zwsp = "\u200b"

// Render turns the agent's Markdown into the messages to send to a platform with
// these capabilities: converted to its dialect, with every mention neutralised,
// and cut into messages of at most MaxReplyChars (at most MaxReplyParts of them;
// beyond that the last one is shortened with a note). Never returns an empty
// list for non-blank text.
func Render(text string, caps inbound.Capabilities) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	limit := caps.MaxReplyChars
	if limit <= 0 {
		return []string{Format(text, caps.Dialect)}
	}
	var out []string
	for _, src := range Split(text, limit) {
		f := Format(src, caps.Dialect)
		if utf8.RuneCountInString(f) <= limit {
			out = append(out, f)
			continue
		}
		// Conversion made it longer (escaping): cut the converted text.
		out = append(out, Split(f, limit)...)
	}
	return capParts(out, max(1, caps.MaxReplyParts), limit)
}

const shortenedNote = "\n\n[The answer was shortened to fit this channel.]"

func capParts(parts []string, n, limit int) []string {
	if len(parts) <= n {
		return parts
	}
	parts = parts[:n]
	last := []rune(parts[n-1])
	keep := limit - utf8.RuneCountInString(shortenedNote)
	if keep < 1 {
		keep = limit
	}
	if len(last) > keep {
		last = last[:keep]
	}
	parts[n-1] = string(last) + shortenedNote
	return parts
}

// Split cuts text into pieces of at most max characters, at a paragraph break if
// there is one in the second half of the piece, else at a line break, else at a
// space, else wherever the limit falls. A code fence that a cut falls inside is
// closed at the end of the piece and opened again at the start of the next.
func Split(text string, max int) []string {
	if max <= 0 || utf8.RuneCountInString(text) <= max {
		return []string{text}
	}
	const fence = "```"
	reserve := 0
	if max > 40 {
		reserve = len(fence) + 2 // room for the closing fence
	}
	var out []string
	rest := text
	for utf8.RuneCountInString(rest) > max {
		runes := []rune(rest)
		window := string(runes[:max-reserve])
		cut := lastBreak(window)
		piece, tail := rest[:cut], rest[cut:]
		if reserve > 0 && strings.Count(piece, fence)%2 == 1 {
			piece += "\n" + fence
			tail = fence + "\n" + strings.TrimLeft(tail, "\n")
		} else {
			tail = strings.TrimLeft(tail, "\n ")
		}
		out = append(out, strings.TrimRight(piece, " \n"))
		rest = tail
	}
	if strings.TrimSpace(rest) != "" {
		out = append(out, rest)
	}
	return out
}

// lastBreak is the byte index at which to cut window.
func lastBreak(window string) int {
	half := len(window) / 2
	if i := strings.LastIndex(window, "\n\n"); i > half {
		return i
	}
	if i := strings.LastIndex(window, "\n"); i > half {
		return i
	}
	if i := strings.LastIndex(window, " "); i > half {
		return i
	}
	return len(window)
}

// Format converts Markdown to a dialect and neutralises everything the platform
// could read as a mention or a link that pings. The conversion is deliberately
// small: bold, italic, strikethrough, inline code, fenced code, headings, bullet
// lists and links. Anything else passes as text (escaped where the dialect needs
// it). Links are kept only for http and https.
func Format(text string, d inbound.Dialect) string {
	var b strings.Builder
	for _, s := range segments(text) {
		switch {
		case s.fence:
			b.WriteString(fenceOut(s.text, d))
		case s.code:
			b.WriteString(codeOut(s.text, d))
		default:
			b.WriteString(proseOut(s.text, d))
		}
	}
	return b.String()
}

type seg struct {
	text        string
	code, fence bool
}

var (
	fenceRe = regexp.MustCompile("(?s)```(.*?)(```|$)")
	spanRe  = regexp.MustCompile("`[^`\n]+`")
)

// segments splits text into prose, inline code and fenced blocks.
func segments(text string) []seg {
	var out []seg
	for len(text) > 0 {
		loc := fenceRe.FindStringIndex(text)
		if loc == nil {
			out = append(out, spans(text)...)
			break
		}
		out = append(out, spans(text[:loc[0]])...)
		out = append(out, seg{text: text[loc[0]:loc[1]], fence: true})
		text = text[loc[1]:]
	}
	return out
}

func spans(text string) []seg {
	var out []seg
	for len(text) > 0 {
		loc := spanRe.FindStringIndex(text)
		if loc == nil {
			return append(out, seg{text: text})
		}
		if loc[0] > 0 {
			out = append(out, seg{text: text[:loc[0]]})
		}
		out = append(out, seg{text: text[loc[0]:loc[1]], code: true})
		text = text[loc[1]:]
	}
	return out
}

var mrkdwnEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeFor(s string, d inbound.Dialect) string {
	switch d {
	case inbound.DialectMrkdwn:
		return mrkdwnEsc.Replace(s)
	case inbound.DialectHTML:
		return html.EscapeString(s)
	}
	return s
}

// fenceOut renders a fenced block. Inside code a platform interprets nothing
// (a mention in a code block does not ping), but the dialect's own escapes still apply.
func fenceOut(raw string, d inbound.Dialect) string {
	body := strings.TrimPrefix(raw, "```")
	closed := strings.HasSuffix(body, "```") && len(body) >= 3
	if closed {
		body = strings.TrimSuffix(body, "```")
	}
	// Drop the language tag: not every platform shows it as a label.
	if i := strings.IndexByte(body, '\n'); i >= 0 && !strings.ContainsAny(body[:i], " \t") {
		body = body[i+1:]
	}
	body = strings.Trim(body, "\n")
	switch d {
	case inbound.DialectHTML:
		return "<pre><code>" + html.EscapeString(body) + "</code></pre>"
	case inbound.DialectMrkdwn:
		return "```\n" + mrkdwnEsc.Replace(strings.ReplaceAll(body, "```", "'''")) + "\n```"
	default:
		return "```\n" + strings.ReplaceAll(body, "```", "'''") + "\n```"
	}
}

func codeOut(raw string, d inbound.Dialect) string {
	body := strings.Trim(raw, "`")
	switch d {
	case inbound.DialectHTML:
		return "<code>" + html.EscapeString(body) + "</code>"
	case inbound.DialectMrkdwn:
		return "`" + mrkdwnEsc.Replace(body) + "`"
	case inbound.DialectMrkdwnRaw:
		return "`" + strings.ReplaceAll(body, "`", "'") + "`"
	default:
		return raw
	}
}

var (
	linkRe    = regexp.MustCompile(`\[([^\]\n]{1,200})\]\(([^)\s]{1,1000})\)`)
	bareURLRe = regexp.MustCompile(`https?://[^\s<>()\[\]` + "`" + `]+`)
	boldRe    = regexp.MustCompile(`\*\*([^\n]+?)\*\*|__([^\n]+?)__`)
	italicRe  = regexp.MustCompile(`(^|[^\w*])\*([^\s*][^*\n]*?)\*`)
	italic2Re = regexp.MustCompile(`(^|[^\w_])_([^\s_][^_\n]*?)_`)
	strikeRe  = regexp.MustCompile(`~~([^\n]+?)~~`)
	headRe    = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.+?)[ \t#]*$`)
	bulletRe  = regexp.MustCompile(`(?m)^([ \t]*)[-*+][ \t]+`)
	mentionRe = regexp.MustCompile(`(^|[^\p{L}\p{N}_.])@([\p{L}\p{N}_])`)
	bangRe    = regexp.MustCompile(`<(!|users/|at[\s>/])`)
)

// BreakMentions makes "@name" inert for any platform: a zero-width space follows
// the "@" wherever it starts a word, so "@here", "@channel", "@everyone", "@all",
// a user name and the "<@id>" form all stop being mentions. "<!", "<users/" and "<at>" (the special
// mention forms of the platforms that write them in angle brackets) are broken too. An e-mail address keeps its "@".
func BreakMentions(s string) string {
	s = bangRe.ReplaceAllString(s, "<"+zwsp+"$1")
	// Run twice: adjacent matches share a boundary character.
	for range 2 {
		s = mentionRe.ReplaceAllString(s, "${1}@"+zwsp+"${2}")
	}
	return s
}

// proseOut converts a run of prose.
func proseOut(s string, d inbound.Dialect) string {
	s = strings.ReplaceAll(s, "\x00", "")
	// Links and bare URLs become placeholders, so neither the conversion nor the
	// mention breaker touches an address (an "@" inside a URL is part of it).
	var holds []string
	hold := func(v string) string {
		holds = append(holds, v)
		return "\x00" + strconv.Itoa(len(holds)-1) + "\x00"
	}
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		u := sub[2]
		if !strings.HasPrefix(strings.ToLower(u), "http://") && !strings.HasPrefix(strings.ToLower(u), "https://") {
			return m
		}
		label := BreakMentions(sub[1])
		return hold(linkOut(label, u, d))
	})
	s = bareURLRe.ReplaceAllStringFunc(s, func(u string) string {
		if d == inbound.DialectHTML {
			return hold(linkOut(u, u, d))
		}
		return hold(escapeFor(u, d))
	})
	s = escapeFor(s, d)

	switch d {
	case inbound.DialectMrkdwn, inbound.DialectMrkdwnRaw:
		s = headRe.ReplaceAllString(s, "**$1**")
		s = bulletRe.ReplaceAllString(s, "${1}• ")
		s = convertEmphasis(s, "*", "_", "~")
	case inbound.DialectHTML:
		s = headRe.ReplaceAllString(s, "**$1**")
		s = bulletRe.ReplaceAllString(s, "${1}• ")
		s = boldRe.ReplaceAllString(s, "<b>${1}${2}</b>")
		s = italicRe.ReplaceAllString(s, "${1}<i>${2}</i>")
		s = strikeRe.ReplaceAllString(s, "<s>$1</s>")
	}
	s = BreakMentions(s)
	if d == inbound.DialectHTML {
		s = strings.ReplaceAll(s, "\n", "<br>")
	}
	for i, h := range holds {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", h, 1)
	}
	return s
}

// convertEmphasis rewrites Markdown emphasis to a dialect that writes bold with
// one marker: **b** or __b__ become <bold>b<bold>, *i* becomes <italic>i<italic>.
func convertEmphasis(s, bold, italic, strike string) string {
	s = boldRe.ReplaceAllString(s, "\x01${1}${2}\x02")
	s = italicRe.ReplaceAllString(s, "${1}"+italic+"${2}"+italic)
	s = italic2Re.ReplaceAllString(s, "${1}"+italic+"${2}"+italic)
	s = strikeRe.ReplaceAllString(s, strike+"$1"+strike)
	s = strings.ReplaceAll(s, "\x01", bold)
	return strings.ReplaceAll(s, "\x02", bold)
}

func linkOut(label, u string, d inbound.Dialect) string {
	switch d {
	case inbound.DialectMrkdwn:
		u = strings.NewReplacer("&", "&amp;", "<", "%3C", ">", "%3E", "|", "%7C", " ", "%20").Replace(u)
		label = mrkdwnEsc.Replace(strings.NewReplacer("|", "/", "\n", " ").Replace(label))
		return "<" + u + "|" + label + ">"
	case inbound.DialectHTML:
		return `<a href="` + html.EscapeString(u) + `">` + html.EscapeString(label) + `</a>`
	case inbound.DialectMrkdwnRaw:
		u = strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C", " ", "%20").Replace(u)
		label = strings.NewReplacer("|", "/", "\n", " ", "<", "(", ">", ")").Replace(label)
		return "<" + u + "|" + label + ">"
	case inbound.DialectMarkdown:
		return "[" + label + "](" + u + ")"
	default:
		return label + " (" + u + ")"
	}
}

// RenderBytes is Render for a platform that limits a message in bytes, not in
// characters (UTF-8 text of other scripts takes up to four bytes a character).
// The character limit of caps is lowered to what the text's own average width
// allows, and a part that is still over the byte limit is cut at a character
// boundary, so no part exceeds maxBytes.
func RenderBytes(text string, caps inbound.Capabilities, maxBytes int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if maxBytes > 0 {
		if n := len(text); n > utf8.RuneCountInString(text) {
			// Formatting can only add (escapes, markers): keep a tenth in hand.
			per := float64(n) / float64(utf8.RuneCountInString(text))
			lim := int(float64(maxBytes) / per * 0.9)
			lim = max(lim, 200)
			if caps.MaxReplyChars == 0 || lim < caps.MaxReplyChars {
				caps.MaxReplyChars = lim
			}
		}
	}
	parts := Render(text, caps)
	if maxBytes <= 0 {
		return parts
	}
	for i, p := range parts {
		for len(p) > maxBytes {
			cut := maxBytes
			for cut > 0 && !utf8.RuneStart(p[cut]) {
				cut--
			}
			p = p[:cut]
		}
		parts[i] = p
	}
	return parts
}
