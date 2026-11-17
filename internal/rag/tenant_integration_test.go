//go:build integration

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
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
)

// Two tenants share one collection (same KnowledgeBase name): each searches
// only its own vectors, and a payload field cannot name another tenant.
func TestTenantIsolationInSharedCollection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	q, err := NewQdrantClient(testQdrantAddr)
	if err != nil {
		t.Fatalf("connecting to qdrant (is it running?): %v", err)
	}
	defer func() { _ = q.Close() }()

	// Start from an empty collection: the legacy point this test backfills
	// into tnt-a would otherwise survive into the next run.
	const collection = "tenant_isolation_test"
	_ = q.client.DeleteCollection(ctx, collection)
	defer func() { _ = q.client.DeleteCollection(ctx, collection) }()
	if err := q.EnsureCollection(ctx, collection, 2); err != nil {
		t.Fatal(err)
	}
	a := Point{ID: pointID("tnt-a", "doc-a", 0), Vector: []float32{1, 0}, Payload: map[string]any{"doc_id": "a", "text": "a-secret"}}
	// B tries to claim A's tenant through its payload; the client overrides it.
	b := Point{ID: pointID("tnt-a", "doc-b", 0), Vector: []float32{1, 0}, Payload: map[string]any{"doc_id": "b", "text": "b-text", TenantPayloadKey: "tnt-a"}}
	if err := q.Upsert(ctx, collection, "tnt-a", []Point{a}); err != nil {
		t.Fatal(err)
	}
	if err := q.Upsert(ctx, collection, "tnt-b", []Point{b}); err != nil {
		t.Fatal(err)
	}

	for tenant, want := range map[string]string{"tnt-a": "a-secret", "tnt-b": "b-text"} {
		res, err := q.Search(ctx, collection, tenant, []float32{1, 0}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Payload["text"] != want {
			t.Errorf("%s sees %+v, want only %q", tenant, res, want)
		}
	}
	if st, err := q.CollectionStats(ctx, collection, "tnt-a"); err != nil || st.PointCount != 1 || st.DocumentCount != 1 {
		t.Errorf("tnt-a stats = %+v, %v", st, err)
	}

	// A point written without a tenant (before tenant IDs existed) is invisible
	// to every tenant until the owner's backfill stamps it.
	legacyPoint := &pb.PointStruct{Id: pb.NewID(pointID("tnt-a", "doc-legacy", 0)), Vectors: pb.NewVectorsDense([]float32{1, 0}),
		Payload: pb.NewValueMap(map[string]any{"doc_id": "legacy", "text": "legacy"})}
	wait := true
	if _, err := q.client.Upsert(ctx, &pb.UpsertPoints{CollectionName: collection, Wait: &wait, Points: []*pb.PointStruct{legacyPoint}}); err != nil {
		t.Fatal(err)
	}
	if res, _ := q.Search(ctx, collection, "tnt-a", []float32{1, 0}, 10); len(res) != 1 {
		t.Errorf("an unattributed point leaked to tnt-a: %+v", res)
	}
	if err := q.BackfillTenant(ctx, collection, "tnt-a"); err != nil {
		t.Fatal(err)
	}
	if res, _ := q.Search(ctx, collection, "tnt-a", []float32{1, 0}, 10); len(res) != 2 {
		t.Errorf("after backfill tnt-a sees %d points, want 2", len(res))
	}
	if res, _ := q.Search(ctx, collection, "tnt-b", []float32{1, 0}, 10); len(res) != 1 {
		t.Errorf("backfill for tnt-a changed tnt-b's view: %+v", res)
	}
}

