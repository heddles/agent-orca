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
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// call posts a JSON body to a Web API method and returns the decoded answer.
// Errors name the method and Slack's error code, never the token or the text.
func (a *Adapter) call(ctx context.Context, c inbound.Connection, method string, payload any, out any) (http.Header, error) {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	return a.send(ctx, c, method, "application/json; charset=utf-8", body, out)
}

// form is call for the methods that take form arguments only.
func (a *Adapter) form(ctx context.Context, c inbound.Connection, method string, args url.Values, out any) error {
	_, err := a.send(ctx, c, method, "application/x-www-form-urlencoded", []byte(args.Encode()), out)
	return err
}

func (a *Adapter) send(ctx context.Context, c inbound.Connection, method, contentType string, body []byte, out any) (http.Header, error) {
	token := strings.TrimSpace(c.Secret.Get(KeyBotToken))
	if token == "" {
		return nil, fmt.Errorf("the Secret has no %q key", KeyBotToken)
	}
	resp, err := a.api().Do(ctx, http.MethodPost, a.base()+"/"+method, http.Header{
		"Authorization": {"Bearer " + token},
		"Content-Type":  {contentType},
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

// CompleteConfig looks up the workspace of the bot token with auth.test.
func (a *Adapter) CompleteConfig(ctx context.Context, c inbound.Connection) (json.RawMessage, error) {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return nil, err
	}
	if cfg.TeamID == "" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var who struct {
			TeamID string `json:"team_id"`
		}
		if _, err := a.call(ctx, c, "auth.test", nil, &who); err != nil {
			return nil, err
		}
		if !teamRe.MatchString(who.TeamID) {
			return nil, errors.New("auth.test: Slack did not say which workspace the token is for")
		}
		cfg.TeamID = who.TeamID
	}
	return json.Marshal(cfg)
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
	a.finishReaction(ctx, c, t, res)
	return nil
}

// Reactions that show how a request ended. The eyes reaction is added by Working
// when the run starts; once the answer is posted it is swapped for one of these.
const (
	reactionWorking = "eyes"
	reactionDone    = "white_check_mark"
	reactionFailed  = "x"
)

// finishReaction swaps the working reaction for done or failed, when the
// connection asks for reactions and the message is known. Best effort: the answer
// is already posted, so a failure here (a missing reactions:write scope, a deleted
// message, a rate limit) is never an error for the reply. A notice the core wrote
// itself about a message it refused (busy, rate limited) never started a run, so
// it has no working reaction to swap. The notice for a run that failed does.
func (a *Adapter) finishReaction(ctx context.Context, c inbound.Connection, t target, res inbound.Result) {
	cfg, err := ParseConfig(c.Config)
	if err != nil || !cfg.WorkingReaction || t.TS == "" || t.Channel == "" || (res.Notice && !res.Failed) {
		return
	}
	final := reactionDone
	if res.Failed {
		final = reactionFailed
	}
	react := func(method, name string) error {
		_, err := a.call(ctx, c, method, map[string]string{"channel": t.Channel, "timestamp": t.TS, "name": name}, nil)
		if err != nil && (strings.HasSuffix(err.Error(), "no_reaction") || strings.HasSuffix(err.Error(), "already_reacted")) {
			return nil
		}
		return err
	}
	// Add the end state first, so the message is never left without a sign.
	if err := react("reactions.add", final); err != nil {
		slog.Debug("showing how the request ended", "connection", c.ID, "err", err)
		return
	}
	if err := react("reactions.remove", reactionWorking); err != nil {
		slog.Debug("removing the working reaction", "connection", c.ID, "err", err)
	}
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
	_, err = a.call(ctx, c, "reactions.add", map[string]string{"channel": t.Channel, "timestamp": t.TS, "name": reactionWorking}, nil)
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
