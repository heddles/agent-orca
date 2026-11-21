package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/redact"
)

func fast() *Sender { return &Sender{Backoff: time.Millisecond, Timeout: 2 * time.Second} }

func sampleMsg() Message {
	return Message{
		Schedule: "digest", Namespace: "team-a", Agent: "digest-agent", Outcome: "Failed", Run: "digest-1763280000",
		Duration: 95 * time.Second, CostUSD: "0.42", Link: "https://ui.example.com/runs/live/team-a/digest-1763280000",
		Reason: "tool timed out", Time: time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC),
	}
}

func webhookChannel(url string) *Channel {
	return &Channel{Name: "ops", Type: TypeWebhook, URL: url, SigningKey: "super-secret-signing-key", Insecure: true, AllowPrivate: true}
}

func TestWebhookIsSignedLikeTheTriggerWebhooks(t *testing.T) {
	var gotBody []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	}))
	defer srv.Close()
	if err := fast().Send(context.Background(), webhookChannel(srv.URL), sampleMsg()); err != nil {
		t.Fatal(err)
	}
	ts := hdr.Get(TimestampHeader)
	if ts == "" || hdr.Get(SignatureHeader) != Sign("super-secret-signing-key", ts, gotBody) {
		t.Fatalf("signature %q over %q does not verify", hdr.Get(SignatureHeader), ts+"."+string(gotBody))
	}
	if !strings.HasPrefix(hdr.Get(SignatureHeader), "sha256=") {
		t.Errorf("signature = %q, want the sha256= form of the trigger webhooks", hdr.Get(SignatureHeader))
	}
	var p map[string]any
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"schedule": "digest", "agent": "digest-agent", "outcome": "Failed", "run": "digest-1763280000",
		"costUSD": "0.42", "durationSeconds": float64(95), "link": sampleMsg().Link,
	} {
		if p[k] != want {
			t.Errorf("%s = %v, want %v", k, p[k], want)
		}
	}
	if _, ok := p["output"]; ok {
		t.Error("output is in the body although it was not asked for")
	}
}