// Tenant B cannot overwrite tenant A's point: the same document ID yields a
// different point, and an upsert that names A's exact point ID is refused.
func TestTenantCannotOverwriteAnotherTenantsPoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	q, err := NewQdrantClient(testQdrantAddr)
	if err != nil {
		t.Fatalf("connecting to qdrant (is it running?): %v", err)
	}
	defer func() { _ = q.Close() }()

	const collection = "tenant_overwrite_test"
	_ = q.client.DeleteCollection(ctx, collection)
	defer func() { _ = q.client.DeleteCollection(ctx, collection) }()
	if err := q.EnsureCollection(ctx, collection, 2); err != nil {
		t.Fatal(err)
	}
	pa := Point{ID: pointID("tnt-a", "doc", 0), Vector: []float32{1, 0}, Payload: map[string]any{"text": "a-original"}}
	if err := q.Upsert(ctx, collection, "tnt-a", []Point{pa}); err != nil {
		t.Fatal(err)
	}

	// Same document ID from B: a different point, A's is untouched.
	pbPoint := Point{ID: pointID("tnt-b", "doc", 0), Vector: []float32{1, 0}, Payload: map[string]any{"text": "b-text"}}
	if err := q.Upsert(ctx, collection, "tnt-b", []Point{pbPoint}); err != nil {
		t.Fatal(err)
	}
	// B naming A's exact ID is refused.
	evil := Point{ID: pa.ID, Vector: []float32{0, 1}, Payload: map[string]any{"text": "b-overwrite"}}
	if err := q.Upsert(ctx, collection, "tnt-b", []Point{evil}); !errors.Is(err, ErrTenantConflict) {
		t.Fatalf("upsert onto A's point: %v, want ErrTenantConflict", err)
	}
	res, err := q.Search(ctx, collection, "tnt-a", []float32{1, 0}, 10)
	if err != nil || len(res) != 1 || res[0].Payload["text"] != "a-original" {
		t.Errorf("tnt-a sees %+v, %v; want only its original point", res, err)
	}
}

// MigrateLegacyPointIDs moves pre-tenant point IDs to tenant-derived ones
// without re-embedding, and a second run is a no-op.
func TestMigrateLegacyPointIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	q, err := NewQdrantClient(testQdrantAddr)
	if err != nil {
		t.Fatalf("connecting to qdrant (is it running?): %v", err)
	}
	defer func() { _ = q.Close() }()

	const collection = "tenant_migrate_test"
	_ = q.client.DeleteCollection(ctx, collection)
	defer func() { _ = q.client.DeleteCollection(ctx, collection) }()
	if err := q.EnsureCollection(ctx, collection, 2); err != nil {
		t.Fatal(err)
	}
	legacy := &pb.PointStruct{Id: pb.NewID(legacyTestID("doc", 0)), Vectors: pb.NewVectorsDense([]float32{1, 0}),
		Payload: pb.NewValueMap(map[string]any{"doc_id": "doc", "chunk_index": 0, "text": "old"})}
	wait := true
	if _, err := q.client.Upsert(ctx, &pb.UpsertPoints{CollectionName: collection, Wait: &wait, Points: []*pb.PointStruct{legacy}}); err != nil {
		t.Fatal(err)
	}
	if err := q.BackfillTenant(ctx, collection, "tnt-a"); err != nil {
		t.Fatal(err)
	}
	if n, err := q.MigrateLegacyPointIDs(ctx, collection, "tnt-a"); err != nil || n != 1 {
		t.Fatalf("migrated %d, %v; want 1", n, err)
	}
	if n, err := q.MigrateLegacyPointIDs(ctx, collection, "tnt-a"); err != nil || n != 0 {
		t.Fatalf("second run migrated %d, %v; want 0", n, err)
	}
	got, err := q.ExistingPoints(ctx, collection, "tnt-a", []string{pointID("tnt-a", "doc", 0), legacyTestID("doc", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[pointID("tnt-a", "doc", 0)]; !ok || len(got) != 1 {
		t.Errorf("points after migration = %v, want only the tenant-derived ID", got)
	}
}

// legacyTestID is the pre-tenant ID scheme, kept here only to seed old data.
func legacyTestID(docID string, idx int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s:%d", docID, idx))
	return formatUUID(h[:16])
}
