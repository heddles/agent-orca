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

package webex

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

// target is what Reply needs, stored with the message while the run works.
type target struct {
	Room   string `json:"r"`
	Parent string `json:"p,omitempty"` // reply in this thread
}

type webhook struct {
	Resource string `json:"resource"`
	Event    string `json:"event"`
	Data     struct {
		ID          string `json:"id"`
		RoomID      string `json:"roomId"`
		RoomType    string `json:"roomType"`
		PersonID    string `json:"personId"`
		PersonEmail string `json:"personEmail"`
		Created     string `json:"created"`
	} `json:"data"`
}

// idRe is what a Webex id looks like (URL-safe base64). Ids are put into request
// paths, so nothing else is let through.
var idRe = regexp.MustCompile(`^[A-Za-z0-9_=-]{8,512}$`)

// Receive verifies the signature and returns the message the webhook announces.
// The webhook holds ids only: the text is fetched by Enrich.
func (a *Adapter) Receive(_ context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	if _, err := ParseConfig(c.Config); err != nil {
		return inbound.Message{}, err
	}
	secret := c.Secret.Get(KeyWebhookSecret)
	if secret == "" {
		return inbound.Message{}, &inbound.AuthError{Msg: "the connection has no webhook secret"}
	}
	if r.ContentLength > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is too large"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is unreadable or too large"}
	}
	if err := verify.VerifyHMACSHA1(verify.Sig{Secret: []byte(secret), Base: body, Got: r.Header.Get("X-Spark-Signature")}); err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "invalid signature"}
	}
	var wh webhook
	if err := json.Unmarshal(body, &wh); err != nil {
		return inbound.Message{}, &inbound.Response{Status: http.StatusOK}
	}
	if wh.Resource != "messages" || wh.Event != "created" {
		return inbound.Message{}, &inbound.Response{Status: http.StatusOK}
	}
	d := wh.Data
	email := strings.ToLower(strings.TrimSpace(d.PersonEmail))
	user := email
	if user == "" {
		user = d.PersonID
	}
	if !idRe.MatchString(d.ID) || !idRe.MatchString(d.RoomID) || user == "" {
		return ignored(d.ID+d.RoomID, user, len(body), "malformed")
	}
	if created, err := time.Parse(time.RFC3339, d.Created); err == nil {
		if age := a.now().Sub(created); age > maxAge || age < -maxSkew {
			return ignored(d.ID, user, len(body), "stale")
		}
	}
	group := ""
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		group = email[i+1:]
	}
	m := inbound.Message{
		Sender: inbound.Sender{ID: user, Group: group, Display: email}, Channel: d.RoomID, Size: len(body), MessageID: d.ID,
	}
	// Other bots (their address ends in @webex.bot) and our own account are never answered.
	if strings.HasSuffix(email, "@webex.bot") || a.isSelf(c, d.PersonID) {
		m.FromBot = true
		m.MessageID = "ignored:" + d.ID
		return m, nil
	}
	t := target{Room: d.RoomID}
	switch d.RoomType {
	case "direct":
		m.Mode = inbound.ModeDirect
		m.ThreadID = d.RoomID + "/dm" // one flat conversation
	case "group":
		m.Mode = inbound.ModeMention
		m.ThreadID = d.RoomID + "/" + d.ID // Enrich moves it to the thread the message is in
		t.Parent = d.ID
	default:
		return ignored(d.ID, user, len(body), "event_not_handled")
	}
	m.Reply, _ = json.Marshal(t)
	return m, nil
}

func ignored(id, user string, size int, reason string) (inbound.Message, error) {
	return inbound.Message{MessageID: "ignored:" + id, Sender: inbound.Sender{ID: user}, Size: size}, &inbound.Rejection{Reason: reason, Ignore: true}
}

type messageDetails struct {
	ID              string   `json:"id"`
	RoomID          string   `json:"roomId"`
	PersonID        string   `json:"personId"`
	PersonEmail     string   `json:"personEmail"`
	ParentID        string   `json:"parentId"`
	Text            string   `json:"text"`
	MentionedPeople []string `json:"mentionedPeople"`
}

// Enrich fetches the message the webhook announced and completes the message with
// its text and thread, after the acknowledgement. When the lookup cannot be made
// the message is dropped as ignored, because without its text there is nothing to
// run (a core Enricher has no way to ask for a retry).
func (a *Adapter) Enrich(ctx context.Context, c inbound.Connection, m *inbound.Message) {
	if m.FromBot || len(m.Reply) == 0 {
		return
	}
	drop := func(why string) {
		slog.Warn("a Webex message was not run", "connection", c.ID, "reason", why)
		m.FromBot = true
	}
	var t target
	if json.Unmarshal(m.Reply, &t) != nil || !idRe.MatchString(m.MessageID) {
		drop("bad target")
		return
	}
	self, err := a.whoAmI(ctx, c)
	if err != nil {
		drop("the bot account could not be read")
		return
	}
	var d messageDetails
	if err := a.get(ctx, c, "/v1/messages/"+m.MessageID, &d); err != nil {
		drop("the message could not be read")
		return
	}
	if d.PersonID == self.ID || strings.HasSuffix(strings.ToLower(d.PersonEmail), "@webex.bot") {
		m.FromBot = true // the bot's own message, or another bot's
		return
	}
	// What the webhook said and what the API says must agree.
	if d.ID != m.MessageID || d.RoomID != m.Channel {
		drop("the message does not match its webhook")
		return
	}
	text := d.Text
	if m.Mode == inbound.ModeMention {
		if !slices.Contains(d.MentionedPeople, self.ID) {
			// Not addressed to us: the core refuses a mode that is not enabled.
			m.Mode = inbound.ModeChannel
			return
		}
		text = stripBotName(text, self)
		if d.ParentID != "" && idRe.MatchString(d.ParentID) {
			t.Parent = d.ParentID
			m.ThreadID = m.Channel + "/" + d.ParentID
			m.Reply, _ = json.Marshal(t)
		}
	}
	m.Text = strings.TrimSpace(text)
	if m.Text == "" {
		m.FromBot = true // nothing to run (a file or a bare mention)
	}
}

// stripBotName removes the bot's name from the start of a mentioning message:
// Webex writes a mention into the plain text as the name, with no "@".
func stripBotName(text string, self me) string {
	t := strings.TrimSpace(text)
	for _, name := range []string{self.DisplayName, self.NickName} {
		if name = strings.TrimSpace(name); name != "" && len(t) >= len(name) && strings.EqualFold(t[:len(name)], name) {
			return strings.TrimLeft(t[len(name):], " :,\t\n")
		}
	}
	return t
}
