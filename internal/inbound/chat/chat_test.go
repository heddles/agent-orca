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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/heddles/agent-orca/internal/inbound"
)

func TestFormat_MrkdwnConvertsAndEscapes(t *testing.T) {
	in := "# Title\n\nSome **bold** and *italic* and ~~gone~~ text with `a<b` code.\n\n- one\n- two\n\nSee [the docs](https://example.com/a?x=1&y=2) or https://example.com/@here/page.\n\n```go\nfmt.Println(\"<hi>\")\n```"
	got := Format(in, inbound.DialectMrkdwn)
	for _, want := range []string{
		"*Title*", "*bold*", "_italic_", "~gone~", "`a&lt;b`", "• one", "• two",
		"<https://example.com/a?x=1&amp;y=2|the docs>", "https://example.com/@here/page.", "```\nfmt.Println(\"&lt;hi&gt;\")\n```",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "```go") {
		t.Error("language tag kept")
	}
}

// The point of the formatter: whatever the agent writes, the platform sees no
// mention. Each of these is a way a platform pings people.
func TestFormat_NothingPingsAnyone(t *testing.T) {
	attacks := []string{
		"@channel look", "hey @here", "@everyone!", "<!here>", "<!channel|channel>", "<!everyone>", "<!subteam^S123|@oncall>",
		"<@U12345>", "<@U12345|bob>", "@all", "@alice please", "(@bob)", "[@here](https://x.example)", "[ok](https://x.example/ @here)",
		"**@channel**", "`@here`", "line one\n@here", "<at>Everyone</at>", "<users/all>", "<@personEmail:a@b.c>", "<at id=\"0\">Bot</at>", "<users/1234567>", "<@all>", "<@personId:Y2lzY2:x>",
	}
	for _, d := range []inbound.Dialect{inbound.DialectNone, inbound.DialectMrkdwn, inbound.DialectMrkdwnRaw, inbound.DialectMarkdown, inbound.DialectHTML} {
		for _, a := range attacks {
			got := Format(a, d)
			plain := strings.ReplaceAll(got, zwsp, "")
			_ = plain
			for _, bad := range []string{"@channel", "@here", "@everyone", "@all", "<!here", "<!channel", "<!everyone", "<!subteam", "<@U", "<@personEmail", "<at>", "<users/all", "<users/1234", "<at id", "<@all", "<@personId"} {
				// Inside code the platform interprets nothing; elsewhere the raw token must be gone.
				if strings.Contains(got, bad) && !insideCode(got, bad) {
					t.Errorf("%q dialect %q still holds %q: %q", a, d, bad, got)
				}
			}
		}
	}
	if got := Format("mail alice@example.com and @bob", inbound.DialectMarkdown); !strings.Contains(got, "alice@example.com") || strings.Contains(got, " @bob") {
		t.Errorf("email kept, mention broken: %q", got)
	}
}

// insideCode reports whether every occurrence of s in text is within backticks.
func insideCode(text, s string) bool {
	for i := strings.Index(text, s); i >= 0; {
		if strings.Count(text[:i], "`")%2 == 0 && !strings.HasSuffix(text[:i], "<code>") {
			return false
		}
		next := strings.Index(text[i+len(s):], s)
		if next < 0 {
			break
		}
		i += len(s) + next
	}
	return true
}

func TestFormat_HTMLAndMarkdownAndNone(t *testing.T) {
	in := "**b** <script>alert(1)</script> [l](https://e.example/x) [bad](javascript:alert(1))\nnext"
	h := Format(in, inbound.DialectHTML)
	for _, want := range []string{"<b>b</b>", "&lt;script&gt;", `<a href="https://e.example/x">l</a>`, "<br>next"} {
		if !strings.Contains(h, want) {
			t.Errorf("html missing %q: %s", want, h)
		}
	}
	if strings.Contains(h, "<script>") || strings.Contains(h, `href="javascript`) {
		t.Errorf("html is unsafe: %s", h)
	}
	if m := Format(in, inbound.DialectMarkdown); !strings.Contains(m, "[l](https://e.example/x)") || !strings.Contains(m, "**b**") {
		t.Errorf("markdown = %s", m)
	}
	if n := Format("[l](https://e.example/x)", inbound.DialectNone); n != "l (https://e.example/x)" {
		t.Errorf("none = %q", n)
	}
	// A NUL in the agent's text cannot forge a placeholder.
	if got := Format("a\x000\x00b https://e.example", inbound.DialectMarkdown); strings.Contains(got, "\x00") {
		t.Errorf("NUL left: %q", got)
	}
}

