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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
)

var qdrantHTTPClient = &http.Client{Timeout: 10 * time.Second}

// TenantPayloadKey is the payload field that holds a point's tenant ID. Every
// point carries it and every read filters on it.
//
// Isolation strategy: one shared collection per KnowledgeBase with a mandatory
// tenant_id payload filter, rather than a collection per tenant. Qdrant's
// guidance for multitenancy is a payload partition (a keyword index marked
// is_tenant co-locates a tenant's vectors and keeps filtered search fast); a
// collection per tenant costs memory and file handles per tenant and does not
// scale to many small tenants. The filter is applied inside QdrantClient, which
// refuses calls with no tenant ID, so no caller can search unfiltered.
const TenantPayloadKey = "tenant_id"

// ErrTenantRequired is returned by a QdrantClient call that has no tenant ID.
var ErrTenantRequired = errors.New("qdrant: a tenant ID is required")

// ErrTenantConflict is returned by Upsert when a point ID is already owned by
// another tenant. Point IDs derive from the tenant ID, so this only happens if
// a caller supplies IDs of its own; the write is refused rather than
// overwriting the other tenant's point.
var ErrTenantConflict = errors.New("qdrant: point ID belongs to another tenant")

// tenantFilter matches only the tenant's points.
func tenantFilter(tenantID string) *pb.Filter {
	return &pb.Filter{Must: []*pb.Condition{pb.NewMatch(TenantPayloadKey, tenantID)}}
}

// QdrantClient wraps the official Qdrant Go gRPC client for vector operations.
type QdrantClient struct {
	client *pb.Client
}

// NewQdrantClient connects to a Qdrant instance at the given address.
// The address should be in "host:port" format (gRPC port, typically 6334).
func NewQdrantClient(addr string) (*QdrantClient, error) {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parsing qdrant address %q: %w", addr, err)
	}
	client, err := pb.NewClient(&pb.Config{
		Host:                   host,
		Port:                   port,
		SkipCompatibilityCheck: true, // Operator may talk to older Qdrant instances that predate this client version.
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to qdrant at %s: %w", addr, err)
	}
	return &QdrantClient{client: client}, nil
}

// EnsureCollection creates a collection if it doesn't already exist.
func (q *QdrantClient) EnsureCollection(ctx context.Context, name string, dimensions uint64) error {
	// Check if collection exists first.
	collections, err := q.client.ListCollections(ctx)
	if err != nil {
		return fmt.Errorf("listing collections: %w", err)
	}
	if !slices.Contains(collections, name) {
		err = q.client.CreateCollection(ctx, &pb.CreateCollection{
			CollectionName: name,
			VectorsConfig: pb.NewVectorsConfig(&pb.VectorParams{
				Size:     dimensions,
				Distance: pb.Distance_Cosine,
			}),
		})
		if err != nil {
			return fmt.Errorf("creating collection %q: %w", name, err)
		}
	}
	// The tenant index is created on existing collections too; creating an index
	// that exists is a no-op.
	isTenant := true
	if _, err := q.client.CreateFieldIndex(ctx, &pb.CreateFieldIndexCollection{
		CollectionName:   name,
		FieldName:        TenantPayloadKey,
		FieldType:        pb.FieldType_FieldTypeKeyword.Enum(),
		FieldIndexParams: pb.NewPayloadIndexParamsKeyword(&pb.KeywordIndexParams{IsTenant: &isTenant}),
	}); err != nil {
		return fmt.Errorf("indexing %s on %q: %w", TenantPayloadKey, name, err)
	}
	return nil
}

// BackfillTenant stamps tenantID on the collection's points that have no tenant
// yet (those ingested before tenant IDs existed). Points that already carry a
// tenant are left alone, so it is safe to repeat.
func (q *QdrantClient) BackfillTenant(ctx context.Context, collection, tenantID string) error {
	if tenantID == "" {
		return ErrTenantRequired
	}
	wait := true
	_, err := q.client.SetPayload(ctx, &pb.SetPayloadPoints{
		CollectionName: collection,
		Wait:           &wait,
		Payload:        pb.NewValueMap(map[string]any{TenantPayloadKey: tenantID}),
		PointsSelector: pb.NewPointsSelectorFilter(&pb.Filter{Must: []*pb.Condition{pb.NewIsEmpty(TenantPayloadKey)}}),
	})
	if err != nil {
		return fmt.Errorf("backfilling %s on %q: %w", TenantPayloadKey, collection, err)
	}
	return nil
}

