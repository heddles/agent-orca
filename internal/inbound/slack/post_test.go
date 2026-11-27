package slack

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPostSendsOneInertMessageAndExplainsRefusals(t *testing.T) {
	api := newFakeSlack(t)
	a, c := adapter(api.srv.URL), conn(t, defaultSettings)
	if err := a.Post(context.Background(), c, "C0REPORTS", "hello", ""); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 || api.calls[0].method != methodPostMessage {
		t.Fatalf("calls = %+v", api.calls)
	}
	b := api.calls[0].body
	if b["channel"] != "C0REPORTS" || b["text"] != "hello" || b["parse"] != "none" || b["link_names"] != false || b["unfurl_links"] != false {
		t.Errorf("body = %+v", b)
	}
	if _, threaded := b["thread_ts"]; threaded {
		t.Errorf("a report starts its own message: %+v", b)
	}
	api.script = []func(http.ResponseWriter){nil, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`)) }}
	err := a.Post(context.Background(), c, "C0REPORTS", "hello", "")
	if err == nil || !strings.Contains(err.Error(), "invite the app to the channel") || strings.Contains(err.Error(), botTok) {
		t.Errorf("err = %v", err)
	}
	if len(api.calls) != 2 {
		t.Errorf("Slack's own refusal was repeated: %d calls", len(api.calls))
	}
}

func TestPostSendsTheWholeReportFormattedInOrder(t *testing.T) {
	api := newFakeSlack(t)
	a, c := adapter(api.srv.URL), conn(t, defaultSettings)
	var pauses []time.Duration
	a.Pause = func(_ context.Context, d time.Duration) error { pauses = append(pauses, d); return nil }
	var body strings.Builder
	body.WriteString("## **Summary**\n\n**All good** @channel <!here>\n\n| Account | Spend |\n|---|---:|\n| **Card** | $10 |\n\n")
	for i := range 600 { // far more than the five messages a reply is cut to
		fmt.Fprintf(&body, "- item %d of the report\n", i)
	}
	body.WriteString("\nTHE LAST LINE")
	if err := a.Post(context.Background(), c, "C0REPORTS", "*Schedule digest*", body.String()); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) < 4 || len(pauses) != len(api.calls)-1 || pauses[0] != reportPause {
		t.Fatalf("%d messages, pauses %v", len(api.calls), pauses)
	}
	var all strings.Builder
	for _, call := range api.calls {
		text, _ := call.body["text"].(string)
		if n := utf8.RuneCountInString(text); n > maxReplyChars {
			t.Errorf("a message is %d characters", n)
		}
		all.WriteString(text + "\n")
	}
	got := all.String()
	if !strings.HasPrefix(got, "*Schedule digest*\n*Summary*\n") {
		t.Errorf("starts %q", got[:60])
	}
	for i := range 600 {
		if !strings.Contains(got, fmt.Sprintf("• item %d of the report\n", i)) {
			t.Fatalf("item %d was lost", i)
		}
	}
	for _, want := range []string{"*All good*", "```\nAccount | Spend\n--------+------\nCard    | $10\n```", "THE LAST LINE"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got[:300])
		}
	}
	if strings.Contains(got, "<!here>") || strings.Contains(got, "@channel") || strings.Contains(got, "shortened") {
		t.Errorf("a mention survived, or text was cut: %q", got[:200])
	}
}

func TestPostSaysHowMuchOfAReportArrived(t *testing.T) {
	api := newFakeSlack(t)
	a, c := adapter(api.srv.URL), conn(t, defaultSettings)
	a.Pause = func(context.Context, time.Duration) error { return nil }
	api.script = []func(http.ResponseWriter){nil, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":false,"error":"is_archived"}`)) }}
	err := a.Post(context.Background(), c, "C0REPORTS", "header", strings.Repeat("word ", 2000))
	if err == nil || !strings.Contains(err.Error(), "only 1 of ") || len(api.calls) != 2 {
		t.Errorf("err = %v after %d calls", err, len(api.calls))
	}
}
