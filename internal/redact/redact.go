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

// Package redact keeps credentials out of anything the model-router writes.
//
// A process registers each credential it holds (the short-lived MCP access token
// a run's router gets from the operator) and every place that writes text out of
// the process (log handler, log writer, trace events, SSE tokens, tool results)
// passes the text through String. The replacement is exact-match on the secret
// and on its JSON-escaped and URL-escaped forms, longest first.
//
// This is the second line of defence: the first is that the token is only ever
// put into the HTTP request to the MCP server and is never handed to the model.
// The registry catches a server that echoes its credential, and a bug that
// formats one into an error.
package redact

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Mask replaces a secret.
const Mask = "[REDACTED]"

// minLen is the shortest secret that is registered: replacing very short strings
// would corrupt ordinary text. Real tokens are far longer.
const minLen = 8

// Registry is a set of secrets to mask.
type Registry struct {
	mu      sync.RWMutex
	secrets map[string]struct{}
	// forms is every form to replace, longest first; rebuilt on change.
	forms []string
}

// Default is the process-wide registry.
var Default = &Registry{}

// Register adds a secret (in all its forms) to the process-wide registry.
func Register(secret string) { Default.Register(secret) }

// Forget removes a secret from the process-wide registry.
func Forget(secret string) { Default.Forget(secret) }

// String masks every registered secret in s.
func String(s string) string { return Default.String(s) }

// Bytes masks every registered secret in b.
func Bytes(b []byte) []byte { return Default.Bytes(b) }

// Register adds a secret. Short values are ignored.
func (r *Registry) Register(secret string) {
	if len(secret) < minLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secrets == nil {
		r.secrets = map[string]struct{}{}
	}
	r.secrets[secret] = struct{}{}
	r.rebuild()
}

// Forget removes a secret.
func (r *Registry) Forget(secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.secrets, secret)
	r.rebuild()
}

func (r *Registry) rebuild() {
	set := map[string]struct{}{}
	for s := range r.secrets {
		set[s] = struct{}{}
		if q := strconv.Quote(s); len(q) > 2 {
			set[q[1:len(q)-1]] = struct{}{} // as it appears inside a JSON or Go string
		}
		set[url.QueryEscape(s)] = struct{}{}
		set[url.PathEscape(s)] = struct{}{}
	}
	r.forms = r.forms[:0]
	for f := range set {
		if len(f) >= minLen {
			r.forms = append(r.forms, f)
		}
	}
	sort.Slice(r.forms, func(i, j int) bool { return len(r.forms[i]) > len(r.forms[j]) })
}

// String masks every registered secret in s.
func (r *Registry) String(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, f := range r.forms {
		if strings.Contains(s, f) {
			s = strings.ReplaceAll(s, f, Mask)
		}
	}
	return s
}

// Bytes masks every registered secret in b. It returns b itself when nothing matched.
func (r *Registry) Bytes(b []byte) []byte {
	if s := string(b); r.String(s) != s {
		return []byte(r.String(s))
	}
	return b
}

// Writer wraps w so everything written is masked. A secret split across two
// Write calls is not caught, which log writers (one call per line) do not do.
func Writer(w io.Writer) io.Writer { return &writer{w: w, r: Default} }

type writer struct {
	w io.Writer
	r *Registry
}

func (w *writer) Write(p []byte) (int, error) {
	out := w.r.Bytes(p)
	if _, err := w.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Handler wraps a slog.Handler so the message and every string, error and
// formatted attribute is masked before it is written.
func Handler(next slog.Handler) slog.Handler { return &handler{next: next, r: Default} }

type handler struct {
	next slog.Handler
	r    *Registry
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = h.attr(a)
	}
	return &handler{next: h.next.WithAttrs(masked), r: h.r}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{next: h.next.WithGroup(name), r: h.r}
}

func (h *handler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.r.String(v.String()))
	case slog.KindGroup:
		g := v.Group()
		out := make([]any, len(g))
		for i, ga := range g {
			out[i] = h.attr(ga)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		// Errors, structs and anything else format through the handler as text or
		// JSON: mask their string form, which is what a text handler would print.
		if e, ok := v.Any().(error); ok {
			return slog.String(a.Key, h.r.String(e.Error()))
		}
		return slog.String(a.Key, h.r.String(stringify(v)))
	}
	return a
}

func stringify(v slog.Value) string {
	if s, ok := v.Any().(interface{ String() string }); ok {
		return s.String()
	}
	return slog.AnyValue(v.Any()).String()
}
