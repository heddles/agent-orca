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
	"log/slog"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// DefaultIdleAfter is how long a connection may go unused before it expires.
const DefaultIdleAfter = 90 * 24 * time.Hour

// LifecycleStore is the slice of postgresql.Store the lifecycle jobs use.
type LifecycleStore interface {
	ListIdleConnections(ctx context.Context, cutoff time.Time, limit int) ([]postgresql.IdleConnection, error)
	ExpireIdleConnection(ctx context.Context, id int64, cutoff time.Time) (postgresql.MCPConnection, bool, error)
	ListConnectionsToRewrap(ctx context.Context, activeKeyID string, afterID int64, limit int) ([]int64, error)
	RewrapConnection(ctx context.Context, id int64, activeKeyID string, fn func(postgresql.MCPConnection) (*postgresql.MCPConnection, error)) (bool, error)
	ListOAuthAppsToRewrap(ctx context.Context, activeKeyID string, afterID int64, limit int) ([]int64, error)
	RewrapOAuthApp(ctx context.Context, id int64, activeKeyID string, fn func(postgresql.MCPOAuthApp) (*postgresql.MCPOAuthApp, error)) (bool, error)
}

// Lifecycle runs the background jobs of the connections: expiring idle
// connections and re-wrapping rows under the active key. Run it on the leader.
type Lifecycle struct {
	Store LifecycleStore
	Svc   *Service
	// Resolve finds the MCPServer a connection is for, to revoke at the provider.
	// An error (the server is gone) skips the revocation, not the expiry.
	Resolve func(ctx context.Context, ns, name string) (ServerRef, error)
	// OnExpire is called after a connection expired, so the caller can audit it.
	OnExpire func(ctx context.Context, c postgresql.MCPConnection, revoked string)
	// IdleAfter is the idle period; zero or negative turns the expiry off.
	IdleAfter time.Duration
	// Interval between passes; zero means ten minutes.
	Interval  time.Duration
	BatchSize int
	Now       func() time.Time
}

func (l *Lifecycle) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Lifecycle) batch() int {
	if l.BatchSize > 0 {
		return l.BatchSize
	}
	return 100
}

