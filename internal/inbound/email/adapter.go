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

package email

import (
	"crypto/tls"
	"encoding/json"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/notify"
)

// Adapter is the email adapter. The zero value is ready to use.
type Adapter struct {
	// SMTP sends the replies; nil means a default notify.Sender. Tests set its
	// TLS to trust a throwaway CA.
	SMTP *notify.Sender
	// TLS overrides the TLS settings of the IMAP connection (tests).
	TLS *tls.Config
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	replay inbound.ReplayCache
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Kind is "email".
func (*Adapter) Kind() string { return Kind }

// Capabilities: mail has threads (References), carries attachments (not sent by
// this adapter), no Markdown, and a reply is cut to 20000 characters.
func (*Adapter) Capabilities() inbound.Capabilities {
	return inbound.Capabilities{Threads: true, Attachments: false, Markdown: false, MaxReplyChars: maxReplyChars}
}

// ValidateConfig checks the adapter's settings.
func (*Adapter) ValidateConfig(raw json.RawMessage) error {
	_, err := ParseConfig(raw)
	return err
}

// Polls is true for the imap mode: a webhook connection has no poller.
func (*Adapter) Polls(raw json.RawMessage) bool {
	c, err := ParseConfig(raw)
	return err == nil && c.Mode == ModeIMAP
}

func (c Config) options(cr Credentials) Options {
	return Options{
		Own: cr.From, RequireAuth: c.RequireAuth(), TrustedAuthservID: c.TrustedAuthservID,
		AcceptTextAttachments: c.AcceptTextAttachments, Domain: domainOfAddr(cr.From),
	}
}

var (
	_ inbound.Receiver = (*Adapter)(nil)
	_ inbound.Poller   = (*Adapter)(nil)
	_ inbound.Tester   = (*Adapter)(nil)
)

// Describe lists the settings and the Secret keys, for the API and the UI.
func (*Adapter) Describe() inbound.Descriptor {
	return inbound.Descriptor{
		Title:   "Email",
		Summary: "People write to a mailbox; the agent answers by mail in the same conversation.",
		Fields: []inbound.Field{
			{Key: "mode", Label: "How mail arrives", Type: "enum", Required: true, Options: []string{ModeIMAP, ModeWebhook}},
			{Key: "folder", Label: "Folder", Type: "string", Help: "IMAP mode. Default INBOX."},
			{Key: "processedFolder", Label: "Move handled mail to", Type: "string", Help: "IMAP mode. Empty marks handled mail as read."},
			{Key: "requireAuthResults", Label: "Require proof of the sender's domain", Type: "bool", Default: true},
			{Key: "trustedAuthservId", Label: "Mail server name", Type: "string", Help: "The host name of your own receiving mail server."},
			{Key: "acceptTextAttachments", Label: "Pass small text attachments", Type: "bool"},
		},
		SecretKeys: []inbound.SecretKey{
			{Key: KeyFrom, Required: true, Help: "The address of the mailbox; replies come from it."},
			{Key: KeySMTPHost, Required: true, Help: "Replies are sent over SMTP."},
		},
		Allow: inbound.AllowHelp{Users: "Sender addresses, exact.", Groups: "Whole domains, for example example.com."},
		Setup: []string{
			"Webhook mode: point your mail provider's relay at {{url}}, signed as described in docs/inbound-channels.md.",
		},
	}
}

var _ inbound.Describer = (*Adapter)(nil)
