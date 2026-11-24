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

// Package chat is what the adapters of chat platforms share on top of the
// inbound core (docs/inbound-channels.md, "Chat contract"): the formatter that
// turns the agent's Markdown into a platform's dialect and makes sure nothing in
// it can ping people, the splitter that cuts long answers into messages, and an
// HTTP client for the platform's API with the guards every outbound call needs.
//
// Nothing here knows a platform by name.
package chat

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/notify"
)

// Client calls a platform's HTTP API on behalf of a connection. The zero value is
// ready to use. It
//   - dials through the SSRF guard (notify.GuardedDialer): loopback, private,
//     link-local and metadata addresses are refused after DNS unless AllowPrivate;
//   - follows no redirect (the token would travel with it) and uses no proxy;
//   - bounds every attempt, and retries transport errors, 5xx and 429, waiting
//     for Retry-After when it is short, and returning the 429 to the caller when
//     it is not (the core retries a whole delivery later);
//   - never puts a body, a header or a URL into an error: they carry tokens and
//     message text.
type Client struct {
	// AllowPrivate allows private addresses: a self-hosted platform. The flag
	// belongs in the connection's Secret, so a builder cannot set it.
	AllowPrivate bool
	// Timeout bounds one attempt (default 15s).
	Timeout time.Duration
	// Attempts is the most tries of one call (default 3).
	Attempts int
	// MaxWait is the longest Retry-After or backoff waited inside a call
	// (default 20s). A longer Retry-After ends the call with the 429.
	MaxWait time.Duration
	// TLS overrides the TLS settings (tests trust a throwaway CA).
	TLS *tls.Config
	// Wait sleeps; nil sleeps for real. Tests replace it.
	Wait func(ctx context.Context, d time.Duration) error

	once sync.Once
	http *http.Client
}

// Response is the final answer of the platform. A 4xx or 5xx after the retries is
// a Response, not an error: the adapter reads the platform's own error code.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// RetryAfter is the platform's Retry-After on a 429, zero if none.
	RetryAfter time.Duration
}

// maxBody is how much of an answer is read.
const maxBody = 1 << 20

func orDur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (c *Client) client() *http.Client {
	c.once.Do(func() {
		timeout := orDur(c.Timeout, 15*time.Second)
		c.http = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil, // a proxy would dial on our behalf, past the guard
				DialContext:         notify.GuardedDialer(c.AllowPrivate, timeout).DialContext,
				TLSClientConfig:     c.TLS,
				TLSHandshakeTimeout: timeout,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     30 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return c.http
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.Wait != nil {
		return c.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrBlocked means the destination is an address the guard refuses.
var ErrBlocked = notify.ErrBlockedAddress

// Do sends a request and returns the platform's final answer.
func (c *Client) Do(ctx context.Context, method, rawURL string, header http.Header, body []byte) (*Response, error) {
	attempts := max(1, orInt(c.Attempts, 3))
	maxWait := orDur(c.MaxWait, 20*time.Second)
	backoff := time.Second
	var last error
	for i := range attempts {
		resp, err := c.once1(ctx, method, rawURL, header, body)
		retry := false
		var delay time.Duration
		switch {
		case err != nil:
			last = err
			retry = !errors.Is(err, ErrBlocked) && ctx.Err() == nil
			delay = backoff
		case resp.Status == http.StatusTooManyRequests:
			delay = resp.RetryAfter
			if delay <= 0 {
				delay = backoff
			}
			retry = delay <= maxWait
		case resp.Status >= 500:
			retry = true
			delay = backoff
		}
		if !retry || i == attempts-1 {
			return resp, err
		}
		if werr := c.wait(ctx, min(delay, maxWait)); werr != nil {
			return resp, werr
		}
		backoff *= 2
	}
	return nil, last
}

func orInt(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

func (c *Client) once1(ctx context.Context, method, rawURL string, header http.Header, body []byte) (*Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, errors.New("the request could not be built")
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "agent-orca-inbound")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, errors.New("the answer could not be read")
	}
	out := &Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
	if resp.StatusCode == http.StatusTooManyRequests {
		out.RetryAfter = ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return out, nil
}

// cleanErr drops the URL and everything else a transport error could repeat.
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, ErrBlocked) {
		return ErrBlocked
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("the platform did not answer in time")
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return fmt.Errorf("the platform could not be reached: %s", transportKind(err))
}

func transportKind(err error) string {
	var op interface{ Timeout() bool }
	if errors.As(err, &op) && op.Timeout() {
		return "timeout"
	}
	return "connection error"
}

// ParseRetryAfter reads a Retry-After header: seconds or an HTTP date. It
// returns zero for anything else, and never a negative duration.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, t.Sub(now))
	}
	return 0
}
