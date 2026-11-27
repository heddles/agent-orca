package slack

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestPostSendsOneInertMessageAndExplainsRefusals(t *testing.T) {
	api := newFakeSlack(t)
	a, c := adapter(api.srv.URL), conn(t, defaultSettings)
	if err := a.Post(context.Background(), c, "C0REPORTS", "hello"); err != nil {
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
	err := a.Post(context.Background(), c, "C0REPORTS", "hello")
	if err == nil || !strings.Contains(err.Error(), "invite the app to the channel") || strings.Contains(err.Error(), botTok) {
		t.Errorf("err = %v", err)
	}
	if len(api.calls) != 2 {
		t.Errorf("Slack's own refusal was repeated: %d calls", len(api.calls))
	}
}