// Start runs the jobs until ctx ends. It satisfies controller-runtime's Runnable,
// which the manager runs on the elected leader only.
func (l *Lifecycle) Start(ctx context.Context) error {
	interval := l.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		l.pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (l *Lifecycle) pass(ctx context.Context) {
	ctx = postgresql.WithPlatform(ctx)
	if _, err := l.ExpireIdle(ctx); err != nil {
		slog.Error("MCP connection idle expiry failed", "err", err)
	}
	if _, err := l.RotateKeys(ctx); err != nil {
		slog.Error("MCP connection key rotation failed", "err", err)
	}
}

// ExpireIdle expires connections unused for IdleAfter: it wipes their tokens and
// asks the provider to revoke the refresh token (best effort; the row is expired
// either way). It returns how many were expired.
func (l *Lifecycle) ExpireIdle(ctx context.Context) (int, error) {
	if l.IdleAfter <= 0 {
		return 0, nil
	}
	cutoff := l.now().Add(-l.IdleAfter)
	total := 0
	for {
		idle, err := l.Store.ListIdleConnections(ctx, cutoff, l.batch())
		if err != nil {
			return total, err
		}
		expired := 0
		for _, c := range idle {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			prev, ok, err := l.Store.ExpireIdleConnection(ctx, c.ID, cutoff)
			if err != nil {
				slog.Error("expiring an idle MCP connection failed", "server", c.ServerNS+"/"+c.ServerName, "err", err)
				continue
			}
			if !ok {
				continue
			}
			expired++
			total++
			l.finishExpiry(ctx, prev)
		}
		// A full page of rows that could not be expired (locked, failing) would
		// repeat forever: stop when a page made no progress or was the last one.
		if expired == 0 || len(idle) < l.batch() {
			return total, nil
		}
	}
}

func (l *Lifecycle) finishExpiry(ctx context.Context, prev postgresql.MCPConnection) {
	outcome := "not_revoked"
	switch {
	case l.Svc == nil || l.Svc.Keys == nil || l.Resolve == nil:
	default:
		ref, err := l.Resolve(ctx, prev.ServerNS, prev.ServerName)
		if err != nil || ref.URL == "" || ref.UID != prev.ServerUID {
			break // the server is gone, was recreated, or opted out of revocation
		}
		revoked, rerr := l.Svc.revokeAtProvider(ctx, prev.TenantID, prev, ref)
		switch {
		case rerr != nil:
			outcome = "failed"
			slog.Warn("revoking an expired MCP connection at the provider failed", "server", prev.ServerNS+"/"+prev.ServerName, "err", rerr)
		case revoked:
			outcome = "revoked"
		}
	}
	idleExpired.WithLabelValues(outcome).Inc()
	if l.OnExpire != nil {
		l.OnExpire(ctx, prev, outcome)
	}
}

// RotateResult is what one rotation pass did.
type RotateResult struct {
	Rewrapped int
	Failed    int
	// Skipped rows were busy (a refresh held them) or already moved.
	Skipped int
}

// RotateKeys re-wraps every connection row that is not under the active key.
// It works in batches by id, so it is resumable (the work list is "key_id is not
// the active key", nothing is remembered between passes) and it never holds more
// than one row lock at a time. A row that cannot be re-wrapped is logged by id and
// counted, and the pass goes on. The refresh and access token are re-wrapped
// together, with the same AAD they were sealed under; an access token that cannot
// be opened is dropped (the next use refreshes), a refresh token that cannot be
// opened fails the row.
func (l *Lifecycle) RotateKeys(ctx context.Context) (RotateResult, error) {
	var res RotateResult
	if l.Svc == nil || l.Svc.Keys == nil {
		return res, nil
	}
	active := l.Svc.Keys.ActiveID()
	l.sweep(ctx, &res, func(after int64) ([]int64, error) {
		return l.Store.ListConnectionsToRewrap(ctx, active, after, l.batch())
	}, func(id int64) (bool, error) { return l.Store.RewrapConnection(ctx, id, active, l.rewrap) })
	// OAuth app client secrets sit under the same keys; the old key cannot be
	// removed until they have moved too. Pending flows live five minutes and are
	// not rotated.
	l.sweep(ctx, &res, func(after int64) ([]int64, error) {
		return l.Store.ListOAuthAppsToRewrap(ctx, active, after, l.batch())
	}, func(id int64) (bool, error) { return l.Store.RewrapOAuthApp(ctx, id, active, l.rewrapApp) })
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	// What is left under another key: failures and rows skipped as busy.
	pending := float64(res.Failed + res.Skipped)
	rotationPending.Set(pending)
	if res.Rewrapped+res.Failed+res.Skipped > 0 {
		slog.Info("MCP connection key rotation pass", "activeKey", active, "rewrapped", res.Rewrapped, "failed", res.Failed, "skipped", res.Skipped)
	}
	return res, nil
}

// sweep walks the ids list returns in batches (by id, so a failing row is passed
// over, not retried in a loop) and runs one on each.
func (l *Lifecycle) sweep(ctx context.Context, res *RotateResult, list func(after int64) ([]int64, error), one func(id int64) (bool, error)) {
	var after int64
	for ctx.Err() == nil {
		ids, err := list(after)
		if err != nil {
			slog.Error("listing rows to re-wrap failed", "err", err)
			res.Failed++
			rotationRows.WithLabelValues("failed").Inc()
			return
		}
		if len(ids) == 0 {
			return
		}
		for _, id := range ids {
			after = id
			ran, err := one(id)
			switch {
			case err != nil:
				res.Failed++
				rotationRows.WithLabelValues("failed").Inc()
				slog.Error("re-wrapping a row failed", "rowID", id, "err", err)
			case !ran:
				res.Skipped++
			default:
				res.Rewrapped++
				rotationRows.WithLabelValues("rewrapped").Inc()
			}
		}
	}
}

func (l *Lifecycle) rewrapApp(cur postgresql.MCPOAuthApp) (*postgresql.MCPOAuthApp, error) {
	keyID, blob, err := l.Svc.Keys.Rewrap(cur.KeyID, cur.ClientSecretEnc, AAD("client_secret", cur.TenantID, cur.ServerNS, cur.AppRef))
	if err != nil {
		return nil, fmt.Errorf("client secret: %w", err)
	}
	next := cur
	next.ClientSecretEnc, next.KeyID = blob, keyID
	return &next, nil
}

func (l *Lifecycle) rewrap(cur postgresql.MCPConnection) (*postgresql.MCPConnection, error) {
	keys := l.Svc.Keys
	keyID, rt, err := keys.Rewrap(cur.KeyID, cur.RefreshTokenEnc, AAD("refresh", cur.TenantID, cur.UserID, cur.ServerUID))
	if err != nil {
		// err names a key id or "decrypt", never key material or a token.
		return nil, fmt.Errorf("refresh token: %w", err)
	}
	next := cur
	next.RefreshTokenEnc, next.KeyID = rt, keyID
	if len(cur.AccessTokenEnc) > 0 {
		accKey, acc, err := keys.Rewrap(cur.KeyID, cur.AccessTokenEnc, AAD("access", cur.TenantID, cur.UserID, cur.ServerUID))
		switch {
		case err == nil && accKey == keyID:
			next.AccessTokenEnc = acc
		case err == nil, errors.Is(err, ErrDecrypt), errors.Is(err, ErrUnknownKey):
			// One key id per row: an access token that cannot join the refresh
			// token's key is dropped, and the next use refreshes.
			next.AccessTokenEnc, next.AccessExpiresAt = nil, nil
		default:
			return nil, fmt.Errorf("access token: %w", err)
		}
	}
	return &next, nil
}
