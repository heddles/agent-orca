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

package state

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// TenantPrefix is what a tenant's keys start with: "t:{tid}:".
func TenantPrefix(tenantID string) string { return "t:" + tenantID + ":" }

// ForTenant returns a view of s whose keys all live under the tenant's prefix
// (checkpoints, spend, token streams, answers, KV, cancel flags), so one
// tenant's keys can never collide with, or be listed by, another's. An empty
// tenantID returns s unchanged: objects not yet stamped with a tenant ID keep
// their old keys.
//
// Migration, per key: read the prefixed key first and fall back to the old,
// unprefixed key; always write the prefixed key. A run that started before the
// upgrade therefore keeps working, and its next write moves it. Old keys age
// out with their TTLs; nothing is deleted eagerly except by DeleteKey, which
// removes both.
//
// Every process that touches a run's keys (the router, the operator, the API)
// must wrap its store with the same tenant ID, which each takes from the run's
// agentorca.io/tenant-id label.
func ForTenant(s Store, tenantID string) Store {
	if tenantID == "" || s == nil {
		return s
	}
	if t, ok := s.(*tenantStore); ok && t.tid == tenantID {
		return s
	}
	return &tenantStore{inner: s, tid: tenantID, prefix: TenantPrefix(tenantID)}
}

type tenantStore struct {
	inner  Store
	tid    string
	prefix string
}

func (t *tenantStore) SaveMessages(ctx context.Context, key string, m []json.RawMessage, ttl time.Duration) error {
	return t.inner.SaveMessages(ctx, t.prefix+key, m, ttl)
}

func (t *tenantStore) LoadMessages(ctx context.Context, key string) ([]json.RawMessage, error) {
	m, err := t.inner.LoadMessages(ctx, t.prefix+key)
	if err != nil || m != nil {
		return m, err
	}
	return t.inner.LoadMessages(ctx, key)
}

func (t *tenantStore) SaveSpend(ctx context.Context, key string, usd float64, ttl time.Duration) error {
	return t.inner.SaveSpend(ctx, t.prefix+key, usd, ttl)
}

// LoadSpend cannot tell "no spend" from "zero spend", so a zero under the new
// key reads the old one: spend only ever grows, which makes the larger value
// the right one.
func (t *tenantStore) LoadSpend(ctx context.Context, key string) (float64, error) {
	v, err := t.inner.LoadSpend(ctx, t.prefix+key)
	if err != nil || v > 0 {
		return v, err
	}
	return t.inner.LoadSpend(ctx, key)
}

func (t *tenantStore) SaveToken(ctx context.Context, key, token string) error {
	return t.inner.SaveToken(ctx, t.prefix+key, token)
}

func (t *tenantStore) SaveTraceEvent(ctx context.Context, key, ev string) error {
	return t.inner.SaveTraceEvent(ctx, t.prefix+key, ev)
}

// streamKey picks the stream to read: the prefixed one, unless it is empty and
// a pre-upgrade stream with the old key has entries.
func (t *tenantStore) streamKey(ctx context.Context, key string) string {
	if es, err := t.inner.ReadTraceEvents(ctx, t.prefix+key); err == nil && len(es) > 0 {
		return t.prefix + key
	}
	if es, err := t.inner.ReadTraceEvents(ctx, key); err == nil && len(es) > 0 {
		return key
	}
	return t.prefix + key
}

func (t *tenantStore) TailTokens(ctx context.Context, key string) (<-chan string, error) {
	return t.inner.TailTokens(ctx, t.streamKey(ctx, key))
}

func (t *tenantStore) ReadTraceEvents(ctx context.Context, key string) ([]TraceEntry, error) {
	return t.inner.ReadTraceEvents(ctx, t.streamKey(ctx, key))
}

func (t *tenantStore) SaveAnswer(ctx context.Context, key, answer string, ttl time.Duration) error {
	return t.inner.SaveAnswer(ctx, t.prefix+key, answer, ttl)
}

func (t *tenantStore) LoadAnswer(ctx context.Context, key string) (string, error) {
	a, err := t.inner.LoadAnswer(ctx, t.prefix+key)
	if err != nil || a != "" {
		return a, err
	}
	return t.inner.LoadAnswer(ctx, key)
}

// httpOutputScope holds http-mode run output. The unwrapped store keys it by run
// name alone, which two tenants can share, so the view stores it as tenant KV.
const httpOutputScope = "agentorca:runs:http-output"

func (t *tenantStore) SaveHTTPOutput(ctx context.Context, runName, output string) error {
	return t.SaveKV(ctx, httpOutputScope, runName, []byte(output), 24*time.Hour)
}

