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
