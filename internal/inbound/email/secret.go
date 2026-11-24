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
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/notify"
)

// Keys of a connection's Secret. An administrator creates the Secret and labels
// it agentorca.io/inbound-secret=true (docs/inbound-channels.md).
const (
	KeyFrom          = "from"          // the connection's address: replies are From it, and it is never answered
	KeyIMAPHost      = "imapHost"      // imap mode
	KeyIMAPPort      = "imapPort"      // default 993 (tls) or 143 (starttls)
	KeyIMAPSecurity  = "imapSecurity"  // tls (default), starttls, or none (needs insecure)
	KeyUsername      = "username"      // imap login
	KeyPassword      = "password"      // imap password or app password
	KeyAccessToken   = "accessToken"   // imap OAuth bearer (XOAUTH2); used instead of the password
	KeySigningSecret = "signingSecret" // webhook mode: the secret the relay signs with
	KeySMTPHost      = "smtpHost"
	KeySMTPPort      = "smtpPort" // default 587
	KeySMTPUsername  = "smtpUsername"
	KeySMTPPassword  = "smtpPassword"
	// KeyInsecure, "true", allows IMAP without TLS and SMTP without STARTTLS.
	KeyInsecure = "insecure"
	// KeyAllowPrivate, "true", allows hosts on loopback, private and link-local
	// addresses (an on-premises mail server). Off by default, as for notification channels.
	KeyAllowPrivate = "allowPrivateAddresses"
)

// Credentials are a connection's Secret, parsed.
type Credentials struct {
	From         string
	Insecure     bool
	AllowPrivate bool

	IMAPHost, IMAPSecurity, Username, Password, AccessToken string
	IMAPPort                                                int

	SigningSecret string

	SMTPHost, SMTPUsername, SMTPPassword string
	SMTPPort                             int
}

// Values are the secret values, for masking.
func (c Credentials) Values() []string {
	var out []string
	for _, v := range []string{c.Password, c.AccessToken, c.SigningSecret, c.SMTPPassword} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ParseCredentials reads the Secret for a mode. Errors name keys, never values.
func ParseCredentials(mode string, s inbound.Secret) (Credentials, error) {
	get := func(k string) string { return strings.TrimSpace(s.Get(k)) }
	c := Credentials{
		Insecure: flag(get(KeyInsecure)), AllowPrivate: flag(get(KeyAllowPrivate)),
		IMAPHost: get(KeyIMAPHost), IMAPSecurity: strings.ToLower(get(KeyIMAPSecurity)), Username: get(KeyUsername),
		Password: s.Get(KeyPassword), AccessToken: get(KeyAccessToken), SigningSecret: get(KeySigningSecret),
		SMTPHost: get(KeySMTPHost), SMTPUsername: get(KeySMTPUsername), SMTPPassword: s.Get(KeySMTPPassword),
	}
	from, err := mail.ParseAddress(get(KeyFrom))
	if err != nil || from.Address == "" || strings.ContainsAny(from.Address, "\r\n") {
		return c, fmt.Errorf("the Secret needs a %q key with the address of the mailbox", KeyFrom)
	}
	c.From = strings.ToLower(from.Address)
	port := func(key string, def int) (int, error) {
		v := get(key)
		if v == "" {
			return def, nil
		}
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return 0, fmt.Errorf("the %q key is not a port number", key)
		}
		return p, nil
	}
	if c.SMTPHost == "" {
		return c, fmt.Errorf("the Secret has no %q key: replies are sent over SMTP", KeySMTPHost)
	}
	if c.SMTPPort, err = port(KeySMTPPort, 587); err != nil {
		return c, err
	}
	if (c.SMTPUsername == "") != (c.SMTPPassword == "") {
		return c, fmt.Errorf("set both %q and %q, or neither", KeySMTPUsername, KeySMTPPassword)
	}
	switch mode {
	case ModeWebhook:
		if c.SigningSecret == "" {
			return c, fmt.Errorf("the Secret has no %q key", KeySigningSecret)
		}
	case ModeIMAP:
		if c.IMAPHost == "" || c.Username == "" {
			return c, fmt.Errorf("the Secret needs %q and %q keys", KeyIMAPHost, KeyUsername)
		}
		if (c.Password == "") == (c.AccessToken == "") {
			return c, fmt.Errorf("the Secret needs exactly one of %q and %q", KeyPassword, KeyAccessToken)
		}
		switch c.IMAPSecurity {
		case "":
			c.IMAPSecurity = securityTLS
		case securityTLS, securityStartTLS:
		case "none":
			if !c.Insecure {
				return c, fmt.Errorf("%s=none needs %s=true: credentials would cross the network in clear", KeyIMAPSecurity, KeyInsecure)
			}
		default:
			return c, fmt.Errorf("the %q key must be tls, starttls or none", KeyIMAPSecurity)
		}
		def := 993
		if c.IMAPSecurity != securityTLS {
			def = 143
		}
		if c.IMAPPort, err = port(KeyIMAPPort, def); err != nil {
			return c, err
		}
	}
	return c, nil
}

func flag(v string) bool { return strings.EqualFold(v, "true") }

// smtpChannel is the notify channel a reply to one recipient goes through.
func (c Credentials) smtpChannel(to string) *notify.Channel {
	return &notify.Channel{
		Name: "email-reply", Type: notify.TypeSMTP, Insecure: c.Insecure, AllowPrivate: c.AllowPrivate,
		Host: c.SMTPHost, Port: c.SMTPPort, Username: c.SMTPUsername, Password: c.SMTPPassword, From: c.From, To: []string{to},
	}
}

func (c Credentials) tlsConfig(base *tls.Config, host string) *tls.Config {
	cfg := base.Clone()
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	cfg.ServerName = host
	return cfg
}

// Values of the imapSecurity key.
const (
	securityTLS      = "tls"
	securityStartTLS = "starttls"
)

const dialTimeout = 20 * time.Second