func (t *tenantStore) LoadHTTPOutput(ctx context.Context, runName string) (string, error) {
	b, err := t.LoadKV(ctx, httpOutputScope, runName)
	if err != nil || b != nil {
		return string(b), err
	}
	return t.inner.LoadHTTPOutput(ctx, runName)
}

func (t *tenantStore) DeleteKey(ctx context.Context, key string) error {
	if err := t.inner.DeleteKey(ctx, t.prefix+key); err != nil {
		return err
	}
	return t.inner.DeleteKey(ctx, key)
}

func (t *tenantStore) SaveKV(ctx context.Context, scope, key string, v []byte, ttl time.Duration) error {
	return t.inner.SaveKV(ctx, t.prefix+scope, key, v, ttl)
}

func (t *tenantStore) LoadKV(ctx context.Context, scope, key string) ([]byte, error) {
	v, err := t.inner.LoadKV(ctx, t.prefix+scope, key)
	if err != nil || v != nil {
		return v, err
	}
	return t.inner.LoadKV(ctx, scope, key)
}

func (t *tenantStore) DeleteKV(ctx context.Context, scope, key string) error {
	if err := t.inner.DeleteKV(ctx, t.prefix+scope, key); err != nil {
		return err
	}
	return t.inner.DeleteKV(ctx, scope, key)
}

func (t *tenantStore) ListKV(ctx context.Context, scope string) ([]string, error) {
	return unionKeys(
		func() ([]string, error) { return t.inner.ListKV(ctx, t.prefix+scope) },
		func() ([]string, error) { return t.inner.ListKV(ctx, scope) },
		"",
	)
}

// ListMessageKeys lists the prefixed matches, with the prefix stripped so the
// keys can be passed back to this view, plus the old unprefixed matches. A
// pattern is never allowed to reach another tenant's prefix: the old-key scan
// skips every "t:" key.
func (t *tenantStore) ListMessageKeys(ctx context.Context, pattern string) ([]string, error) {
	return unionKeys(
		func() ([]string, error) { return t.inner.ListMessageKeys(ctx, t.prefix+pattern) },
		func() ([]string, error) { return t.inner.ListMessageKeys(ctx, pattern) },
		t.prefix,
	)
}

func unionKeys(prefixed, old func() ([]string, error), strip string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	ks, err := prefixed()
	if err != nil {
		return nil, err
	}
	for _, k := range ks {
		add(strings.TrimPrefix(k, strip))
	}
	ks, err = old()
	if err != nil {
		return nil, err
	}
	for _, k := range ks {
		if strings.HasPrefix(k, "t:") { // another tenant's key matched a broad pattern
			continue
		}
		add(k)
	}
	return out, nil
}

// SignalCancel sets the flag under the tenant's namespace key. IsCancelled also
// honours the flag external systems set with the documented key
// "cancel:<ns>:<run>".
func (t *tenantStore) SignalCancel(ctx context.Context, ns, runName string) error {
	return t.inner.SignalCancel(ctx, t.prefix+ns, runName)
}

func (t *tenantStore) IsCancelled(ctx context.Context, ns, runName string) (bool, error) {
	if ok, err := t.inner.IsCancelled(ctx, t.prefix+ns, runName); err != nil || ok {
		return ok, err
	}
	return t.inner.IsCancelled(ctx, ns, runName)
}

func (t *tenantStore) Ping(ctx context.Context) error { return t.inner.Ping(ctx) }

// Close does nothing: the view shares its connection with the store it wraps,
// which its owner closes.
func (t *tenantStore) Close() error { return nil }

// PrefixEnvVar is the escape hatch for tenant key prefixes. They are on by
// default; setting it to "false" in the operator's environment turns them off
// for the operator and the API, and, through the tenant ID the operator hands
// it, for the model-router, so every process keeps agreeing on a run's keys.
// It is read when a key is built, so flip it with no runs in flight: a run that
// started under one setting is read through the old-key fallback only, and its
// writes land under the new setting.
const PrefixEnvVar = "AGENTORCA_STATE_TENANT_PREFIX"

// PrefixEnabled reports whether tenant key prefixes are on: always, unless
// PrefixEnvVar is "false".
func PrefixEnabled() bool { return os.Getenv(PrefixEnvVar) != "false" }

// ForTenantIfEnabled is ForTenant while PrefixEnabled, else s unchanged.
func ForTenantIfEnabled(s Store, tenantID string) Store {
	if !PrefixEnabled() {
		return s
	}
	return ForTenant(s, tenantID)
}
