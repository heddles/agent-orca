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
package connections

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// The lifecycle methods of the in-memory store. They model what the real store
// guarantees: the same lock as WithConnectionLock covers a re-wrap, an expiry
// re-checks idleness under it, and a re-wrap only runs on a row not yet under the
// active key.

func (m *memStore) ListIdleConnections(_ context.Context, cutoff time.Time, limit int) ([]postgresql.IdleConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []postgresql.IdleConnection
	for _, c := range m.conns {
		if idle(c, cutoff) && len(out) < limit {
			out = append(out, postgresql.IdleConnection{ID: c.ID, TenantID: c.TenantID, UserID: c.UserID, ServerNS: c.ServerNS, ServerName: c.ServerName})
		}
	}
	return out, nil
}

func idle(c postgresql.MCPConnection, cutoff time.Time) bool {
	if c.Status != postgresql.ConnectionActive && c.Status != postgresql.ConnectionNeedsReauth {
		return false
	}
	last := c.CreatedAt
	if c.LastUsedAt != nil {
		last = *c.LastUsedAt
	}
	return last.Before(cutoff)
}

func (m *memStore) ExpireIdleConnection(_ context.Context, id int64, cutoff time.Time) (postgresql.MCPConnection, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.conns {
		if c.ID == id && idle(c, cutoff) {
			next := c
			next.Status, next.RefreshTokenEnc, next.AccessTokenEnc, next.AccessExpiresAt = postgresql.ConnectionExpired, []byte{}, nil, nil
			m.conns[k] = next
			return c, true, nil
		}
	}
	return postgresql.MCPConnection{}, false, nil
}

func (m *memStore) ListConnectionsToRewrap(_ context.Context, active string, after int64, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for _, c := range m.conns {
		if c.KeyID != active && c.ID > after && (c.Status == postgresql.ConnectionActive || c.Status == postgresql.ConnectionNeedsReauth) {
			ids = append(ids, c.ID)
		}
	}
	sortIDs(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func sortIDs(ids []int64) {
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
}

func (m *memStore) RewrapConnection(_ context.Context, id int64, active string, fn func(postgresql.MCPConnection) (*postgresql.MCPConnection, error)) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.conns {
		if c.ID != id || c.KeyID == active {
			continue
		}
		next, err := fn(c)
		if err != nil {
			return false, err
		}
		if next != nil {
			m.conns[k] = *next
		}
		return true, nil
	}
	return false, nil
}

func (m *memStore) ListOAuthAppsToRewrap(_ context.Context, active string, _ int64, _ int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for _, a := range m.apps {
		if a.KeyID != active {
			ids = append(ids, 1) // the tests register one app
		}
	}
	return ids, nil
}

func (m *memStore) RewrapOAuthApp(_ context.Context, id int64, active string, fn func(postgresql.MCPOAuthApp) (*postgresql.MCPOAuthApp, error)) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.apps {
		if id != 1 || a.KeyID == active {
			continue
		}
		next, err := fn(a)
		if err != nil {
			return false, err
		}
		m.apps[k] = *next
		return true, nil
	}
	return false, nil
}

func (m *memStore) put(c postgresql.MCPConnection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[c.UserID+"/"+c.ServerNS+"/"+c.ServerName] = c
}

func (m *memStore) get(user string) postgresql.MCPConnection {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[user+"/team/github"]
}

func (h *harness) lifecycle() *Lifecycle {
	return &Lifecycle{Store: h.store, Svc: h.svc, IdleAfter: 90 * 24 * time.Hour,
		Resolve: func(context.Context, string, string) (ServerRef, error) { return h.ref, nil }}
}

// seedConn stores a connection for user, sealed under the harness keyring.
func (h *harness) seedConn(t *testing.T, id int64, user string, last *time.Time, created time.Time) {
	t.Helper()
	aad := func(p string) []byte { return AAD(p, "tnt_a", user, h.ref.UID) }
	keyID, rt, err := h.svc.Keys.Seal([]byte("refresh-"+user), aad("refresh"))
	if err != nil {
		t.Fatal(err)
	}
	_, at, err := h.svc.Keys.Seal([]byte("access-"+user), aad("access"))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	h.store.put(postgresql.MCPConnection{ID: id, TenantID: "tnt_a", UserID: user, ServerNS: "team", ServerName: "github", ServerUID: h.ref.UID,
		Status: postgresql.ConnectionActive, RefreshTokenEnc: rt, AccessTokenEnc: at, AccessExpiresAt: &exp, KeyID: keyID,
		LastUsedAt: last, CreatedAt: created})
}

