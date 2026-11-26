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

package inbound

import (
	"context"
	"crypto/rand"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// PairingTTL is how long a pairing code works, and how long the sender of one
// that nobody approves is remembered.
const PairingTTL = 10 * time.Minute

const (
	// No 0/O, 1/I/L: the code is read off one screen and typed into another.
	pairingAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	pairingLength   = 6
)

var pairingRe = regexp.MustCompile(`(?i)^pair\s+([2-9A-HJKMNP-Z]{6})$`)

// NewPairingCode makes a random code.
func NewPairingCode() (string, error) {
	var b strings.Builder
	for range pairingLength {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pairingAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(pairingAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// PairingCode returns the code of a message that is only "pair <code>", else "".
func PairingCode(text string) string {
	m := pairingRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// PairingHash is what is stored in place of a code.
func PairingHash(connectionID, code string) string {
	return postgresql.HashKey("pairing", connectionID, strings.ToUpper(code))
}

// pairingMessage handles a message that is a pairing code the connection waits
// for: it records the sender and tells them to finish in agent-orca. It reports
// false for any other message.
func (p *Pipeline) pairingMessage(ctx context.Context, c Connection, m Message, caps Capabilities) (bool, error) {
	code := PairingCode(m.Text)
	if code == "" || len(caps.Modes) == 0 {
		return false, nil
	}
	matched, err := p.pair(ctx, c, m, code)
	if matched {
		p.notify(ctx, c, m, "Got your code. Approve it in agent-orca to finish.")
	}
	return matched, err
}

// pair records the sender of a pairing code, if the code is the connection's
// current one. Guessing is held back by the connection's rate limit.
func (p *Pipeline) pair(ctx context.Context, c Connection, m Message, code string) (bool, error) {
	if p.Limiter != nil {
		if _, ok := p.Limiter.Allow("inbound-pair:"+c.ID, c.Settings.RatePerMin); !ok {
			return false, nil
		}
	}
	return p.Store.ReceiveInboundPairing(postgresql.WithTenant(ctx, c.TenantID), postgresql.InboundPairing{
		ConnectionID: c.ID, CodeHash: PairingHash(c.ID, code),
		SenderID: m.Sender.ID, SenderName: oneLine(m.Sender.Display, maxLabelChars), ChannelID: m.Channel,
		Mode: string(m.Mode), ReplyTarget: m.Reply,
	})
}
