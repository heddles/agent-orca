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

// reactions is how a request's progress looks on its post.
var reactions = map[inbound.Progress]string{
	inbound.ProgressWorking:  "eyes",
	inbound.ProgressApproval: "raised_hand",
	inbound.ProgressQuestion: "question",
	inbound.ProgressDone:     "white_check_mark",
	inbound.ProgressFailed:   "x",
}

// Show swaps the bot's reaction on the sender's post, unless the connection
// turned reactions off. A slash command has no post to react to.
func (a *Adapter) Show(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, from, now inbound.Progress) error {
	cfg, err := ParseConfig(c.Config)
	if err != nil || !cfg.reactions() {
		return nil
	}
	var t target
	if json.Unmarshal(to, &t) != nil || t.Root == "" {
		return nil
	}
	cr, err := ParseCredentials(c.Secret)
	if err != nil {
		return err
	}
	// A reaction belongs to a user, so the bot needs its own id.
	bot, err := a.botID(ctx, c.ID, cr)
	if err != nil {
		return fmt.Errorf("reading the bot account: %w", err)
	}
	// Add before removing, so a failure never leaves the post without a sign.
	if name := reactions[now]; name != "" {
		if err := a.do(ctx, cr, http.MethodPost, "/api/v4/reactions", map[string]string{"user_id": bot, "post_id": t.Root, "emoji_name": name}, nil); err != nil {
			return fmt.Errorf("adding a reaction: %w", err)
		}
	}
	if name := reactions[from]; name != "" {
		path := "/api/v4/users/" + url.PathEscape(bot) + "/posts/" + url.PathEscape(t.Root) + "/reactions/" + name
		if err := a.do(ctx, cr, http.MethodDelete, path, nil, nil); err != nil {
			return fmt.Errorf("removing a reaction: %w", err)
		}
	}
	return nil
}

func (a *Adapter) botID(ctx context.Context, connectionID string, cr Credentials) (string, error) {
	if id, ok := a.botIDs.Load(connectionID); ok {
		return id.(string), nil
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := a.get(ctx, cr, "/api/v4/users/me", &me); err != nil {
		return "", err
	}
	if me.ID == "" {
		return "", errors.New("the server did not say who the bot is")
	}
	a.botIDs.Store(connectionID, me.ID)
	return me.ID, nil
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
