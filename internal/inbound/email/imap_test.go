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
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
)

type recorder struct {
	mu   sync.Mutex
	msgs []inbound.Message
	errs []error
	fail bool
}

func (r *recorder) Handle(_ context.Context, _ inbound.Connection, m inbound.Message, recvErr error) (inbound.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return inbound.OutcomeFailed, errors.New("the database is down")
	}
	r.msgs = append(r.msgs, m)
	r.errs = append(r.errs, recvErr)
	return inbound.OutcomeStarted, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

const imapCfg = `{"mode":"imap","trustedAuthservId":"mx.agents.example"}`

func goodMail(id string) []byte {
	return raw([]string{"From: " + goodFrom, "To: " + ownAddr, "Subject: Question " + id, "Message-ID: <" + id + "@example.com>", "Authentication-Results: " + authPass}, "Body of "+id)
}

func poll(t *testing.T, a *Adapter, c inbound.Connection, h inbound.Handler) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return a.PollOnce(ctx, c, h)
}

func TestIMAPPollMarksMessagesOnlyAfterTheyAreRecorded(t *testing.T) {
	for _, mode := range []string{"tls", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			srv, a := startIMAP(t, mode)
			srv.put(t, goodMail("one"))
			srv.put(t, goodMail("two"))
			c := connFor(secretFor(srv, nil, map[string]string{KeyIMAPSecurity: mode}), imapCfg)

			// The store is down: nothing is marked, so the next poll sees both again.
			down := &recorder{fail: true}
			if err := poll(t, a, c, down); err == nil || strings.Contains(err.Error(), "database") {
				t.Errorf("poll with a failing store: %v", err)
			}
			if msgs, unseen := srv.counts(t, "INBOX"); msgs != 2 || unseen != 2 {
				t.Fatalf("after a failed record: %d messages, %d unseen; want nothing marked", msgs, unseen)
			}

			rec := &recorder{}
			if err := poll(t, a, c, rec); err != nil {
				t.Fatal(err)
			}
			if rec.count() != 2 || rec.msgs[0].Title != "Question one" || rec.msgs[1].Title != "Question two" || rec.msgs[0].Text != "Body of one" {
				t.Fatalf("messages = %+v", rec.msgs)
			}
			if msgs, unseen := srv.counts(t, "INBOX"); msgs != 2 || unseen != 0 {
				t.Errorf("after recording: %d messages, %d unseen; want all marked seen", msgs, unseen)
			}
			// A third poll finds nothing new.
			again := &recorder{}
			if err := poll(t, a, c, again); err != nil || again.count() != 0 {
				t.Errorf("third poll: %v, %d messages", err, again.count())
			}
		})
	}
}

func TestIMAPPollRecordsRefusedMessagesToo(t *testing.T) {
	srv, a := startIMAP(t, "tls")
	srv.put(t, raw([]string{"From: " + goodFrom, "Subject: spoof", "Message-ID: <spoof@example.com>"}, "no auth results here"))
	srv.put(t, raw([]string{"From: " + goodFrom, "Subject: auto", "Message-ID: <auto@example.com>", "Auto-Submitted: auto-replied", "Authentication-Results: " + authPass}, "out of office"))
	srv.put(t, []byte("this is not a mail message at all"))
	c := connFor(secretFor(srv, nil, nil), imapCfg)
	rec := &recorder{}
	if err := poll(t, a, c, rec); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 3 {
		t.Fatalf("recorded %d, want 3", rec.count())
	}
	var rej *inbound.Rejection
	for i, want := range []string{"no_auth_results", "auto_submitted"} {
		if !errors.As(rec.errs[i], &rej) || rej.Reason != want {
			t.Errorf("message %d: %v, want %s", i, rec.errs[i], want)
		}
	}
	if rec.msgs[0].Text != "" {
		t.Errorf("a refused message carried its text: %q", rec.msgs[0].Text)
	}
	if msgs, unseen := srv.counts(t, "INBOX"); msgs != 3 || unseen != 0 {
		t.Errorf("%d messages, %d unseen", msgs, unseen)
	}
}

func TestIMAPOversizeMessageIsNotDownloaded(t *testing.T) {
	srv, a := startIMAP(t, "tls")
	srv.put(t, raw([]string{"From: " + goodFrom, "Subject: huge", "Message-ID: <huge@example.com>", "Authentication-Results: " + authPass}, strings.Repeat("x", MaxMessageBytes+10)))
	c := connFor(secretFor(srv, nil, nil), imapCfg)
	rec := &recorder{}
	if err := poll(t, a, c, rec); err != nil {
		t.Fatal(err)
	}
	var rej *inbound.Rejection
	if rec.count() != 1 || !errors.As(rec.errs[0], &rej) || rej.Reason != "too_large" || rec.msgs[0].Size <= MaxMessageBytes || rec.msgs[0].Text != "" {
		t.Errorf("recorded %+v / %v", rec.msgs, rec.errs)
	}
	if !strings.HasPrefix(rec.msgs[0].MessageID, "imap:") {
		t.Errorf("id = %q", rec.msgs[0].MessageID)
	}
}

