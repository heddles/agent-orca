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
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/notify"
)

// maxPerPoll bounds the messages handled in one poll; the rest wait for the next.
const maxPerPoll = 20

// xoauth2 is the SASL XOAUTH2 mechanism (Exchange Online, Gmail): the bearer
// token stands in for the password.
type xoauth2 struct{ user, token string }

func (x xoauth2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + x.user + "\x01auth=Bearer " + x.token + "\x01\x01"), nil
}

// Next answers the server's error challenge with an empty response, as the
// mechanism requires; the command then fails with the server's reason.
func (x xoauth2) Next([]byte) ([]byte, error) { return []byte{}, nil }

// session is an authenticated IMAP connection.
type session struct {
	c    *imapclient.Client
	stop func() bool
}

func (s *session) close() {
	s.stop()
	_ = s.c.Close()
}

// connect opens the connection, over TLS (the default), STARTTLS, or, only when
// the Secret says insecure, in clear, and logs in. The error text is generic: what
// the server said may name accounts.
func (a *Adapter) connect(ctx context.Context, cr Credentials) (*session, error) {
	addr := net.JoinHostPort(cr.IMAPHost, strconv.Itoa(cr.IMAPPort))
	conn, err := notify.GuardedDialer(cr.AllowPrivate, dialTimeout).DialContext(ctx, "tcp", addr)
	if err != nil {
		if errors.Is(err, notify.ErrBlockedAddress) {
			return nil, notify.ErrBlockedAddress
		}
		return nil, errors.New("connecting to the mail server failed")
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	tlsCfg := cr.tlsConfig(a.TLS, cr.IMAPHost)
	var c *imapclient.Client
	switch cr.IMAPSecurity {
	case securityTLS:
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			stop()
			_ = conn.Close()
			return nil, errors.New("the TLS handshake with the mail server failed")
		}
		c = imapclient.New(tc, nil)
	case securityStartTLS:
		c, err = imapclient.NewStartTLS(conn, &imapclient.Options{TLSConfig: tlsCfg})
		if err != nil {
			stop()
			_ = conn.Close()
			return nil, errors.New("STARTTLS with the mail server failed")
		}
	default: // none: the Secret asked for it with insecure=true
		c = imapclient.New(conn, nil)
	}
	s := &session{c: c, stop: stop}
	if cr.AccessToken != "" {
		err = c.Authenticate(xoauth2{cr.Username, cr.AccessToken})
	} else {
		err = c.Login(cr.Username, cr.Password).Wait()
	}
	if err != nil {
		s.close()
		return nil, errors.New("the mail server refused the login")
	}
	return s, nil
}

// PollOnce fetches the unseen messages of the folder, hands each to h, and marks
// a message processed (\Seen, or moved to the processed folder) only after h
// returned nil: after the message is safely recorded. A message that is
// refused by policy is recorded too, and so is marked.
func (a *Adapter) PollOnce(ctx context.Context, c inbound.Connection, h inbound.Handler) error {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return err
	}
	cr, err := ParseCredentials(cfg.Mode, c.Secret)
	if err != nil {
		return err
	}
	if cfg.Mode != ModeIMAP {
		return nil
	}
	s, err := a.connect(ctx, cr)
	if err != nil {
		return err
	}
	defer s.close()
	cl := s.c

	sel, err := cl.Select(cfg.Folder, &imap.SelectOptions{}).Wait()
	if err != nil {
		return errors.New("the folder could not be opened")
	}
	if cfg.ProcessedFolder != "" {
		_ = cl.Create(cfg.ProcessedFolder, nil).Wait() // exists already is fine
	}
	found, err := cl.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		return errors.New("searching the folder failed")
	}
	uids := found.AllUIDs()
	slices.Sort(uids)
	if len(uids) > maxPerPoll {
		uids = uids[:maxPerPoll]
	}
	if len(uids) == 0 {
		_ = cl.Logout().Wait()
		return nil
	}
	sizes, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return errors.New("reading the message sizes failed")
	}
	size := map[imap.UID]int64{}
	for _, m := range sizes {
		size[m.UID] = m.RFC822Size
	}
	opts := cfg.options(cr)
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return err
		}
		synthetic := fmt.Sprintf("imap:%d:%d", sel.UIDValidity, uid)
		var m inbound.Message
		var rerr error
		if size[uid] > MaxMessageBytes {
			// Not downloaded at all: recorded as refused by its id.
			m, rerr = inbound.Message{MessageID: synthetic, Size: int(size[uid])}, &inbound.Rejection{Reason: "too_large"}
		} else {
			raw, err := fetchRaw(cl, uid)
			if err != nil {
				return err
			}
			d, derr := decodeRFC822(raw)
			if derr != nil {
				m, rerr = inbound.Message{MessageID: synthetic, Size: len(raw)}, &inbound.Rejection{Reason: "unreadable"}
			} else {
				m, rerr = d.evaluate(opts)
			}
		}
		if _, err := h.Handle(ctx, c, m, rerr); err != nil {
			// Not recorded: leave it unmarked so the next poll tries again.
			_ = err
			return errors.New("recording a message failed")
		}
		if err := mark(cl, uid, cfg); err != nil {
			return err
		}
	}
	_ = cl.Logout().Wait()
	return nil
}

