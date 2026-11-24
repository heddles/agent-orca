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
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

type target struct {
	Channel string `json:"c"`
	Root    string `json:"r,omitempty"` // the post to answer under; the server resolves it to the thread's root
}

// payload holds what an outgoing webhook or a slash command sends.
type payload struct {
	Token       string `json:"token"`
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	PostID      string `json:"post_id"`
	Text        string `json:"text"`
	TriggerWord string `json:"trigger_word"`
	Command     string `json:"command"`
	TriggerID   string `json:"trigger_id"`
}

func parsePayload(r *http.Request, body []byte) (payload, error) {
	var p payload
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return p, json.Unmarshal(body, &p)
	}
	f, err := url.ParseQuery(string(body))
	if err != nil {
		return p, err
	}
	p = payload{
		Token: f.Get("token"), TeamID: f.Get("team_id"), ChannelID: f.Get("channel_id"), UserID: f.Get("user_id"), UserName: f.Get("user_name"),
		PostID: f.Get("post_id"), Text: f.Get("text"), TriggerWord: f.Get("trigger_word"), Command: f.Get("command"), TriggerID: f.Get("trigger_id"),
	}
	return p, nil
}

func ignored(id, user string, size int, reason string) (inbound.Message, error) {
	return inbound.Message{MessageID: "ignored:" + id, Sender: inbound.Sender{ID: user}, Size: size}, &inbound.Rejection{Reason: reason, Ignore: true}
}

// Receive verifies the token and returns the message.
func (a *Adapter) Receive(_ context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.Message{}, err
	}
	tokens := splitTokens(c.Secret.Get(KeyTokens))
	if len(tokens) == 0 {
		return inbound.Message{}, &inbound.AuthError{Msg: "the connection has no webhook tokens"}
	}
	if r.ContentLength > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is too large"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is unreadable or too large"}
	}
	p, err := parsePayload(r, body)
	if err != nil {
		return inbound.Message{}, err
	}
	// The token travels in the body, so it is checked before anything is believed.
	if !verify.TokenAny(p.Token, tokens) {
		return inbound.Message{}, &inbound.AuthError{Msg: "invalid token"}
	}
	id := p.PostID
	if p.Command != "" {
		id = "cmd:" + p.TriggerID
	}
	if id == "" || p.UserID == "" || p.ChannelID == "" {
		return ignored(p.PostID+p.TriggerID+p.ChannelID, p.UserID, len(body), "malformed")
	}
	if cfg.TeamID != "" && p.TeamID != cfg.TeamID {
		return ignored(id, p.UserID, len(body), "team_mismatch")
	}
	m := inbound.Message{
		Sender: inbound.Sender{ID: p.UserID, Group: p.TeamID, Display: p.UserName}, Channel: p.ChannelID, Size: len(body),
		MessageID: id, ThreadID: p.TeamID + "/" + p.ChannelID + "/" + id,
	}
	text := p.Text
	switch {
	case p.Command != "":
		m.Mode = inbound.ModeCommand
		m.ThreadID = p.TeamID + "/" + p.ChannelID + "/cmd:" + p.UserID
		m.Reply, _ = json.Marshal(target{Channel: p.ChannelID})
	case p.TriggerWord != "":
		m.Mode = inbound.ModeMention
		text = stripTrigger(text, p.TriggerWord)
	default:
		m.Mode = inbound.ModeChannel
		if !c.Settings.ModeEnabled(a.Capabilities(), inbound.ModeChannel) {
			return ignored(id, p.UserID, len(body), "channel_mode_off")
		}
	}
	if m.Mode != inbound.ModeCommand {
		m.Reply, _ = json.Marshal(target{Channel: p.ChannelID, Root: p.PostID})
	}
	m.Text = strings.TrimSpace(text)
	if own, ok := a.botIDs.Load(c.ID); ok && own.(string) == p.UserID {
		m.FromBot = true
	}
	if m.Text == "" {
		return m, &inbound.Rejection{Reason: "empty", Ignore: true}
	}
	return m, nil
}

func stripTrigger(text, word string) string {
	t := strings.TrimSpace(text)
	if len(t) >= len(word) && strings.EqualFold(t[:len(word)], word) {
		return strings.TrimLeft(t[len(word):], " :,\t")
	}
	return t
}

// AckResponse gives a slash command its immediate line when it started a run;
// a refused command and every webhook get the default.
func (a *Adapter) AckResponse(m inbound.Message, o inbound.Outcome) *inbound.Response {
	if m.Mode != inbound.ModeCommand {
		return &inbound.Response{Status: http.StatusOK} // no "text": nothing is posted for a webhook
	}
	if o == inbound.OutcomeStarted || o == inbound.OutcomeAccepted {
		return &inbound.Response{Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{"response_type":"ephemeral","text":"Working on it. The answer will be posted in this channel."}`)}
	}
	return &inbound.Response{Status: http.StatusOK}
}

// Enrich finds the thread a webhook post belongs to and whether the author is a
// bot (another integration, or our own bot), with one bounded lookup. The
// webhook does not say; a post's root_id and props do. A failed lookup leaves the
// message as it was: it then starts its own conversation.
func (a *Adapter) Enrich(ctx context.Context, c inbound.Connection, m *inbound.Message) {
	if m.Mode == inbound.ModeCommand || len(m.Reply) == 0 || m.FromBot {
		return
	}
	cr, err := ParseCredentials(c.Secret)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, ok := a.botIDs.Load(c.ID); !ok {
		var me struct {
			ID string `json:"id"`
		}
		if a.get(ctx, cr, "/api/v4/users/me", &me) == nil && me.ID != "" {
			a.botIDs.Store(c.ID, me.ID)
			if me.ID == m.Sender.ID {
				m.FromBot = true
				return
			}
		}
	}
	var t target
	_ = json.Unmarshal(m.Reply, &t)
	if t.Root == "" {
		return
	}
	var post struct {
		RootID string         `json:"root_id"`
		Type   string         `json:"type"`
		Props  map[string]any `json:"props"`
	}
	if a.get(ctx, cr, "/api/v4/posts/"+url.PathEscape(t.Root), &post) != nil {
		return
	}
	if post.Type != "" || post.Props["from_bot"] == "true" || post.Props["from_webhook"] == "true" {
		m.FromBot = true
		return
	}
	if post.RootID != "" {
		m.ThreadID = m.Sender.Group + "/" + m.Channel + "/" + post.RootID
	}
}
