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

package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func sampleConn(user string) MCPConnection {
	return MCPConnection{UserID: user, ServerNS: "team", ServerName: "github", ServerUID: "uid-github",
		Scopes: []string{"read", "https://api.example.com/auth/drive"}, AccountName: user + "@example.com",
		RefreshTokenEnc: []byte("enc-refresh-" + user), AccessTokenEnc: []byte("enc-access-" + user), KeyID: "k1"}
}

func TestMCPConnectionsAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	ctx := context.Background()
	a := WithTenant(ctx, tenantA)
	b := WithTenant(ctx, tenantB)

	conn, err := s.UpsertConnection(a, sampleConn("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if conn.TenantID != tenantA || conn.Status != ConnectionActive || len(conn.Scopes) != 2 || conn.Scopes[1] != "https://api.example.com/auth/drive" {
		t.Fatalf("stored = %+v", conn)
	}
	if _, err := s.UpsertConnection(a, sampleConn("bob")); err != nil {
		t.Fatal(err)
	}
	// Reconnecting replaces the row (same id) and clears needs_reauth.
	if err := s.SetConnectionStatus(a, "alice", "team", "github", ConnectionNeedsReauth); err != nil {
		t.Fatal(err)
	}
	again, err := s.UpsertConnection(a, sampleConn("alice"))
	if err != nil || again.ID != conn.ID || again.Status != ConnectionActive {
		t.Fatalf("reconnect = %+v, %v", again, err)
	}

	checkMCPConnectionIsolation(t, s, raw, a, b)
	checkMCPConnectionUpdates(t, s, a, b)
}