func TestSlackPostsText(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get(SignatureHeader) != "" {
			t.Error("a Slack incoming webhook is not signed")
		}
	}))
	defer srv.Close()
	m := sampleMsg()
	m.Output = "see <@everyone> & ```code```"
	ch := &Channel{Name: "s", Type: TypeSlack, URL: srv.URL + "/services/T0/B0/xyzxyzxyz", Insecure: true, AllowPrivate: true}
	if err := fast().Send(context.Background(), ch, m); err != nil {
		t.Fatal(err)
	}
	text := body["text"]
	for _, want := range []string{"Schedule digest: Failed", "digest-1763280000", "$0.42", "1m35s", "<" + m.Link + "|Open the run>", "&lt;@everyone&gt; &amp;"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<@everyone>") || strings.Count(text, "```") != 2 {
		t.Errorf("output must not make a mention or close the code block:\n%s", text)
	}
}

func TestRetriesTransientFailuresThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	if err := fast().Send(context.Background(), webhookChannel(srv.URL), sampleMsg()); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Errorf("attempts = %d, want 3", n.Load())
	}
}

func TestRetryIsBoundedAndClientErrorsAreNotRetried(t *testing.T) {
	var n atomic.Int32
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	err := fast().Send(context.Background(), webhookChannel(srv.URL), sampleMsg())
	if err == nil || n.Load() != 3 {
		t.Fatalf("err = %v after %d attempts, want a failure after exactly 3", err, n.Load())
	}
	n.Store(0)
	status = http.StatusForbidden
	if err := fast().Send(context.Background(), webhookChannel(srv.URL), sampleMsg()); err == nil || n.Load() != 1 {
		t.Fatalf("err = %v after %d attempts: a 403 is final", err, n.Load())
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	if err := fast().Send(context.Background(), webhookChannel(srv.URL), sampleMsg()); err == nil {
		t.Error("a redirect answer is not a delivery")
	}
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestSSRFGuard(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "fd00:ec2::254", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "0.0.0.0", "::ffff:127.0.0.1", "fe80::1", "224.0.0.1"} {
		if !blocked(netip.MustParseAddr(ip)) {
			t.Errorf("%s is not blocked", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if blocked(netip.MustParseAddr(ip)) {
			t.Errorf("%s is blocked", ip)
		}
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	// Loopback by IP, by name (resolved at connect time), and the metadata address.
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), "http://169.254.169.254/latest/meta-data/"} {
		ch := &Channel{Name: "x", Type: TypeWebhook, URL: u, SigningKey: "k", Insecure: true}
		err := fast().Send(context.Background(), ch, sampleMsg())
		if !errors.Is(errors.Unwrap(err), ErrBlockedAddress) && (err == nil || !strings.Contains(err.Error(), ErrBlockedAddress.Error())) {
			t.Errorf("%s: err = %v, want the address refused", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Error("a request reached a blocked address")
	}
	// http needs the explicit flag, even for a public host.
	ch := &Channel{Name: "x", Type: TypeSlack, URL: "http://hooks.example.com/x"}
	if err := fast().Send(context.Background(), ch, sampleMsg()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http without insecure: %v", err)
	}
	// An explicit flag allows the in-cluster receiver.
	if err := fast().Send(context.Background(), webhookChannel(srv.URL), sampleMsg()); err != nil || hits.Load() != 1 {
		t.Errorf("allowPrivate: err = %v, hits = %d", err, hits.Load())
	}
}

func TestErrorsNeverCarryTheSecret(t *testing.T) {
	const token = "T0AAAA/B0BBBB/s3cr3tt0k3nvalue"
	// A refused connection: the URL (a credential) is in the transport's error.
	ch := &Channel{Name: "s", Type: TypeSlack, URL: "http://127.0.0.1:1/services/" + token, Insecure: true, AllowPrivate: true}
	err := fast().Send(context.Background(), ch, sampleMsg())
	if err == nil {
		t.Fatal("want a failure")
	}
	for _, leak := range []string{token, "s3cr3tt0k3nvalue", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err, leak)
		}
	}
}

func TestOutputIsRedactedAndTruncated(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewDecoder(r.Body).Decode(&body) }))
	defer srv.Close()
	ch := webhookChannel(srv.URL)
	m := sampleMsg()
	m.Output = "the key is super-secret-signing-key and the url is " + srv.URL + " " + strings.Repeat("x", 5000)
	m.Reason = "failed with super-secret-signing-key"
	if err := fast().Send(context.Background(), ch, m); err != nil {
		t.Fatal(err)
	}
	out := body["output"].(string)
	if strings.Contains(out, "super-secret-signing-key") || strings.Contains(out, srv.URL) {
		t.Errorf("the channel's own secrets are in the output: %.120s", out)
	}
	if n := len([]rune(out)); n > MaxOutput+1 {
		t.Errorf("output is %d characters, want at most %d", n, MaxOutput)
	}
	if strings.Contains(body["reason"].(string), "super-secret-signing-key") {
		t.Error("the reason leaks the key")
	}
}

func TestFromSecretValidation(t *testing.T) {
	d := func(kv ...string) map[string][]byte {
		m := map[string][]byte{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = []byte(kv[i+1])
		}
		return m
	}
	for name, tc := range map[string]struct {
		typ      string
		insecure bool
		data     map[string][]byte
		wantErr  string
	}{
		"slack ok":              {TypeSlack, false, d("url", "https://hooks.slack.com/services/x"), ""},
		"slack http":            {TypeSlack, false, d("url", "http://hooks.slack.com/x"), "https is required"},
		"slack http insecure":   {TypeSlack, true, d("url", "http://hooks.slack.com/x"), ""},
		"slack missing":         {TypeSlack, false, d(), `"url"`},
		"webhook needs signing": {TypeWebhook, false, d("url", "https://x.example.com"), "signingKey"},
		"webhook ok":            {TypeWebhook, false, d("url", "https://x.example.com", "signingKey", "k"), ""},
		"scheme":                {TypeSlack, false, d("url", "ftp://x.example.com/"), "https"},
		"smtp ok":               {TypeSMTP, false, d("host", "smtp.example.com", "port", "587", "from", "a@example.com", "to", "b@example.com, c@example.com"), ""},
		"smtp bad port":         {TypeSMTP, false, d("host", "h", "port", "x", "from", "a@example.com", "to", "b@example.com"), "port"},
		"smtp no recipient":     {TypeSMTP, false, d("host", "h", "from", "a@example.com"), `"to"`},
		"smtp half credentials": {TypeSMTP, false, d("host", "h", "from", "a@example.com", "to", "b@example.com", "username", "u"), "both"},
		"smtp header injection": {TypeSMTP, false, d("host", "h", "from", "a@example.com", "to", "b@example.com\r\nBcc: evil@example.com"), "to"},
		"unknown type":          {"pager", false, d(), "unknown"},
	} {
		ch, err := FromSecret("c", tc.typ, tc.insecure, false, tc.data)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.wantErr)
		case err == nil && tc.typ == TypeSMTP && ch.Port != 587 && tc.data["port"] == nil:
			t.Errorf("%s: default port = %d", name, ch.Port)
		}
		if err != nil {
			for k, v := range tc.data {
				if len(v) > 3 && strings.Contains(err.Error(), string(v)) {
					t.Errorf("%s: the error repeats the value of %s", name, k)
				}
			}
		}
	}
}

// ── a fake SMTP server ───────────────────────────────────────────────────────

type fakeSMTP struct {
	ln       net.Listener
	cfg      *tls.Config
	starttls bool
	mu       sync.Mutex
	mails    []string
	rcpts    []string
	auth     string
}

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

func startSMTP(t *testing.T, starttls bool) (*fakeSMTP, *Sender) {
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
	s := fast()
	s.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return f, s
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
				cur.WriteString(l)
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

func (f *fakeSMTP) channel() *Channel {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	p, _ := net.LookupPort("tcp", port)
	_ = host
	return &Channel{Name: "mail", Type: TypeSMTP, Host: "localhost", Port: p, From: "orca@example.com", To: []string{"a@example.com", "b@example.com"}, AllowPrivate: true}
}

func TestSMTPOverSTARTTLS(t *testing.T) {
	f, s := startSMTP(t, true)
	ch := f.channel()
	ch.Username, ch.Password = "mailer", "mailpass-123"
	m := sampleMsg()
	m.Output = ".leading dot line\nsecond line"
	if err := s.Send(context.Background(), ch, m); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mails) != 1 || len(f.rcpts) != 2 {
		t.Fatalf("mails = %d, rcpts = %d", len(f.mails), len(f.rcpts))
	}
	for _, want := range []string{"From: orca@example.com", "To: a@example.com, b@example.com", "Subject: Schedule digest: Failed", "Run: digest-1763280000", "Cost: $0.42", ".leading dot line"} {
		if !strings.Contains(f.mails[0], want) {
			t.Errorf("mail lacks %q:\n%s", want, f.mails[0])
		}
	}
	if f.auth == "" {
		t.Error("no login was sent")
	}
}

func TestSMTPWithoutSTARTTLSIsRefusedUnlessInsecure(t *testing.T) {
	f, s := startSMTP(t, false)
	ch := f.channel()
	ch.Username, ch.Password = "mailer", "mailpass-123"
	err := s.Send(context.Background(), ch, sampleMsg())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want STARTTLS required", err)
	}
	f.mu.Lock()
	if f.auth != "" || len(f.mails) != 0 {
		t.Error("credentials or mail were sent over a plain connection")
	}
	f.mu.Unlock()
	ch.Insecure = true
	if err := s.Send(context.Background(), ch, sampleMsg()); err != nil {
		t.Fatalf("insecure: %v", err)
	}
}

func TestSMTPRefusesInternalAddressesByDefault(t *testing.T) {
	f, s := startSMTP(t, true)
	ch := f.channel()
	ch.AllowPrivate = false
	if err := s.Send(context.Background(), ch, sampleMsg()); err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("err = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mails) != 0 {
		t.Error("mail reached a loopback server")
	}
}

func TestSMTPCannotBeHeaderInjected(t *testing.T) {
	f, s := startSMTP(t, true)
	m := sampleMsg()
	m.Schedule = "x\r\nBcc: evil@example.com"
	if err := s.Send(context.Background(), f.channel(), m); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(f.mails[0], "\r\nBcc:") {
		t.Errorf("a header was injected:\n%s", f.mails[0])
	}
}

func TestScrubMasksBeforeItCuts(t *testing.T) {
	// A secret straddling the cut would survive a cut-then-mask order.
	secret := "straddling-secret-value"
	reg := &redact.Registry{}
	reg.Register(secret)
	got := Message{Output: strings.Repeat("a", MaxOutput-5) + secret}.Scrub(reg)
	if strings.Contains(got.Output, "straddling") {
		t.Fatal("a partial secret is in the output")
	}
}
