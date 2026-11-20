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
	"time"
)

func newTrigger(t *testing.T, ns, agent, name string) (Trigger, string) {
	t.Helper()
	id, secret, err := NewTriggerCredentials()
	if err != nil {
		t.Fatal(err)
	}
	return Trigger{ID: id, Namespace: ns, Agent: agent, Name: name, SigningKey: SigningKeyFor(secret),
		RateLimitPerMin: DefaultTriggerRatePerMin, CreatedBy: evalUser}, secret
}

func TestTriggersAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	b := WithTenant(context.Background(), tenantB)

	tr, secret := newTrigger(t, evalNSA, "support", "github")
	got, err := s.CreateTrigger(a, tr)
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != tenantA || !got.Enabled || got.SigningKey != SigningKeyFor(secret) || got.SigningKey == secret {
		t.Fatalf("created = %+v", got)
	}
	if _, err := s.CreateTrigger(a, func() Trigger { x, _ := newTrigger(t, evalNSA, "support", "github"); return x }()); !errors.Is(err, ErrTriggerExists) {
		t.Errorf("duplicate name err = %v", err)
	}

	// Another tenant sees none of it, by any path.
	if _, err := s.GetTrigger(b, tr.ID); !errors.Is(err, ErrTriggerNotFound) {
		t.Errorf("other tenant get = %v", err)
	}
	if list, err := s.ListTriggers(b, nil, ""); err != nil || len(list) != 0 {
		t.Errorf("other tenant list = %v, %v", list, err)
	}
	if _, err := s.SetTriggerEnabled(b, tr.ID, false); !errors.Is(err, ErrTriggerNotFound) {
		t.Errorf("other tenant disable = %v", err)
	}
	if _, err := s.DeleteTrigger(b, tr.ID); !errors.Is(err, ErrTriggerNotFound) {
		t.Errorf("other tenant delete = %v", err)
	}
	// A tenant cannot write a row for another tenant.
	other, _ := newTrigger(t, evalNSB, "support", "x")
	other.TenantID = tenantA
	if _, err := s.CreateTrigger(b, other); err == nil {
		t.Error("a row of another tenant was accepted")
	}
	// No scope, no access.
	if _, err := s.GetTrigger(context.Background(), tr.ID); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped get = %v", err)
	}
	// The platform scope (the webhook endpoint) finds it by id and sees the tenant.
	if p, err := s.GetTrigger(WithPlatform(context.Background()), tr.ID); err != nil || p.TenantID != tenantA {
		t.Errorf("platform get = %+v, %v", p, err)
	}

	list, err := s.ListTriggers(a, []string{evalNSA}, "support")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list, _ := s.ListTriggers(a, []string{evalNSB}, ""); len(list) != 0 {
		t.Errorf("namespace filter = %v", list)
	}

	if err := s.RecordTriggerFire(a, tr.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	fired, _ := s.GetTrigger(a, tr.ID)
	if fired.FireCount != 1 || fired.LastFiredAt == nil {
		t.Errorf("fire not recorded: %+v", fired)
	}
	off, err := s.SetTriggerEnabled(a, tr.ID, false)
	if err != nil || off.Enabled {
		t.Errorf("disable = %+v, %v", off, err)
	}
	if _, err := s.DeleteTrigger(a, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTrigger(a, tr.ID); !errors.Is(err, ErrTriggerNotFound) {
		t.Errorf("after delete = %v", err)
	}
}

func TestTriggerLimitAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	for i := range MaxTriggersPerAgent {
		tr, _ := newTrigger(t, evalNSA, "busy", "t"+string(rune('a'+i)))
		if _, err := s.CreateTrigger(a, tr); err != nil {
			t.Fatal(err)
		}
	}
	tr, _ := newTrigger(t, evalNSA, "busy", "one-more")
	if _, err := s.CreateTrigger(a, tr); !errors.Is(err, ErrTooManyTriggers) {
		t.Errorf("over the limit = %v", err)
	}
}

func TestTriggerValidate(t *testing.T) {
	good, _ := newTrigger(t, "ns", "agent", "n")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Trigger){
		"no namespace": func(x *Trigger) { x.Namespace = "" },
		"bad agent":    func(x *Trigger) { x.Agent = "Bad Agent" },
		"no name":      func(x *Trigger) { x.Name = " " },
		"rate zero":    func(x *Trigger) { x.RateLimitPerMin = 0 },
		"rate high":    func(x *Trigger) { x.RateLimitPerMin = MaxTriggerRatePerMin + 1 },
		"bad id":       func(x *Trigger) { x.ID = "trg_short" },
		"no creator":   func(x *Trigger) { x.CreatedBy = "" },
	} {
		x := good
		mut(&x)
		if err := x.Validate(); !errors.Is(err, ErrInvalidTrigger) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ValidTriggerID("trg_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") || !ValidTriggerID(good.ID) {
		t.Error("ValidTriggerID")
	}
}
