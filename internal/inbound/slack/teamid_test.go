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

package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/inbound"
)

// A channel saved before its credentials exist has no workspace id. Until it has
// one it must not accept any workspace's messages, not even one that names none.
func TestWithoutAWorkspaceIDEveryMessageIsIgnored(t *testing.T) {
	a := adapter("")
	c := conn(t, defaultSettings)
	c.Config = json.RawMessage(`{}`)
	mention := map[string]any{"type": "app_mention", "user": "U1", "text": "<@B> hi", "ts": ts1, "channel": "C1"}
	for name, body := range map[string]string{
		"with a workspace":    eventBody(mention, nil),
		"without a workspace": eventBody(mention, map[string]any{"team_id": ""}),
	} {
		_, err := a.Receive(context.Background(), c, signedReq(body, "application/json"))
		var rej *inbound.Rejection
		if !errors.As(err, &rej) || !rej.Ignore || rej.Reason != "workspace_mismatch" {
			t.Errorf("%s: err = %v, want an ignored workspace_mismatch", name, err)
		}
	}
	cmd := "team_id=&user_id=U1&channel_id=C1&trigger_id=t1&text=hi"
	_, err := a.Receive(context.Background(), c, signedReq(cmd, "application/x-www-form-urlencoded"))
	var rej *inbound.Rejection
	if !errors.As(err, &rej) || rej.Reason != "workspace_mismatch" {
		t.Errorf("slash command: err = %v, want workspace_mismatch", err)
	}
}

// The address check still works: Slack verifies it before the id is known.
func TestTheChallengeIsAnsweredWithoutAWorkspaceID(t *testing.T) {
	c := conn(t, defaultSettings)
	c.Config = nil
	_, err := adapter("").Receive(context.Background(), c, signedReq(`{"type":"url_verification","challenge":"abc"}`, "application/json"))
	var direct *inbound.Response
	if !errors.As(err, &direct) || string(direct.Body) != "abc" {
		t.Fatalf("err = %v, want the challenge answered", err)
	}
}

func TestCompleteConfigLooksUpTheWorkspaceID(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/auth.test") || r.Header.Get("Authorization") != "Bearer "+botTok {
			t.Errorf("unexpected call %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true,"team_id":"T0FOUND","user_id":"UBOT"}`))
	}))
	defer srv.Close()
	a := adapter(srv.URL)
	c := conn(t, defaultSettings)

	c.Config = json.RawMessage(`{"workingReaction":true}`)
	got, err := a.CompleteConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(got)
	if err != nil || cfg.TeamID != "T0FOUND" || !cfg.WorkingReaction {
		t.Fatalf("config = %s (%v), want the id added and the rest kept", got, err)
	}

	c.Config = json.RawMessage(`{"teamId":"T0TYPED"}`)
	got, err = a.CompleteConfig(context.Background(), c)
	if err != nil || !strings.Contains(string(got), "T0TYPED") || calls != 1 {
		t.Fatalf("a typed id must be kept without a call: %s %v, calls %d", got, err, calls)
	}
}

func TestCompleteConfigReportsABadTokenWithoutIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()
	c := conn(t, defaultSettings)
	c.Config = nil
	_, err := adapter(srv.URL).CompleteConfig(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") || strings.Contains(err.Error(), botTok) {
		t.Fatalf("err = %v", err)
	}
}
