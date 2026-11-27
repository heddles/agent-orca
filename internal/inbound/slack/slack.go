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

// Package slack is the Slack adapter of the inbound channel core
// (internal/inbound): the Events API over HTTPS (app mentions, direct messages,
// and, for allow-listed channels only, plain channel messages) and slash
// commands, answered in the thread with chat.postMessage.
//
// Every request is verified with the app's signing secret (v0 HMAC over
// "v0:<timestamp>:<body>", five minute window, constant time). Slack retries a
// request it did not get a 200 for, and sends a message that mentions the bot
// twice when both app_mention and message.channels are subscribed; both are
// de-duplicated by the message's own (workspace, channel, timestamp), not by the
// event id, so the second delivery of the same message never starts a second run.
// Bots, apps, edits and the connection's own bot are ignored. Replies are mrkdwn
// with every mention neutralised (internal/inbound/chat). Socket Mode is not
// built. Message text is never logged or stored by this package.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
)

// Kind is the adapter's kind.
const Kind = "slack"

// Keys of the connection's Secret.
const (
	KeyBotToken      = "botToken"      // xoxb-...: the bot user's token
	KeySigningSecret = "signingSecret" // the app's signing secret
)

const (
	defaultAPI    = "https://slack.com/api"
	maxBody       = 1 << 20
	maxReplyChars = 3500
	maxReplyParts = 5
	// maxReportParts stops output that has run away from flooding a channel:
	// about 140,000 characters, far above a report meant to be read.
	maxReportParts = 40
	// reportPause is between a report's messages: Slack takes about one message
	// a second in a channel.
	reportPause    = 1100 * time.Millisecond
	signatureWait  = 5 * time.Minute
	maxClientBytes = 512 << 10
)

// Config is the adapter's own, non-secret settings.
type Config struct {
	// TeamID is the workspace (or enterprise) the connection serves, T... or E...
	// Events from any other workspace the app is installed in are ignored: the
	// bot token is that of one workspace. It is looked up from the bot token
	// (CompleteConfig); while it is empty every message is ignored.
	TeamID string `json:"teamId,omitempty"`
	// WorkingReaction shows each request's progress as a reaction on its message
	// (needs the reactions:write scope). On unless set to false.
	WorkingReaction *bool `json:"workingReaction,omitempty"`
}

func (c Config) reactions() bool { return c.WorkingReaction == nil || *c.WorkingReaction }

var teamRe = regexp.MustCompile(`^[TEW][A-Z0-9]{2,30}$`)

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
	if c.TeamID != "" && !teamRe.MatchString(c.TeamID) {
		return c, errors.New("adapter.teamId must be the id of the workspace, like T0123ABCD")
	}
	return c, nil
}

// Adapter is the Slack adapter. The zero value is ready to use.
type Adapter struct {
	// APIBase is Slack's Web API base; empty is https://slack.com/api (tests).
	APIBase string
	// Client calls the Web API; nil is a default guarded client (tests allow loopback).
	Client *chat.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Pause waits between the messages of a report; nil sleeps (tests).
	Pause func(ctx context.Context, d time.Duration) error

	posting    sync.Map // connection id and channel -> *sync.Mutex
	clientOnce sync.Once
	client     *chat.Client
	botIDs     sync.Map // connection id -> bot user id, learned from auth.test and events
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
	a.clientOnce.Do(func() { a.client = &chat.Client{} })
	return a.client
}

func (a *Adapter) base() string {
	if a.APIBase != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return defaultAPI
}

// Kind is "slack".
func (*Adapter) Kind() string { return Kind }

// Capabilities: threads, mrkdwn, long answers split into up to five messages,
// all four modes, acknowledged first, with an optional reaction as the indicator.
func (*Adapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{
		Threads: true, Markdown: true, Dialect: inbound.DialectMrkdwn, MaxReplyChars: maxReplyChars, MaxReplyParts: maxReplyParts,
		Modes:    []inbound.Mode{inbound.ModeDirect, inbound.ModeMention, inbound.ModeCommand, inbound.ModeChannel},
		AckFirst: true, WorkingIndicator: true,
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
		Title:   "Slack",
		Summary: "People mention the bot, message it, or use a slash command; the agent answers in the thread.",
		Fields: []inbound.Field{
			{Key: "teamId", Label: "Workspace id", Type: "string", Placeholder: "T0123ABCD",
				Help: "Leave empty: it is looked up from the bot token. Events from other workspaces are ignored."},
			{Key: "workingReaction", Label: "Show progress with reactions", Type: "bool", Default: true,
				Help: "On the message: eyes while working, a raised hand when approval is needed, then a check mark or a cross. Needs the reactions:write scope."},
		},
		SecretKeys: []inbound.SecretKey{
			{Key: KeyBotToken, Required: true, Help: "The Bot User OAuth Token (xoxb-...)."},
			{Key: KeySigningSecret, Required: true, Help: "Signing Secret of the app (Basic Information)."},
		},
		Allow: inbound.AllowHelp{
			Users:    "Slack member ids, like U0123ABCD (profile, three dots, Copy member ID).",
			Groups:   "Workspace ids, like T0123ABCD. Admits everyone in that workspace.",
			Channels: "Channel ids, like C0123ABCD or D... for a direct message. Required to answer plain channel messages.",
		},
		Setup: []string{
			"Create an app at api.slack.com/apps (from scratch) in your workspace.",
			"OAuth & Permissions, Bot Token Scopes: app_mentions:read, chat:write, im:history (direct messages), commands (slash command), channels:history and groups:history only for the channel mode, reactions:write for the progress reactions. Install the app and copy the Bot User OAuth Token.",
			"Basic Information: copy the Signing Secret.",
			"Ask your administrator to create a Secret with the keys botToken and signingSecret, labelled agentorca.io/inbound-secret=true, and give its name to this channel.",
			"Event Subscriptions: turn on, Request URL {{url}} (it must show Verified; save this channel first), subscribe the bot to app_mention, message.im and, for the channel mode, message.channels.",
			"App Home: allow messages from the Messages tab, for direct messages.",
			"Slash Commands, if wanted: create /agent with the Request URL {{url}}. The bot must be a member of the channel the command is used in.",
			"Invite the bot to a channel (/invite @name), then press Test connection.",
		},
	}
}

var (
	_ inbound.Receiver     = (*Adapter)(nil)
	_ inbound.Tester       = (*Adapter)(nil)
	_ inbound.Describer    = (*Adapter)(nil)
	_ inbound.Acknowledger = (*Adapter)(nil)
	_ inbound.Indicator    = (*Adapter)(nil)
	_ inbound.Completer    = (*Adapter)(nil)
	_ inbound.SetupLinker  = (*Adapter)(nil)
	_ inbound.Directory    = (*Adapter)(nil)
)
