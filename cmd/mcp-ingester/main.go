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

// Command mcp-ingester runs inside a Kubernetes Job to fetch documents from an
// MCP server, chunk and embed them, and upsert vectors directly into Qdrant.
// The KnowledgeBase controller queries Qdrant directly for counts.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"

	"github.com/floppyfish14/agent-orc/internal/mcp"
	"github.com/floppyfish14/agent-orc/internal/rag"
)

// IngestConfig is the JSON configuration mounted from the config ConfigMap.
type IngestConfig struct {
	// MCP server connection.
	Transport  string   `json:"transport"`           // stdio, http, sse
	URL        string   `json:"url,omitempty"`       // http/sse endpoint
	BinaryCmd  string   `json:"binaryCmd,omitempty"` // stdio binary path
	BinaryArgs []string `json:"binaryArgs,omitempty"`
	Env        []string `json:"env,omitempty"` // KEY=VALUE pairs for stdio process

	// Discover/fetch workflow.
	DiscoverTool      string          `json:"discoverTool,omitempty"`
	DiscoverArgs      json.RawMessage `json:"discoverArgs,omitempty"`
	FetchTool         string          `json:"fetchTool"`
	FetchArgs         json.RawMessage `json:"fetchArgs"` // may contain {{item}}
	ItemExtractor     string          `json:"itemExtractor"`
	ItemField         string          `json:"itemField,omitempty"`
	ItemFilterKey     string          `json:"itemFilterKey,omitempty"`
	ItemFilterVal     string          `json:"itemFilterVal,omitempty"`
	DiscoverRecursive bool            `json:"discoverRecursive,omitempty"`
	DirFilterKey      string          `json:"dirFilterKey,omitempty"`
	DirFilterVal      string          `json:"dirFilterVal,omitempty"`
	IncludePatterns   []string        `json:"includePatterns,omitempty"`
	ExcludePatterns   []string        `json:"excludePatterns,omitempty"`
	MaxItems          int             `json:"maxItems"`

	// Document ID template.
	MCPServerName      string `json:"mcpServerName"`
	DocumentIDTemplate string `json:"documentIDTemplate,omitempty"`

	// Embedding configuration.
	EmbeddingEndpoint string `json:"embeddingEndpoint"`
	EmbeddingModel    string `json:"embeddingModel"`
	EmbeddingKeyFile  string `json:"embeddingKeyFile"`
	EmbeddingDims     int    `json:"embeddingDims"`
	ChunkSize         int    `json:"chunkSize"`
	ChunkOverlap      int    `json:"chunkOverlap"`

	// Qdrant destination.
	QdrantURL      string `json:"qdrantURL"`
	CollectionName string `json:"collectionName"`
}

const configPath = "/etc/mcp-ingest/config.json"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx); err != nil {
		slog.Error("mcp-ingester failed", "err", err)
		os.Exit(1)
	}
	slog.Info("mcp-ingester completed successfully")
}

func run(ctx context.Context) error {
	// Load config from mounted ConfigMap.
	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading config at %s: %w", configPath, err)
	}
	var cfg IngestConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Connect to MCP server.
	serverCfg := mcp.ServerConfig{
		Name:      cfg.MCPServerName,
		Transport: mcp.Transport(cfg.Transport),
		URL:       cfg.URL,
		Cmd:       cfg.BinaryCmd,
		Args:      cfg.BinaryArgs,
		Env:       cfg.Env,
	}
	mcpClient := mcp.New(ctx, []mcp.ServerConfig{serverCfg})
	defer mcpClient.Close()

	if len(mcpClient.Tools()) == 0 {
		return fmt.Errorf("MCP server %q returned no tools", cfg.MCPServerName)
	}

	// Run discover/fetch workflow to get documents.
	docs, err := runWorkflow(ctx, mcpClient, &cfg)
	if err != nil {
		return fmt.Errorf("workflow: %w", err)
	}

	slog.Info("Fetched documents", "count", len(docs))

	if len(docs) == 0 {
		slog.Info("No documents to ingest")
		return nil
	}

	// Chunk, embed, and upsert directly into Qdrant.
	embedder := rag.NewEmbeddingClient(cfg.EmbeddingEndpoint, cfg.EmbeddingKeyFile, cfg.EmbeddingModel)

	chunkCfg := rag.ChunkConfig{
		ChunkSize:    cfg.ChunkSize,
		ChunkOverlap: cfg.ChunkOverlap,
	}
	if chunkCfg.ChunkSize == 0 {
		chunkCfg = rag.DefaultChunkConfig()
	}

	result, err := rag.IngestDocuments(ctx, docs, cfg.QdrantURL, cfg.CollectionName, uint64(cfg.EmbeddingDims), embedder, chunkCfg)
	if err != nil {
		return fmt.Errorf("ingesting documents: %w", err)
	}

	slog.Info("Ingestion complete", "documents", result.DocumentCount, "chunks", result.ChunkCount)
	return nil
}

