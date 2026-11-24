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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// call sends a request to the Webex API as the bot and decodes a JSON answer.
// Errors name the path and Webex's status, never the token or a body.
func (a *Adapter) call(ctx context.Context, c inbound.Connection, method, path string, payload, out any) error {
	token := strings.TrimSpace(c.Secret.Get(KeyBotToken))
	if token == "" {
		return errNoToken
	}
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	h := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}}
	if body != nil {
		h.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := a.api().Do(ctx, method, a.base()+path, h, body)
	if err != nil {
		return fmt.Errorf("webex api: %w", err)
	}
	switch {
	case resp.Status == http.StatusTooManyRequests:
		return fmt.Errorf("webex api: Webex asks to wait %s", resp.RetryAfter.Round(time.Second))
	case resp.Status/100 != 2:
		return fmt.Errorf("webex api: Webex answered %d", resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return errors.New("webex api: the answer is not JSON")
		}
	}
	return nil
}

func (a *Adapter) get(ctx context.Context, c inbound.Connection, path string, out any) error {
	return a.call(ctx, c, http.MethodGet, path, nil, out)
}

func selfKey(c inbound.Connection) string {
	h := sha256.Sum256([]byte(c.Secret.Get(KeyBotToken)))
	return c.ID + ":" + hex.EncodeToString(h[:8])
}

// isSelf: the id is the bot's own, as far as it has been learned.
func (a *Adapter) isSelf(c inbound.Connection, personID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.self[selfKey(c)]
	return ok && personID != "" && s.ID == personID
}

// whoAmI is the bot's own account, read once per connection and token.
func (a *Adapter) whoAmI(ctx context.Context, c inbound.Connection) (me, error) {
	key := selfKey(c)
	a.mu.Lock()
	s, ok := a.self[key]
	a.mu.Unlock()
	if ok {
		return s, nil
	}
	var p struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		NickName    string `json:"nickName"`
		Type        string `json:"type"`
	}
	if err := a.get(ctx, c, "/v1/people/me", &p); err != nil {
		return me{}, err
	}
	if p.ID == "" {
		return me{}, errors.New("webex api: the account has no id")
	}
	s = me{ID: p.ID, DisplayName: p.DisplayName, NickName: p.NickName}
	a.mu.Lock()
	if a.self == nil {
		a.self = map[string]me{}
	}
	a.self[key] = s
	a.mu.Unlock()
	return s, nil
}

// Reply posts the answer to the space, in the message's thread when there is
// one, as one or more messages of at most 7439 bytes. Mentions are neutralised.
func (a *Adapter) Reply(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, res inbound.Result) error {
	var t target
	if err := json.Unmarshal(to, &t); err != nil || !idRe.MatchString(t.Room) || (t.Parent != "" && !idRe.MatchString(t.Parent)) {
		return errors.New("the reply target is not valid")
	}
	for _, part := range chat.RenderBytes(res.Text, a.Capabilities(), maxReplyBytes) {
		msg := map[string]string{"roomId": t.Room, "markdown": part}
		if t.Parent != "" {
			msg["parentId"] = t.Parent
		}
		if err := a.call(ctx, c, http.MethodPost, "/v1/messages", msg, nil); err != nil {
			return err
		}
	}
	return nil
}

// Test calls GET /v1/people/me: it shows the bot the token belongs to and sends
// nothing to a space.
func (a *Adapter) Test(ctx context.Context, c inbound.Connection) inbound.TestReport {
	if _, err := ParseConfig(c.Config); err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	details := map[string]string{"webhookSecret": "configured"}
	if c.Secret.Get(KeyWebhookSecret) == "" {
		details["webhookSecret"] = "missing"
		return inbound.TestReport{Details: details, Error: errNoCreds.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var p struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		NickName    string `json:"nickName"`
		Type        string `json:"type"`
	}
	if err := a.get(ctx, c, "/v1/people/me", &p); err != nil {
		return inbound.TestReport{Details: details, Error: err.Error()}
	}
	details["bot"] = p.DisplayName
	if p.Type != "bot" {
		return inbound.TestReport{Details: details, Error: "the token does not belong to a bot (it is " + safeWord(p.Type) + "): use the bot's own access token"}
	}
	a.mu.Lock()
	if a.self == nil {
		a.self = map[string]me{}
	}
	a.self[selfKey(c)] = me{ID: p.ID, DisplayName: p.DisplayName, NickName: p.NickName}
	a.mu.Unlock()
	return inbound.TestReport{OK: true, Details: details}
}

func safeWord(s string) string {
	if s == "" || len(s) > 32 || strings.ContainsAny(s, " \n\t\"'<>") {
		return "not a bot"
	}
	return "a " + s
}
