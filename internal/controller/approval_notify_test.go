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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

func TestWebhookApprovalNotifier(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	n := &WebhookApprovalNotifier{URL: srv.URL, UIURL: "https://agentorca.example/"}
	a := postgresql.Approval{
		ID: "a1", Namespace: "team-a", RunName: "coder-run", Summary: "/usr/bin/curl wants to reach example.com:443",
		ApproverGroups: []string{"platform"}, ExpiresAt: time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC),
	}
	if err := n.NotifyApproval(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	text, _ := got["text"].(string)
	for _, want := range []string{"team-a", "coder-run", "example.com:443", "platform", "23:00 UTC", "<https://agentorca.example/?tab=approvals|Review it>"} {
		if !strings.Contains(text, want) {
			t.Errorf("message %q lacks %q", text, want)
		}
	}
	if appr, _ := got["approval"].(map[string]any); appr["id"] != "a1" {
		t.Errorf("approval payload = %v", got["approval"])
	}
}

func TestWebhookApprovalNotifierReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid_token", http.StatusForbidden)
	}))
	defer srv.Close()
	n := &WebhookApprovalNotifier{URL: srv.URL}
	if err := n.NotifyApproval(context.Background(), postgresql.Approval{ExpiresAt: time.Now()}); err == nil {
		t.Error("a 403 from the webhook was not reported")
	}
}
