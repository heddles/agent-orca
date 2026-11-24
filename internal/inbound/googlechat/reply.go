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
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// serviceAccount is the part of a downloaded key file that is used. The file's
// own token_uri is ignored: the token endpoint is pinned, so a key file cannot
// aim the signed assertion at another host.
type serviceAccount struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	ProjectID    string `json:"project_id"`
}

var errNoKey = fmt.Errorf("the Secret has no %q key", KeyServiceAccount)

// parseKey reads the service account key of a connection. Errors never repeat
// the key.
func parseKey(c inbound.Connection) (serviceAccount, *rsa.PrivateKey, error) {
	var sa serviceAccount
	raw := strings.TrimSpace(c.Secret.Get(KeyServiceAccount))
	if raw == "" {
		return sa, nil, errNoKey
	}
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return sa, nil, errors.New("the service account key is not valid JSON (use the file Google Cloud gave you)")
	}
	if sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return sa, nil, errors.New("the service account key is not a service account key (type, client_email and private_key are needed)")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(sa.PrivateKey))
	if err != nil {
		return sa, nil, errors.New("the service account key has no usable private key")
	}
	return sa, key, nil
}

// token returns the connection's access token for the Chat API: a signed JWT
// assertion exchanged at the token endpoint (the JWT bearer grant), cached.
func (a *Adapter) token(ctx context.Context, c inbound.Connection) (chat.Token, error) {
	sa, key, err := parseKey(c)
	if err != nil {
		return chat.Token{}, err
	}
	return a.tokens.Get(ctx, c.ID, fingerprint(c), func(ctx context.Context) (chat.Token, time.Duration, error) {
		now := a.now()
		assertion := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": sa.ClientEmail, "scope": chatScope, "aud": a.tokenURL(), "iat": now.Unix(), "exp": now.Add(assertionMaxLife).Unix(),
		})
		if sa.PrivateKeyID != "" {
			assertion.Header["kid"] = sa.PrivateKeyID
		}
		signed, err := assertion.SignedString(key)
		if err != nil {
			return chat.Token{}, 0, errors.New("the assertion could not be signed")
		}
		form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {signed}}
		resp, err := a.api().Do(ctx, http.MethodPost, a.tokenURL(),
			http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}}, []byte(form.Encode()))
		if err != nil {
			return chat.Token{}, 0, fmt.Errorf("token request: %w", err)
		}
		var out struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int64  `json:"expires_in"`
			Error       string `json:"error"`
		}
		_ = json.Unmarshal(resp.Body, &out)
		if resp.Status == http.StatusTooManyRequests {
			return chat.Token{}, 0, fmt.Errorf("token request: Google asks to wait %s", resp.RetryAfter.Round(time.Second))
		}
		if resp.Status != http.StatusOK || out.AccessToken == "" {
			return chat.Token{}, 0, fmt.Errorf("token request: Google answered %d %s", resp.Status, safeCode(out.Error))
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

// post creates a message in a space. A 401 drops the cached token and retries once.
func (a *Adapter) post(ctx context.Context, c inbound.Connection, t target, text string) error {
	if !spaceRe.MatchString(t.Space) || (t.Thread != "" && (!threadRe.MatchString(t.Thread) || !strings.HasPrefix(t.Thread, t.Space+"/threads/"))) {
		return errors.New("the reply target is not valid")
	}
	msg := map[string]any{"text": text}
	endpoint := a.base() + "/v1/" + t.Space + "/messages"
	if t.Thread != "" {
		msg["thread"] = map[string]string{"name": t.Thread}
		// Reply in the thread; if it is gone (or the space has no threads) start a new one.
		endpoint += "?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	}
	body, _ := json.Marshal(msg)
	for attempt := range 2 {
		tok, err := a.token(ctx, c)
		if err != nil {
			return err
		}
		resp, err := a.api().Do(ctx, http.MethodPost, endpoint, http.Header{
			"Authorization": {"Bearer " + tok.Value()}, "Content-Type": {"application/json; charset=utf-8"},
		}, body)
		if err != nil {
			return fmt.Errorf("chat api: %w", err)
		}
		switch {
		case resp.Status == http.StatusUnauthorized && attempt == 0:
			a.tokens.Forget(c.ID)
			continue
		case resp.Status == http.StatusTooManyRequests:
			return fmt.Errorf("chat api: Google asks to wait %s", resp.RetryAfter.Round(time.Second))
		case resp.Status/100 != 2:
			var e struct {
				Error struct {
					Status string `json:"status"`
				} `json:"error"`
			}
			_ = json.Unmarshal(resp.Body, &e)
			return fmt.Errorf("chat api: Google answered %d %s", resp.Status, safeCode(e.Error.Status))
		}
		return nil
	}
	return errors.New("chat api: the token was refused")
}

// Reply posts the answer to the space, in the message's thread when there is
// one, as one or more messages. Mentions are neutralised.
func (a *Adapter) Reply(ctx context.Context, c inbound.Connection, to inbound.ReplyTarget, res inbound.Result) error {
	var t target
	if err := json.Unmarshal(to, &t); err != nil || t.Space == "" {
		return errors.New("the reply target is not valid")
	}
	for _, part := range chat.Render(res.Text, a.Capabilities()) {
		if err := a.post(ctx, c, t, part); err != nil {
			return err
		}
	}
	return nil
}

// Test checks the configuration and fetches an access token with the service
// account key: it proves the key, and sends nothing to Chat.
func (a *Adapter) Test(ctx context.Context, c inbound.Connection) inbound.TestReport {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	details := map[string]string{"audienceType": cfg.AudienceType}
	sa, _, err := parseKey(c)
	if err != nil {
		return inbound.TestReport{Details: details, Error: err.Error()}
	}
	details["serviceAccount"] = sa.ClientEmail
	if sa.ProjectID != "" {
		details["project"] = sa.ProjectID
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a.tokens.Forget(c.ID)
	if _, err := a.token(ctx, c); err != nil {
		return inbound.TestReport{Details: details, Error: err.Error()}
	}
	details["token"] = "obtained for the chat.bot scope"
	return inbound.TestReport{OK: true, Details: details}
}