// Point represents a vector with its metadata for upsert operations.
type Point struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}

// SearchResult represents a single search hit with its similarity score.
type SearchResult struct {
	Score   float32
	Payload map[string]any
}

// Upsert inserts or updates points in the given collection.
func (q *QdrantClient) Upsert(ctx context.Context, collection, tenantID string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	if tenantID == "" {
		return ErrTenantRequired
	}

	// Defence in depth behind tenant-derived point IDs: never overwrite a point
	// another tenant owns. Points with no tenant (pre-backfill) may be claimed.
	ids := make([]string, len(points))
	for i, p := range points {
		ids[i] = p.ID
	}
	owners, err := q.pointOwners(ctx, collection, ids)
	if err != nil {
		return err
	}
	for id, owner := range owners {
		if owner != "" && owner != tenantID {
			return fmt.Errorf("%w: %s in %q", ErrTenantConflict, id, collection)
		}
	}

	pbPoints := make([]*pb.PointStruct, len(points))
	for i, p := range points {
		payload := make(map[string]any, len(p.Payload)+1)
		maps.Copy(payload, p.Payload)
		// Set last: source metadata must not be able to name another tenant.
		payload[TenantPayloadKey] = tenantID
		pbPoints[i] = &pb.PointStruct{
			Id:      pb.NewID(p.ID),
			Vectors: pb.NewVectorsDense(p.Vector),
			Payload: pb.NewValueMap(payload),
		}
	}

	wait := true
	_, err = q.client.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: collection,
		Wait:           &wait,
		Points:         pbPoints,
	})
	if err != nil {
		return fmt.Errorf("upserting %d points to %q: %w", len(points), collection, err)
	}
	return nil
}

