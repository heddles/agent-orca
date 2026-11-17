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

package rag

import (
	"context"
	"errors"
	"testing"
)

// A QdrantClient call with no tenant ID is refused before it reaches Qdrant, so
// no caller can read or write unfiltered. The nil client proves nothing was sent.
func TestQdrantCallsRequireATenant(t *testing.T) {
	q := &QdrantClient{}
	ctx := context.Background()
	if _, err := q.Search(ctx, "c", "", []float32{1}, 3); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("Search: %v", err)
	}
	if err := q.Upsert(ctx, "c", "", []Point{{ID: "x"}}); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("Upsert: %v", err)
	}
	if _, err := q.ExistingPoints(ctx, "c", "", []string{"x"}); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("ExistingPoints: %v", err)
	}
	if _, err := q.CollectionStats(ctx, "c", ""); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("CollectionStats: %v", err)
	}
	if err := q.BackfillTenant(ctx, "c", ""); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("BackfillTenant: %v", err)
	}
	if _, err := IngestDocuments(ctx, []Document{{ID: "d"}}, "localhost:1", "c", 2, nil, ChunkConfig{}, ""); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("IngestDocuments: %v", err)
	}
}

func TestTenantFilterMatchesOnlyTheTenant(t *testing.T) {
	f := tenantFilter("tnt-a")
	if len(f.GetMust()) != 1 {
		t.Fatalf("filter = %v", f)
	}
	m := f.GetMust()[0].GetField()
	if m.GetKey() != TenantPayloadKey || m.GetMatch().GetKeyword() != "tnt-a" {
		t.Errorf("condition = %v", m)
	}
}

// The same document and chunk from two tenants must get different point IDs,
// so tenant B's upsert lands on a new point instead of overwriting tenant A's.
func TestPointIDsNeverCollideAcrossTenants(t *testing.T) {
	for _, doc := range []string{"doc", "url/http://example.com/a.pdf", ""} {
		for idx := range 3 {
			if pointID("tnt-a", doc, idx) == pointID("tnt-b", doc, idx) {
				t.Errorf("tenants share point ID for doc %q chunk %d", doc, idx)
			}
		}
	}
	// Shifting a separator between tenant and document must not collide either.
	if pointID("ab", "c", 0) == pointID("a", "bc", 0) {
		t.Error("tenant/doc boundary is ambiguous")
	}
}

func TestCollectionNameForIncludesTheTenant(t *testing.T) {
	if CollectionNameFor("tnt-a", "docs") == CollectionNameFor("tnt-b", "docs") {
		t.Error("same-named KnowledgeBases of two tenants share a collection")
	}
	if got := CollectionNameFor("", "docs"); got != "docs" {
		t.Errorf("no tenant: got %q, want the bare name", got)
	}
}

func TestMigrateLegacyPointIDsRequiresATenant(t *testing.T) {
	if _, err := (&QdrantClient{}).MigrateLegacyPointIDs(context.Background(), "c", ""); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("MigrateLegacyPointIDs: %v", err)
	}
}