func TestIMAPMovesToTheProcessedFolder(t *testing.T) {
	srv, a := startIMAP(t, "tls")
	srv.put(t, goodMail("one"))
	c := connFor(secretFor(srv, nil, nil), `{"mode":"imap","trustedAuthservId":"mx.agents.example","processedFolder":"Processed"}`)
	rec := &recorder{}
	if err := poll(t, a, c, rec); err != nil || rec.count() != 1 {
		t.Fatalf("poll: %v, %d", err, rec.count())
	}
	if msgs, _ := srv.counts(t, "INBOX"); msgs != 0 {
		t.Errorf("INBOX still holds %d", msgs)
	}
	if msgs, _ := srv.counts(t, "Processed"); msgs != 1 {
		t.Errorf("Processed holds %d", msgs)
	}
}

func TestIMAPLoginMethods(t *testing.T) {
	srv, a := startIMAP(t, "tls")
	srv.put(t, goodMail("one"))

	// A wrong password is refused, with a message that does not echo it.
	bad := connFor(secretFor(srv, nil, map[string]string{KeyPassword: "wrong-password-value"}), imapCfg)
	err := poll(t, a, bad, &recorder{})
	if err == nil || strings.Contains(err.Error(), "wrong-password-value") || !strings.Contains(err.Error(), "refused the login") {
		t.Errorf("wrong password: %v", err)
	}

	// An OAuth bearer token (XOAUTH2) logs in without a password.
	tok := connFor(secretFor(srv, nil, map[string]string{KeyPassword: "", KeyAccessToken: imapTok}), imapCfg)
	rec := &recorder{}
	if err := poll(t, a, tok, rec); err != nil || rec.count() != 1 {
		t.Errorf("xoauth2: %v, %d messages", err, rec.count())
	}
	wrong := connFor(secretFor(srv, nil, map[string]string{KeyPassword: "", KeyAccessToken: "an-expired-token"}), imapCfg)
	if err := poll(t, a, wrong, &recorder{}); err == nil || strings.Contains(err.Error(), "an-expired-token") {
		t.Errorf("bad token: %v", err)
	}
}

func TestIMAPRequiresTLSUnlessInsecureIsSet(t *testing.T) {
	srv, a := startIMAP(t, "none")
	srv.put(t, goodMail("one"))
	// Plain text needs the explicit flag, and is refused before anything is sent.
	if _, err := ParseCredentials(ModeIMAP, secretFor(srv, nil, map[string]string{KeyIMAPSecurity: "none"})); err == nil {
		t.Error("security none without insecure was accepted")
	}
	// Implicit TLS against a clear-text server fails the handshake: no password is ever sent.
	c := connFor(secretFor(srv, nil, nil), imapCfg)
	if err := poll(t, a, c, &recorder{}); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("tls against a clear server: %v", err)
	}
	// STARTTLS against a server without it fails too.
	c = connFor(secretFor(srv, nil, map[string]string{KeyIMAPSecurity: "starttls"}), imapCfg)
	if err := poll(t, a, c, &recorder{}); err == nil {
		t.Error("starttls against a server without it succeeded")
	}
	// With the explicit flag it works (a lab server).
	c = connFor(secretFor(srv, nil, map[string]string{KeyIMAPSecurity: "none", KeyInsecure: "true"}), imapCfg)
	rec := &recorder{}
	if err := poll(t, a, c, rec); err != nil || rec.count() != 1 {
		t.Errorf("insecure: %v, %d", err, rec.count())
	}
}

func TestIMAPRefusesAnUntrustedCertificate(t *testing.T) {
	srv, _ := startIMAP(t, "tls")
	c := connFor(secretFor(srv, nil, nil), imapCfg)
	// No trusted root for the throwaway certificate: the system pool is used.
	if err := poll(t, &Adapter{}, c, &recorder{}); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("untrusted certificate: %v", err)
	}
}

func TestIMAPDoesNotReachInternalAddressesUnlessAllowed(t *testing.T) {
	srv, a := startIMAP(t, "tls")
	c := connFor(secretFor(srv, nil, map[string]string{KeyAllowPrivate: ""}), imapCfg)
	err := poll(t, a, c, &recorder{})
	if err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Errorf("loopback without allowPrivateAddresses: %v", err)
	}
}

