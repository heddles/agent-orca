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
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/notify"
)

func throwawayCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail.test"}, DNSNames: []string{"mail.test", "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// ── a fake SMTP server (the style of internal/notify's tests) ────────────────

type fakeSMTP struct {
	ln       net.Listener
	cfg      *tls.Config
	starttls bool
	mu       sync.Mutex
	mails    []string
	rcpts    []string
	auth     string
}

func startSMTP(t *testing.T, starttls bool) (*fakeSMTP, *notify.Sender) {
	t.Helper()
	cert, pool := throwawayCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, starttls: starttls, cfg: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f, &notify.Sender{Backoff: time.Millisecond, Timeout: 3 * time.Second, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

func (f *fakeSMTP) port() string {
	_, p, _ := net.SplitHostPort(f.ln.Addr().String())
	return p
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 mail.test ESMTP")
	var cur strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			ext := "250-mail.test\r\n250-AUTH PLAIN\r\n"
			if f.starttls {
				ext += "250-STARTTLS\r\n"
			}
			_, _ = c.Write([]byte(ext + "250 OK\r\n"))
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, f.cfg)
			if tc.Handshake() != nil {
				return
			}
			c = tc
			r = bufio.NewReader(tc)
		case strings.HasPrefix(up, "AUTH PLAIN"):
			f.mu.Lock()
			f.auth = line
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM"):
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO"):
			f.mu.Lock()
			f.rcpts = append(f.rcpts, line)
			f.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			cur.Reset()
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				cur.WriteString(strings.TrimPrefix(l, "."))
			}
			f.mu.Lock()
			f.mails = append(f.mails, cur.String())
			f.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mails...)
}

// ── an in-process IMAP server (go-imap's in-memory server) ───────────────────

const (
	imapUser = "orca@agents.example"
	imapPass = "app-password-1234"
	imapTok  = "bearer-token-value-xyz"
)

type fakeIMAP struct {
	ln   net.Listener
	user *imapmemserver.User
	srv  *imapserver.Server
	mem  *imapmemserver.Server
}

// oauthSession adds XOAUTH2 to the in-memory server's sessions: a valid bearer
// token logs the user in.
type oauthSession struct {
	imapserver.SessionIMAP4rev2
	pass string
}

func (s *oauthSession) AuthenticateMechanisms() []string { return []string{"XOAUTH2"} }

func (s *oauthSession) Authenticate(mech string) (sasl.Server, error) {
	if mech != "XOAUTH2" {
		return nil, imapserver.ErrAuthFailed
	}
	return xoauthServer{s}, nil
}

type xoauthServer struct{ s *oauthSession }

func (x xoauthServer) Next(resp []byte) ([]byte, bool, error) {
	want := "user=" + imapUser + "\x01auth=Bearer " + imapTok + "\x01\x01"
	if string(resp) != want {
		return nil, true, imapserver.ErrAuthFailed
	}
	if err := x.s.Login(imapUser, x.s.pass); err != nil {
		return nil, true, err
	}
	return nil, true, nil
}

// startIMAP serves implicit TLS ("tls"), STARTTLS ("starttls") or clear text ("none").
func startIMAP(t *testing.T, mode string) (*fakeIMAP, *Adapter) {
	t.Helper()
	cert, pool := throwawayCert(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(imapUser, imapPass)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	opts := &imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &oauthSession{SessionIMAP4rev2: mem.NewSession().(imapserver.SessionIMAP4rev2), pass: imapPass}, nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapMove: {}, imap.CapUIDPlus: {}},
		InsecureAuth: mode == "none",
	}
	if mode == "starttls" {
		opts.TLSConfig = tlsCfg
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serve := ln
	if mode == "tls" {
		serve = tls.NewListener(ln, tlsCfg)
	}
	go func() { _ = srv.Serve(serve) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &fakeIMAP{ln: ln, user: user, srv: srv, mem: mem}, &Adapter{TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

func (f *fakeIMAP) port() string {
	_, p, _ := net.SplitHostPort(f.ln.Addr().String())
	return p
}

func (f *fakeIMAP) put(t *testing.T, msg []byte) {
	t.Helper()
	if _, err := f.user.Append("INBOX", newLiteral(msg), &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
}

// counts returns how many messages a mailbox holds and how many are unseen.
func (f *fakeIMAP) counts(t *testing.T, mailbox string) (msgs, unseen int) {
	t.Helper()
	st, err := f.user.Status(mailbox, &imap.StatusOptions{NumMessages: true, NumUnseen: true})
	if err != nil {
		t.Fatalf("status %s: %v", mailbox, err)
	}
	return int(*st.NumMessages), int(*st.NumUnseen)
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

func newLiteral(b []byte) imap.LiteralReader { return literal{bytes.NewReader(b)} }

// ── a connection for the adapter ─────────────────────────────────────────────

func secretFor(f *fakeIMAP, s *fakeSMTP, extra map[string]string) inbound.Secret {
	sec := inbound.Secret{
		KeyFrom: []byte(imapUser), KeyIMAPHost: []byte("localhost"), KeyUsername: []byte(imapUser), KeyPassword: []byte(imapPass),
		KeyAllowPrivate: []byte("true"), KeySMTPHost: []byte("localhost"), KeySigningSecret: []byte("relay-signing-secret"),
	}
	if f != nil {
		sec[KeyIMAPPort] = []byte(f.port())
	}
	if s != nil {
		sec[KeySMTPPort] = []byte(s.port())
	}
	for k, v := range extra {
		if v == "" {
			delete(sec, k)
		} else {
			sec[k] = []byte(v)
		}
	}
	return sec
}

func connFor(sec inbound.Secret, adapterJSON string) inbound.Connection {
	return inbound.Connection{ID: "inb_" + strings.Repeat("e", 32), TenantID: "t-1", Kind: Kind, Namespace: "ns", Agent: "support", Secret: sec, Config: []byte(adapterJSON)}
}
