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

package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// ProviderHTTPError is a non-200 answer from a provider. Error() keeps the
// text the forwarders always produced so log and string matches still work.
type ProviderHTTPError struct {
	Provider string
	Status   int
	Body     string
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("provider %s returned %d: %s", e.Provider, e.Status, e.Body)
}

// Retryable reports whether the same provider may succeed on a retry:
// timeouts, rate limits and server errors. Any other status rejected this
// request as sent.
func (e *ProviderHTTPError) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Rejected reports a 4xx that is not retryable: retrying the same provider
// with the same request cannot help, but another provider may accept it.
func (e *ProviderHTTPError) Rejected() bool {
	return e.Status >= 400 && e.Status < 500 && !e.Retryable()
}

func newProviderHTTPError(provider string, status int, body []byte) *ProviderHTTPError {
	return &ProviderHTTPError{Provider: provider, Status: status, Body: string(body)}
}

// isRetryableProviderError is true for retryable HTTP statuses and for
// network or timeout failures.
func isRetryableProviderError(err error) bool {
	var pe *ProviderHTTPError
	if errors.As(err, &pe) {
		return pe.Retryable()
	}
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne)
}

// isRejectedProviderError is true when the provider refused the request itself.
func isRejectedProviderError(err error) bool {
	var pe *ProviderHTTPError
	return errors.As(err, &pe) && pe.Rejected()
}

// maxRoleSequenceLen caps roleSequence output.
const maxRoleSequenceLen = 200

// roleSequence renders the roles of messages in order, comma separated, with
// runs of one role collapsed ("tool x12"). Contents are never included.
func roleSequence(msgs []Message) string {
	var parts []string
	for i := 0; i < len(msgs); {
		j := i
		for j < len(msgs) && msgs[j].Role == msgs[i].Role {
			j++
		}
		if n := j - i; n > 3 {
			parts = append(parts, fmt.Sprintf("%s x%d", msgs[i].Role, n))
		} else {
			for k := i; k < j; k++ {
				parts = append(parts, msgs[k].Role)
			}
		}
		i = j
	}
	out := strings.Join(parts, ",")
	if len(out) > maxRoleSequenceLen {
		out = out[:maxRoleSequenceLen] + "..."
	}
	return out
}

// logProviderFailure logs a failed provider call. A request the provider
// rejected also logs the outbound role sequence (never content) so shape
// errors are diagnosable.
func logProviderFailure(provider string, err error, msgs []Message, haveFallback bool) {
	class := classifyProviderError(err)
	attrs := []any{"provider", provider, "errorClass", class, "err", err}
	var pe *ProviderHTTPError
	if errors.As(err, &pe) {
		attrs = append(attrs, "status", pe.Status)
	}
	switch {
	case haveFallback:
		slog.Warn("provider failed, trying fallback", attrs...)
	default:
		slog.Warn("provider failed, no fallback configured", attrs...)
	}
	logRejectedRoles(provider, err, msgs)
}

// logRejectedRoles logs the outbound shape of a request the provider rejected.
func logRejectedRoles(provider string, err error, msgs []Message) {
	if isRejectedProviderError(err) {
		slog.Warn("provider rejected the request", "provider", provider,
			"roles", roleSequence(msgs), "messages", len(msgs))
	}
}

// fallbackCandidates reports whether anything besides failed could be tried.
func (r *Router) fallbackCandidates(order []string, failed string) []string {
	if len(order) == 0 {
		order = r.cfg.FallbackChain
	}
	var out []string
	for _, n := range order {
		if n != failed {
			out = append(out, n)
		}
	}
	return out
}
