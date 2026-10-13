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

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/heddles/agent-orca/internal/rag"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// maxURLFetchBytes mirrors the operator's old 10MB URL fetch limit.
const maxURLFetchBytes = 10 << 20

// maxS3ObjectBytes is the upper bound for ingested S3 objects (100MB).
const maxS3ObjectBytes = 100 << 20

// runStaticWorkflow collects documents from the static sources declared in the
// config — ConfigMaps, then URLs, then S3 objects — in that order. Per-item
// failures are logged and skipped so one bad source never fails the job.
func runStaticWorkflow(ctx context.Context, cfg *IngestConfig) ([]rag.Document, error) {
	var docs []rag.Document

	cmDocs, err := fetchConfigMapDocs(ctx, cfg)
	if err != nil {
		return nil, err
	}
	docs = append(docs, cmDocs...)
	slog.Info("ConfigMap documents", "count", len(cmDocs))

	urlDocs, err := fetchURLDocs(ctx, cfg.URLs)
	if err != nil {
		return nil, err
	}
	docs = append(docs, urlDocs...)
	slog.Info("URL documents", "count", len(urlDocs))

	s3Docs, err := fetchS3Docs(ctx, cfg)
	if err != nil {
		return nil, err
	}
	docs = append(docs, s3Docs...)
	slog.Info("S3 documents", "count", len(s3Docs))

	return docs, nil
}

// fetchConfigMapDocs reads the configured ConfigMaps from the in-cluster
// Kubernetes API and returns one document per string data key. A missing
// ConfigMap is logged and skipped.
func fetchConfigMapDocs(ctx context.Context, cfg *IngestConfig) ([]rag.Document, error) {
	if len(cfg.ConfigMapNames) == 0 {
		return nil, nil
	}

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		return nil, fmt.Errorf("POD_NAMESPACE is not set but configMapNames is configured; the Job pod spec must set it (fieldRef metadata.namespace)")
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("building in-cluster Kubernetes config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}

	var docs []rag.Document
	for _, name := range cfg.ConfigMapNames {
		if ctx.Err() != nil {
			return docs, ctx.Err()
		}
		cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			slog.Warn("Fetching ConfigMap failed, skipping", "configMap", name, "namespace", namespace, "err", err)
			continue
		}
		for _, key := range sortedKeys(cm.Data) {
			content, docType, extracted, err := extractDocument(key, "", cm.Data[key])
			if err != nil {
				slog.Warn("Extracting ConfigMap document failed, skipping", "configMap", name, "key", key, "err", err)
				continue
			}
			docs = append(docs, rag.Document{
				ID:      fmt.Sprintf("configmap/%s/%s", name, key),
				Content: content,
				Metadata: metadataWithDocType(map[string]any{
					"source":    "configmap",
					"configmap": name,
					"key":       key,
				}, docType, extracted),
			})
		}
	}
	return docs, nil
}

// fetchURLDocs downloads each configured URL and returns one document per URL.
// Fetch failures are logged and skipped.
func fetchURLDocs(ctx context.Context, urls []string) ([]rag.Document, error) {
	if len(urls) == 0 {
		return nil, nil
	}

	var docs []rag.Document
	for _, rawURL := range urls {
		if ctx.Err() != nil {
			return docs, ctx.Err()
		}
		doc, err := fetchURLDoc(ctx, rawURL)
		if err != nil {
			slog.Warn("Fetching URL failed, skipping", "url", rawURL, "err", err)
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// fetchURLDoc downloads a single URL, extracts its text, and builds a document.
func fetchURLDoc(ctx context.Context, rawURL string) (rag.Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return rag.Document{}, fmt.Errorf("building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rag.Document{}, fmt.Errorf("fetching: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return rag.Document{}, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxURLFetchBytes))
	if err != nil {
		return rag.Document{}, fmt.Errorf("reading body: %w", err)
	}

	name := urlName(rawURL)
	content, docType, extracted, err := extractDocument(name, resp.Header.Get("Content-Type"), string(body))
	if err != nil {
		return rag.Document{}, fmt.Errorf("extracting %s: %w", name, err)
	}
	return rag.Document{
		ID:      fmt.Sprintf("url/%s", rawURL),
		Content: content,
		Metadata: metadataWithDocType(map[string]any{
			"source": "url",
			"url":    rawURL,
		}, docType, extracted),
	}, nil
}

// urlName returns the URL path's basename, or the URL itself when the path
// has no useful basename.
func urlName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
		return base
	}
	return rawURL
}