func runWorkflow(ctx context.Context, client *mcp.Client, cfg *IngestConfig) ([]rag.Document, error) {
	// Determine items to fetch.
	var items []string

	if cfg.DiscoverTool != "" {
		var err error
		if cfg.DiscoverRecursive {
			items, err = discoverRecursive(ctx, client, cfg)
		} else {
			items, err = discoverFlat(ctx, client, cfg)
		}
		if err != nil {
			return nil, err
		}

		// Apply include/exclude glob patterns.
		items = filterByPatterns(items, cfg.IncludePatterns, cfg.ExcludePatterns)

		if cfg.MaxItems > 0 && len(items) > cfg.MaxItems {
			slog.Info("Capping items", "total", len(items), "max", cfg.MaxItems)
			items = items[:cfg.MaxItems]
		}
		slog.Info("Discovered items", "count", len(items))
	} else {
		// No discover step — fetch runs once with static args.
		items = []string{""}
	}

	// Build document ID template.
	idTmpl := cfg.DocumentIDTemplate
	if idTmpl == "" {
		idTmpl = "mcp/{{.MCPServer}}/{{.Item}}"
	}
	tmpl, err := template.New("docID").Parse(idTmpl)
	if err != nil {
		return nil, fmt.Errorf("parsing document ID template: %w", err)
	}

	// Fetch each item.
	var docs []rag.Document
	for i, item := range items {
		if ctx.Err() != nil {
			return docs, ctx.Err()
		}

		fetchArgs := substituteItem(string(cfg.FetchArgs), item)

		result, err := client.Call(ctx, cfg.FetchTool, fetchArgs)
		if err != nil {
			slog.Warn("Fetch failed, skipping", "item", item, "err", err)
			continue
		}

		if strings.TrimSpace(result) == "" {
			continue
		}

		// Generate document ID.
		var idBuf bytes.Buffer
		_ = tmpl.Execute(&idBuf, map[string]string{
			"Item":      item,
			"MCPServer": cfg.MCPServerName,
			"Tool":      cfg.FetchTool,
		})

		docs = append(docs, rag.Document{
			ID:      idBuf.String(),
			Content: result,
			Metadata: map[string]interface{}{
				"source":    "mcp",
				"mcpServer": cfg.MCPServerName,
				"tool":      cfg.FetchTool,
				"item":      item,
			},
		})

		if (i+1)%50 == 0 {
			slog.Info("Fetch progress", "completed", i+1, "total", len(items))
		}
	}

	return docs, nil
}

