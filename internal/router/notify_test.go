package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecuteWebhookNotify_PostsWebhook(t *testing.T) {
	var gotMethod, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		gotMethod, gotCT, gotBody = r.Method, r.Header.Get("Content-Type"), string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "posted"})
	}))
	defer srv.Close()
	t.Setenv("WEBHOOK_URL", srv.URL)

	r := &Router{}
	out := r.executeWebhookNotify(context.Background(), `{"text":"hello slack"}`)
	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %q", gotMethod)
	}
	if gotCT != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", gotCT)
	}
	if gotBody != `{"text":"hello slack"}` {
		t.Fatalf("body = %q, want {\"text\":\"hello slack\"}", gotBody)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("response = %q, want ok:true", out)
	}
}

func TestExecuteWebhookNotify_MissingEnv(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "")
	r := &Router{}
	out := r.executeWebhookNotify(context.Background(), `{"text":"hi"}`)
	if !strings.Contains(out, "not configured") {
		t.Fatalf("expected not-configured error, got %q", out)
	}
}