// fetchS3Docs lists and downloads objects from the configured S3 bucket and
// returns one document per object. Per-object failures are logged and skipped.
func fetchS3Docs(ctx context.Context, cfg *IngestConfig) ([]rag.Document, error) {
	if cfg.S3Bucket == "" {
		return nil, nil
	}

	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("S3 ingestion requires AWS credentials: configure secretRef on the KnowledgeBase S3 source so the Job receives AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}

	endpoint, secure := s3Endpoint(cfg.S3Endpoint)
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
		Region: cfg.S3Region,
	})
	if err != nil {
		return nil, fmt.Errorf("creating S3 client: %w", err)
	}

	var docs []rag.Document
	fetched := 0
	for obj := range client.ListObjects(ctx, cfg.S3Bucket, minio.ListObjectsOptions{
		Prefix:    cfg.S3Prefix,
		Recursive: true,
	}) {
		if cfg.MaxItems > 0 && fetched >= cfg.MaxItems {
			slog.Info("Reached maxItems for S3 objects", "max", cfg.MaxItems)
			break
		}
		if obj.Err != nil {
			slog.Warn("Listing S3 objects failed", "bucket", cfg.S3Bucket, "err", obj.Err)
			continue
		}
		if obj.Size == 0 {
			continue
		}
		if obj.Size > maxS3ObjectBytes {
			slog.Warn("S3 object too large, skipping", "bucket", cfg.S3Bucket, "key", obj.Key, "size", obj.Size)
			continue
		}
		fetched++

		doc, err := fetchS3ObjectDoc(ctx, client, cfg.S3Bucket, obj.Key, obj.Size)
		if err != nil {
			slog.Warn("Fetching S3 object failed, skipping", "bucket", cfg.S3Bucket, "key", obj.Key, "err", err)
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// fetchS3ObjectDoc downloads a single S3 object, extracts its text, and builds
// a document.
func fetchS3ObjectDoc(ctx context.Context, client *minio.Client, bucket, key string, size int64) (rag.Document, error) {
	stat, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return rag.Document{}, fmt.Errorf("stat object: %w", err)
	}

	obj, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return rag.Document{}, fmt.Errorf("opening object: %w", err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		return rag.Document{}, fmt.Errorf("reading object: %w", err)
	}

	content, docType, extracted, err := extractDocument(key, stat.ContentType, string(data))
	if err != nil {
		return rag.Document{}, fmt.Errorf("extracting %s: %w", key, err)
	}
	return rag.Document{
		ID:      fmt.Sprintf("s3/%s/%s", bucket, key),
		Content: content,
		Metadata: metadataWithDocType(map[string]any{
			"source": "s3",
			"bucket": bucket,
			"key":    key,
			"size":   size,
		}, docType, extracted),
	}, nil
}

// s3Endpoint splits an optional scheme off the S3 endpoint for
// minio.NewWithRegion and reports whether to use TLS. An empty endpoint
// defaults to AWS S3 over HTTPS.
func s3Endpoint(endpoint string) (host string, secure bool) {
	if endpoint == "" {
		return "s3.amazonaws.com", true
	}
	lower := strings.ToLower(endpoint)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return strings.TrimSuffix(endpoint[len("https://"):], "/"), true
	case strings.HasPrefix(lower, "http://"):
		return strings.TrimSuffix(endpoint[len("http://"):], "/"), false
	default:
		return strings.TrimSuffix(endpoint, "/"), true
	}
}

// sortedKeys returns the map keys in sorted order for deterministic document
// ordering.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