func extractItems(result string, cfg *IngestConfig) ([]string, error) {
	switch cfg.ItemExtractor {
	case "jsonArray":
		var arr []string
		if err := json.Unmarshal([]byte(result), &arr); err != nil {
			return nil, fmt.Errorf("parsing JSON array: %w", err)
		}
		return arr, nil
	case "jsonObjects":
		if cfg.ItemField == "" {
			return nil, fmt.Errorf("itemField is required when itemExtractor is \"jsonObjects\"")
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal([]byte(result), &arr); err != nil {
			return nil, fmt.Errorf("parsing JSON array of objects: %w", err)
		}
		var items []string
		for _, obj := range arr {
			// Apply optional filter.
			if cfg.ItemFilterKey != "" {
				if v, ok := obj[cfg.ItemFilterKey].(string); !ok || v != cfg.ItemFilterVal {
					continue
				}
			}
			if val, ok := obj[cfg.ItemField].(string); ok && val != "" {
				items = append(items, val)
			}
		}
		return items, nil
	case "lines", "":
		var items []string
		for _, line := range strings.Split(result, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				items = append(items, line)
			}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unknown item extractor: %q", cfg.ItemExtractor)
	}
}

func discoverFlat(ctx context.Context, client *mcp.Client, cfg *IngestConfig) ([]string, error) {
	argsStr := "{}"
	if cfg.DiscoverArgs != nil {
		argsStr = string(cfg.DiscoverArgs)
	}
	slog.Info("Running discover", "tool", cfg.DiscoverTool)
	result, err := client.Call(ctx, cfg.DiscoverTool, argsStr)
	if err != nil {
		return nil, fmt.Errorf("discover tool %q: %w", cfg.DiscoverTool, err)
	}
	return extractItems(result, cfg)
}

func discoverRecursive(ctx context.Context, client *mcp.Client, cfg *IngestConfig) ([]string, error) {
	queue := []string{""} // Start at root (empty path).
	var allItems []string
	seen := map[string]bool{}

	for len(queue) > 0 {
		if ctx.Err() != nil {
			return allItems, ctx.Err()
		}
		dir := queue[0]
		queue = queue[1:]

		// Substitute {{item}} in discover args with current directory path.
		argsStr := "{}"
		if cfg.DiscoverArgs != nil {
			argsStr = substituteItem(string(cfg.DiscoverArgs), dir)
		}
		slog.Info("Discovering directory", "path", dir)
		result, err := client.Call(ctx, cfg.DiscoverTool, argsStr)
		if err != nil {
			slog.Warn("Discover failed for directory, skipping", "path", dir, "err", err)
			continue
		}

		// For non-jsonObjects extractors, fall back to flat extraction.
		if cfg.ItemExtractor != "jsonObjects" {
			items, err := extractItems(result, cfg)
			if err != nil {
				return nil, err
			}
			allItems = append(allItems, items...)
			continue
		}

		var arr []map[string]interface{}
		if err := json.Unmarshal([]byte(result), &arr); err != nil {
			slog.Warn("Could not parse discover result as JSON array", "path", dir, "err", err)
			continue
		}

		for _, obj := range arr {
			val, ok := obj[cfg.ItemField].(string)
			if !ok || val == "" {
				continue
			}
			if seen[val] {
				continue
			}
			seen[val] = true

			// Check if this is a directory to recurse into.
			if cfg.DirFilterKey != "" {
				if v, ok := obj[cfg.DirFilterKey].(string); ok && v == cfg.DirFilterVal {
					queue = append(queue, val)
					continue
				}
			}

			// Check if this matches the item filter (e.g. type=file).
			if cfg.ItemFilterKey != "" {
				if v, ok := obj[cfg.ItemFilterKey].(string); !ok || v != cfg.ItemFilterVal {
					continue
				}
			}
			allItems = append(allItems, val)
		}

		// Safety: cap during discovery to prevent runaway.
		if cfg.MaxItems > 0 && len(allItems) >= cfg.MaxItems {
			slog.Info("Reached maxItems during recursive discovery", "count", len(allItems))
			break
		}
	}
	return allItems, nil
}

// filterByPatterns applies include/exclude glob patterns to the item list.
func filterByPatterns(items []string, include, exclude []string) []string {
	if len(include) == 0 && len(exclude) == 0 {
		return items
	}
	var result []string
	for _, item := range items {
		if len(include) > 0 && !matchesAny(item, include) {
			continue
		}
		if matchesAny(item, exclude) {
			continue
		}
		result = append(result, item)
	}
	slog.Info("Applied glob patterns", "before", len(items), "after", len(result),
		"includePatterns", len(include), "excludePatterns", len(exclude))
	return result
}

func matchesAny(path string, patterns []string) bool {
	for _, p := range patterns {
		if matchGlob(p, path) {
			return true
		}
	}
	return false
}

// matchGlob supports standard glob patterns plus "**" for multi-segment matching.
func matchGlob(pattern, path string) bool {
	if strings.Contains(pattern, "**") {
		return matchDoublestar(pattern, path)
	}
	// Try matching full path.
	if matched, _ := filepath.Match(pattern, path); matched {
		return true
	}
	// Try matching just the filename (for patterns like "*.go").
	if matched, _ := filepath.Match(pattern, filepath.Base(path)); matched {
		return true
	}
	return false
}

func matchDoublestar(pattern, path string) bool {
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")

	// Check prefix matches.
	if prefix != "" && !strings.HasPrefix(path, prefix+"/") && path != prefix {
		return false
	}

	// If no suffix, ** matches everything under prefix.
	if suffix == "" {
		return true
	}

	// Check if any path suffix matches the remaining pattern.
	remaining := path
	if prefix != "" {
		remaining = strings.TrimPrefix(path, prefix+"/")
	}
	segments := strings.Split(remaining, "/")
	for i := range segments {
		candidate := strings.Join(segments[i:], "/")
		if matched, _ := filepath.Match(suffix, candidate); matched {
			return true
		}
	}
	// Also try just the filename.
	if matched, _ := filepath.Match(suffix, filepath.Base(path)); matched {
		return true
	}
	return false
}

func substituteItem(argsJSON, item string) string {
	return strings.ReplaceAll(argsJSON, "{{item}}", item)
}