func TestSplit(t *testing.T) {
	para := strings.Repeat("word ", 30)
	text := para + "\n\n" + para + "\n\n" + para
	parts := Split(text, 200)
	if len(parts) < 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 200 {
			t.Errorf("part of %d chars", utf8.RuneCountInString(p))
		}
	}
	if strings.Join(strings.Fields(strings.Join(parts, " ")), " ") != strings.Join(strings.Fields(text), " ") {
		t.Error("text lost or reordered by the split")
	}
	// A code fence cut in two is closed and reopened.
	code := "intro\n```\n" + strings.Repeat("line of code\n", 40) + "```\noutro"
	cp := Split(code, 150)
	for i, p := range cp {
		if strings.Count(p, "```")%2 != 0 {
			t.Errorf("part %d has an unbalanced fence: %q", i, p)
		}
	}
	// No break at all: a hard cut that respects characters.
	if hp := Split(strings.Repeat("é", 100), 30); len(hp) != 4 || utf8.RuneCountInString(hp[0]) != 30 {
		t.Errorf("hard cut = %d parts", len(hp))
	}
	if one := Split("short", 100); len(one) != 1 || one[0] != "short" {
		t.Errorf("short = %v", one)
	}
}

func TestRender_PartsAndCap(t *testing.T) {
	caps := inbound.Capabilities{Dialect: inbound.DialectMrkdwn, MaxReplyChars: 100, MaxReplyParts: 3}
	long := strings.Repeat("a < b & c. ", 100) // escaping makes it longer
	parts := Render(long, caps)
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 100 {
			t.Errorf("part too long: %d", utf8.RuneCountInString(p))
		}
	}
	if !strings.Contains(parts[2], "shortened") {
		t.Error("the last part does not say it was shortened")
	}
	if got := Render("hi", caps); len(got) != 1 || got[0] != "hi" {
		t.Errorf("short = %v", got)
	}
	if Render("  \n", caps) != nil {
		t.Error("blank text produced a message")
	}
	one := Render(strings.Repeat("x", 500), inbound.Capabilities{MaxReplyChars: 100})
	if len(one) != 1 || !strings.Contains(one[0], "shortened") {
		t.Errorf("without parts the reply is cut: %d parts", len(one))
	}
}

// ── client ───────────────────────────────────────────────────────────────────

func fastClient() *Client {
	return &Client{AllowPrivate: true, Wait: func(context.Context, time.Duration) error { return nil }}
}

func TestClient_RetriesAndRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var waits []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("header lost on retry")
		}
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	c := fastClient()
	c.Wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	resp, err := c.Do(context.Background(), http.MethodPost, srv.URL, http.Header{"Authorization": {"Bearer tok"}}, []byte(`{}`))
	if err != nil || resp.Status != 200 || calls.Load() != 2 || len(waits) != 1 || waits[0] != 7*time.Second {
		t.Fatalf("resp=%+v err=%v calls=%d waits=%v", resp, err, calls.Load(), waits)
	}
}

func TestClient_LongRetryAfterIsReturnedNotWaited(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	resp, err := fastClient().Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err != nil || resp.Status != 429 || resp.RetryAfter != time.Hour || calls.Load() != 1 {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls.Load())
	}
}

func TestClient_ServerErrorsAreBoundedAndBodiesNeverInErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("secret-body"))
	}))
	defer srv.Close()
	resp, err := fastClient().Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err != nil || resp.Status != 502 || calls.Load() != 3 {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls.Load())
	}
	// A dead server: the error names neither the URL nor anything else.
	srv.Close()
	_, err = fastClient().Do(context.Background(), http.MethodGet, srv.URL+"/path?token=abc", nil, nil)
	if err == nil || strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error = %v", err)
	}
}

