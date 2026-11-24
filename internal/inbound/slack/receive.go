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
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

// target is what Reply needs, stored with the message while the run works.
type target struct {
	Channel  string `json:"c"`
	ThreadTS string `json:"t,omitempty"` // reply in this thread
	TS       string `json:"m,omitempty"` // the message, for the reaction
}

type envelope struct {
	Type           string `json:"type"`
	Challenge      string `json:"challenge"`
	TeamID         string `json:"team_id"`
	EventID        string `json:"event_id"`
	Authorizations []struct {
		UserID string `json:"user_id"`
		IsBot  bool   `json:"is_bot"`
	} `json:"authorizations"`
	Event struct {
		Type        string `json:"type"`
		Subtype     string `json:"subtype"`
		User        string `json:"user"`
		BotID       string `json:"bot_id"`
		Text        string `json:"text"`
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"`
		Team        string `json:"team"`
	} `json:"event"`
}

// Receive verifies the request and returns the message it carries.
func (a *Adapter) Receive(_ context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.Message{}, err
	}
	body, err := a.verified(r, c.Secret.Get(KeySigningSecret))
	if err != nil {
		return inbound.Message{}, err
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return a.command(cfg, body)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return inbound.Message{}, err
	}
	switch env.Type {
	case "url_verification":
		return inbound.Message{}, &inbound.Response{Status: http.StatusOK, ContentType: "text/plain", Body: []byte(env.Challenge)}
	case "event_callback":
		return a.event(c, cfg, env, len(body))
	}
	return inbound.Message{}, &inbound.Response{Status: http.StatusOK} // anything else: nothing to do
}

