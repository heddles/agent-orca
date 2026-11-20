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
	"errors"
	"testing"
)

func newWidgetKey(t *testing.T, ns, agent, name string, origins ...string) WidgetKey {
	t.Helper()
	id, err := NewWidgetKeyID()
	if err != nil {
		t.Fatal(err)
	}
	return WidgetKey{ID: id, Namespace: ns, Agent: agent, Name: name, AllowedOrigins: origins,
		RateLimitPerMin: DefaultWidgetRatePerMin, BudgetUSDPerDay: DefaultWidgetBudgetPerDay, CreatedBy: evalUser}
}

func TestWidgetKeysAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)

	k, err := s.CreateWidgetKey(a, newWidgetKey(t, evalNSA, "support", "site", "https://Example.com", "https://example.com/", "http://localhost:3000"))
	if err != nil {
		t.Fatal(err)
	}
	if k.TenantID != tenantA || !k.Enabled || len(k.AllowedOrigins) != 2 || k.AllowedOrigins[0] != "https://example.com" || k.AllowedOrigins[1] != "http://localhost:3000" {
		t.Fatalf("created = %+v", k)
	}
	if !k.OriginAllowed("https://EXAMPLE.com") || k.OriginAllowed("https://evil.example.com") || k.OriginAllowed("null") || k.OriginAllowed("") {
		t.Error("OriginAllowed")
	}
	if _, err := s.CreateWidgetKey(a, newWidgetKey(t, evalNSA, "support", "site", "https://x.example")); !errors.Is(err, ErrWidgetKeyExists) {
		t.Errorf("duplicate name = %v", err)
	}

	checkWidgetKeyIsolation(t, s, b, k)

	off := false
	rate, budget := 5, 0.5
	u, err := s.UpdateWidgetKey(a, k.ID, WidgetKeyPatch{Enabled: &off, AllowedOrigins: []string{"https://shop.example"}, RateLimitPerMin: &rate, BudgetUSDPerDay: &budget})
	if err != nil || u.Enabled || u.RateLimitPerMin != 5 || u.BudgetUSDPerDay != 0.5 || len(u.AllowedOrigins) != 1 || u.AllowedOrigins[0] != "https://shop.example" {
		t.Fatalf("update = %+v, %v", u, err)
	}
	bad := []string{"*"}
	if _, err := s.UpdateWidgetKey(a, k.ID, WidgetKeyPatch{AllowedOrigins: bad}); !errors.Is(err, ErrInvalidWidgetKey) {
		t.Errorf("wildcard origin = %v", err)
	}
	zero := 0.0
	if _, err := s.UpdateWidgetKey(a, k.ID, WidgetKeyPatch{BudgetUSDPerDay: &zero}); !errors.Is(err, ErrInvalidWidgetKey) {
		t.Errorf("zero budget = %v", err)
	}
	if list, _ := s.ListWidgetKeys(a, []string{evalNSA}, "support"); len(list) != 1 {
		t.Errorf("list = %v", list)
	}
	if _, err := s.DeleteWidgetKey(a, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWidgetKey(a, k.ID); !errors.Is(err, ErrWidgetKeyNotFound) {
		t.Errorf("after delete = %v", err)
	}
}

// checkWidgetKeyIsolation checks that tenant b sees none of tenant a's key k, by any path.
func checkWidgetKeyIsolation(t *testing.T, s *Store, b context.Context, k WidgetKey) {
	t.Helper()
	// Another tenant sees none of it, by any path.
	if _, err := s.GetWidgetKey(b, k.ID); !errors.Is(err, ErrWidgetKeyNotFound) {
		t.Errorf("other tenant get = %v", err)
	}
	if list, err := s.ListWidgetKeys(b, nil, ""); err != nil || len(list) != 0 {
		t.Errorf("other tenant list = %v, %v", list, err)
	}
	off := false
	if _, err := s.UpdateWidgetKey(b, k.ID, WidgetKeyPatch{Enabled: &off}); !errors.Is(err, ErrWidgetKeyNotFound) {
		t.Errorf("other tenant update = %v", err)
	}
	if _, err := s.DeleteWidgetKey(b, k.ID); !errors.Is(err, ErrWidgetKeyNotFound) {
		t.Errorf("other tenant delete = %v", err)
	}
	foreign := newWidgetKey(t, evalNSB, "support", "x", "https://x.example")
	foreign.TenantID = tenantA
	if _, err := s.CreateWidgetKey(b, foreign); err == nil {
		t.Error("a row of another tenant was accepted")
	}
	if _, err := s.GetWidgetKey(context.Background(), k.ID); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped get = %v", err)
	}
	if p, err := s.GetWidgetKey(WithPlatform(context.Background()), k.ID); err != nil || p.TenantID != tenantA {
		t.Errorf("platform get = %+v, %v", p, err)
	}

}

func TestWidgetKeyLimitAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	for i := range MaxWidgetKeysPerAgent {
		if _, err := s.CreateWidgetKey(a, newWidgetKey(t, evalNSA, "busy", "k"+string(rune('a'+i)), "https://x.example")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateWidgetKey(a, newWidgetKey(t, evalNSA, "busy", "one-more", "https://x.example")); !errors.Is(err, ErrTooManyWidgetKeys) {
		t.Errorf("over the limit = %v", err)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Example.com":      "https://example.com",
		"https://example.com/":     "https://example.com",
		"https://example.com:8443": "https://example.com:8443",
		"http://localhost:3000":    "http://localhost:3000",
		"http://127.0.0.1:5173":    "http://127.0.0.1:5173",
	} {
		if got, err := NormalizeOrigin(in); err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "*", "https://*.example.com", "http://example.com", "https://example.com/path", "https://u:p@example.com",
		"https://example.com?x=1", "ftp://example.com", "example.com", "null", "javascript:alert(1)", "https://"} {
		if _, err := NormalizeOrigin(in); !errors.Is(err, ErrInvalidWidgetKey) {
			t.Errorf("NormalizeOrigin(%q) accepted", in)
		}
	}
	if _, err := NormalizeOrigins(nil); err == nil {
		t.Error("no origins accepted")
	}
}
