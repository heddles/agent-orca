package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/heddles/agent-orca/internal/redact"
)

// Header names of a signed webhook. They are the trigger webhooks' (see
// internal/apiserver/triggers_hook.go): "sha256=" and the hex HMAC-SHA256 of
// "<timestamp>.<body>", with the Unix time the sender signed.
const (
	SignatureHeader = "X-AgentOrca-Signature"
	TimestampHeader = "X-AgentOrca-Timestamp"
)

// Sign returns the signature header value for a body, as the trigger webhooks
// verify it.
func Sign(key, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ErrBlockedAddress means a destination resolves to an address the channel may
// not reach (the SSRF guard).
var ErrBlockedAddress = errors.New("the destination is an internal address; set allowPrivateAddresses on the channel to allow it")

// permanent wraps an error that a retry cannot fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Sender delivers messages. The zero value is usable.
type Sender struct {
	// Timeout bounds one attempt; default 10s.
	Timeout time.Duration
	// Attempts is how many times a delivery is tried; default 3.
	Attempts int
	// Backoff is the wait before the second attempt, doubled after each;
	// default 2s.
	Backoff time.Duration
	// TLS overrides the TLS settings (tests trust a throwaway CA).
	TLS *tls.Config
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (s *Sender) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 10 * time.Second
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Send delivers msg to ch, retrying transient failures with a growing wait. The
// returned error never contains a Secret value. A refused address or a bad
// configuration is not retried.
func (s *Sender) Send(ctx context.Context, ch *Channel, msg Message) error {
	reg := &redact.Registry{}
	for _, v := range ch.Secrets() {
		reg.Register(v)
	}
	if msg.Time.IsZero() {
		msg.Time = s.now()
	}
	msg = msg.Scrub(reg)

	return s.retry(ctx, ch, reg, func(actx context.Context) error { return s.once(actx, ch, msg) })
}

// SendMail delivers an RFC 5322 message (the caller built it, header lines
// CRLF-terminated) over the SMTP channel ch to its recipients, with the same
// retries, TLS rules and address guard as Send. It is for callers that write
// their own mail, such as the replies of an inbound email channel. The returned
// error never contains a Secret value.
func (s *Sender) SendMail(ctx context.Context, ch *Channel, rfc822 string) error {
	if ch.Type != TypeSMTP {
		return fmt.Errorf("sending to channel %q failed: not an SMTP channel", ch.Name)
	}
	reg := &redact.Registry{}
	for _, v := range ch.Secrets() {
		reg.Register(v)
	}
	return s.retry(ctx, ch, reg, func(actx context.Context) error { return s.smtpSend(actx, ch, rfc822) })
}

// retry runs one attempt up to Attempts times, waiting longer between tries,
// and stops at once on a permanent error.
func (s *Sender) retry(ctx context.Context, ch *Channel, reg *redact.Registry, once func(context.Context) error) error {
	attempts, wait := s.Attempts, s.Backoff
	if attempts <= 0 {
		attempts = 3
	}
	if wait <= 0 {
		wait = 2 * time.Second
	}
	var err error
	for i := 0; i < attempts; i++ {
		actx, cancel := context.WithTimeout(ctx, s.timeout())
		err = once(actx)
		cancel()
		if err == nil {
			return nil
		}
		var p permanent
		if errors.As(err, &p) || ctx.Err() != nil || i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait *= 2
	}
	return fmt.Errorf("sending to channel %q failed: %s", ch.Name, reg.String(err.Error()))
}

func (s *Sender) once(ctx context.Context, ch *Channel, msg Message) error {
	switch ch.Type {
	case TypeWebhook, TypeSlack:
		return s.post(ctx, ch, msg)
	case TypeSMTP:
		return s.mail(ctx, ch, msg)
	}
	return permanent{fmt.Errorf("unknown channel type %q", ch.Type)}
}

// ── SSRF guard ───────────────────────────────────────────────────────────────

// blocked reports whether ip is an address a channel may not reach by default:
// loopback, link-local (which holds the cloud metadata endpoints), private,
// unspecified, multicast, carrier-grade NAT, and the IPv6 forms of them.
func blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || // carrier-grade NAT
		netip.MustParsePrefix("192.0.0.0/24").Contains(ip) ||
		netip.MustParsePrefix("198.18.0.0/15").Contains(ip) ||
		netip.MustParsePrefix("240.0.0.0/4").Contains(ip)
}

// dialer refuses, at connect time, an address the channel may not reach. The
// check is on the address actually dialled, after DNS, so a name that resolves
// to an internal address (or changes between a check and the connection) is
// caught.
func dialer(ch *Channel, timeout time.Duration) *net.Dialer {
	d := &net.Dialer{Timeout: timeout}
	if !ch.AllowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlockedAddress
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || blocked(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	return d
}

// cleanErr removes the URL (a credential for Slack) from a transport error.
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, ErrBlockedAddress) {
		return permanent{ErrBlockedAddress}
	}
	return err
}