func TestClient_SSRFGuardAndNoRedirects(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "http://169.254.169.254/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	// The default client refuses a loopback destination, and does not retry that.
	_, err := (&Client{Wait: func(context.Context, time.Duration) error { return nil }}).Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err == nil || hits.Load() != 0 {
		t.Errorf("loopback reached: err=%v hits=%d", err, hits.Load())
	}
	// A redirect is returned as it is, not followed.
	resp, err := fastClient().Do(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err != nil || resp.Status != http.StatusFound || hits.Load() != 1 {
		t.Errorf("redirect: resp=%+v err=%v hits=%d", resp, err, hits.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"": 0, "5": 5 * time.Second, "-3": 0, "junk": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
	} {
		if got := ParseRetryAfter(in, now); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFormat_MrkdwnRawKeepsEntitiesAndLinks(t *testing.T) {
	in := "# Title\n\n**bold** *it* ~~x~~ 1 < 2 & 3 > 2 `a<b`\n\n- one\n\n[docs](https://e.example/a?x=1&y=2|z) https://e.example/b\n\n```go\nif a < b {}\n```"
	got := Format(in, inbound.DialectMrkdwnRaw)
	for _, want := range []string{"*Title*", "*bold*", "_it_", "~x~", "1 < 2 & 3 > 2", "`a<b`", "• one", "<https://e.example/a?x=1&y=2%7Cz|docs>", "https://e.example/b", "if a < b {}"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "&amp;") || strings.Contains(got, "&lt;") {
		t.Errorf("entities in a dialect that does not decode them: %s", got)
	}
}

func TestRenderBytesNeverExceedsTheByteLimit(t *testing.T) {
	caps := inbound.Capabilities{Dialect: inbound.DialectMarkdown, MaxReplyChars: 7000, MaxReplyParts: 5}
	for name, text := range map[string]string{
		"ascii": strings.Repeat("word ", 4000),
		"cjk":   strings.Repeat("日本語のテキスト。", 1500),
		"mixed": strings.Repeat("emoji 😀 and text. ", 1200),
	} {
		parts := RenderBytes(text, caps, 7439)
		if len(parts) == 0 || len(parts) > 5 {
			t.Errorf("%s: %d parts", name, len(parts))
		}
		for _, p := range parts {
			if len(p) > 7439 || !utf8.ValidString(p) {
				t.Errorf("%s: a part of %d bytes (valid=%v)", name, len(p), utf8.ValidString(p))
			}
		}
	}
	if RenderBytes("  ", caps, 100) != nil {
		t.Error("blank text produced parts")
	}
}

func TestTokenCache(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &TokenCache{Now: func() time.Time { return now }}
	n := 0
	var fetchErr error
	fetch := func(context.Context) (Token, time.Duration, error) {
		n++
		return NewToken("tok" + string(rune('0'+n))), time.Hour, fetchErr
	}
	get := func(conn, fp string) Token {
		tok, err := c.Get(context.Background(), conn, fp, fetch)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	first := get("a", "f1")
	if again := get("a", "f1"); first.Value() != "tok1" || again.Value() != "tok1" || n != 1 {
		t.Fatalf("a token was not reused (fetches=%d)", n)
	}
	if get("b", "f1").Value() != "tok2" {
		t.Error("connections share a token")
	}
	now = now.Add(56 * time.Minute) // inside the five minute margin of an hour
	if get("a", "f1").Value() != "tok3" {
		t.Error("a token about to expire was reused")
	}
	if get("a", "f2").Value() != "tok4" {
		t.Error("the credentials changed but the token was kept")
	}
	// A failed fetch is not cached and does not leave a token behind.
	if _, err := c.Get(context.Background(), "a", "f3", func(context.Context) (Token, time.Duration, error) { return Token{}, 0, context.DeadlineExceeded }); err == nil {
		t.Error("error swallowed")
	}
	if get("a", "f3").Value() == "" {
		t.Error("no token after a failed fetch")
	}
	// A token that lives shorter than twice the margin still gets used.
	short := &TokenCache{Now: func() time.Time { return now }}
	if _, err := short.Get(context.Background(), "s", "f", func(context.Context) (Token, time.Duration, error) { return NewToken("x"), 4 * time.Minute, nil }); err != nil {
		t.Fatal(err)
	}
	n = 0
	if tok, _ := short.Get(context.Background(), "s", "f", fetch); tok.Value() != "x" || n != 0 {
		t.Error("a short-lived token was refetched at once")
	}
	if s := fmt2(NewToken("sekrit")); strings.Contains(s, "sekrit") {
		t.Errorf("the token prints: %s", s)
	}
}

func fmt2(t Token) string { return fmt.Sprintf("%v %+v %#v %s", t, t, t, t) }
