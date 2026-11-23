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

// Package notify sends the result notifications of scheduled runs
// (docs/schedules.md): a webhook (signed like the trigger webhooks), a Slack
// incoming webhook, or an email. A channel's destination lives in a Secret; the
// URL of a Slack webhook is itself a credential, so nothing in this package puts
// it, or any other Secret value, in an error, a log line or a message.
package notify

import (
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
)

// Channel types, as in TenantConfig spec.notifications.channels[].type.
const (
	TypeWebhook = "webhook"
	TypeSlack   = "slack"
	TypeSMTP    = "smtp"
)

// Keys of a channel's Secret.
const (
	KeyURL        = "url"
	KeySigningKey = "signingKey"
	KeyHost       = "host"
	KeyPort       = "port"
	KeyUsername   = "username"
	KeyPassword   = "password"
	KeyFrom       = "from"
	KeyTo         = "to"
)

// Channel is a notification destination with its Secret already read.
type Channel struct {
	Name string
	Type string
	// Insecure allows http:// and SMTP without STARTTLS.
	Insecure bool
	// AllowPrivate allows loopback, link-local, private and other internal
	// destinations.
	AllowPrivate bool

	URL        string // webhook, slack
	SigningKey string // webhook

	Host     string // smtp
	Port     int
	Username string
	Password string
	From     string
	To       []string
}

// Secrets are the Secret values of the channel, for masking.
func (c *Channel) Secrets() []string {
	var out []string
	for _, v := range []string{c.URL, c.SigningKey, c.Password, c.Username, c.Host} {
		if v != "" {
			out = append(out, v)
		}
	}
	if u, err := url.Parse(c.URL); err == nil && u.Path != "" && u.Path != "/" {
		out = append(out, strings.Trim(u.Path, "/")) // the token of a Slack URL
	}
	if u, err := url.Parse(c.URL); err == nil && u.Host != "" {
		out = append(out, u.Host) // appears in dial and DNS errors
	}
	return out
}

// FromSecret builds a channel from its Secret's data. Errors name the keys that
// are wrong, never their values.
func FromSecret(name, typ string, insecure, allowPrivate bool, data map[string][]byte) (*Channel, error) {
	get := func(k string) string { return strings.TrimSpace(string(data[k])) }
	c := &Channel{Name: name, Type: typ, Insecure: insecure, AllowPrivate: allowPrivate}
	switch typ {
	case TypeWebhook, TypeSlack:
		c.URL = get(KeyURL)
		if c.URL == "" {
			return nil, fmt.Errorf("the Secret has no %q key", KeyURL)
		}
		u, err := url.Parse(c.URL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("the %q key is not a URL", KeyURL)
		}
		switch {
		case u.Scheme == "https":
		case u.Scheme == "http" && insecure:
		case u.Scheme == "http":
			return nil, fmt.Errorf("the %q key is an http URL: https is required unless the channel sets insecure", KeyURL)
		default:
			return nil, fmt.Errorf("the %q key must be an https URL", KeyURL)
		}
		if typ == TypeWebhook {
			if c.SigningKey = get(KeySigningKey); c.SigningKey == "" {
				return nil, fmt.Errorf("a webhook channel's Secret needs a %q key to sign the body with", KeySigningKey)
			}
		}
	case TypeSMTP:
		c.Host, c.Username, c.Password = get(KeyHost), get(KeyUsername), string(data[KeyPassword])
		if c.Host == "" {
			return nil, fmt.Errorf("the Secret has no %q key", KeyHost)
		}
		port := get(KeyPort)
		if port == "" {
			port = "587"
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("the %q key is not a port number", KeyPort)
		}
		c.Port = p
		from, err := mail.ParseAddress(get(KeyFrom))
		if err != nil {
			return nil, fmt.Errorf("the %q key is not an email address", KeyFrom)
		}
		c.From = from.Address
		for a := range strings.SplitSeq(get(KeyTo), ",") {
			if strings.TrimSpace(a) == "" {
				continue
			}
			to, err := mail.ParseAddress(strings.TrimSpace(a))
			if err != nil {
				return nil, fmt.Errorf("the %q key is not a list of email addresses", KeyTo)
			}
			c.To = append(c.To, to.Address)
		}
		if len(c.To) == 0 {
			return nil, fmt.Errorf("the Secret has no %q key", KeyTo)
		}
		if (c.Username == "") != (c.Password == "") {
			return nil, fmt.Errorf("set both %q and %q, or neither", KeyUsername, KeyPassword)
		}
	default:
		return nil, fmt.Errorf("unknown channel type %q", typ)
	}
	return c, nil
}