// verified reads the body and checks Slack's signature and timestamp.
func (a *Adapter) verified(r *http.Request, secret string) ([]byte, error) {
	if secret == "" {
		return nil, &inbound.AuthError{Msg: "the connection has no signing secret"}
	}
	if r.ContentLength > maxBody {
		return nil, &inbound.AuthError{Msg: "the body is too large"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, &inbound.AuthError{Msg: "the body is unreadable or too large"}
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	if _, err := verify.Timestamp(ts, a.now(), signatureWait); err != nil {
		return nil, &inbound.AuthError{Msg: err.Error()}
	}
	base := append([]byte("v0:"+ts+":"), body...)
	if err := verify.VerifyHMAC(verify.Sig{Secret: []byte(secret), Base: base, Got: r.Header.Get("X-Slack-Signature"), Prefix: "v0="}); err != nil {
		return nil, &inbound.AuthError{Msg: "invalid signature"}
	}
	return body, nil
}

func ignored(id, user string, size int, reason string) (inbound.Message, error) {
	return inbound.Message{MessageID: "ignored:" + id, Sender: inbound.Sender{ID: user}, Size: size}, &inbound.Rejection{Reason: reason, Ignore: true}
}

func (a *Adapter) event(c inbound.Connection, cfg Config, env envelope, size int) (inbound.Message, error) {
	ev := env.Event
	team := env.TeamID
	if team == "" {
		team = ev.Team
	}
	for _, au := range env.Authorizations {
		if au.IsBot && au.UserID != "" {
			a.botIDs.Store(c.ID, au.UserID)
		}
	}
	own, _ := a.botIDs.Load(c.ID)
	user := ev.User
	if team != cfg.TeamID {
		return ignored(env.EventID, user, size, "workspace_mismatch")
	}
	var mode inbound.Mode
	switch {
	case ev.Type == "app_mention":
		mode = inbound.ModeMention
	case ev.Type == "message" && ev.ChannelType == "im":
		mode = inbound.ModeDirect
	case ev.Type == "message" && (ev.ChannelType == "channel" || ev.ChannelType == "group"):
		if !c.Settings.ModeEnabled(a.Capabilities(), inbound.ModeChannel) {
			// A mention arrives twice (app_mention too): do not claim its message id here.
			return ignored(env.EventID, user, size, "channel_mode_off")
		}
		mode = inbound.ModeChannel
	default:
		return ignored(env.EventID, user, size, "event_not_handled")
	}
	if ev.Channel == "" || ev.TS == "" {
		return ignored(env.EventID, user, size, "malformed")
	}
	m := inbound.Message{
		Sender: inbound.Sender{ID: user, Group: team}, Channel: ev.Channel, Mode: mode, Size: size,
		MessageID: team + ":" + ev.Channel + ":" + ev.TS,
	}
	// Bots, apps, edits, deletions, joins: ignored, and never answered.
	if ev.BotID != "" || ev.Subtype != "" || user == "" || (own != nil && user == own.(string)) {
		m.FromBot = true
		m.MessageID = "ignored:" + env.EventID
		return m, nil
	}
	text := ev.Text
	if mode == inbound.ModeMention {
		text = stripLeadingMention(text)
	}
	m.Text = plainText(text)
	if strings.TrimSpace(m.Text) == "" {
		m.MessageID = "ignored:" + env.EventID
		return m, &inbound.Rejection{Reason: "empty", Ignore: true}
	}
	thread := ev.ThreadTS
	reply := target{Channel: ev.Channel, ThreadTS: ev.ThreadTS, TS: ev.TS}
	if thread == "" {
		if mode == inbound.ModeDirect {
			thread = "dm" // a direct conversation is one conversation; the reply stays flat
		} else {
			thread = ev.TS
			reply.ThreadTS = ev.TS // the answer opens a thread under the message
		}
	}
	m.ThreadID = team + "/" + ev.Channel + "/" + thread
	m.Reply, _ = json.Marshal(reply)
	return m, nil
}

func (a *Adapter) command(cfg Config, body []byte) (inbound.Message, error) {
	f, err := url.ParseQuery(string(body))
	if err != nil {
		return inbound.Message{}, err
	}
	team, user, channel, trigger := f.Get("team_id"), f.Get("user_id"), f.Get("channel_id"), f.Get("trigger_id")
	if team != cfg.TeamID {
		return ignored("cmd:"+trigger, user, len(body), "workspace_mismatch")
	}
	if user == "" || channel == "" || trigger == "" {
		return inbound.Message{}, &inbound.Rejection{Reason: "malformed", Ignore: true}
	}
	m := inbound.Message{
		Sender: inbound.Sender{ID: user, Group: team, Display: f.Get("user_name")}, Channel: channel, Mode: inbound.ModeCommand, Size: len(body),
		MessageID: "cmd:" + team + ":" + trigger, ThreadID: team + "/" + channel + "/cmd:" + user, Text: plainText(f.Get("text")),
	}
	m.Reply, _ = json.Marshal(target{Channel: channel})
	if strings.TrimSpace(m.Text) == "" {
		return m, &inbound.Rejection{Reason: "empty", Ignore: true}
	}
	return m, nil
}

// AckResponse gives a slash command its immediate line, only for a message that
// started a run. Everything else gets the default, which tells the sender nothing
// about why a message was refused.
func (a *Adapter) AckResponse(m inbound.Message, o inbound.Outcome) *inbound.Response {
	if m.Mode != inbound.ModeCommand {
		return nil
	}
	if o == inbound.OutcomeStarted || o == inbound.OutcomeAccepted {
		return &inbound.Response{Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{"response_type":"ephemeral","text":"Working on it. The answer will be posted in this channel."}`)}
	}
	return &inbound.Response{Status: http.StatusOK}
}

var (
	leadMentionRe = regexp.MustCompile(`^\s*<@[A-Z0-9]+(\|[^>]*)?>[\s:,]*`)
	angleRe       = regexp.MustCompile(`<([^<>]+)>`)
	unescape      = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

func stripLeadingMention(s string) string { return leadMentionRe.ReplaceAllString(s, "") }

// plainText turns Slack's mrkdwn markup of a message into text an agent can read:
// mentions and links lose their angle brackets, entities are decoded.
func plainText(s string) string {
	s = angleRe.ReplaceAllStringFunc(s, func(m string) string {
		in := m[1 : len(m)-1]
		head, label, _ := strings.Cut(in, "|")
		switch {
		case strings.HasPrefix(head, "@"):
			return "@" + strings.TrimPrefix(head, "@")
		case strings.HasPrefix(head, "#"):
			if label != "" {
				return "#" + label
			}
			return head
		case strings.HasPrefix(head, "!"):
			return "@" + strings.TrimPrefix(head, "!")
		case label != "":
			return label + " (" + head + ")"
		}
		return head
	})
	return strings.TrimSpace(unescape.Replace(s))
}