func TestIMAPTestReportsFolderCountsAndSendsNothing(t *testing.T) {
	srv, a := startIMAP(t, "tls")
	srv.put(t, goodMail("one"))
	srv.put(t, goodMail("two"))
	c := connFor(secretFor(srv, nil, nil), imapCfg)
	rep := a.Test(context.Background(), c)
	if !rep.OK || rep.Details["folder"] != "INBOX: 2 messages, 2 unseen" || rep.Details["requireAuthResults"] != "true" {
		t.Errorf("report = %+v", rep)
	}
	if msgs, unseen := srv.counts(t, "INBOX"); msgs != 2 || unseen != 2 {
		t.Errorf("the test changed the mailbox: %d, %d", msgs, unseen)
	}
	// A missing folder and a wrong password are reported without secrets.
	c = connFor(secretFor(srv, nil, nil), `{"mode":"imap","folder":"Nope"}`)
	if rep := a.Test(context.Background(), c); rep.OK || !strings.Contains(rep.Error, "folder") {
		t.Errorf("missing folder = %+v", rep)
	}
	c = connFor(secretFor(srv, nil, map[string]string{KeyPassword: "wrong-password-value"}), imapCfg)
	if rep := a.Test(context.Background(), c); rep.OK || strings.Contains(rep.Error, "wrong-password-value") {
		t.Errorf("wrong password = %+v", rep)
	}
	// Without trustedAuthservId the report warns.
	c = connFor(secretFor(srv, nil, nil), `{"mode":"imap"}`)
	if rep := a.Test(context.Background(), c); !rep.OK || rep.Details["warning"] == "" {
		t.Errorf("no warning = %+v", rep)
	}
}

func TestSecretAndConfigValidation(t *testing.T) {
	good := secretFor(nil, nil, nil)
	for name, tc := range map[string]struct {
		mode  string
		extra map[string]string
		want  string
	}{
		"no from":         {ModeIMAP, map[string]string{KeyFrom: ""}, `"from"`},
		"no smtp host":    {ModeIMAP, map[string]string{KeySMTPHost: ""}, "smtpHost"},
		"no imap host":    {ModeIMAP, map[string]string{KeyIMAPHost: ""}, "imapHost"},
		"both credential": {ModeIMAP, map[string]string{KeyAccessToken: "tok"}, "exactly one"},
		"no credential":   {ModeIMAP, map[string]string{KeyPassword: ""}, "exactly one"},
		"bad port":        {ModeIMAP, map[string]string{KeyIMAPPort: "99999"}, "port"},
		"bad security":    {ModeIMAP, map[string]string{KeyIMAPSecurity: "ssl3"}, "tls, starttls or none"},
		"half smtp login": {ModeIMAP, map[string]string{KeySMTPUsername: "u"}, "both"},
		"webhook secret":  {ModeWebhook, map[string]string{KeySigningSecret: ""}, "signingSecret"},
		"injected from":   {ModeIMAP, map[string]string{KeyFrom: "a@example.com\r\nBcc: x@example.com"}, `"from"`},
	} {
		s := inbound.Secret{}
		maps.Copy(s, good)
		for k, v := range tc.extra {
			if v == "" {
				delete(s, k)
			} else {
				s[k] = []byte(v)
			}
		}
		if _, err := ParseCredentials(tc.mode, s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if _, err := ParseCredentials(ModeIMAP, good); err != nil {
		t.Errorf("good secret: %v", err)
	}
	for name, cfg := range map[string]string{
		"empty": ``, "no mode": `{}`, "bad mode": `{"mode":"pop3"}`, "unknown field": `{"mode":"imap","password":"x"}`,
		"webhook with folder": `{"mode":"webhook","folder":"X"}`, "same folder": `{"mode":"imap","folder":"A","processedFolder":"A"}`,
		"bad id": `{"mode":"imap","trustedAuthservId":"mx example"}`, "newline folder": `{"mode":"imap","folder":"A\nB"}`,
	} {
		if _, err := ParseConfig([]byte(cfg)); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
	c, err := ParseConfig([]byte(`{"mode":"imap"}`))
	if err != nil || !c.RequireAuth() || c.Folder != "INBOX" {
		t.Errorf("defaults = %+v, %v", c, err)
	}
	a := &Adapter{}
	if !a.Polls([]byte(`{"mode":"imap"}`)) || a.Polls([]byte(`{"mode":"webhook"}`)) || a.Polls(nil) {
		t.Error("Polls is wrong")
	}
}
