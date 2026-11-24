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

// Package webex is the Cisco Webex adapter of the inbound channel core
// (internal/inbound): a Webex bot that receives "message created" webhooks for
// direct spaces and for group spaces where it is @mentioned, answered in the
// space (in the message's thread) with POST /v1/messages.
//
// Every webhook is verified with the X-Spark-Signature header: an HMAC-SHA1 of
// the raw body under the webhook's secret, compared in constant time (Webex
// signs with HMAC-SHA1 and offers nothing else). A webhook carries only ids, so
// after the acknowledgement the adapter fetches the message with the bot token
// (GET /v1/messages/{id}), learns the bot itself with GET /v1/people/me, drops
// the bot's own and other bots' messages, and checks that a group message names
// the bot. Replies are Markdown with every mention neutralised, split to the
// 7439 bytes Webex allows per message. Message text is never logged or stored by
// this package.
package webex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// Kind is the adapter's kind.
const Kind = "webex"

// Keys of the connection's Secret.
const (
	KeyBotToken      = "botToken"      // the bot's access token
	KeyWebhookSecret = "webhookSecret" // the secret given when the webhook was created
)

const (
	defaultAPI    = "https://webexapis.com"
	maxBody       = 256 << 10
	maxReplyBytes = 7439
	maxReplyChars = 7000
	maxReplyParts = 5
	// A webhook older than this is a replay (or a very late retry) and is not run.
	maxAge = time.Hour
	// ... and one from the future by more than this has a wrong clock.
	maxSkew = 5 * time.Minute
)

// Config is the adapter's own settings. It has none yet; the type exists so an
// unknown setting is refused instead of silently ignored.
type Config struct{}

// ParseConfig reads and validates the settings.
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
	return c, nil
}

// me is what is known of the bot's own account.
type me struct {
	ID          string
	DisplayName string
	NickName    string
}

// Adapter is the Webex adapter. The zero value is ready to use.
type Adapter struct {
	// APIBase is the Webex API base; empty is https://webexapis.com (tests).
	APIBase string
	// Client calls the Webex API; nil is a default guarded client (tests allow loopback).
	Client *chat.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu     sync.Mutex
	client *chat.Client
	self   map[string]me // connection id + token fingerprint -> the bot
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) api() *chat.Client {
	if a.Client != nil {
		return a.Client
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		a.client = &chat.Client{}
	}
	return a.client
}

func (a *Adapter) base() string {
	if a.APIBase != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return defaultAPI
}

// Kind is "webex".
func (*Adapter) Kind() string { return Kind }

// Capabilities: threads, Markdown, long answers split into up to five messages,
// direct and mention modes (Webex delivers a group message to a bot only when it
// is mentioned), acknowledged first. There is no working indicator.
func (*Adapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{
		Threads: true, Markdown: true, Dialect: inbound.DialectMarkdown, MaxReplyChars: maxReplyChars, MaxReplyParts: maxReplyParts,
		Modes:    []inbound.Mode{inbound.ModeDirect, inbound.ModeMention},
		AckFirst: true,
	}
}

// ValidateConfig checks the settings.
func (*Adapter) ValidateConfig(raw json.RawMessage) error {
	_, err := ParseConfig(raw)
	return err
}

// Describe lists the Secret keys and the setup steps.
func (*Adapter) Describe() inbound.Descriptor {
	return inbound.Descriptor{
		Title:   "Webex",
		Summary: "People message the bot or @mention it in a space; the agent answers in the thread.",
		SecretKeys: []inbound.SecretKey{
			{Key: KeyBotToken, Required: true, Help: "The bot's access token (developer.webex.com, My Webex Apps, the bot)."},
			{Key: KeyWebhookSecret, Required: true, Help: "A long random string you choose; give the same value when you create the webhook."},
		},
		Allow: inbound.AllowHelp{
			Users:    "Email addresses of the people, like ann@example.com.",
			Groups:   "Email domains of the people, like example.com.",
			Channels: "Space ids (roomId), as in the webhook payload or the Webex API (long base64 strings).",
		},
		Setup: []string{
			"Create a bot at developer.webex.com (My Webex Apps, Create a Bot). Copy its access token; it is shown once.",
			"Choose a long random string as the webhook secret.",
			"Ask your administrator to create a Secret with the keys botToken and webhookSecret, labelled agentorca.io/inbound-secret=true, and name it above. For example: kubectl -n NAMESPACE create secret generic webex-creds --from-literal=botToken=... --from-literal=webhookSecret=...; kubectl -n NAMESPACE label secret webex-creds agentorca.io/inbound-secret=true",
			"Create the webhook with the bot's token: curl -X POST https://webexapis.com/v1/webhooks -H \"Authorization: Bearer $BOT_TOKEN\" -H \"Content-Type: application/json\" -d '{\"name\":\"agent-orca\",\"targetUrl\":\"{{url}}\",\"resource\":\"messages\",\"event\":\"created\",\"secret\":\"$WEBHOOK_SECRET\"}' (save this channel first so the URL exists). Do not set a filter: the bot only receives its direct messages and the messages that mention it.",
			"Add the bot to a space (or message it directly). In a group space, @mention it.",
			"Press Test connection: it calls the Webex API as the bot and shows its name; nothing is sent to a space.",
		},
	}
}

var (
	_ inbound.Receiver  = (*Adapter)(nil)
	_ inbound.Tester    = (*Adapter)(nil)
	_ inbound.Describer = (*Adapter)(nil)
	_ inbound.Enricher  = (*Adapter)(nil)
)

var errNoCreds = fmt.Errorf("the Secret needs the keys %s and %s", KeyBotToken, KeyWebhookSecret)

var errNoToken = errors.New("the Secret has no botToken key")
