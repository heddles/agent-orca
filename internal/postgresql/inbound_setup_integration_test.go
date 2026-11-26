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
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestInboundGuidedSetupAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)
	plat := WithPlatform(context.Background())
	c := newInbound(t, evalNSA, "guided")
	if _, err := s.CreateInboundConnection(a, c); err != nil {
		t.Fatal(err)
	}

	// Credentials: only the owner can set them, and they replace the Secret's name.
	cfg := json.RawMessage(`{"allowSenders":["x"]}`)
	if _, err := s.SetInboundCredentials(b, c.ID, "k1", []byte("sealed"), cfg); !errors.Is(err, ErrInboundNotFound) {
		t.Errorf("other tenant set = %v", err)
	}
	got, err := s.SetInboundCredentials(a, c.ID, "k1", []byte("sealed"), cfg)
	if err != nil || string(got.CredentialsEnc) != "sealed" || got.CredentialsKeyID != "k1" || got.SecretRef != "" {
		t.Fatalf("set = %+v, %v", got, err)
	}
	// Key rotation finds them and replaces them in place.
	if ids, err := s.ListInboundCredentialsToRewrap(plat, "k2"); err != nil || len(ids) != 1 || ids[0] != c.ID {
		t.Errorf("to re-wrap = %v, %v", ids, err)
	}
	ran, err := s.RewrapInboundCredentials(plat, c.ID, "k2", func(cur InboundConnection) (string, []byte, error) {
		return "k2", append([]byte("re-"), cur.CredentialsEnc...), nil
	})
	if err != nil || !ran {
		t.Fatalf("re-wrap = %v, %v", ran, err)
	}
	if got, _ = s.GetInboundConnection(a, c.ID); got.CredentialsKeyID != "k2" || string(got.CredentialsEnc) != "re-sealed" {
		t.Errorf("after re-wrap = %q %q", got.CredentialsKeyID, got.CredentialsEnc)
	}
	if ids, _ := s.ListInboundCredentialsToRewrap(plat, "k2"); len(ids) != 0 {
		t.Errorf("still to re-wrap: %v", ids)
	}
	// An unrelated change keeps them; naming a Secret drops them.
	off := false
	if got, err = s.UpdateInboundConnection(a, c.ID, InboundPatch{Enabled: &off}); err != nil || len(got.CredentialsEnc) == 0 {
		t.Fatalf("after disable = %+v, %v", got, err)
	}
	ref := c.SecretRef
	if got, err = s.UpdateInboundConnection(a, c.ID, InboundPatch{SecretRef: &ref}); err != nil || got.CredentialsEnc != nil || got.CredentialsKeyID != "" {
		t.Fatalf("after naming a Secret = %+v, %v", got, err)
	}

	if err := s.RecordInboundRequest(a, c.ID, "invalid signature"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetInboundConnection(a, c.ID); got.LastAuthError != "invalid signature" || got.LastAuthErrorAt == nil || got.LastRequestAt != nil {
		t.Errorf("after a refused request = %+v", got)
	}
	if err := s.RecordInboundRequest(a, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetInboundConnection(a, c.ID); got.LastAuthError != "" || got.LastAuthErrorAt != nil || got.LastRequestAt == nil {
		t.Errorf("after a good request = %+v", got)
	}

	checkPairings(t, s, a, b, plat, c)
}

// checkPairings: one code at a time, used once, invisible to other tenants, gone
// when it expires or the connection is deleted.
func checkPairings(t *testing.T, s *Store, a, b, plat context.Context, c InboundConnection) {
	t.Helper()
	pair := func(id, hash string, ttl time.Duration) {
		t.Helper()
		if err := s.CreateInboundPairing(a, InboundPairing{ID: id, ConnectionID: c.ID, CodeHash: hash, CreatedBy: "u", ExpiresAt: time.Now().Add(ttl)}); err != nil {
			t.Fatal(err)
		}
	}
	pair("p1", "h1", time.Minute)
	pair("p2", "h2", time.Minute)
	if _, err := s.GetInboundPairing(a, c.ID, "p1"); !errors.Is(err, ErrPairingNotFound) {
		t.Errorf("a replaced code = %v", err)
	}
	seen := func(ctx context.Context, hash, who string) bool {
		t.Helper()
		ok, err := s.ReceiveInboundPairing(ctx, InboundPairing{ConnectionID: c.ID, CodeHash: hash, SenderID: who, ChannelID: "C1", Mode: "mention", ReplyTarget: json.RawMessage(`{"c":"C1"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if seen(a, "h1", "x") || seen(b, "h2", "x") {
		t.Error("a replaced code or another tenant matched")
	}
	if !seen(a, "h2", "first") || seen(a, "h2", "second") {
		t.Error("the code must work exactly once")
	}
	if _, err := s.TakeInboundPairing(b, c.ID, "p2"); !errors.Is(err, ErrPairingNotFound) {
		t.Errorf("other tenant take = %v", err)
	}
	taken, err := s.TakeInboundPairing(a, c.ID, "p2")
	if err != nil || taken.SenderID != "first" || taken.ChannelID != "C1" || taken.ReceivedAt == nil || len(taken.ReplyTarget) == 0 {
		t.Fatalf("take = %+v, %v", taken, err)
	}
	if _, err := s.TakeInboundPairing(a, c.ID, "p2"); !errors.Is(err, ErrPairingNotFound) {
		t.Errorf("second take = %v", err)
	}
	pair("p3", "h3", -time.Second)
	if seen(a, "h3", "late") {
		t.Error("an expired code matched")
	}
	if err := s.DeleteExpiredInboundPairings(plat); err != nil {
		t.Fatal(err)
	}

	pair("p4", "h4", time.Minute)
	if _, err := s.DeleteInboundConnection(a, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInboundPairing(a, c.ID, "p4"); !errors.Is(err, ErrPairingNotFound) {
		t.Errorf("after deleting the connection = %v", err)
	}
}
