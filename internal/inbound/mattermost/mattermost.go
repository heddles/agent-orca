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

// Package mattermost is the Mattermost adapter of the inbound channel core
// (internal/inbound): an outgoing webhook (a trigger word, or every post of a
// channel) or a slash command posts to us; the answer is posted back through the
// REST API as the bot, in the thread of the message.
//
// A webhook or command carries a token Mattermost generated; it is compared in
// constant time with the tokens in the connection's Secret. Where the server is
// (baseUrl), the bot's access token and the allowPrivateAddresses switch (a
// self-hosted server is usually on a private address) are also in the Secret and
// not in the connection, so a builder cannot aim the operator, or the bot's
// token, at a host of their choosing. The bot-account websocket (direct messages)
// is not built. Message text is never logged or stored by this package.
package mattermost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// Kind is the adapter's kind.
const Kind = "mattermost"

// Keys of the connection's Secret.
const (
	KeyBaseURL      = "baseUrl"               // https://chat.example.com (a sub-path is allowed)
	KeyBotToken     = "botToken"              // the bot account's access token
	KeyTokens       = "tokens"                // the tokens of the outgoing webhooks and slash commands, separated by commas or newlines
	KeyInsecure     = "insecure"              // "true" allows an http baseUrl
	KeyAllowPrivate = "allowPrivateAddresses" // "true" allows a server on a private address
)

const (
	maxBody       = 64 << 10
	maxReplyChars = 4000
	maxReplyParts = 5
)

// Config is the adapter's own, non-secret settings.
type Config struct {
	// TeamID, when set, restricts the connection to one team (its id, 26 characters).
	TeamID string `json:"teamId,omitempty"`
}

var idRe = regexp.MustCompile(`^[a-z0-9]{26}$`)

// ParseConfig reads and validates the settings. All of them are optional.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("adapter settings: %w", err)
	}
	if c.TeamID != "" && !idRe.MatchString(c.TeamID) {
		return c, errors.New("adapter.teamId must be a Mattermost team id (26 lower-case letters and digits)")
	}
	return c, nil
}

// Credentials are the Secret, parsed.
type Credentials struct {
	BaseURL      string
	BotToken     string
	Tokens       []string
	AllowPrivate bool
}

// ParseCredentials reads the Secret. Errors name keys, never values.
func ParseCredentials(s inbound.Secret) (Credentials, error) {
	get := func(k string) string { return strings.TrimSpace(s.Get(k)) }
	cr := Credentials{BotToken: get(KeyBotToken), Tokens: splitTokens(s.Get(KeyTokens)), AllowPrivate: strings.EqualFold(get(KeyAllowPrivate), "true")}
	u, err := url.Parse(get(KeyBaseURL))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && (u.Scheme != "http" || !strings.EqualFold(get(KeyInsecure), "true"))) {
		return cr, fmt.Errorf("the Secret needs a %q key with the https address of the server (http needs %s=true)", KeyBaseURL, KeyInsecure)
	}
	cr.BaseURL = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
	if cr.BotToken == "" {
		return cr, fmt.Errorf("the Secret has no %q key", KeyBotToken)
	}
	return cr, nil
}

func splitTokens(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' })
}

// Adapter is the Mattermost adapter. The zero value is ready to use.
type Adapter struct {
	// Client overrides the HTTP client (tests allow loopback). Nil uses a guarded
	// client chosen by the Secret's allowPrivateAddresses.
	Client *chat.Client

	mu      sync.Mutex
	clients [2]*chat.Client
	botIDs  sync.Map // connection id -> bot user id
}

func (a *Adapter) api(allowPrivate bool) *chat.Client {
	if a.Client != nil {
		return a.Client
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := 0
	if allowPrivate {
		i = 1
	}
	if a.clients[i] == nil {
		a.clients[i] = &chat.Client{AllowPrivate: allowPrivate, Timeout: 10 * time.Second}
	}
	return a.clients[i]
}

// Kind is "mattermost".
func (*Adapter) Kind() string { return Kind }

// Capabilities: threads, markdown, long answers split into up to five posts,
// outgoing webhooks and slash commands, acknowledged first. No working indicator.
func (*Adapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{
		Threads: true, Markdown: true, Dialect: inbound.DialectMarkdown, MaxReplyChars: maxReplyChars, MaxReplyParts: maxReplyParts,
		Modes:    []inbound.Mode{inbound.ModeMention, inbound.ModeCommand, inbound.ModeChannel},
		AckFirst: true,
	}
}

// ValidateConfig checks the settings.
func (*Adapter) ValidateConfig(raw json.RawMessage) error {
	_, err := ParseConfig(raw)
	return err
}

// Describe lists the settings, the Secret keys and the setup steps.
func (*Adapter) Describe() inbound.Descriptor {
	return inbound.Descriptor{
		Title:   "Mattermost",
		Summary: "An outgoing webhook (trigger word) or a slash command starts the agent; the answer is posted as a bot in the thread.",
		Fields: []inbound.Field{
			{Key: "teamId", Label: "Team id", Type: "string", Help: "Optional. Restricts the connection to one team."},
		},
		SecretKeys: []inbound.SecretKey{
			{Key: KeyBaseURL, Required: true, Help: "https://chat.example.com. In the Secret so that the bot token can only go there."},
			{Key: KeyBotToken, Required: true, Help: "Access token of a bot account."},
			{Key: KeyTokens, Required: true, Help: "The token of each outgoing webhook and slash command, comma or newline separated."},
			{Key: KeyAllowPrivate, Help: "true if the server is on a private address."},
			{Key: KeyInsecure, Help: "true allows an http server address."},
		},
		Allow: inbound.AllowHelp{
			Users:    "Mattermost user ids (26 characters; System Console, Users, or the API).",
			Groups:   "Team ids. Admits everyone writing in that team.",
			Channels: "Channel ids. Required to answer every post of a channel (no trigger word).",
		},
		Setup: []string{
			"Integrations, Bot Accounts: create a bot, copy its access token, and add the bot to the team and to the channels it should answer in.",
			"Integrations, Outgoing Webhooks: add one with the callback URL {{url}}, either a trigger word (for example @agent) or no trigger word for one channel. Copy its token. Or Slash Commands: add one with the request URL {{url}}, method POST, and copy its token.",
			"Ask your administrator to create a Secret with baseUrl, botToken and tokens (and allowPrivateAddresses=true for a private server), labelled agentorca.io/inbound-secret=true, and name it above.",
			"The Mattermost server must be able to reach this address: set the server's AllowedUntrustedInternalConnections if it is on a private network.",
			"Press Test connection.",
		},
	}
}

var (
	_ inbound.Receiver     = (*Adapter)(nil)
	_ inbound.Tester       = (*Adapter)(nil)
	_ inbound.Describer    = (*Adapter)(nil)
	_ inbound.Acknowledger = (*Adapter)(nil)
	_ inbound.Enricher     = (*Adapter)(nil)
)
