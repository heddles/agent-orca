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

// Package googlechat is the Google Chat adapter of the inbound channel core
// (internal/inbound): a Chat app with an HTTP endpoint that Google Chat calls
// for direct messages, @mentions in spaces and slash commands, answered in the
// space (in the message's thread) with the Chat REST API.
//
// Every request is authenticated with the bearer token Google Chat sends, in
// one of the two ways the app can be set up: a self-signed JWT issued by the
// Chat service account (audience = the Google Cloud project number, keys from
// the service account's JWK set), or a Google-signed ID token (audience = the
// app's HTTP endpoint URL, email claim = the Chat service account, verified).
// RS256 only, issuer and audience pinned, one minute of clock skew. The app
// answers with the Chat REST API as itself: a service account (its key is in
// the connection's Secret) exchanges a signed JWT for an access token with the
// chat.bot scope, cached per connection until shortly before it expires and
// never logged. Replies are Chat markup with every mention neutralised
// (internal/inbound/chat). Only the Chat app (non add-on) event format is read.
// Message text is never logged or stored by this package.
package googlechat

import (
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/heddles/agent-orca/internal/inbound/verify"
)

// Kind is the adapter's kind.
const Kind = "googlechat"

// KeyServiceAccount is the key of the connection's Secret that holds the
// service account's JSON key (the file Google Cloud lets you download).
const KeyServiceAccount = "serviceAccountKey"

// Audience types.
const (
	AudienceProjectNumber = "projectNumber"
	AudienceAppURL        = "appUrl"
)

const (
	chatIssuer       = "chat@system.gserviceaccount.com"
	chatJWKS         = "https://www.googleapis.com/service_accounts/v1/jwk/chat@system.gserviceaccount.com"
	googleCerts      = "https://www.googleapis.com/oauth2/v3/certs"
	defaultAPI       = "https://chat.googleapis.com"
	defaultTokenURL  = "https://oauth2.googleapis.com/token"
	chatScope        = "https://www.googleapis.com/auth/chat.bot"
	maxBody          = 512 << 10
	maxReplyChars    = 4000
	maxReplyParts    = 5
	leeway           = time.Minute
	assertionMaxLife = 50 * time.Minute
)

// Config is the adapter's own, non-secret settings.
type Config struct {
	// AudienceType says what the app's "Authentication Audience" is set to:
	// projectNumber or appUrl.
	AudienceType string `json:"audienceType"`
	// Audience is the Google Cloud project number, or the app's HTTP endpoint URL.
	Audience string `json:"audience"`
}

var projectNumberRe = regexp.MustCompile(`^[0-9]{4,20}$`)

// ParseConfig reads and validates the settings.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, errors.New("adapter settings are required: set adapter.audienceType and adapter.audience")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("adapter settings: %w", err)
	}
	switch c.AudienceType {
	case AudienceProjectNumber:
		if !projectNumberRe.MatchString(c.Audience) {
			return c, errors.New("adapter.audience must be the project number (digits), not the project id")
		}
	case AudienceAppURL:
		u, err := url.Parse(c.Audience)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return c, errors.New("adapter.audience must be the https URL of the app's HTTP endpoint, as set in the Chat API configuration")
		}
	default:
		return c, fmt.Errorf("adapter.audienceType must be %s or %s", AudienceProjectNumber, AudienceAppURL)
	}
	return c, nil
}

