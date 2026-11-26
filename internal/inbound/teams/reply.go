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
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

var (
	tenantRe = regexp.MustCompile(`^[0-9A-Za-z.-]{1,255}$`)
	appIDRe  = regexp.MustCompile(`^[0-9A-Za-z-]{8,64}$`)
	aadsRe   = regexp.MustCompile(`AADSTS\d{3,7}`)
)

// token returns the connection's access token for the Bot Connector, fetching
// one (client credentials) when there is none or it is about to expire.
func (a *Adapter) token(ctx context.Context, c inbound.Connection) (chat.Token, error) {
	appID, pw, tenant := strings.TrimSpace(c.Secret.Get(KeyAppID)), c.Secret.Get(KeyAppPassword), strings.TrimSpace(c.Secret.Get(KeyTenantID))
	if appID == "" || pw == "" {
		return chat.Token{}, errNoCreds
	}
	if !appIDRe.MatchString(appID) {
		return chat.Token{}, errors.New("the app id is not valid")
	}
	if tenant == "" {
		tenant = multiTenant
	}
	if !tenantRe.MatchString(tenant) {
		return chat.Token{}, errors.New("the tenant id is not valid")
	}
	return a.tokens.Get(ctx, c.ID, fingerprint(c), func(ctx context.Context) (chat.Token, time.Duration, error) {
		form := url.Values{"grant_type": {"client_credentials"}, "client_id": {appID}, "client_secret": {pw}, "scope": {scope}}
		resp, err := a.api().Do(ctx, http.MethodPost, a.authBase()+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token",
			http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}}, []byte(form.Encode()))
		if err != nil {
			return chat.Token{}, 0, fmt.Errorf("token request: %w", err)
		}
		var out struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int64  `json:"expires_in"`
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(resp.Body, &out)
		if resp.Status == http.StatusTooManyRequests {
			return chat.Token{}, 0, fmt.Errorf("token request: the login service asks to wait %s", resp.RetryAfter.Round(time.Second))
		}
		if resp.Status != http.StatusOK || out.AccessToken == "" {
			// The description can echo request details; only the code and the AADSTS number are kept.
			msg := safeCode(out.Error)
			if code := aadsRe.FindString(out.Description); code != "" {
				msg += " (" + code + ")"
			}
			return chat.Token{}, 0, fmt.Errorf("token request: the login service answered %d %s", resp.Status, msg)
		}
		ttl := time.Duration(out.ExpiresIn) * time.Second
		if ttl <= 0 {
			ttl = time.Hour
		}
		return chat.NewToken(out.AccessToken), ttl, nil
	})
}

// safeCode keeps an error code to what one looks like.
func safeCode(s string) string {
	if s == "" || len(s) > 64 || strings.ContainsAny(s, " \n\t\"'<>") {
		return "error"
	}
	return s
}

// send posts an activity to the conversation, replying to the message when one
// is named. A 401 drops the cached token and tries once more with a fresh one.
func (a *Adapter) send(ctx context.Context, c inbound.Connection, t target, act map[string]any) error {
	if !a.trustedServiceURL(t.ServiceURL) || t.Conversation == "" {
		return errors.New("the reply target is not valid")
	}
	path := strings.TrimRight(t.ServiceURL, "/") + "/v3/conversations/" + url.PathEscape(t.Conversation) + "/activities"
	if t.Activity != "" {
		path += "/" + url.PathEscape(t.Activity)
		act["replyToId"] = t.Activity
	}
	act["conversation"] = map[string]string{"id": t.Conversation}
	if t.Bot != "" {
		act["from"] = map[string]string{"id": t.Bot}
	}
	body, _ := json.Marshal(act)
	for attempt := range 2 {
		tok, err := a.token(ctx, c)
		if err != nil {
			return err
		}
		resp, err := a.api().Do(ctx, http.MethodPost, path, http.Header{
			"Authorization": {"Bearer " + tok.Value()}, "Content-Type": {"application/json; charset=utf-8"},
		}, body)
		if err != nil {
			return fmt.Errorf("bot connector: %w", err)
		}
		switch {
		case resp.Status == http.StatusUnauthorized && attempt == 0:
			a.tokens.Forget(c.ID)
			continue
		case resp.Status == http.StatusTooManyRequests:
			return fmt.Errorf("bot connector: Teams asks to wait %s", resp.RetryAfter.Round(time.Second))
		case resp.Status/100 != 2:
			var e struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(resp.Body, &e)
			return fmt.Errorf("bot connector: Teams answered %d %s", resp.Status, safeCode(e.Error.Code))
		}
		return nil
	}
	return errors.New("bot connector: the token was refused")
}

// Reply posts the answer in the conversation, as one or more messages, in
// Markdown with every mention neutralised.
func (a *Adapter) Reply(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, res inbound.Result) error {
	var t target
	if err := json.Unmarshal(to, &t); err != nil || t.ServiceURL == "" || t.Conversation == "" {
		return errors.New("the reply target is not valid")
	}
	for _, part := range chat.RenderBytes(res.Text, a.Capabilities(), maxReplyBytes) {
		if err := a.send(ctx, c, t, map[string]any{"type": "message", "textFormat": "markdown", "text": part}); err != nil {
			return err
		}
	}
	return nil
}

// Show sends "typing" when work starts and the connection asks for it. A bot
// cannot react through this API, so the other states are not shown.
func (a *Adapter) Show(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, from, now inbound.Progress) error {
	cfg, err := ParseConfig(c.Config)
	if err != nil || !cfg.TypingIndicator || from != inbound.ProgressNone || now != inbound.ProgressWorking {
		return nil
	}
	var t target
	if json.Unmarshal(to, &t) != nil || t.ServiceURL == "" {
		return nil
	}
	t.Activity = "" // a typing activity is sent to the conversation, not as a reply
	return a.send(ctx, c, t, map[string]any{"type": "typing"})
}

// Test fetches a token with the app id and secret: it proves the credentials
// (and the tenant, for a single-tenant app) and sends nothing to Teams.
func (a *Adapter) Test(ctx context.Context, c inbound.Connection) inbound.TestReport {
	if _, err := ParseConfig(c.Config); err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	mode := "multi-tenant"
	if strings.TrimSpace(c.Secret.Get(KeyTenantID)) != "" {
		mode = "single-tenant"
	}
	details := map[string]string{"appId": strings.TrimSpace(c.Secret.Get(KeyAppID)), "registration": mode}
	if details["appId"] == "" || c.Secret.Get(KeyAppPassword) == "" {
		return inbound.TestReport{Details: details, Error: errNoCreds.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a.tokens.Forget(c.ID) // a test is for the credentials as they are now
	if _, err := a.token(ctx, c); err != nil {
		return inbound.TestReport{Details: details, Error: err.Error()}
	}
	details["token"] = "obtained for " + scope
	return inbound.TestReport{OK: true, Details: details}
}
