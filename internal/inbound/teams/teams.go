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

// Package teams is the Microsoft Teams adapter of the inbound channel core
// (internal/inbound): a bot registered with the Bot Framework that Teams calls
// over HTTPS for personal chats, group chats and channels, answered in the
// conversation through the Bot Connector.
//
// Every request is authenticated as the Bot Framework requires: a bearer JWT
// signed with a key from the Bot Framework key set that is endorsed for the
// Teams channel, issuer https://api.botframework.com, audience the bot's app
// id, RS256 only, one minute of clock skew, and its serviceUrl claim equal to
// the serviceUrl of the activity. The bot answers by calling that serviceUrl
// (https only, on a Microsoft host, through the SSRF-guarded client) with an
// OAuth2 client-credentials token for https://api.botframework.com/.default
// from the tenant's authority (single-tenant registration) or botframework.com
// (multi-tenant), cached per connection until shortly before it expires and
// never logged. The conversation reference (service URL, conversation and
// activity ids) lives only in the pending reply's reply_target. Replies are
// Markdown with every mention neutralised (internal/inbound/chat). Message text
// is never logged or stored by this package.
package teams

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

// Kind is the adapter's kind.
const Kind = "teams"

// Keys of the connection's Secret.
const (
	KeyAppID       = "appId"       // the bot's Microsoft app (client) id
	KeyAppPassword = "appPassword" // its client secret
	KeyTenantID    = "tenantId"    // single-tenant registrations: the directory (tenant) id; empty for multi-tenant
)

const (
	defaultKeysURL  = "https://login.botframework.com/v1/.well-known/keys"
	defaultAuthBase = "https://login.microsoftonline.com"
	issuer          = "https://api.botframework.com"
	scope           = "https://api.botframework.com/.default"
	multiTenant     = "botframework.com"
	teamsChannel    = "msteams" // channelId of Teams, and the endorsement a key needs for it
	maxBody         = 512 << 10
	// A Teams message is limited to about 28 KB; stay below it.
	maxReplyBytes = 27000
	maxReplyChars = 20000
	maxReplyParts = 4
	leeway        = time.Minute
)

// Config is the adapter's own, non-secret settings.
type Config struct {
	// TypingIndicator shows "typing" in the conversation once the run started.
	TypingIndicator bool `json:"typingIndicator,omitempty"`
}

// ParseConfig reads and validates the settings (all optional).
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