func checkMCPConnectionIsolation(t *testing.T, s *Store, raw *sql.DB, a, b context.Context) {
	t.Helper()
	ctx := context.Background()
	// Cross-tenant reads return nothing, by every read path.
	if _, err := s.GetConnection(b, "alice", "team", "github"); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("tenant B get = %v", err)
	}
	if list, err := s.ListConnections(b, "alice"); err != nil || len(list) != 0 {
		t.Errorf("tenant B list = %v, %v", list, err)
	}
	if list, err := s.ListServerConnections(b, "team", "github"); err != nil || len(list) != 0 {
		t.Errorf("tenant B server list = %v, %v", list, err)
	}
	if _, err := s.DeleteConnection(b, "alice", "team", "github"); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("tenant B delete = %v", err)
	}
	if err := s.SetConnectionStatus(b, "alice", "team", "github", ConnectionRevoked); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("tenant B set status = %v", err)
	}
	if err := s.WithConnectionLock(b, "alice", "team", "github", func(MCPConnection) (*MCPConnection, error) { return nil, nil }); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("tenant B lock = %v", err)
	}
	// Cross-user reads inside one tenant return nothing.
	if _, err := s.GetConnection(a, "mallory", "team", "github"); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("other user get = %v", err)
	}
	if list, _ := s.ListConnections(a, "alice"); len(list) != 1 || list[0].UserID != "alice" {
		t.Errorf("alice's list = %v", list)
	}
	// No scope fails closed.
	if _, err := s.GetConnection(ctx, "alice", "team", "github"); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("no scope get = %v", err)
	}
	if _, err := s.UpsertConnection(ctx, sampleConn("alice")); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("no scope upsert = %v", err)
	}
	// A tenant-scoped call cannot write a row for another tenant.
	other := sampleConn("alice")
	other.TenantID = tenantB
	if _, err := s.UpsertConnection(a, other); err == nil {
		t.Error("wrote a row for another tenant")
	}
	// The raw table, read as the non-bypass role with no scope, shows nothing; with B's scope nothing of A's.
	var n int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM mcp_connections`).Scan(&n); err != nil || n != 0 {
		t.Errorf("unscoped raw count = %d, %v", n, err)
	}
	// The admin view lists users and status and selects no token column.
	sum, err := s.ListServerConnections(a, "team", "github")
	if err != nil || len(sum) != 2 {
		t.Fatalf("summary = %v, %v", sum, err)
	}
	// Platform scope sees both tenants.
	if _, err := s.UpsertConnection(b, sampleConn("alice")); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListConnections(WithPlatform(ctx), "alice"); len(list) != 2 {
		t.Errorf("platform list = %d", len(list))
	}

}

func checkMCPConnectionUpdates(t *testing.T, s *Store, a, b context.Context) {
	t.Helper()
	// WithConnectionLock updates under the row lock.
	exp := time.Now().Add(time.Hour).UTC()
	err := s.WithConnectionLock(a, "alice", "team", "github", func(cur MCPConnection) (*MCPConnection, error) {
		cur.AccessTokenEnc = []byte("new-access")
		cur.AccessExpiresAt = &exp
		now := time.Now()
		cur.LastUsedAt = &now
		return &cur, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetConnection(a, "alice", "team", "github")
	if !bytes.Equal(got.AccessTokenEnc, []byte("new-access")) || got.AccessExpiresAt == nil || got.LastUsedAt == nil {
		t.Errorf("after lock update = %+v", got)
	}
	// Tenant B's own row was not touched.
	gotB, _ := s.GetConnection(b, "alice", "team", "github")
	if bytes.Equal(gotB.AccessTokenEnc, []byte("new-access")) {
		t.Error("update crossed tenants")
	}

	// Disconnect returns the row once.
	del, err := s.DeleteConnection(a, "alice", "team", "github")
	if err != nil || !bytes.Equal(del.RefreshTokenEnc, []byte("enc-refresh-alice")) {
		t.Fatalf("delete = %+v, %v", del, err)
	}
	if _, err := s.GetConnection(a, "alice", "team", "github"); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("after delete = %v", err)
	}
	if err := s.SetConnectionStatus(a, "bob", "team", "github", "bogus"); !errors.Is(err, ErrInvalidConnection) {
		t.Errorf("bad status = %v", err)
	}
}

func TestMCPOAuthAppsAndFlowsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := context.Background()
	a := WithTenant(ctx, tenantA)
	b := WithTenant(ctx, tenantB)

	app := MCPOAuthApp{ServerNS: "team", AppRef: "github", ClientID: "cid", ClientSecretEnc: []byte("enc-secret"), KeyID: "k1",
		RedirectURI: "https://ui.example/api/connections/callback", ScopesAllowed: []string{"read"}, UpdatedBy: "admin"}
	out, created, err := s.UpsertOAuthApp(a, app)
	if err != nil || !created || !out.HasSecret || out.TenantID != tenantA {
		t.Fatalf("register = %+v, %v, %v", out, created, err)
	}
	// Rotating the client id without a new secret keeps the secret.
	app.ClientID, app.ClientSecretEnc = "cid2", nil
	out, created, err = s.UpsertOAuthApp(a, app)
	if err != nil || created || out.ClientID != "cid2" || !bytes.Equal(out.ClientSecretEnc, []byte("enc-secret")) {
		t.Fatalf("rotate = %+v, %v, %v", out, created, err)
	}
	if _, err := s.GetOAuthApp(b, "team", "github"); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("tenant B app = %v", err)
	}
	if _, err := s.GetOAuthApp(ctx, "team", "github"); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("no scope app = %v", err)
	}
	// Tenant B registers its own app under the same name.
	if _, created, err := s.UpsertOAuthApp(b, app); err != nil || !created {
		t.Errorf("tenant B register = %v, %v", created, err)
	}

	flow := MCPOAuthFlow{UserID: "alice", ServerNS: "team", ServerName: "github", ServerUID: "uid-github", VerifierEnc: []byte("enc-v"),
		KeyID: "k1", RequestedScopes: []string{"read"}, ExpiresAt: time.Now().Add(testFlowTTL)}
	h := StateHash("state-1")
	if err := s.CreateOAuthFlow(a, h, flow); err != nil {
		t.Fatal(err)
	}
	// Another tenant cannot consume it, and failing to does not burn it.
	if _, err := s.ConsumeOAuthFlow(b, h); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("tenant B consume = %v", err)
	}
	got, err := s.ConsumeOAuthFlow(a, h)
	if err != nil || got.UserID != "alice" || got.TenantID != tenantA || got.ServerUID != "uid-github" || len(got.RequestedScopes) != 1 {
		t.Fatalf("consume = %+v, %v", got, err)
	}
	// Single use.
	if _, err := s.ConsumeOAuthFlow(a, h); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("replay = %v", err)
	}
	// Expired.
	flow.ExpiresAt = time.Now().Add(-time.Minute)
	h2 := StateHash("state-2")
	if err := s.CreateOAuthFlow(a, h2, flow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeOAuthFlow(a, h2); !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("expired = %v", err)
	}
	if err := s.CreateOAuthFlow(ctx, StateHash("x"), flow); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("no scope create = %v", err)
	}
}

// testFlowTTL is a lifetime that outlasts a test.
const testFlowTTL = 5 * time.Minute

// Two callers that both find the access token expired must not both refresh: the
// row lock makes the second wait and then see the first's update.
func TestMCPConnectionLockSerializesRefreshAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	if _, err := s.UpsertConnection(a, sampleConn("alice")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	refreshes := 0
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			err := s.WithConnectionLock(a, "alice", "team", "github", func(cur MCPConnection) (*MCPConnection, error) {
				if bytes.Equal(cur.AccessTokenEnc, []byte("refreshed")) {
					return nil, nil // somebody else already refreshed
				}
				time.Sleep(50 * time.Millisecond) // the provider call
				mu.Lock()
				refreshes++
				mu.Unlock()
				cur.AccessTokenEnc = []byte("refreshed")
				return &cur, nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
}

func TestMCPConnectionLifecycleAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := context.Background()
	a := WithTenant(ctx, tenantA)
	b := WithTenant(ctx, tenantB)
	platform := WithPlatform(ctx)

	mk := func(c context.Context, user string, key string) MCPConnection {
		in := sampleConn(user)
		in.KeyID = key
		out, err := s.UpsertConnection(c, in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	old, fresh, other := mk(a, "old", "k1"), mk(a, "fresh", "k1"), mk(b, "other", "k2")
	// "old" was last used 100 days ago, "fresh" yesterday, "other" (tenant B) never, created 100 days ago.
	for id, q := range map[int64]string{
		old.ID:   `UPDATE mcp_connections SET last_used_at = now() - interval '100 days' WHERE id = $1`,
		fresh.ID: `UPDATE mcp_connections SET last_used_at = now() - interval '1 day' WHERE id = $1`,
		other.ID: `UPDATE mcp_connections SET created_at = now() - interval '100 days' WHERE id = $1`,
	} {
		// The app role is bound by row-level security: write as the platform.
		if err := s.scoped(platform, func(tx dbtx, _ string) error { _, err := tx.ExecContext(ctx, q, id); return err }); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-90 * 24 * time.Hour)

	// Counts: platform sees everything, a tenant only its own.
	if got, err := s.CountConnectionsByStatus(platform); err != nil || got[ConnectionActive] != 3 {
		t.Errorf("platform counts = %v, %v", got, err)
	}
	if got, _ := s.CountConnectionsByStatus(a); got[ConnectionActive] != 2 {
		t.Errorf("tenant A counts = %v", got)
	}

	// Idle selection: by last use, else creation; a tenant call stays in its tenant.
	idle, err := s.ListIdleConnections(platform, cutoff, 10)
	if err != nil || len(idle) != 2 || idle[0].ID != old.ID || idle[1].ID != other.ID {
		t.Fatalf("idle = %+v, %v", idle, err)
	}
	if idle, _ := s.ListIdleConnections(a, cutoff, 10); len(idle) != 1 || idle[0].UserID != "old" {
		t.Errorf("tenant A idle = %+v", idle)
	}
	if idle, _ := s.ListIdleConnections(platform, cutoff, 1); len(idle) != 1 {
		t.Errorf("limit ignored: %+v", idle)
	}

	// Expiry wipes the tokens, returns the row as it was, and rechecks idleness.
	if _, ok, err := s.ExpireIdleConnection(platform, fresh.ID, cutoff); err != nil || ok {
		t.Errorf("expired a connection used yesterday: %v, %v", ok, err)
	}
	prev, ok, err := s.ExpireIdleConnection(platform, old.ID, cutoff)
	if err != nil || !ok || !bytes.Equal(prev.RefreshTokenEnc, []byte("enc-refresh-old")) {
		t.Fatalf("expire = %+v, %v, %v", prev, ok, err)
	}
	got, err := s.GetConnection(a, "old", "team", "github")
	if err != nil || got.Status != ConnectionExpired || len(got.RefreshTokenEnc) != 0 || len(got.AccessTokenEnc) != 0 || got.AccessExpiresAt != nil {
		t.Errorf("expired row = %+v, %v", got, err)
	}
	if _, ok, _ := s.ExpireIdleConnection(platform, old.ID, cutoff); ok {
		t.Error("an expired connection was expired again")
	}
	if idle, _ := s.ListIdleConnections(platform, cutoff, 10); len(idle) != 1 || idle[0].UserID != "other" {
		t.Errorf("idle after expiry = %+v", idle)
	}
	if got, _ := s.CountConnectionsByStatus(platform); got[ConnectionExpired] != 1 || got[ConnectionActive] != 2 {
		t.Errorf("counts after expiry = %v", got)
	}
	// A reconnect brings an expired connection back.
	if again := mk(a, "old", "k1"); again.Status != ConnectionActive {
		t.Errorf("reconnect = %+v", again)
	}

	checkRewrapAgainstPostgres(t, s, platform, fresh.ID, other.ID)
}

func checkRewrapAgainstPostgres(t *testing.T, s *Store, platform context.Context, freshID, otherID int64) {
	t.Helper()
	// Rows under a key other than k2: fresh (k1), old (k1, reconnected), other (k2 is fine).
	ids, err := s.ListConnectionsToRewrap(platform, "k2", 0, 10)
	if err != nil || len(ids) != 2 {
		t.Fatalf("to re-wrap = %v, %v", ids, err)
	}
	if page, _ := s.ListConnectionsToRewrap(platform, "k2", ids[0], 10); len(page) != 1 || page[0] != ids[1] {
		t.Errorf("cursor page = %v", page)
	}
	if ran, err := s.RewrapConnection(platform, otherID, "k2", func(MCPConnection) (*MCPConnection, error) { return nil, nil }); err != nil || ran {
		t.Errorf("re-wrapped a row already under the active key: %v, %v", ran, err)
	}
	ran, err := s.RewrapConnection(platform, freshID, "k2", func(c MCPConnection) (*MCPConnection, error) {
		c.RefreshTokenEnc, c.AccessTokenEnc, c.KeyID = []byte("rewrapped-refresh"), nil, "k2"
		return &c, nil
	})
	if err != nil || !ran {
		t.Fatalf("rewrap = %v, %v", ran, err)
	}
	got, err := s.GetConnection(WithTenant(context.Background(), tenantA), "fresh", "team", "github")
	if err != nil || got.KeyID != "k2" || string(got.RefreshTokenEnc) != "rewrapped-refresh" || len(got.AccessTokenEnc) != 0 || got.Status != ConnectionActive {
		t.Errorf("rewrapped = %+v, %v", got, err)
	}
	// A failing fn leaves the row as it was, and a bad result is refused.
	boom := errors.New("boom")
	if _, err := s.RewrapConnection(platform, ids[0], "k2", func(MCPConnection) (*MCPConnection, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("fn error = %v", err)
	}
	if _, err := s.RewrapConnection(platform, ids[0], "k2", func(c MCPConnection) (*MCPConnection, error) { c.KeyID = ""; return &c, nil }); err == nil {
		t.Error("a row without a key id was stored")
	}
	if left, _ := s.ListConnectionsToRewrap(platform, "k2", 0, 10); len(left) != 1 {
		t.Errorf("left to re-wrap = %v", left)
	}
}

// A re-wrap skips a row a refresh holds (SKIP LOCKED) and never overwrites what the
// refresh stored: the refresh seals under the active key itself.
func TestMCPRewrapSkipsRowsHeldByARefreshAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	platform := WithPlatform(context.Background())
	conn, err := s.UpsertConnection(a, sampleConn("alice"))
	if err != nil {
		t.Fatal(err)
	}
	holding, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithConnectionLock(a, "alice", "team", "github", func(cur MCPConnection) (*MCPConnection, error) {
			close(holding)
			<-release
			cur.RefreshTokenEnc, cur.KeyID = []byte("refreshed-under-k2"), "k2"
			return &cur, nil
		})
	}()
	<-holding
	ran, err := s.RewrapConnection(platform, conn.ID, "k2", func(c MCPConnection) (*MCPConnection, error) {
		t.Error("re-wrap ran on a locked row")
		return nil, nil
	})
	if err != nil || ran {
		t.Errorf("rewrap of a locked row = %v, %v", ran, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetConnection(a, "alice", "team", "github")
	if string(got.RefreshTokenEnc) != "refreshed-under-k2" || got.KeyID != "k2" {
		t.Errorf("row = %+v", got)
	}
}

func TestMCPOAuthAppRewrapAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	platform := WithPlatform(context.Background())
	if _, _, err := s.UpsertOAuthApp(a, MCPOAuthApp{ServerNS: "team", AppRef: "github", ClientID: "c", RedirectURI: "https://ui/cb",
		ClientSecretEnc: []byte("enc-secret"), KeyID: "k1", UpdatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertOAuthApp(a, MCPOAuthApp{ServerNS: "team", AppRef: "public", ClientID: "c2", RedirectURI: "https://ui/cb",
		KeyID: "k1", UpdatedBy: "admin"}); err != nil { // a public client has no secret to re-wrap
		t.Fatal(err)
	}
	ids, err := s.ListOAuthAppsToRewrap(platform, "k2", 0, 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("apps to re-wrap = %v, %v", ids, err)
	}
	ran, err := s.RewrapOAuthApp(platform, ids[0], "k2", func(app MCPOAuthApp) (*MCPOAuthApp, error) {
		app.ClientSecretEnc, app.KeyID = []byte("enc-secret-2"), "k2"
		return &app, nil
	})
	if err != nil || !ran {
		t.Fatalf("rewrap app = %v, %v", ran, err)
	}
	got, err := s.GetOAuthApp(a, "team", "github")
	if err != nil || got.KeyID != "k2" || string(got.ClientSecretEnc) != "enc-secret-2" || got.UpdatedBy != "admin" {
		t.Errorf("app = %+v, %v", got, err)
	}
	if left, _ := s.ListOAuthAppsToRewrap(platform, "k2", 0, 10); len(left) != 0 {
		t.Errorf("left = %v", left)
	}
}
