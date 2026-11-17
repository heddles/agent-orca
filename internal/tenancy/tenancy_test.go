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

package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	idA = "tnt_01935c7e-1111-7aaa-8bbb-000000000001"
	idB = "tnt_01935c7e-2222-7aaa-8bbb-000000000002"
)

func TestNewIDIsValid(t *testing.T) {
	a, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewID()
	if !ValidID(a) || a == b {
		t.Fatalf("NewID() = %q, %q: want two distinct valid IDs", a, b)
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{idA, true},
		{"", false},
		{"acme", false},
		{"01935c7e-1111-7aaa-8bbb-000000000001", false},                             // no prefix
		{"tnt_01935c7e-1111-4aaa-8bbb-000000000001", false},                         // UUIDv4
		{"tnt_01935C7E-1111-7AAA-8BBB-000000000001", false},                         // upper case
		{"tnt_01935c7e-1111-7aaa-8bbb-00000000000", false},                          // short
		{"tnt_01935c7e-1111-7aaa-8bbb-000000000001-extra", false},                   // trailing text
		{"x_tnt_01935c7e-1111-7aaa-8bbb-000000000001", false},                       // leading text
		{"tnt_01935c7e-1111-7aaa-cbbb-000000000001", false},                         // bad variant
		{"tnt_01935c7e-1111-7aaa-8bbb-000000000001\ntnt_x", false},                  // newline
		{"tnt_01935c7e-1111-7aaa-8bbb-000000000001 ", false},                        // space
		{"tnt_01935c7e-1111-7aaa-8bbb-00000000000g", false},                         // non-hex
		{"tnt_ffffffff-ffff-7fff-bfff-ffffffffffff", true},                          // bounds
		{"tnt_00000000-0000-7000-8000-000000000000", true},                          // bounds
		{"tnt_00000000-0000-8000-8000-000000000000", false},                         // UUIDv8
		{"tnt_00000000-0000-7000-7000-000000000000", false},                         // variant 7
		{"tnt_00000000-0000-7000-8000-0000000000000", false},                        // long
		{"tnt_0000000-00000-7000-8000-000000000000", false},                         // misplaced dash
		{"TNT_00000000-0000-7000-8000-000000000000", false},                         // upper prefix
		{"tnt_00000000000070008000000000000000", false},                             // no dashes
		{"tnt_{00000000-0000-7000-8000-000000000000}", false},                       // braces
		{"tnt_00000000-0000-7000-8000-000000000000" + string(rune(0)), false},       // NUL
		{"tnt_00000000-0000-7000-8000-000000000000/../../etc", false},               // path
		{"tnt_00000000-0000-7000-8000-000000000000'; DROP TABLE tenants;--", false}, // injection
		{"tnt_00000000-0000-7000-8000-00000000000١", false},                         // non-ASCII digit
		{"tnt_00000000-0000-7000-8000-000000000000\r", false},                       // CR
		{" tnt_00000000-0000-7000-8000-000000000000", false},                        // leading space
		{"tnt_00000000-0000-7000-8000-000000000000tnt_", false},                     // suffix
		{"tnt_00000000-0000-7000-8000-000000000000.", false},                        // dot
		{"tnt__00000000-0000-7000-8000-000000000000", false},                        // double underscore
		{"tnt-00000000-0000-7000-8000-000000000000", false},                         // dash separator
		{"tnt_00000000-0000-7000-9000-000000000000", true},                          // variant 9
		{"tnt_00000000-0000-7000-a000-000000000000", true},                          // variant a
		{"tnt_00000000-0000-7000-b000-000000000000", true},                          // variant b
		{"tnt_00000000-0000-7000-c000-000000000000", false},                         // variant c
	}
	for _, tt := range tests {
		if got := ValidID(tt.id); got != tt.want {
			t.Errorf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestSync(t *testing.T) {
	ctx := context.Background()
	live := func(id, uid string) Record {
		return Record{ID: id, Name: "acme", Namespace: "acme-ns", UID: uid, State: StateActive, CreatedAt: t0}
	}
	deleted := func(id, uid string) Record {
		r := live(id, uid)
		r.State, r.DeletedAt = StateDeleted, &t0
		return r
	}
	claim := Claim{Name: "acme", Namespace: "acme-ns", UID: "uid-new"}

	tests := []struct {
		name     string
		seed     []Record
		claim    Claim
		wantID   string // "" means any fresh valid ID that is not in avoid
		avoid    []string
		wantErr  error
		wantLive map[string]State
	}{
		{
			name:  "create generates an ID",
			claim: claim,
		},
		{
			name:   "pinned fresh ID is registered",
			claim:  Claim{Name: "acme", Namespace: "acme-ns", UID: "u", Pinned: idA},
			wantID: idA,
		},
		{
			name:   "mirrored ID is kept",
			seed:   []Record{live(idA, "uid-new")},
			claim:  Claim{Name: "acme", Namespace: "acme-ns", UID: "uid-new", Current: idA},
			wantID: idA,
		},
		{
			name:   "registry rebuilt from the mirror",
			claim:  Claim{Name: "acme", Namespace: "acme-ns", UID: "uid-new", Current: idA},
			wantID: idA,
		},
		{
			name:  "same-name recreate gets a new ID",
			seed:  []Record{deleted(idA, "uid-old")},
			claim: claim,
			avoid: []string{idA},
			wantLive: map[string]State{
				idA: StateDeleted,
			},
		},
		{
			name:  "a deleted mirrored ID is not revived",
			seed:  []Record{deleted(idA, "uid-old")},
			claim: Claim{Name: "acme", Namespace: "acme-ns", UID: "uid-new", Current: idA},
			avoid: []string{idA},
		},
		{
			name:     "pinning a deleted ID restores it",
			seed:     []Record{deleted(idA, "uid-old")},
			claim:    Claim{Name: "acme", Namespace: "acme-ns", UID: "uid-new", Pinned: idA},
			wantID:   idA,
			wantLive: map[string]State{idA: StateActive},
		},
		{
			name:    "pinned ID of another live TenantConfig is refused",
			seed:    []Record{live(idA, "uid-other")},
			claim:   Claim{Name: "other", Namespace: "other-ns", UID: "uid-new", Pinned: idA},
			wantErr: ErrIDInUse,
		},
		{
			name:     "an orphaned live record of the same name is retired",
			seed:     []Record{live(idB, "uid-forced")},
			claim:    claim,
			avoid:    []string{idB},
			wantLive: map[string]State{idB: StateDeleted},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewMemoryRegistry()
			for _, r := range tt.seed {
				_ = reg.PutTenant(ctx, r)
			}
			id, err := Sync(ctx, reg, tt.claim, t0.Add(time.Hour))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantID != "" && id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
			if !ValidID(id) {
				t.Errorf("id %q is not valid", id)
			}
			for _, a := range tt.avoid {
				if id == a {
					t.Errorf("id = %q, must not reuse it", id)
				}
			}
			rec, _ := reg.GetTenant(ctx, id)
			if rec == nil || rec.State != StateActive || rec.UID != tt.claim.UID || rec.Name != tt.claim.Name {
				t.Errorf("registry record for %s = %+v, want active and owned by the claim", id, rec)
			}
			for want, state := range tt.wantLive {
				if got, _ := reg.GetTenant(ctx, want); got == nil || got.State != state {
					t.Errorf("record %s = %+v, want state %s", want, got, state)
				}
			}
		})
	}
}

func TestMarkDeletedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	reg := NewMemoryRegistry()
	_ = reg.PutTenant(ctx, Record{ID: idA, Name: "acme", Namespace: "ns", State: StateActive, CreatedAt: t0})
	later := t0.Add(time.Hour)
	for range 2 {
		if err := MarkDeleted(ctx, reg, idA, later); err != nil {
			t.Fatal(err)
		}
		later = later.Add(time.Hour)
	}
	rec, _ := reg.GetTenant(ctx, idA)
	if rec.State != StateDeleted || rec.DeletedAt == nil || !rec.DeletedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("record = %+v, want deleted at the first call", rec)
	}
	if err := MarkDeleted(ctx, reg, idB, later); err != nil {
		t.Errorf("unknown ID: %v, want no error", err)
	}
}