// ExistingPoints returns, for each of the given point IDs that already exists
// in the collection, the stored "content_hash" payload value (empty string
// for points written before content hashing was introduced). Missing points
// are absent from the map.
func (q *QdrantClient) ExistingPoints(ctx context.Context, collection, tenantID string, ids []string) (map[string]string, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	existing := make(map[string]string, len(ids))
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		batch := ids[start:end]
		pbIDs := make([]*pb.PointId, len(batch))
		for i, id := range batch {
			pbIDs[i] = pb.NewID(id)
		}
		// The input IDs are canonical hyphenated UUIDs (see pointID); Qdrant
		// returns point keys in the same canonical form, so GetUuid keys map
		// 1:1 back onto the input IDs.
		points, err := q.client.Get(ctx, &pb.GetPoints{
			CollectionName: collection,
			Ids:            pbIDs,
			WithPayload: &pb.WithPayloadSelector{
				SelectorOptions: &pb.WithPayloadSelector_Include{
					Include: &pb.PayloadIncludeSelector{Fields: []string{"content_hash", TenantPayloadKey}},
				},
			},
			WithVectors: &pb.WithVectorsSelector{
				SelectorOptions: &pb.WithVectorsSelector_Enable{Enable: false},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("fetching %d existing points from %q: %w", len(batch), collection, err)
		}
		for _, p := range points {
			// A point another tenant owns does not exist for this one.
			if owner, ok := p.GetPayload()[TenantPayloadKey]; ok && owner.GetStringValue() != tenantID {
				continue
			}
			hash := ""
			if v, ok := p.GetPayload()["content_hash"]; ok {
				hash = v.GetStringValue()
			}
			existing[p.GetId().GetUuid()] = hash
		}
	}
	return existing, nil
}

// Search performs a nearest neighbor search and returns the top-K results.
func (q *QdrantClient) Search(ctx context.Context, collection, tenantID string, vector []float32, topK int) ([]SearchResult, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	limit := uint64(topK)
	scored, err := q.client.Query(ctx, &pb.QueryPoints{
		CollectionName: collection,
		Query:          pb.NewQueryDense(vector),
		Filter:         tenantFilter(tenantID),
		Limit:          &limit,
		WithPayload:    pb.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("querying collection %q: %w", collection, err)
	}

	results := make([]SearchResult, len(scored))
	for i, sp := range scored {
		results[i] = SearchResult{
			Score:   sp.GetScore(),
			Payload: payloadToMap(sp.GetPayload()),
		}
	}
	return results, nil
}

// CollectionStats returns the number of points and unique documents in the
// collection, along with the on-disk storage size in bytes.
type CollectionStats struct {
	PointCount    uint64
	DocumentCount int
}

// CollectionStats returns point count, unique document count, and storage size.
func (q *QdrantClient) CollectionStats(ctx context.Context, collection, tenantID string) (*CollectionStats, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	exact := true
	count, err := q.client.Count(ctx, &pb.CountPoints{CollectionName: collection, Filter: tenantFilter(tenantID), Exact: &exact})
	if err != nil {
		return nil, fmt.Errorf("counting points in %q: %w", collection, err)
	}

	stats := &CollectionStats{
		PointCount: count,
	}

	// Count unique doc_id values by scrolling with minimal payload.
	docIDs := map[string]struct{}{}
	var offset *pb.PointId
	batchSize := uint32(250)
	for {
		points, next, err := q.client.ScrollAndOffset(ctx, &pb.ScrollPoints{
			CollectionName: collection,
			Filter:         tenantFilter(tenantID),
			Limit:          &batchSize,
			Offset:         offset,
			WithPayload: &pb.WithPayloadSelector{
				SelectorOptions: &pb.WithPayloadSelector_Include{
					Include: &pb.PayloadIncludeSelector{Fields: []string{"doc_id"}},
				},
			},
			WithVectors: &pb.WithVectorsSelector{
				SelectorOptions: &pb.WithVectorsSelector_Enable{Enable: false},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("scrolling collection %q for doc counts: %w", collection, err)
		}
		for _, p := range points {
			if v, ok := p.GetPayload()["doc_id"]; ok {
				if s := v.GetStringValue(); s != "" {
					docIDs[s] = struct{}{}
				}
			}
		}
		if next == nil || len(points) == 0 {
			break
		}
		offset = next
	}
	stats.DocumentCount = len(docIDs)

	return stats, nil
}

// Close releases the underlying gRPC connection.
func (q *QdrantClient) Close() error {
	return q.client.Close()
}

// payloadToMap converts Qdrant protobuf payload to a Go map.
func payloadToMap(payload map[string]*pb.Value) map[string]any {
	if payload == nil {
		return nil
	}
	m := make(map[string]any, len(payload))
	for k, v := range payload {
		m[k] = valueToInterface(v)
	}
	return m
}

func valueToInterface(v *pb.Value) any {
	if v == nil {
		return nil
	}
	switch kind := v.GetKind().(type) {
	case *pb.Value_StringValue:
		return kind.StringValue
	case *pb.Value_IntegerValue:
		return kind.IntegerValue
	case *pb.Value_DoubleValue:
		return kind.DoubleValue
	case *pb.Value_BoolValue:
		return kind.BoolValue
	case *pb.Value_NullValue:
		return nil
	default:
		return fmt.Sprintf("%v", v)
	}
}

// QdrantServerVersion queries the Qdrant HTTP root endpoint and returns the
// reported server version string (e.g. "1.17.1"). The addr should be
// "host:httpPort" (typically port 6333).
func QdrantServerVersion(ctx context.Context, httpAddr string) (string, error) {
	url := fmt.Sprintf("http://%s/", httpAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating version request: %w", err)
	}
	resp, err := qdrantHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying qdrant version at %s: %w", httpAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("reading version response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qdrant version endpoint returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing version response: %w", err)
	}
	if result.Version == "" {
		return "", fmt.Errorf("qdrant version response missing version field")
	}
	return result.Version, nil
}

// CreateCollectionSnapshot triggers a full snapshot of the given collection
// via the Qdrant HTTP API and returns the snapshot name. The addr should be
// "host:httpPort" (typically port 6333).
func CreateCollectionSnapshot(ctx context.Context, httpAddr, collection string) (string, error) {
	url := fmt.Sprintf("http://%s/collections/%s/snapshots", httpAddr, collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating snapshot request: %w", err)
	}
	resp, err := qdrantHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating snapshot for %q at %s: %w", collection, httpAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("reading snapshot response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("snapshot endpoint returned %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Result struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing snapshot response: %w", err)
	}
	if result.Result.Name == "" {
		return "", fmt.Errorf("snapshot response missing name field")
	}
	return result.Result.Name, nil
}

func splitHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// Maybe no port — check if it looks like host:port
		if strings.Contains(addr, ":") {
			return "", 0, err
		}
		return addr, 6334, nil
	}
	var port int
	_, err = fmt.Sscanf(portStr, "%d", &port)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	return host, port, nil
}

