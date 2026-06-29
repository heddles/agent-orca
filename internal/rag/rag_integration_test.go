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

// Integration test for the RAG pipeline.
//
// Prerequisites:
//   docker run -d --name qdrant -p 6333:6333 -p 6334:6334 qdrant/qdrant:v1.17.1
//   ollama pull nomic-embed-text
//
// Run:
//   go test -tags integration -v ./internal/rag/ -run TestRAG

package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testQdrantAddr = "localhost:6334"
	testOllamaURL  = "http://localhost:11434"
	testModel      = "nomic-embed-text"
	testCollection = "rag_integration_test"
	testDimensions = 768 // nomic-embed-text outputs 768-dim vectors
)

func TestRAGEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Write a temp file to act as the "API key file" (Ollama doesn't need one, but the client expects a file).
	keyDir := t.TempDir()
	keyFile := filepath.Join(keyDir, "api-key")
	if err := os.WriteFile(keyFile, []byte("ollama-no-key"), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Connect to Qdrant and clean up any previous test collection.
	qClient, err := NewQdrantClient(testQdrantAddr)
	if err != nil {
		t.Fatalf("connecting to qdrant (is it running?): %v", err)
	}
	// Delete the test collection if it exists from a previous run.
	_ = qClient.client.DeleteCollection(ctx, testCollection)
	qClient.Close()

	// 2. Create embedding client pointing at Ollama.
	embedder := NewEmbeddingClient(testOllamaURL, keyFile, testModel)

	// Quick smoke test: embed a single string.
	vectors, err := embedder.Embed(ctx, []string{"hello world"})
	if err != nil {
		t.Fatalf("embedding smoke test failed (is ollama running with %s?): %v", testModel, err)
	}
	if len(vectors) != 1 || len(vectors[0]) != testDimensions {
		t.Fatalf("expected 1 vector of %d dims, got %d vectors", testDimensions, len(vectors))
	}
	t.Logf("embedding smoke test passed: got %d-dim vector", len(vectors[0]))

	// 3. Ingest test documents via the pipeline.
	docs := []Document{
		{
			ID:       "doc-kubernetes",
			Content:  "Kubernetes is an open-source container orchestration platform. It automates deployment, scaling, and management of containerized applications. Pods are the smallest deployable units in Kubernetes.",
			Metadata: map[string]interface{}{"topic": "infrastructure"},
		},
		{
			ID:       "doc-rag",
			Content:  "Retrieval-Augmented Generation combines information retrieval with text generation. Documents are chunked, embedded into vectors, and stored in a vector database. At query time, relevant chunks are retrieved and provided as context to a language model.",
			Metadata: map[string]interface{}{"topic": "ai"},
		},
		{
			ID:       "doc-golang",
			Content:  "Go is a statically typed, compiled programming language designed at Google. It features garbage collection, structural typing, and CSP-style concurrency. The standard library includes packages for HTTP servers, JSON encoding, and cryptography.",
			Metadata: map[string]interface{}{"topic": "programming"},
		},
	}

	chunkCfg := ChunkConfig{ChunkSize: 128, ChunkOverlap: 16}

	result, err := IngestDocuments(ctx, docs, testQdrantAddr, testCollection, testDimensions, embedder, chunkCfg)
	if err != nil {
		t.Fatalf("IngestDocuments failed: %v", err)
	}
	t.Logf("ingested %d docs → %d chunks", result.DocumentCount, result.ChunkCount)

	if result.DocumentCount != 3 {
		t.Errorf("expected 3 documents, got %d", result.DocumentCount)
	}
	if result.ChunkCount == 0 {
		t.Error("expected at least 1 chunk")
	}

	// 4. Search for relevant documents.
	qClient2, err := NewQdrantClient(testQdrantAddr)
	if err != nil {
		t.Fatalf("reconnecting to qdrant: %v", err)
	}
	defer qClient2.Close()

	tests := []struct {
		query       string
		expectTopic string
	}{
		{"container orchestration and pods", "infrastructure"},
		{"vector database and embeddings", "ai"},
		{"compiled programming language with goroutines", "programming"},
	}

	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			// Embed the query.
			qVec, err := embedder.Embed(ctx, []string{tc.query})
			if err != nil {
				t.Fatalf("embedding query: %v", err)
			}

			results, err := qClient2.Search(ctx, testCollection, qVec[0], 3)
			if err != nil {
				t.Fatalf("search failed: %v", err)
			}

			if len(results) == 0 {
				t.Fatal("no results returned")
			}

			topResult := results[0]
			t.Logf("top result: score=%.4f text=%q", topResult.Score, truncate(topResult.Payload["text"], 80))

			// Check the top result's topic matches what we expect.
			if topic, ok := topResult.Payload["topic"]; ok {
				if topic != tc.expectTopic {
					t.Errorf("expected topic %q in top result, got %q", tc.expectTopic, topic)
				}
			}
		})
	}

	// 5. Cleanup.
	_ = qClient2.client.DeleteCollection(ctx, testCollection)
	t.Log("test collection cleaned up")
}

func truncate(v interface{}, maxLen int) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