// Adapter is the Google Chat adapter. The zero value is ready to use.
type Adapter struct {
	// APIBase is the Chat REST API base; empty is https://chat.googleapis.com (tests).
	APIBase string
	// TokenURL is the OAuth token endpoint; empty is Google's (tests).
	TokenURL string
	// KeysURL overrides the key set both audience types verify against (tests).
	KeysURL string
	// Client calls the token endpoint and the Chat API; nil is a default guarded client.
	Client *chat.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Insecure lets tests use http and loopback addresses.
	Insecure bool

	tokens chat.TokenCache

	mu        sync.Mutex
	client    *chat.Client
	verifiers map[string]*verify.JWTVerifier // audience type + audience -> verifier
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

func (a *Adapter) tokenURL() string {
	if a.TokenURL != "" {
		return a.TokenURL
	}
	return defaultTokenURL
}

func (a *Adapter) verifier(cfg Config) (*verify.JWTVerifier, error) {
	id := cfg.AudienceType + "|" + cfg.Audience
	a.mu.Lock()
	defer a.mu.Unlock()
	if v := a.verifiers[id]; v != nil {
		return v, nil
	}
	jc := verify.JWTConfig{
		JWKSURL: chatJWKS, Issuers: []string{chatIssuer}, Audiences: []string{cfg.Audience}, Algs: []string{"RS256"},
		Leeway: leeway, Now: a.Now, AllowPrivate: a.Insecure, AllowInsecureHTTP: a.Insecure,
	}
	if cfg.AudienceType == AudienceAppURL {
		jc.JWKSURL, jc.Issuers = googleCerts, []string{"https://accounts.google.com", "accounts.google.com"}
	}
	if a.KeysURL != "" {
		jc.JWKSURL = a.KeysURL
	}
	v, err := verify.NewJWTVerifier(jc)
	if err != nil {
		return nil, err
	}
	if a.verifiers == nil {
		a.verifiers = map[string]*verify.JWTVerifier{}
	}
	a.verifiers[id] = v
	return v, nil
}

// Kind is "googlechat".
func (*Adapter) Kind() string { return Kind }

// Capabilities: threads, Chat markup, long answers split into up to five
// messages, direct, mention and command modes (Google Chat delivers a space
// message to the app only when it is mentioned or is a command), acknowledged
// first. There is no working indicator.
func (*Adapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{
		Threads: true, Markdown: true, Dialect: inbound.DialectMrkdwnRaw, MaxReplyChars: maxReplyChars, MaxReplyParts: maxReplyParts,
		Modes:    []inbound.Mode{inbound.ModeDirect, inbound.ModeMention, inbound.ModeCommand},
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
		Title:   "Google Chat",
		Summary: "People message the app, @mention it in a space or use a slash command; the agent answers in the thread.",
		Fields: []inbound.Field{
			{Key: "audienceType", Label: "Authentication audience", Type: "enum", Required: true, Options: []string{AudienceProjectNumber, AudienceAppURL}, Default: AudienceProjectNumber,
				Help: "What the app's Authentication Audience is set to in the Chat API configuration: Project Number, or HTTP endpoint URL."},
			{Key: "audience", Label: "Project number or endpoint URL", Type: "string", Required: true, Placeholder: "123456789012",
				Help: "The Google Cloud project number (not the project id), or the exact endpoint URL you entered in the configuration."},
		},
		SecretKeys: []inbound.SecretKey{
			{Key: KeyServiceAccount, Required: true, Help: "The JSON key of a service account of the project (the app answers as it)."},
		},
		Allow: inbound.AllowHelp{
			Users:    "Google Chat user ids, like users/123456789012345678901 (shown in the event, or from the People API).",
			Groups:   "Email domains of the people, like example.com (Workspace domain id when no email is sent).",
			Channels: "Space names, like spaces/AAAAxxxxxxx (the last part of the space's URL).",
		},
		Setup: []string{
			"In Google Cloud, pick or create a project and enable the Google Chat API. Note the project number (Dashboard).",
			"IAM & Admin, Service Accounts: create a service account for the app and a JSON key for it. No project role is needed: the Chat app is added to spaces, it does not use the project's resources.",
			"Ask your administrator to create a Secret with the key serviceAccountKey holding that JSON file, labelled agentorca.io/inbound-secret=true, and give its name to this channel. For example: kubectl -n NAMESPACE create secret generic gchat-creds --from-file=serviceAccountKey=./key.json; kubectl -n NAMESPACE label secret gchat-creds agentorca.io/inbound-secret=true",
			"Google Chat API, Configuration: untick \"Build this Chat app as a Workspace add-on\" (only the Chat app event format is understood), set the app name and description, tick \"Receive 1:1 messages\" and \"Join spaces and group conversations\".",
			"Connection settings: choose HTTP endpoint URL and enter {{url}} as the endpoint (save this channel first). Authentication Audience: Project Number, or HTTP endpoint URL (then the audience below is that same URL). Enter the same choice in the settings of this channel.",
			"Visibility: make the app available to the people or the domain that should use it, then save.",
			"Optional slash command: Configuration, Slash commands, add /agent. A command answers in the space like a mention.",
			"In Google Chat, find the app, press Add to a space (or message it), then press Test connection here: it fetches an access token with the service account key and sends nothing to Chat.",
		},
	}
}

var (
	_ inbound.Receiver     = (*Adapter)(nil)
	_ inbound.Tester       = (*Adapter)(nil)
	_ inbound.Describer    = (*Adapter)(nil)
	_ inbound.Acknowledger = (*Adapter)(nil)
)

// fingerprint identifies the credentials a cached token was fetched with.
func fingerprint(c inbound.Connection) string {
	h := sha256.Sum256([]byte(c.Secret.Get(KeyServiceAccount)))
	return hex.EncodeToString(h[:])
}
