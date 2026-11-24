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

package teams

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

// target is the conversation reference of one message. It is what Reply needs
// and lives only in the pending reply's reply_target, never anywhere else.
type target struct {
	ServiceURL   string `json:"u"`
	Conversation string `json:"c"`
	Activity     string `json:"a,omitempty"` // the message answered
	Bot          string `json:"b,omitempty"` // the bot's id in the conversation
}

type account struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AADObjectID string `json:"aadObjectId"`
	Role        string `json:"role"`
}

type activity struct {
	Type         string  `json:"type"`
	ID           string  `json:"id"`
	ChannelID    string  `json:"channelId"`
	ServiceURL   string  `json:"serviceUrl"`
	Text         string  `json:"text"`
	From         account `json:"from"`
	Recipient    account `json:"recipient"`
	Conversation struct {
		ID               string `json:"id"`
		ConversationType string `json:"conversationType"`
		TenantID         string `json:"tenantId"`
	} `json:"conversation"`
	Entities []struct {
		Type      string  `json:"type"`
		Mentioned account `json:"mentioned"`
		Text      string  `json:"text"`
	} `json:"entities"`
	ChannelData struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
	} `json:"channelData"`
}

// Receive verifies the request and returns the message it carries.
func (a *Adapter) Receive(ctx context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	if _, err := ParseConfig(c.Config); err != nil {
		return inbound.Message{}, err
	}
	appID := strings.TrimSpace(c.Secret.Get(KeyAppID))
	if appID == "" {
		return inbound.Message{}, &inbound.AuthError{Msg: "the connection has no app id"}
	}
	if r.ContentLength > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is too large"}
	}
	tok, err := verify.BearerToken(r.Header.Get("Authorization"))
	if err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "no bearer token"}
	}
	v, err := a.verifier(appID)
	if err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "the connection cannot verify tokens"}
	}
	claims, err := v.VerifyEndorsed(ctx, tok, teamsChannel)
	if err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "invalid token: " + authReason(err)}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is unreadable or too large"}
	}
	var act activity
	if err := json.Unmarshal(body, &act); err != nil {
		return inbound.Message{}, errors.New("the activity is not valid JSON")
	}
	// The token vouches for one service URL; the activity must come from it.
	claim, _ := claims["serviceurl"].(string)
	if claim == "" {
		claim, _ = claims["serviceUrl"].(string)
	}
	if claim == "" || act.ServiceURL == "" || strings.TrimRight(claim, "/") != strings.TrimRight(act.ServiceURL, "/") {
		return inbound.Message{}, &inbound.AuthError{Msg: "the service URL does not match the token"}
	}
	if !a.trustedServiceURL(act.ServiceURL) {
		return inbound.Message{}, &inbound.AuthError{Msg: "the service URL is not a Bot Connector address"}
	}
	return a.message(c, act, len(body))
}

// authReason is the part of a verification failure that is safe to show.
func authReason(err error) string {
	switch {
	case errors.Is(err, verify.ErrNotEndorsed):
		return "the key is not endorsed for Teams"
	case errors.Is(err, verify.ErrKeyFetch):
		return "signing keys unavailable"
	case errors.Is(err, verify.ErrUnknownKey):
		return "unknown signing key"
	}
	// ErrBadJWT carries a short reason (expired, wrong audience, ...), no token content.
	return strings.TrimPrefix(err.Error(), verify.ErrBadJWT.Error()+": ")
}

// serviceHosts are the domains the Bot Connector is served from. The token the
// bot sends there is a credential, so it goes only to these.
var serviceHosts = []string{".trafficmanager.net", ".botframework.com", ".microsoft.com", ".microsoft.us", ".skype.com", ".azure.com"}

