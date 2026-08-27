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
	"fmt"
	"log/slog"
	"maps"
)

// Document is a single document to ingest.
type Document struct {
	ID       string
	Content  string
	Metadata map[string]any
}

// IngestResult contains counts from an ingestion operation.
type IngestResult struct {
	DocumentCount int
	ChunkCount    int
}

// IngestDocuments chunks, embeds, and upserts documents into Qdrant.
// This is the shared pipeline used by both the API handler and the controller.
// Documents are processed one-by-one to avoid timeouts with local embedding models.
func IngestDocuments(
	ctx context.Context,
	docs []Document,
	qdrantAddr string,
	collection string,
	dimensions uint64,
	embedder *EmbeddingClient,
	chunkCfg ChunkConfig,
) (*IngestResult, error) {
	if len(docs) == 0 {
		return &IngestResult{}, nil
	}

	// Connect to Qdrant.
	qClient, err := NewQdrantClient(qdrantAddr)
	if err != nil {
		return nil, fmt.Errorf("connecting to qdrant: %w", err)
	}
	defer func() { _ = qClient.Close() }()

	// Ensure collection exists.
	if err := qClient.EnsureCollection(ctx, collection, dimensions); err != nil {
		return nil, fmt.Errorf("ensuring collection: %w", err)
	}

	slog.Info("ingesting documents", "docs", len(docs))

	totalChunks := 0
	for docIdx, doc := range docs {
		if ctx.Err() != nil {
			return &IngestResult{DocumentCount: docIdx, ChunkCount: totalChunks}, ctx.Err()
		}

		// Chunk this document.
		chunks := ChunkText(doc.ID, doc.Content, chunkCfg, doc.Metadata)
		if len(chunks) == 0 {
			continue
		}

		// Extract texts for embedding.
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}

		// Embed chunks for this document (forQuery=false → uses docPrompt).
		vectors, err := embedder.Embed(ctx, texts, false)
		if err != nil {
			return nil, fmt.Errorf("document %q: embedding chunks: %w", doc.ID, err)
		}

		// Build Qdrant points for this document.
		points := make([]Point, len(chunks))
		for i, chunk := range chunks {
			payload := map[string]any{
				"text":        chunk.Text,
				"doc_id":      chunk.DocID,
				"chunk_index": chunk.Index,
			}
			maps.Copy(payload, chunk.Metadata)
			points[i] = Point{
				ID:      pointID(chunk.DocID, chunk.Index),
				Vector:  vectors[i],
				Payload: payload,
			}
		}

		// Upsert this document's points.
		if err := qClient.Upsert(ctx, collection, points); err != nil {
			return nil, fmt.Errorf("document %q: upserting: %w", doc.ID, err)
		}
		totalChunks += len(chunks)
	}

	return &IngestResult{
		DocumentCount: len(docs),
		ChunkCount:    totalChunks,
	}, nil
}

// pointID generates a deterministic ID from doc ID and chunk index.
func pointID(docID string, chunkIndex int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s:%d", docID, chunkIndex))
	return fmt.Sprintf("%x", h[:16])
}
