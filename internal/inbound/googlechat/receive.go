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

package googlechat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

// target is what Reply needs, stored with the message while the run works.
// userTypeBot is the type of a user that is an app.
const userTypeBot = "BOT"

type target struct {
	Space  string `json:"s"`
	Thread string `json:"t,omitempty"` // reply in this thread
}

type user struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type"`
	Email       string `json:"email"`
	DomainID    string `json:"domainId"`
}

type space struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	SpaceType      string `json:"spaceType"`
	SingleUserBotD bool   `json:"singleUserBotDm"`
}

type annotation struct {
	Type        string `json:"type"`
	StartIndex  int    `json:"startIndex"`
	Length      int    `json:"length"`
	UserMention struct {
		User user `json:"user"`
	} `json:"userMention"`
}

type message struct {
	Name         string `json:"name"`
	Sender       user   `json:"sender"`
	Text         string `json:"text"`
	ArgumentText string `json:"argumentText"`
	Thread       struct {
		Name string `json:"name"`
	} `json:"thread"`
	Space        space        `json:"space"`
	Annotations  []annotation `json:"annotations"`
	SlashCommand *struct {
		CommandID json.Number `json:"commandId"`
	} `json:"slashCommand"`
}

type event struct {
	Type    string  `json:"type"`
	User    user    `json:"user"`
	Space   space   `json:"space"`
	Message message `json:"message"`
}

var (
	spaceRe  = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]{1,128}$`)
	threadRe = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]{1,128}/threads/[A-Za-z0-9_.-]{1,256}$`)
)

// Receive verifies the request and returns the message it carries.
func (a *Adapter) Receive(ctx context.Context, c inbound.Connection, r *http.Request) (inbound.Message, error) {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.Message{}, err
	}
	if r.ContentLength > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is too large"}
	}
	tok, err := verify.BearerToken(r.Header.Get("Authorization"))
	if err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "no bearer token"}
	}
	v, err := a.verifier(cfg)
	if err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "the connection cannot verify tokens"}
	}
	claims, err := v.Verify(ctx, tok)
	if err != nil {
		return inbound.Message{}, &inbound.AuthError{Msg: "invalid token: " + authReason(err)}
	}
	if cfg.AudienceType == AudienceAppURL {
		// An ID token of any Google account has the right issuer: it must be the Chat service account's.
		email, _ := claims["email"].(string)
		verified, _ := claims["email_verified"].(bool)
		if !verify.TokenEqual(email, chatIssuer) || !verified {
			return inbound.Message{}, &inbound.AuthError{Msg: "invalid token: not issued for the Chat service account"}
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return inbound.Message{}, &inbound.AuthError{Msg: "the body is unreadable or too large"}
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		return inbound.Message{}, errors.New("the event is not valid JSON")
	}
	return toMessage(ev, len(body))
}

func authReason(err error) string {
	switch {
	case errors.Is(err, verify.ErrKeyFetch):
		return "signing keys unavailable"
	case errors.Is(err, verify.ErrUnknownKey):
		return "unknown signing key"
	}
	return strings.TrimPrefix(err.Error(), verify.ErrBadJWT.Error()+": ")
}

func ignored(id, user string, size int, reason string) (inbound.Message, error) {
	return inbound.Message{MessageID: "ignored:" + id, Sender: inbound.Sender{ID: user}, Size: size}, &inbound.Rejection{Reason: reason, Ignore: true}
}

func emptyJSON() *inbound.Response {
	return &inbound.Response{Status: http.StatusOK, ContentType: "application/json", Body: []byte("{}")}
}