// Adapter is the Teams adapter. The zero value is ready to use.
type Adapter struct {
	// KeysURL is the Bot Framework key set; empty is the published one (tests).
	KeysURL string
	// AuthBase is the OAuth authority host; empty is login.microsoftonline.com (tests).
	AuthBase string
	// Client calls the token endpoint and the Bot Connector; nil is a default guarded client.
	Client *chat.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Insecure lets tests use http, loopback addresses and any service URL host.
	Insecure bool

	tokens chat.TokenCache

	mu        sync.Mutex
	client    *chat.Client
	verifiers map[string]*verify.JWTVerifier // app id -> verifier
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

func (a *Adapter) keysURL() string {
	if a.KeysURL != "" {
		return a.KeysURL
	}
	return defaultKeysURL
}

func (a *Adapter) authBase() string {
	if a.AuthBase != "" {
		return strings.TrimRight(a.AuthBase, "/")
	}
	return defaultAuthBase
}

// verifier returns the (cached) JWT verifier of an app id.
func (a *Adapter) verifier(appID string) (*verify.JWTVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v := a.verifiers[appID]; v != nil {
		return v, nil
	}
	v, err := verify.NewJWTVerifier(verify.JWTConfig{
		JWKSURL: a.keysURL(), Issuers: []string{issuer}, Audiences: []string{appID}, Algs: []string{"RS256"},
		Leeway: leeway, Now: a.Now, AllowPrivate: a.Insecure, AllowInsecureHTTP: a.Insecure,
	})
	if err != nil {
		return nil, err
	}
	if a.verifiers == nil {
		a.verifiers = map[string]*verify.JWTVerifier{}
	}
	a.verifiers[appID] = v
	return v, nil
}

// Kind is "teams".
func (*Adapter) Kind() string { return Kind }

// Capabilities: threads (channel threads), Markdown, long answers split into up
// to four messages, direct, mention and channel modes (the last needs the RSC
// permission that delivers every channel message), acknowledged first, with an
// optional typing indicator.
func (*Adapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{
		Threads: true, Markdown: true, Dialect: inbound.DialectMarkdown, MaxReplyChars: maxReplyChars, MaxReplyParts: maxReplyParts,
		Modes:    []inbound.Mode{inbound.ModeDirect, inbound.ModeMention, inbound.ModeChannel},
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
		Title:   "Microsoft Teams",
		Summary: "People message the bot or @mention it in a team channel or group chat; the agent answers in the conversation.",
		Fields: []inbound.Field{
			{Key: "typingIndicator", Label: "Show typing while working", Type: "bool", Help: "Sends a typing activity once the run started."},
		},
		SecretKeys: []inbound.SecretKey{
			{Key: KeyAppID, Required: true, Help: "The bot's Microsoft App ID (application / client id)."},
			{Key: KeyAppPassword, Required: true, Help: "A client secret of that app registration (its value, not its id)."},
			{Key: KeyTenantID, Help: "Directory (tenant) id, for a single-tenant app registration. Leave it out for a multi-tenant one."},
		},
		Allow: inbound.AllowHelp{
			Users:    "Microsoft Entra object ids of the people (a GUID, from Entra ID, Users).",
			Groups:   "Tenant ids (a GUID). Admits everyone in that Microsoft 365 tenant, guests included when their home tenant is listed.",
			Channels: "Conversation ids: a channel's id (19:...@thread.tacv2, from the channel link) or a chat's id (19:...@thread.v2, or a:... for a personal chat). Required to answer plain channel messages.",
		},
		Setup: []string{
			"In the Azure portal create an Azure Bot resource (or use the Teams Developer Portal, Tools, Bot management). Choose a single-tenant app registration if you can; multi-tenant works too.",
			"Set the bot's Messaging endpoint to {{url}} (save this channel first so the URL exists).",
			"Copy the Microsoft App ID. In the app registration, Certificates & secrets, create a client secret and copy its value. For a single-tenant app also note the Directory (tenant) ID.",
			"Ask your administrator to create a Secret with the keys appId, appPassword and (single-tenant only) tenantId, labelled agentorca.io/inbound-secret=true, and give its name to this channel. For example: kubectl -n NAMESPACE create secret generic teams-creds --from-literal=appId=... --from-literal=appPassword=... --from-literal=tenantId=...; kubectl -n NAMESPACE label secret teams-creds agentorca.io/inbound-secret=true",
			"Azure Bot, Channels: add Microsoft Teams.",
			"Make a Teams app (Developer Portal or an app manifest) whose bot id is the Microsoft App ID, with the scopes personal, team and groupChat. For the channel mode only, add the resource-specific consent permission ChannelMessage.Read.Group. Upload it to Teams (Apps, Manage your apps, Upload) and add it to a team or chat.",
			"Press Test connection: it fetches a token with the app id and secret and sends nothing to Teams.",
		},
	}
}

var (
	_ inbound.Receiver     = (*Adapter)(nil)
	_ inbound.Tester       = (*Adapter)(nil)
	_ inbound.Describer    = (*Adapter)(nil)
	_ inbound.Acknowledger = (*Adapter)(nil)
	_ inbound.Indicator    = (*Adapter)(nil)
)

// fingerprint identifies the credentials a cached token was fetched with.
func fingerprint(c inbound.Connection) string {
	h := sha256.Sum256([]byte(c.Secret.Get(KeyAppID) + "\x00" + c.Secret.Get(KeyAppPassword) + "\x00" + c.Secret.Get(KeyTenantID)))
	return hex.EncodeToString(h[:])
}

var errNoCreds = errors.New("the Secret needs the keys appId and appPassword")