func (a *Adapter) trustedServiceURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if a.Insecure {
		return u.Scheme == "http" || u.Scheme == "https"
	}
	if u.Scheme != "https" || u.Port() != "" && u.Port() != "443" || net.ParseIP(u.Hostname()) != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, s := range serviceHosts {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

func ignored(id, user string, size int, reason string) (inbound.Message, error) {
	return inbound.Message{MessageID: "ignored:" + id, Sender: inbound.Sender{ID: user}, Size: size}, &inbound.Rejection{Reason: reason, Ignore: true}
}

// baseConversation drops the ";messageid=..." a channel thread's conversation id carries.
func baseConversation(id string) string {
	base, _, _ := strings.Cut(id, ";")
	return base
}

func (a *Adapter) message(c inbound.Connection, act activity, size int) (inbound.Message, error) {
	if act.Type != "message" {
		return inbound.Message{}, &inbound.Response{Status: http.StatusOK} // conversationUpdate, typing, ...: nothing to do
	}
	user := act.From.AADObjectID
	if user == "" {
		user = act.From.ID
	}
	if act.ChannelID != teamsChannel {
		return ignored(act.ID, user, size, "channel_not_supported")
	}
	if act.ID == "" || act.Conversation.ID == "" || user == "" {
		return ignored(act.ID+act.Conversation.ID, user, size, "malformed")
	}
	tenant := act.ChannelData.Tenant.ID
	if tenant == "" {
		tenant = act.Conversation.TenantID
	}
	space := baseConversation(act.Conversation.ID)
	m := inbound.Message{
		Sender:  inbound.Sender{ID: user, Group: tenant, Display: act.From.Name},
		Channel: space, Size: size, MessageID: tenant + ":" + space + ":" + act.ID,
		ThreadID: tenant + "/" + act.Conversation.ID,
	}
	// The bot's own activities, other bots and apps are never answered.
	if act.From.Role == "bot" || strings.HasPrefix(act.From.ID, "28:") || (act.Recipient.ID != "" && act.From.ID == act.Recipient.ID) {
		m.FromBot = true
		m.MessageID = "ignored:" + act.ID
		return m, nil
	}
	mentioned := false
	text := act.Text
	for _, e := range act.Entities {
		if e.Type == "mention" && act.Recipient.ID != "" && e.Mentioned.ID == act.Recipient.ID {
			mentioned = true
			if e.Text != "" {
				text = strings.Replace(text, e.Text, "", 1) // the bot's own <at>name</at>
			}
		}
	}
	switch act.Conversation.ConversationType {
	case "personal":
		m.Mode = inbound.ModeDirect
		m.ThreadID = tenant + "/" + space + "/dm" // one flat conversation
	case "groupChat", "channel":
		m.Mode = inbound.ModeMention
		if !mentioned {
			if !c.Settings.ModeEnabled(a.Capabilities(), inbound.ModeChannel) {
				return ignored(act.ID, user, size, "channel_mode_off")
			}
			m.Mode = inbound.ModeChannel
		}
	default:
		return ignored(act.ID, user, size, "event_not_handled")
	}
	m.Text = plainText(text)
	if strings.TrimSpace(m.Text) == "" {
		m.MessageID = "ignored:" + act.ID
		return m, &inbound.Rejection{Reason: "empty", Ignore: true}
	}
	t := target{ServiceURL: act.ServiceURL, Conversation: act.Conversation.ID, Bot: act.Recipient.ID}
	if m.Mode != inbound.ModeDirect {
		t.Activity = act.ID // threads the answer under the message in a channel
	}
	m.Reply, _ = json.Marshal(t)
	return m, nil
}

var (
	atTagRe = regexp.MustCompile(`(?is)<at[^>]*>(.*?)</at>`)
	tagRe   = regexp.MustCompile(`</?[a-zA-Z][^<>]{0,200}>`)
)

// plainText turns the text of an activity into what an agent can read: mentions
// of other people become @name, other markup is dropped, entities decoded.
func plainText(s string) string {
	s = atTagRe.ReplaceAllString(s, "@$1")
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n").Replace(s)
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(strings.ReplaceAll(s, " ", " "))
}

// AckResponse answers Teams with an empty 200 whatever became of the message.
func (a *Adapter) AckResponse(inbound.Message, inbound.Outcome) *inbound.Response {
	return &inbound.Response{Status: http.StatusOK}
}