func toMessage(ev event, size int) (inbound.Message, error) {
	if ev.Type != "MESSAGE" {
		// ADDED_TO_SPACE, REMOVED_FROM_SPACE, card events: nothing to run. The app
		// does not greet; an empty answer is what Chat expects.
		return inbound.Message{}, emptyJSON()
	}
	msg := ev.Message
	from := ev.User
	if from.Name == "" {
		from = msg.Sender
	}
	sp := msg.Space
	if sp.Name == "" {
		sp = ev.Space
	}
	if msg.Name == "" || from.Name == "" || !spaceRe.MatchString(sp.Name) {
		return ignored(msg.Name+sp.Name, from.Name, size, "malformed")
	}
	group := strings.ToLower(from.Email)
	if i := strings.LastIndexByte(group, '@'); i >= 0 {
		group = group[i+1:]
	} else {
		group = from.DomainID
	}
	m := inbound.Message{
		Sender: inbound.Sender{ID: from.Name, Group: group, Display: from.DisplayName}, Channel: sp.Name, Size: size, MessageID: msg.Name,
	}
	if from.Type == userTypeBot || msg.Sender.Type == userTypeBot {
		m.FromBot = true
		m.MessageID = "ignored:" + msg.Name
		return m, nil
	}
	direct := sp.Type == "DM" || sp.SpaceType == "DIRECT_MESSAGE" || sp.SingleUserBotD || ev.Space.Type == "DM" || ev.Space.SingleUserBotD
	thread := msg.Thread.Name
	if thread != "" && (!threadRe.MatchString(thread) || !strings.HasPrefix(thread, sp.Name+"/threads/")) {
		thread = ""
	}
	var text string
	switch {
	case msg.SlashCommand != nil:
		m.Mode = inbound.ModeCommand
		text = msg.ArgumentText
		if direct {
			m.ThreadID = sp.Name + "/dm"
		} else {
			m.ThreadID = sp.Name + "/cmd:" + from.Name
		}
	case direct:
		m.Mode = inbound.ModeDirect
		m.ThreadID = sp.Name + "/dm" // one flat conversation
		text = stripBotMentions(msg)
	default:
		m.Mode = inbound.ModeMention
		m.ThreadID = thread
		if thread == "" {
			m.ThreadID = msg.Name // no thread named: the message starts its own conversation
		}
		text = stripBotMentions(msg)
	}
	m.Text = strings.TrimSpace(text)
	if m.Text == "" {
		m.MessageID = "ignored:" + msg.Name
		return m, &inbound.Rejection{Reason: "empty", Ignore: true}
	}
	t := target{Space: sp.Name}
	if !direct {
		t.Thread = thread // the answer joins the message's thread
	}
	m.Reply, _ = json.Marshal(t)
	return m, nil
}

// stripBotMentions removes the mentions of the app from the text, using the
// message's annotations. An annotation whose range does not hold an "@" is not
// trusted (the index unit is not what we assumed); the text then falls back to
// argumentText, which Google Chat has already stripped of app mentions.
func stripBotMentions(msg message) string {
	var cut []runeSpan
	for _, an := range msg.Annotations {
		if an.Type != "USER_MENTION" || an.UserMention.User.Type != userTypeBot {
			continue
		}
		sp, ok := mentionSpan(msg.Text, an.StartIndex, an.Length)
		if !ok {
			if strings.TrimSpace(msg.ArgumentText) != "" {
				return msg.ArgumentText
			}
			return msg.Text
		}
		cut = append(cut, sp)
	}
	if len(cut) == 0 {
		return msg.Text
	}
	runes := []rune(msg.Text)
	var b strings.Builder
	pos := 0
	for _, sp := range cut {
		if sp.from < pos {
			continue
		}
		b.WriteString(string(runes[pos:sp.from]))
		pos = sp.to
		// "a @Bot b" must not become "a  b".
		if cur := b.String(); cur == "" || strings.HasSuffix(cur, " ") {
			for pos < len(runes) && runes[pos] == ' ' {
				pos++
			}
		}
	}
	b.WriteString(string(runes[pos:]))
	return strings.TrimLeft(b.String(), " :,\t")
}

type runeSpan struct{ from, to int }

// mentionSpan converts an annotation's range to rune offsets, accepting the range
// as counted in code points or in UTF-16 units, and only when it starts at "@".
func mentionSpan(text string, start, length int) (runeSpan, bool) {
	runes := []rune(text)
	if start >= 0 && length > 0 && start+length <= len(runes) && runes[start] == '@' {
		return runeSpan{start, start + length}, true
	}
	u := utf16.Encode(runes)
	if start >= 0 && length > 0 && start+length <= len(u) && u[start] == '@' {
		// map UTF-16 offsets back to rune offsets
		off, from, to := 0, -1, -1
		for i, r := range runes {
			if off == start {
				from = i
			}
			off += len(utf16.Encode([]rune{r}))
			if off == start+length {
				to = i + 1
			}
		}
		if from >= 0 && to > from {
			return runeSpan{from, to}, true
		}
	}
	return runeSpan{}, false
}

// AckResponse answers Google Chat with an empty message object, whatever became
// of the message: the answer is posted later through the REST API, and nothing is
// said about why a message was refused.
func (a *Adapter) AckResponse(inbound.Message, inbound.Outcome) *inbound.Response {
	return emptyJSON()
}
