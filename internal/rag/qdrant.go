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
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
)

var qdrantHTTPClient = &http.Client{Timeout: 10 * time.Second}

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
	if slices.Contains(collections, name) {
		return nil
	}

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
func (q *QdrantClient) Upsert(ctx context.Context, collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}

	pbPoints := make([]*pb.PointStruct, len(points))
	for i, p := range points {
		pbPoints[i] = &pb.PointStruct{
			Id:      pb.NewID(p.ID),
			Vectors: pb.NewVectorsDense(p.Vector),
			Payload: pb.NewValueMap(p.Payload),
		}
	}

	wait := true
	_, err := q.client.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: collection,
		Wait:           &wait,
		Points:         pbPoints,
	})
	if err != nil {
		return fmt.Errorf("upserting %d points to %q: %w", len(points), collection, err)
	}
	return nil
}

// Search performs a nearest neighbor search and returns the top-K results.
func (q *QdrantClient) Search(ctx context.Context, collection string, vector []float32, topK int) ([]SearchResult, error) {
	limit := uint64(topK)
	scored, err := q.client.Query(ctx, &pb.QueryPoints{
		CollectionName: collection,
		Query:          pb.NewQueryDense(vector),
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
func (q *QdrantClient) CollectionStats(ctx context.Context, collection string) (*CollectionStats, error) {
	info, err := q.client.GetCollectionInfo(ctx, collection)
	if err != nil {
		return nil, fmt.Errorf("getting collection info for %q: %w", collection, err)
	}

	stats := &CollectionStats{
		PointCount: info.GetPointsCount(),
	}

	// Count unique doc_id values by scrolling with minimal payload.
	docIDs := map[string]struct{}{}
	var offset *pb.PointId
	batchSize := uint32(250)
	for {
		points, next, err := q.client.ScrollAndOffset(ctx, &pb.ScrollPoints{
			CollectionName: collection,
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