func fetchRaw(cl *imapclient.Client, uid imap.UID) ([]byte, error) {
	msgs, err := cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil || len(msgs) != 1 || len(msgs[0].BodySection) == 0 {
		return nil, errors.New("downloading a message failed")
	}
	return msgs[0].BodySection[0].Bytes, nil
}

func mark(cl *imapclient.Client, uid imap.UID, cfg Config) error {
	set := imap.UIDSetNum(uid)
	if cfg.ProcessedFolder != "" {
		if _, err := cl.Move(set, cfg.ProcessedFolder).Wait(); err != nil {
			return errors.New("moving a message to the processed folder failed")
		}
		return nil
	}
	if err := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
		return errors.New("marking a message as read failed")
	}
	return nil
}

// Test connects and logs in and reports the message counts of the folders. It
// reads no message, changes no flag and sends nothing.
func (a *Adapter) Test(ctx context.Context, c inbound.Connection) inbound.TestReport {
	cfg, err := ParseConfig(c.Config)
	if err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	cr, err := ParseCredentials(cfg.Mode, c.Secret)
	if err != nil {
		return inbound.TestReport{Error: err.Error()}
	}
	details := map[string]string{"mode": cfg.Mode, "from": cr.From, "smtp": "configured", "requireAuthResults": strconv.FormatBool(cfg.RequireAuth())}
	if cfg.RequireAuth() && cfg.TrustedAuthservID == "" {
		details["warning"] = "trustedAuthservId is not set: the topmost Authentication-Results header is trusted"
	}
	if cfg.Mode == ModeWebhook {
		details["signing"] = "configured"
		return inbound.TestReport{OK: true, Details: details}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := a.connect(ctx, cr)
	if err != nil {
		return inbound.TestReport{Details: details, Error: err.Error()}
	}
	defer s.close()
	for _, f := range []struct{ key, name string }{{"folder", cfg.Folder}, {"processedFolder", cfg.ProcessedFolder}} {
		if f.name == "" {
			continue
		}
		st, err := s.c.Status(f.name, &imap.StatusOptions{NumMessages: true, NumUnseen: true}).Wait()
		if err != nil {
			details[f.key] = f.name + ": not found"
			if f.key == "folder" {
				return inbound.TestReport{Details: details, Error: "the folder does not exist"}
			}
			continue
		}
		var msgs, unseen uint32
		if st.NumMessages != nil {
			msgs = *st.NumMessages
		}
		if st.NumUnseen != nil {
			unseen = *st.NumUnseen
		}
		details[f.key] = fmt.Sprintf("%s: %d messages, %d unseen", f.name, msgs, unseen)
	}
	_ = s.c.Logout().Wait()
	return inbound.TestReport{OK: true, Details: details}
}
