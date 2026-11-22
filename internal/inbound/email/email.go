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

// Package email is the first adapter of the inbound channel core
// (internal/inbound): people (or systems) write to a mailbox, an agent runs on
// the message, and the answer is mailed back to the sender.
//
// Two ways in, one adapter: IMAP polling of a dedicated mailbox, or a signed
// HTTP request from a mail provider's inbound parse (a relay verifies the
// provider's own signature and re-signs with the scheme of the trigger
// webhooks). Both end in the same policy (policy.go): sender authentication
// evidence, loop prevention, size and attachment limits, safe text extraction.
// Replies go out over SMTP through internal/notify.
//
// Nothing in a message is trusted: the text is untrusted input to the run, no
// link is followed, no HTML is rendered, no attachment is passed on except a
// small plain-text one when the connection asks for it. Message bodies are not
// logged and not stored by this package.
package email

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Kind is the adapter's kind, as stored in a connection.
const Kind = "email"

// Modes: how messages arrive.
const (
	ModeIMAP    = "imap"
	ModeWebhook = "webhook"
)

// Limits.
const (
	// MaxMessageBytes is the largest message taken (headers, body, attachments).
	MaxMessageBytes = 1 << 20
	// MaxBodyChars is the most characters of new text passed to the agent.
	MaxBodyChars = 16000
	// MaxTextAttachmentBytes is the largest attachment passed on, when allowed.
	MaxTextAttachmentBytes = 100 << 10
	// MaxAcceptedAttachments is how many text attachments pass.
	MaxAcceptedAttachments = 3
	maxParts               = 50
	maxDepth               = 5
	maxListedAttachments   = 20
	maxReplyChars          = 20000
)

// Config is the adapter's own settings (the "adapter" object of a connection's
// config). Where the mailbox is and how to log in is NOT here but in the
// connection's Secret (secret.go): a builder who writes a connection must not
// be able to aim the operator at a host or choose the sender address.
type Config struct {
	// Mode is imap or webhook.
	Mode string `json:"mode"`
	// Folder is the mailbox folder polled (imap). Default INBOX.
	Folder string `json:"folder,omitempty"`
	// ProcessedFolder, when set, is where a processed message is moved (imap).
	// Empty marks it \Seen instead.
	ProcessedFolder string `json:"processedFolder,omitempty"`
	// RequireAuthResults requires evidence in an Authentication-Results header
	// that the sender's domain is authenticated (DMARC pass, or aligned DKIM or
	// SPF pass). Default true: without it anyone can write From: an allowed address.
	RequireAuthResults *bool `json:"requireAuthResults,omitempty"`
	// TrustedAuthservID restricts the evidence to headers carrying this
	// authserv-id, the host name of YOUR receiving mail server. Strongly
	// recommended: see docs/inbound-channels.md for the trust model.
	TrustedAuthservID string `json:"trustedAuthservId,omitempty"`
	// AcceptTextAttachments passes small text/plain, csv and markdown
	// attachments to the agent. Default false: only names and sizes.
	AcceptTextAttachments bool `json:"acceptTextAttachments,omitempty"`
}

// RequireAuth is RequireAuthResults with its default.
func (c Config) RequireAuth() bool { return c.RequireAuthResults == nil || *c.RequireAuthResults }

func notHostChar(r rune) bool {
	isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	return !isAlnum && r != '.' && r != '-' && r != '_'
}

// ParseConfig reads and validates the adapter's settings.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, errors.New("adapter settings are required: set adapter.mode to imap or webhook")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("adapter settings: %w", err)
	}
	switch c.Mode {
	case ModeIMAP, ModeWebhook:
	default:
		return c, errors.New("adapter.mode must be imap or webhook")
	}
	if c.Mode == ModeWebhook && (c.Folder != "" || c.ProcessedFolder != "") {
		return c, errors.New("adapter.folder and adapter.processedFolder are for the imap mode")
	}
	for _, f := range []struct{ name, v string }{{"folder", c.Folder}, {"processedFolder", c.ProcessedFolder}} {
		if len(f.v) > 100 || strings.ContainsAny(f.v, "\r\n\x00") {
			return c, fmt.Errorf("adapter.%s is not a folder name", f.name)
		}
	}
	if c.Folder == "" {
		c.Folder = "INBOX"
	}
	if c.ProcessedFolder != "" && c.ProcessedFolder == c.Folder {
		return c, errors.New("adapter.processedFolder must differ from adapter.folder")
	}
	if id := c.TrustedAuthservID; id != "" {
		if len(id) > 253 || strings.IndexFunc(id, notHostChar) >= 0 {
			return c, errors.New("adapter.trustedAuthservId must be a host name")
		}
		c.TrustedAuthservID = strings.ToLower(id)
	}
	return c, nil
}
