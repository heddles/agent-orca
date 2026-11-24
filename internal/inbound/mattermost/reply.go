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

package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// do calls the REST API. Errors name the path's purpose and the status, never the
// token, the body or the text.
func (a *Adapter) do(ctx context.Context, cr Credentials, method, path string, payload, out any) error {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	h := http.Header{"Authorization": {"Bearer " + cr.BotToken}, "Content-Type": {"application/json"}}
	resp, err := a.api(cr.AllowPrivate).Do(ctx, method, cr.BaseURL+path, h, body)
	if err != nil {
		return err
	}
	if resp.Status == http.StatusTooManyRequests {
		return fmt.Errorf("the server asks to wait %s", resp.RetryAfter.Round(time.Second))
	}
	if resp.Status < 200 || resp.Status > 299 {
		return fmt.Errorf("the server answered %d", resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return errors.New("the answer is not JSON")
		}
	}
	return nil
}

func (a *Adapter) get(ctx context.Context, cr Credentials, path string, out any) error {
	return a.do(ctx, cr, http.MethodGet, path, nil, out)
}

// Reply posts the answer as the bot, under the message, as one or more posts.
func (a *Adapter) Reply(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, res inbound.Result) error {
	var t target
	if err := json.Unmarshal(to, &t); err != nil || t.Channel == "" {
		return errors.New("the reply target is not valid")
	}
	cr, err := ParseCredentials(c.Secret)
	if err != nil {
		return err
	}
	for _, part := range chat.Render(res.Text, a.Capabilities()) {
		post := map[string]string{"channel_id": t.Channel, "message": part}
		if t.Root != "" {
			post["root_id"] = t.Root
		}
		if err := a.do(ctx, cr, http.MethodPost, "/api/v4/posts", post, nil); err != nil {
			return fmt.Errorf("posting the reply: %w", err)
		}
	}
	return nil
}

// Test reads the bot's own account (and the team, if configured). It posts nothing.
func (a *Adapter) Test(ctx context.Context, c inbound.Connection) inbound.TestReport {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	cr, err := ParseCredentials(c.Secret)
	if err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	details := map[string]string{"tokens": fmt.Sprintf("%d configured", len(cr.Tokens))}
	if u, err := url.Parse(cr.BaseURL); err == nil {
		details["server"] = u.Host
	}
	if len(cr.Tokens) == 0 {
		return inbound.TestReport{Details: details, Error: fmt.Sprintf("the Secret has no %q key: webhooks cannot be verified", KeyTokens)}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
	}
	if err := a.get(ctx, cr, "/api/v4/users/me", &me); err != nil {
		return inbound.TestReport{Details: details, Error: "the bot account could not be read: " + err.Error()}
	}
	a.botIDs.Store(c.ID, me.ID)
	details["bot"] = me.Username
	details["botId"] = me.ID
	if !me.IsBot {
		details["warning"] = "the token belongs to a person, not a bot account"
	}
	if cfg.TeamID != "" {
		var tm struct {
			DisplayName string `json:"display_name"`
			Name        string `json:"name"`
		}
		if err := a.get(ctx, cr, "/api/v4/teams/"+url.PathEscape(cfg.TeamID), &tm); err != nil {
			return inbound.TestReport{Details: details, Error: "the team could not be read: " + err.Error()}
		}
		details["team"] = strings.TrimSpace(tm.DisplayName)
	}
	return inbound.TestReport{OK: true, Details: details}
}