// pointOwners returns the stored tenant ID of each of the given point IDs that
// exists (empty string for a point with no tenant). Missing points are absent.
func (q *QdrantClient) pointOwners(ctx context.Context, collection string, ids []string) (map[string]string, error) {
	owners := make(map[string]string)
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		batch := ids[start:min(start+batchSize, len(ids))]
		pbIDs := make([]*pb.PointId, len(batch))
		for i, id := range batch {
			pbIDs[i] = pb.NewID(id)
		}
		points, err := q.client.Get(ctx, &pb.GetPoints{
			CollectionName: collection,
			Ids:            pbIDs,
			WithPayload: &pb.WithPayloadSelector{
				SelectorOptions: &pb.WithPayloadSelector_Include{
					Include: &pb.PayloadIncludeSelector{Fields: []string{TenantPayloadKey}},
				},
			},
			WithVectors: &pb.WithVectorsSelector{SelectorOptions: &pb.WithVectorsSelector_Enable{Enable: false}},
		})
		if err != nil {
			return nil, fmt.Errorf("checking owners of %d points in %q: %w", len(batch), collection, err)
		}
		for _, p := range points {
			owners[p.GetId().GetUuid()] = p.GetPayload()[TenantPayloadKey].GetStringValue()
		}
	}
	return owners, nil
}

// MigrateLegacyPointIDs rewrites the tenant's points that still carry a
// pre-tenant point ID (a hash of document ID and chunk index only) to the
// tenant-derived ID, keeping vector and payload, then deletes the old point.
// It needs no re-embedding, is idempotent, and only touches points the tenant
// already owns (run BackfillTenant first so legacy points are stamped).
// Points whose payload lacks doc_id or chunk_index cannot be re-derived and
// are left under their old ID: still tenant-filtered on read, but not covered
// by the cross-tenant overwrite guarantee until re-ingested. It returns the
// number of points moved.
func (q *QdrantClient) MigrateLegacyPointIDs(ctx context.Context, collection, tenantID string) (int, error) {
	if tenantID == "" {
		return 0, ErrTenantRequired
	}
	moved := 0
	var offset *pb.PointId
	limit := uint32(100)
	for {
		points, next, err := q.client.ScrollAndOffset(ctx, &pb.ScrollPoints{
			CollectionName: collection,
			Filter:         tenantFilter(tenantID),
			Limit:          &limit,
			Offset:         offset,
			WithPayload:    pb.NewWithPayload(true),
			WithVectors:    &pb.WithVectorsSelector{SelectorOptions: &pb.WithVectorsSelector_Enable{Enable: true}},
		})
		if err != nil {
			return moved, fmt.Errorf("scrolling %q for point migration: %w", collection, err)
		}
		var upserts []*pb.PointStruct
		var stale []*pb.PointId
		for _, p := range points {
			docID := p.GetPayload()["doc_id"].GetStringValue()
			idx := p.GetPayload()["chunk_index"]
			vec := p.GetVectors().GetVector().GetDense().GetData()
			if docID == "" || idx == nil || len(vec) == 0 {
				continue
			}
			want := pointID(tenantID, docID, int(idx.GetIntegerValue()))
			if p.GetId().GetUuid() == want {
				continue
			}
			upserts = append(upserts, &pb.PointStruct{Id: pb.NewID(want), Vectors: pb.NewVectorsDense(vec), Payload: p.GetPayload()})
			stale = append(stale, p.GetId())
		}
		if len(upserts) > 0 {
			wait := true
			if _, err := q.client.Upsert(ctx, &pb.UpsertPoints{CollectionName: collection, Wait: &wait, Points: upserts}); err != nil {
				return moved, fmt.Errorf("writing migrated points to %q: %w", collection, err)
			}
			if _, err := q.client.Delete(ctx, &pb.DeletePoints{CollectionName: collection, Wait: &wait, Points: pb.NewPointsSelector(stale...)}); err != nil {
				return moved, fmt.Errorf("deleting legacy points from %q: %w", collection, err)
			}
			moved += len(upserts)
		}
		if next == nil || len(points) == 0 {
			break
		}
		offset = next
	}
	return moved, nil
}