func TestExpireIdleSelectsOnlyIdleConnections(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	ago := func(d int) *time.Time { v := now.Add(-time.Duration(d) * 24 * time.Hour); return &v }
	for i, c := range []struct {
		user    string
		last    *time.Time
		created time.Time
	}{
		{"old", ago(120), *ago(200)},
		{"recent", ago(10), *ago(200)},
		{"neverUsedOld", nil, *ago(100)},
		{"neverUsedNew", nil, *ago(5)},
		{"reauthOld", ago(95), *ago(200)},
	} {
		h.seedConn(t, int64(i+1), c.user, c.last, c.created)
	}
	rc := h.store.get("reauthOld")
	rc.Status = postgresql.ConnectionNeedsReauth
	h.store.put(rc)

	var audited []string
	life := h.lifecycle()
	life.OnExpire = func(_ context.Context, c postgresql.MCPConnection, outcome string) {
		audited = append(audited, c.UserID+":"+outcome)
	}
	n, err := life.ExpireIdle(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("expired %d, %v (audited %v)", n, err, audited)
	}
	for u, want := range map[string]string{"old": "expired", "neverUsedOld": "expired", "reauthOld": "expired", "recent": "active", "neverUsedNew": "active"} {
		if got := h.store.get(u).Status; got != want {
			t.Errorf("%s status = %s, want %s", u, got, want)
		}
	}
	// The tokens of an expired row are gone, and the provider was asked to revoke.
	if c := h.store.get("old"); len(c.RefreshTokenEnc) != 0 || len(c.AccessTokenEnc) != 0 {
		t.Errorf("expired row keeps tokens: %+v", c)
	}
	if len(h.as.revoked) != 3 || !strings.HasPrefix(h.as.revoked[0], "refresh-") {
		t.Errorf("revoked at provider = %v", h.as.revoked)
	}
	if len(audited) != 3 || !strings.HasSuffix(audited[0], ":revoked") {
		t.Errorf("audited = %v", audited)
	}
	// A second pass finds nothing, and a use of an expired connection says why.
	if n, _ := life.ExpireIdle(context.Background()); n != 0 {
		t.Errorf("second pass expired %d", n)
	}
	_, err = h.svc.AccessToken(context.Background(), TokenRequest{TenantID: "tnt_a", UserID: "old", Server: h.ref})
	if te := AsTokenError(err); te == nil || te.Code != CodeNeedsReauth || !strings.Contains(te.Message, "expired") {
		t.Errorf("token of an expired connection = %v", err)
	}
	if got := testutil.ToFloat64(idleExpired.WithLabelValues("revoked")); got < 3 {
		t.Errorf("expired counter = %v", got)
	}
}

func TestExpireIdleStillExpiresWhenRevocationFails(t *testing.T) {
	h := newHarness(t)
	old := time.Now().Add(-100 * 24 * time.Hour)
	h.seedConn(t, 1, "alice", &old, old)
	h.as.Close() // the provider is unreachable
	life := h.lifecycle()
	var outcome string
	life.OnExpire = func(_ context.Context, _ postgresql.MCPConnection, o string) { outcome = o }
	if n, err := life.ExpireIdle(context.Background()); err != nil || n != 1 || outcome != "failed" {
		t.Fatalf("expired %d, %v, outcome %q", n, err, outcome)
	}
	if h.store.get("alice").Status != postgresql.ConnectionExpired {
		t.Error("connection not expired")
	}
	// The server being gone skips revocation without failing.
	h2 := newHarness(t)
	h2.seedConn(t, 1, "alice", &old, old)
	l2 := h2.lifecycle()
	l2.Resolve = func(context.Context, string, string) (ServerRef, error) { return ServerRef{}, errors.New("gone") }
	if n, _ := l2.ExpireIdle(context.Background()); n != 1 || len(h2.as.revoked) != 0 {
		t.Errorf("expired %d, revoked %v", n, h2.as.revoked)
	}
}