// ── webhook and Slack ────────────────────────────────────────────────────────

func (s *Sender) post(ctx context.Context, ch *Channel, msg Message) error {
	var payload any
	if ch.Type == TypeSlack {
		payload = map[string]string{"text": msg.slackText()}
	} else {
		payload = msg.payload()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return permanent{err}
	}
	u, err := url.Parse(ch.URL)
	if err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !ch.Insecure)) {
		return permanent{errors.New("the channel's URL must be https unless the channel sets insecure")}
	}
	// A literal IP is checked before dialling, to answer with the reason.
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !ch.AllowPrivate && blocked(ip) {
		return permanent{ErrBlockedAddress}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL, bytes.NewReader(body))
	if err != nil {
		return permanent{errors.New("the channel's URL is not valid")}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agent-orca-notify")
	if ch.Type == TypeWebhook {
		ts := strconv.FormatInt(s.now().Unix(), 10)
		req.Header.Set(TimestampHeader, ts)
		req.Header.Set(SignatureHeader, Sign(ch.SigningKey, ts, body))
	}
	client := &http.Client{
		Timeout: s.timeout(),
		Transport: &http.Transport{
			Proxy:               nil, // a proxy would dial on our behalf, past the guard
			DialContext:         dialer(ch, s.timeout()).DialContext,
			TLSClientConfig:     s.TLS,
			TLSHandshakeTimeout: s.timeout(),
		},
		// A redirect could lead anywhere; the destination is exactly the Secret's URL.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return cleanErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // never read into a log
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		return fmt.Errorf("the receiver answered %d", resp.StatusCode)
	}
	return permanent{fmt.Errorf("the receiver answered %d", resp.StatusCode)}
}

// ── SMTP ─────────────────────────────────────────────────────────────────────

// plainAuth is PLAIN authentication that does not insist on TLS: the channel's
// insecure flag is the explicit choice to allow it.
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

func (s *Sender) mail(ctx context.Context, ch *Channel, msg Message) error {
	return s.smtpSend(ctx, ch, mailBody(ch, msg))
}

// smtpSend delivers a ready message body.
func (s *Sender) smtpSend(ctx context.Context, ch *Channel, body string) error {
	addr := net.JoinHostPort(ch.Host, strconv.Itoa(ch.Port))
	conn, err := dialer(ch, s.timeout()).DialContext(ctx, "tcp", addr)
	if err != nil {
		return cleanErr(err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := s.TLS.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tlsCfg.ServerName = ch.Host
	implicit := ch.Port == 465
	if implicit {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return errors.New("the TLS handshake with the mail server failed")
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, ch.Host)
	if err != nil {
		return errors.New("the mail server did not greet")
	}
	defer func() { _ = c.Close() }()
	secure := implicit
	if !implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return errors.New("STARTTLS with the mail server failed")
			}
			secure = true
		}
	}
	if !secure && !ch.Insecure {
		return permanent{errors.New("the mail server does not offer STARTTLS: it is required unless the channel sets insecure")}
	}
	if ch.Username != "" {
		if err := c.Auth(plainAuth{ch.Username, ch.Password}); err != nil {
			return permanent{errors.New("the mail server refused the login")}
		}
	}
	if err := c.Mail(ch.From); err != nil {
		return errors.New("the mail server refused the sender")
	}
	for _, to := range ch.To {
		if err := c.Rcpt(to); err != nil {
			return errors.New("the mail server refused a recipient")
		}
	}
	w, err := c.Data()
	if err != nil {
		return errors.New("the mail server refused the message")
	}
	if _, err := io.WriteString(w, body); err != nil {
		return errors.New("writing the message failed")
	}
	if err := w.Close(); err != nil {
		return errors.New("the mail server did not accept the message")
	}
	_ = c.Quit()
	return nil
}

// mailBody is an RFC 5322 message. Header values are single-line.
func mailBody(ch *Channel, msg Message) string {
	var b strings.Builder
	b.WriteString("From: " + ch.From + "\r\n")
	b.WriteString("To: " + strings.Join(ch.To, ", ") + "\r\n")
	b.WriteString("Subject: " + msg.subject() + "\r\n")
	b.WriteString("Date: " + msg.Time.UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(msg.text(), "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.String()
}

// GuardedDialer is a dialer that refuses, at connect time and after DNS, an
// address a channel may not reach unless allowPrivate is set: loopback,
// link-local (cloud metadata), private and other internal addresses. Other
// packages that open connections on behalf of a configured destination (the
// inbound email channel's IMAP client) use it for the same guard.
func GuardedDialer(allowPrivate bool, timeout time.Duration) *net.Dialer {
	return dialer(&Channel{AllowPrivate: allowPrivate}, timeout)
}
