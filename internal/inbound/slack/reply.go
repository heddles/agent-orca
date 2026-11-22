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
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// call posts a JSON body to a Web API method and returns the decoded answer.
// Errors name the method and Slack's error code, never the token or the text.
func (a *Adapter) call(ctx context.Context, c inbound.Connection, method string, payload any, out any) (http.Header, error) {
	token := strings.TrimSpace(c.Secret.Get(KeyBotToken))
	if token == "" {
		return nil, fmt.Errorf("the Secret has no %q key", KeyBotToken)
	}
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	resp, err := a.api().Do(ctx, http.MethodPost, a.base()+"/"+method, http.Header{
		"Authorization": {"Bearer " + token},
		"Content-Type":  {"application/json; charset=utf-8"},
	}, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if resp.Status == http.StatusTooManyRequests {
		return resp.Header, fmt.Errorf("%s: Slack asks to wait %s", method, resp.RetryAfter.Round(time.Second))
	}
	if resp.Status != http.StatusOK {
		return resp.Header, fmt.Errorf("%s: Slack answered %d", method, resp.Status)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return resp.Header, fmt.Errorf("%s: the answer is not JSON", method)
	}
	if !env.OK {
		return resp.Header, fmt.Errorf("%s: %s", method, safeCode(env.Error))
	}
	if out != nil {
		_ = json.Unmarshal(resp.Body, out)
	}
	return resp.Header, nil
}

// safeCode keeps a Slack error code to what one looks like.
func safeCode(s string) string {
	if s == "" || len(s) > 64 || strings.ContainsAny(s, " \n\t\"'<>") {
		return "error"
	}
	return s
}

// Reply posts the answer, in the thread when there is one, as one or more
// messages. Mentions are neutralised and links are not unfurled.
func (a *Adapter) Reply(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, res inbound.Result) error {
	var t target
	if err := json.Unmarshal(to, &t); err != nil || t.Channel == "" {
		return errors.New("the reply target is not valid")
	}
	for _, part := range chat.Render(res.Text, a.Capabilities()) {
		msg := map[string]any{
			"channel": t.Channel, "text": part, "mrkdwn": true, "parse": "none", "link_names": false,
			"unfurl_links": false, "unfurl_media": false,
		}
		if t.ThreadTS != "" {
			msg["thread_ts"] = t.ThreadTS
		}
		if _, err := a.call(ctx, c, "chat.postMessage", msg, nil); err != nil {
			return err
		}
	}
	return nil
}

// Working adds the reaction, when the connection asks for it.
func (a *Adapter) Working(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget) error {
	cfg, err := ParseConfig(c.Config)
	if err != nil || !cfg.WorkingReaction {
		return nil
	}
	var t target
	if json.Unmarshal(to, &t) != nil || t.Channel == "" || t.TS == "" {
		return nil
	}
	_, err = a.call(ctx, c, "reactions.add", map[string]string{"channel": t.Channel, "timestamp": t.TS, "name": "eyes"}, nil)
	if err != nil && strings.HasSuffix(err.Error(), "already_reacted") {
		return nil
	}
	return err
}

// Test calls auth.test: it shows the bot and the workspace and sends nothing.
func (a *Adapter) Test(ctx context.Context, c inbound.Connection) inbound.TestReport {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	details := map[string]string{"signingSecret": "configured"}
	if c.Secret.Get(KeySigningSecret) == "" {
		details["signingSecret"] = "missing"
		return inbound.TestReport{Details: details, Error: fmt.Sprintf("the Secret has no %q key", KeySigningSecret)}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var who struct {
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
		BotID  string `json:"bot_id"`
	}
	hdr, err := a.call(ctx, c, "auth.test", nil, &who)
	if err != nil {
		return inbound.TestReport{Details: details, Error: err.Error()}
	}
	a.botIDs.Store(c.ID, who.UserID)
	details["bot"] = who.User
	details["workspace"] = who.Team
	details["workspaceId"] = who.TeamID
	if scopes := hdr.Get("X-OAuth-Scopes"); scopes != "" {
		details["scopes"] = scopes
		for _, need := range []string{"chat:write", "app_mentions:read"} {
			if !strings.Contains(","+strings.ReplaceAll(scopes, " ", "")+",", ","+need+",") {
				details["warning"] = "the token lacks the " + need + " scope"
			}
		}
	}
	if who.TeamID != cfg.TeamID {
		return inbound.TestReport{Details: details, Error: fmt.Sprintf("the token belongs to workspace %s, not %s", who.TeamID, cfg.TeamID)}
	}
	return inbound.TestReport{OK: true, Details: details}
}