func TestExpireIdleOffAndUseKeepsAliveConnection(t *testing.T) {
	h := newHarness(t)
	old := time.Now().Add(-100 * 24 * time.Hour)
	h.seedConn(t, 1, "alice", &old, old)
	life := h.lifecycle()
	life.IdleAfter = 0
	if n, _ := life.ExpireIdle(context.Background()); n != 0 {
		t.Errorf("expiry off still expired %d", n)
	}
	// A use that wins the race (the store re-checks idleness under the lock) keeps it.
	used := time.Now()
	c := h.store.get("alice")
	c.LastUsedAt = &used
	h.store.put(c)
	life.IdleAfter = 90 * 24 * time.Hour
	if _, ok, _ := h.store.ExpireIdleConnection(context.Background(), 1, time.Now().Add(-life.IdleAfter)); ok {
		t.Error("a connection used since was expired")
	}
}

func twoKeyRing(t *testing.T, active string) *Keyring {
	t.Helper()
	k, err := NewKeyring(active, map[string][]byte{"k1": testKey(1), "k2": testKey(2)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRotateKeysRewrapsAndKeepsOldRowsReadable(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	for i, u := range []string{"a", "b", "c", "d"} {
		h.seedConn(t, int64(i+1), u, &now, now)
	}
	h.svc.Keys = twoKeyRing(t, "k2")

	// Before the job, the old rows still open (both keys are in the keyring).
	c := h.store.get("a")
	if _, err := h.svc.Keys.Open(c.KeyID, c.RefreshTokenEnc, AAD("refresh", "tnt_a", "a", h.ref.UID)); err != nil {
		t.Fatalf("old row does not open under the rotated keyring: %v", err)
	}
	// Row "c" is corrupt: it fails, and the batch goes on (batch size 1 forces several pages).
	bad := h.store.get("c")
	bad.RefreshTokenEnc = []byte("garbage-not-a-blob-garbage-not-a-blob-garbage-not-a-blob-garbage-not-a-blob")
	h.store.put(bad)
	// Row "d" has an access token that cannot be opened: it is dropped, the refresh token moves.
	d := h.store.get("d")
	d.AccessTokenEnc = []byte("garbage")
	h.store.put(d)

	life := h.lifecycle()
	life.BatchSize = 1
	res, err := life.RotateKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewrapped != 4 || res.Failed != 1 { // a, b, d, and the app secret; c fails
		t.Fatalf("result = %+v", res)
	}
	for _, u := range []string{"a", "b", "d"} {
		row := h.store.get(u)
		if row.KeyID != "k2" {
			t.Errorf("%s key = %s", u, row.KeyID)
		}
		// Opens under the new key alone: drop k1 from the keyring.
		only2, _ := NewKeyring("k2", map[string][]byte{"k2": testKey(2)})
		pt, err := only2.Open(row.KeyID, row.RefreshTokenEnc, AAD("refresh", "tnt_a", u, h.ref.UID))
		if err != nil || string(pt) != "refresh-"+u {
			t.Errorf("%s refresh = %q, %v", u, pt, err)
		}
		// The AAD is preserved: the blob does not open for another user.
		if _, err := only2.Open(row.KeyID, row.RefreshTokenEnc, AAD("refresh", "tnt_a", "mallory", h.ref.UID)); err == nil {
			t.Errorf("%s: re-wrapped blob opens under another AAD", u)
		}
	}
	if d := h.store.get("d"); len(d.AccessTokenEnc) != 0 || d.AccessExpiresAt != nil {
		t.Errorf("unreadable access token kept: %+v", d)
	}
	if a := h.store.get("a"); len(a.AccessTokenEnc) == 0 {
		t.Error("readable access token dropped")
	} else if pt, err := h.svc.Keys.Open(a.KeyID, a.AccessTokenEnc, AAD("access", "tnt_a", "a", h.ref.UID)); err != nil || string(pt) != "access-a" {
		t.Errorf("access token = %q, %v", pt, err)
	}
	if h.store.get("c").KeyID != "k1" {
		t.Error("the failing row was changed")
	}
	// The client secret moved too, so the old key can be removed.
	app := h.store.apps["team/github"]
	only2, _ := NewKeyring("k2", map[string][]byte{"k2": testKey(2)})
	if pt, err := only2.Open(app.KeyID, app.ClientSecretEnc, AAD("client_secret", "tnt_a", "team", "github")); err != nil || string(pt) != "client-secret-xyz" {
		t.Errorf("client secret = %q, %v", pt, err)
	}
	// Resumable and idempotent: a second pass only retries the failing row.
	res2, _ := life.RotateKeys(context.Background())
	if res2.Rewrapped != 0 || res2.Failed != 1 {
		t.Errorf("second pass = %+v", res2)
	}
	if got := testutil.ToFloat64(rotationPending); got != 1 {
		t.Errorf("pending gauge = %v", got)
	}
}

// A rotating refresh token refreshed while the rotation job runs must not lose the
// new token: the job re-wraps under the row lock, so it sees either the row before
// the refresh or after it, never a stale copy it then writes back.
func TestRotateKeysUnderConcurrentRefresh(t *testing.T) {
	h := newHarness(t)
	h.as.rotate = true
	h.connect(t)
	h.svc.Keys = twoKeyRing(t, "k2")
	h.svc.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	life := h.lifecycle()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 8 {
		wg.Go(func() {
			if _, err := h.token(t); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if _, err := life.RotateKeys(context.Background()); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent refresh or rotation failed: %v", err)
	}
	row := h.store.get(testUser)
	if row.KeyID != "k2" {
		t.Errorf("row key = %s, want k2", row.KeyID)
	}
	rt, err := h.svc.Keys.Open(row.KeyID, row.RefreshTokenEnc, AAD("refresh", "tnt_a", testUser, "uid-1"))
	if err != nil || string(rt) != h.as.validRefresh {
		t.Errorf("stored refresh token = %q (%v), provider accepts %q: the rotated token was lost", rt, err, h.as.validRefresh)
	}
	if h.as.refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want 1", h.as.refreshCalls)
	}
}

func TestRefreshFailureMetricsAndMessages(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	grant := refreshFailures.WithLabelValues(reasonInvalidGrant)
	before := testutil.ToFloat64(grant)
	h.as.validRefresh = "other"
	h.svc.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, err := h.token(t)
	te := AsTokenError(err)
	if te == nil || te.Code != CodeNeedsReauth || !strings.Contains(te.Message, "reconnect") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(te.Message, "refresh-1") || strings.Contains(te.Message, "invalid_grant") {
		t.Errorf("message leaks provider detail: %q", te.Message)
	}
	if got := testutil.ToFloat64(grant) - before; got != 1 {
		t.Errorf("invalid_grant counter rose by %v", got)
	}
	// An unreachable provider counts as unavailable and leaves the connection usable.
	h2 := newHarness(t)
	h2.connect(t)
	h2.as.Close()
	h2.svc.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	un := refreshFailures.WithLabelValues(reasonUnavailable)
	b2 := testutil.ToFloat64(un)
	_, _ = h2.token(t)
	if testutil.ToFloat64(un)-b2 != 1 || h2.store.get(testUser).Status != postgresql.ConnectionActive {
		t.Error("an unreachable provider must count as unavailable and keep the connection active")
	}
}

type fakeCounter struct {
	counts map[string]int
	err    error
	calls  int
}

func (f *fakeCounter) CountConnectionsByStatus(context.Context) (map[string]int, error) {
	f.calls++
	return f.counts, f.err
}

func TestStatusCollector(t *testing.T) {
	fc := &fakeCounter{counts: map[string]int{"active": 3, "needs_reauth": 1}}
	col := NewStatusCollector(fc)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)
	// Every status is reported, 0 included; no label but status.
	if n, err := testutil.GatherAndCount(reg, "agentorca_mcp_connections"); err != nil || n != 3 {
		t.Fatalf("series = %d, %v", n, err)
	}
	want := `
# HELP agentorca_mcp_connections Per-user MCP connections by status.
# TYPE agentorca_mcp_connections gauge
agentorca_mcp_connections{status="active"} 3
agentorca_mcp_connections{status="expired"} 0
agentorca_mcp_connections{status="needs_reauth"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentorca_mcp_connections"); err != nil {
		t.Error(err)
	}
	if fc.calls != 1 {
		t.Errorf("store read %d times in one scrape window", fc.calls)
	}
	// A database error yields no series, not stale zeros.
	broken := NewStatusCollector(&fakeCounter{err: fmt.Errorf("db down")})
	reg2 := prometheus.NewRegistry()
	reg2.MustRegister(broken)
	if n, _ := testutil.GatherAndCount(reg2, "agentorca_mcp_connections"); n != 0 {
		t.Errorf("series on error = %d", n)
	}
}
