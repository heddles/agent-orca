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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/podbuilder"
)

const (
	// ControllerPrincipal is the reserved name in MCPServer.allowedAgents that
	// grants the KnowledgeBase controller access to the MCP server.
	ControllerPrincipal = "_controller"

	// mcpIngestConfigKey is the key in the config ConfigMap.
	mcpIngestConfigKey = "config.json"

	// mcpIngestConfigDir is where config is mounted in the Job pod.
	mcpIngestConfigDir = "/etc/mcp-ingest"

	// defaultMCPIngesterImage is the fallback image for the mcp-ingester binary.
	defaultMCPIngesterImage = "ghcr.io/agentorc/mcp-ingester:latest"

	// labelKBMCPIngest marks resources created for MCP ingestion Jobs.
	labelKBMCPIngest = "agentorc.io/kb-mcp-ingest"
)

// mcpIngestConfig mirrors the IngestConfig struct in cmd/mcp-ingester/main.go.
type mcpIngestConfig struct {
	Transport         string          `json:"transport"`
	URL               string          `json:"url,omitempty"`
	BinaryCmd         string          `json:"binaryCmd,omitempty"`
	BinaryArgs        []string        `json:"binaryArgs,omitempty"`
	Env               []string        `json:"env,omitempty"`
	DiscoverTool      string          `json:"discoverTool,omitempty"`
	DiscoverArgs      json.RawMessage `json:"discoverArgs,omitempty"`
	FetchTool         string          `json:"fetchTool"`
	FetchArgs         json.RawMessage `json:"fetchArgs"`
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
	MCPServerName     string          `json:"mcpServerName"`
	DocIDTemplate     string          `json:"documentIDTemplate,omitempty"`

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

// runMCPIngestion manages the lifecycle of an MCP ingestion Job for a single
// MCPIngestionSource. The Job fetches documents from MCP, embeds them, and
// writes vectors directly to Qdrant. This method is idempotent: if the Job
// already exists it checks status; if it completed it cleans up; if it hasn't
// been created yet it creates the Job.
//
// Returns (true, nil) when the Job completed or no Job was needed (sync
// interval not elapsed). Returns (false, nil) when a Job is still running.
func (r *KnowledgeBaseReconciler) runMCPIngestion(
	ctx context.Context,
	kb *agentorcv1alpha1.KnowledgeBase,
	src agentorcv1alpha1.MCPIngestionSource,
	idx int,
) (bool, error) {
	logger := log.FromContext(ctx)

	// Validate MCPServer exists and grants _controller access.
	var server agentorcv1alpha1.MCPServer
	if err := r.Get(ctx, client.ObjectKey{Name: src.MCPServerRef, Namespace: kb.Namespace}, &server); err != nil {
		return false, fmt.Errorf("getting MCPServer %q: %w", src.MCPServerRef, err)
	}
	if !hasControllerAccess(&server) {
		return false, fmt.Errorf("MCPServer %q does not include %q in allowedAgents", src.MCPServerRef, ControllerPrincipal)
	}

	// Find a child Tool CR to get transport/URL/auth/ociRef details.
	toolCR, err := r.findMCPTool(ctx, &server)
	if err != nil {
		return false, fmt.Errorf("finding Tool for MCPServer %q: %w", src.MCPServerRef, err)
	}

	jobName := mcpIngestJobName(kb.Name, idx)
	configCMName := jobName + "-config"

	// Check for existing Job — always handle completed/running Jobs.
	var existingJob batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: kb.Namespace}, &existingJob)
	if err == nil {
		return r.handleExistingJob(ctx, kb, &existingJob)
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("checking existing Job: %w", err)
	}

	// No existing Job. Only create a new one if sync interval has elapsed.
	if kb.Status.LastSyncTime != nil && kb.Spec.Ingestion != nil {
		interval := time.Duration(kb.Spec.Ingestion.SyncIntervalSeconds) * time.Second
		if interval > 0 && time.Since(kb.Status.LastSyncTime.Time) < interval {
			return true, nil
		}
	}

	// Resolve embedding ModelProvider (already pinned in KB status).
	mpName := kb.Status.EmbeddingModelProvider
	if mpName == "" {
		return false, fmt.Errorf("KnowledgeBase %q has no pinned embedding provider yet", kb.Name)
	}
	var mp agentorcv1alpha1.ModelProvider
	if err := r.Get(ctx, client.ObjectKey{Name: mpName, Namespace: kb.Namespace}, &mp); err != nil {
		return false, fmt.Errorf("getting ModelProvider %q: %w", mpName, err)
	}

	// Build and create resources.
	logger.Info("Creating MCP ingestion Job", "mcpServer", src.MCPServerRef, "job", jobName)

	ingestCfg, err := r.buildIngestConfig(ctx, kb, &server, toolCR, &mp, &src)
	if err != nil {
		return false, fmt.Errorf("building ingest config: %w", err)
	}
	cfgJSON, err := json.Marshal(ingestCfg)
	if err != nil {
		return false, fmt.Errorf("marshaling ingest config: %w", err)
	}

	// Create config ConfigMap.
	configCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configCMName,
			Namespace: kb.Namespace,
			Labels:    map[string]string{labelKBMCPIngest: kb.Name},
		},
		Data: map[string]string{mcpIngestConfigKey: string(cfgJSON)},
	}
	if err := ctrl.SetControllerReference(kb, configCM, r.Scheme); err != nil {
		return false, fmt.Errorf("setting owner ref on config ConfigMap: %w", err)
	}
	if err := r.Create(ctx, configCM); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("creating config ConfigMap: %w", err)
	}

	// Build and create the Job.
	job := r.buildIngestJob(kb, toolCR, &src, &mp, jobName, configCMName)
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return false, fmt.Errorf("setting owner ref on Job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return false, fmt.Errorf("creating Job: %w", err)
	}

	// Job just started — caller should requeue.
	return false, nil
}

// handleExistingJob checks the status of an existing MCP ingestion Job.
// Documents are already in Qdrant; the caller queries Qdrant for counts.
func (r *KnowledgeBaseReconciler) handleExistingJob(
	ctx context.Context,
	kb *agentorcv1alpha1.KnowledgeBase,
	job *batchv1.Job,
) (bool, error) {
	logger := log.FromContext(ctx)

	if job.Status.Succeeded > 0 {
		logger.Info("MCP ingestion Job completed", "job", job.Name)
		r.cleanupIngestResources(ctx, kb.Namespace, job.Name)
		return true, nil
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "MCP ingestion Job failed", "job", job.Name)
		r.cleanupIngestResources(ctx, kb.Namespace, job.Name)
		return false, fmt.Errorf("MCP ingestion Job %q failed", job.Name)
	}

	// Still running. Caller will requeue via Job watch.
	logger.Info("MCP ingestion Job still running", "job", job.Name)
	return false, nil
}

// cleanupIngestResources deletes the Job and associated ConfigMaps.
func (r *KnowledgeBaseReconciler) cleanupIngestResources(ctx context.Context, namespace, jobName string) {
	logger := log.FromContext(ctx)
	propagation := metav1.DeletePropagationBackground

	// Delete Job.
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: namespace}, &job); err == nil {
		if err := r.Delete(ctx, &job, &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "deleting MCP ingestion Job", "job", jobName)
		}
	}

	// Delete config and results ConfigMaps.
	for _, suffix := range []string{"-config", "-results"} {
		cmName := jobName + suffix
		var cm corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Name: cmName, Namespace: namespace}, &cm); err == nil {
			if err := r.Delete(ctx, &cm); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "deleting ConfigMap", "configMap", cmName)
			}
		}
	}
}

// buildIngestConfig builds the JSON config for the mcp-ingester binary.
func (r *KnowledgeBaseReconciler) buildIngestConfig(
	ctx context.Context, //nolint:unparam

	kb *agentorcv1alpha1.KnowledgeBase,
	server *agentorcv1alpha1.MCPServer,
	tool *agentorcv1alpha1.Tool,
	mp *agentorcv1alpha1.ModelProvider,
	src *agentorcv1alpha1.MCPIngestionSource,
) (*mcpIngestConfig, error) { //nolint:unparam

	embeddingEndpoint := mp.Spec.BaseURL
	if embeddingEndpoint == "" {
		embeddingEndpoint = embeddingBaseURL(mp.Spec.LiteLLMModel)
	}
	embeddingModel := mp.Spec.LiteLLMModel
	if idx := strings.Index(embeddingModel, "/"); idx >= 0 {
		embeddingModel = embeddingModel[idx+1:]
	}

	dims := kb.Status.EmbeddingDimensions

	cfg := &mcpIngestConfig{
		Transport:     tool.Spec.MCPConfig.Transport,
		URL:           tool.Spec.MCPConfig.URL,
		FetchTool:     src.Fetch.Tool,
		ItemExtractor: src.ItemExtractor,
		ItemField:     src.ItemField,
		MaxItems:      src.MaxItems,
		MCPServerName: server.Name,
		DocIDTemplate: src.DocumentIDTemplate,

		EmbeddingEndpoint: embeddingEndpoint,
		EmbeddingModel:    embeddingModel,
		EmbeddingKeyFile:  fmt.Sprintf("/etc/embed-key/%s", mp.Spec.CredentialsRef.Key),
		EmbeddingDims:     dims,
		ChunkSize:         kb.Spec.Embedding.ChunkSize,
		ChunkOverlap:      kb.Spec.Embedding.ChunkOverlap,

		QdrantURL:      kb.Status.VectorStoreURL,
		CollectionName: kb.Status.CollectionName,
	}

	if cfg.ItemExtractor == "" {
		cfg.ItemExtractor = "lines"
	}
	if src.ItemFilter != nil {
		cfg.ItemFilterKey = src.ItemFilter.Key
		cfg.ItemFilterVal = src.ItemFilter.Value
	}
	cfg.DiscoverRecursive = src.DiscoverRecursive
	if src.DirectoryFilter != nil {
		cfg.DirFilterKey = src.DirectoryFilter.Key
		cfg.DirFilterVal = src.DirectoryFilter.Value
	}
	cfg.IncludePatterns = src.IncludePatterns
	cfg.ExcludePatterns = src.ExcludePatterns
	if cfg.MaxItems == 0 {
		cfg.MaxItems = 500
	}

	// Fetch args.
	if src.Fetch.Arguments != nil {
		cfg.FetchArgs = src.Fetch.Arguments.Raw
	} else {
		cfg.FetchArgs = json.RawMessage("{}")
	}

	// Discover args.
	if src.Discover != nil {
		cfg.DiscoverTool = src.Discover.Tool
		if src.Discover.Arguments != nil {
			cfg.DiscoverArgs = src.Discover.Arguments.Raw
		}
	}

	// stdio transport: resolve binary path.
	if tool.Spec.MCPConfig.Transport == "stdio" { //nolint:goconst

		// The MCP binary is mounted via image volume at /mcp-img/<serverName>/.
		// Tool args contain the original binary path which needs to be rewritten.
		mountDir := fmt.Sprintf("%s/%s", podbuilder.MCPBinDir, server.Name)
		if len(tool.Spec.MCPConfig.Args) > 0 {
			cfg.BinaryCmd = mountDir + tool.Spec.MCPConfig.Args[0]
			if len(tool.Spec.MCPConfig.Args) > 1 {
				cfg.BinaryArgs = tool.Spec.MCPConfig.Args[1:]
			}
		}

		// Literal env vars from the MCPConfig.
		for k, v := range tool.Spec.MCPConfig.Env {
			cfg.Env = append(cfg.Env, k+"="+v)
		}
	}

	return cfg, nil
}

// buildIngestJob constructs the Kubernetes Job spec for MCP ingestion.
func (r *KnowledgeBaseReconciler) buildIngestJob(
	kb *agentorcv1alpha1.KnowledgeBase,
	tool *agentorcv1alpha1.Tool,
	src *agentorcv1alpha1.MCPIngestionSource,
	mp *agentorcv1alpha1.ModelProvider,
	jobName, configCMName string,
) *batchv1.Job {
	labels := map[string]string{
		labelKBMCPIngest:               kb.Name,
		"app.kubernetes.io/name":       "mcp-ingester",
		"app.kubernetes.io/managed-by": "agentorc",
	}

	volumes := []corev1.Volume{
		{
			Name: "ingest-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configCMName},
				},
			},
		},
		{
			Name: "embed-key",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: mp.Spec.CredentialsRef.Name,
					Items: []corev1.KeyToPath{
						{Key: mp.Spec.CredentialsRef.Key, Path: mp.Spec.CredentialsRef.Key},
					},
				},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "ingest-config",
			MountPath: mcpIngestConfigDir,
			ReadOnly:  true,
		},
		{
			Name:      "embed-key",
			MountPath: "/etc/embed-key",
			ReadOnly:  true,
		},
	}

	var envVars []corev1.EnvVar

	// For stdio transport: mount the MCP server OCI image via image volume.
	if tool.Spec.MCPConfig != nil && tool.Spec.MCPConfig.Transport == "stdio" && tool.Spec.OCIRef != "" {
		volName := "mcp-img-" + podbuilder.SanitizeVolumeName(tool.Name)
		mountDir := fmt.Sprintf("%s/%s", podbuilder.MCPBinDir, src.MCPServerRef)
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{
					Reference:  tool.Spec.OCIRef,
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: mountDir,
			ReadOnly:  true,
		})

		// Mount secret-backed env vars for stdio MCP servers.
		if tool.Spec.MCPConfig != nil {
			for _, ev := range tool.Spec.MCPConfig.EnvFrom {
				if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
					envVars = append(envVars, corev1.EnvVar{
						Name: ev.Name,
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: ev.ValueFrom.SecretKeyRef.Name,
								},
								Key: ev.ValueFrom.SecretKeyRef.Key,
							},
						},
					})
				} else if ev.Value != "" {
					envVars = append(envVars, corev1.EnvVar{
						Name:  ev.Name,
						Value: ev.Value,
					})
				}
			}

			// Literal env vars.
			for k, v := range tool.Spec.MCPConfig.Env {
				envVars = append(envVars, corev1.EnvVar{
					Name:  k,
					Value: v,
				})
			}
		}
	}

	// For http/sse transport: mount auth secrets as files.
	if tool.Spec.MCPConfig != nil && tool.Spec.MCPConfig.Auth != nil {
		auth := tool.Spec.MCPConfig.Auth

		if auth.BearerToken != nil {
			volName := "auth-bearer"
			filePath := podbuilder.ToolSecretFilePath(tool.Name, auth.BearerToken.Name, auth.BearerToken.Key)
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: auth.BearerToken.Name,
						Items:      []corev1.KeyToPath{{Key: auth.BearerToken.Key, Path: auth.BearerToken.Key}},
					},
				},
			})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: filePath,
				SubPath:   auth.BearerToken.Key,
				ReadOnly:  true,
			})
		}

		if auth.APIKey != nil {
			volName := "auth-apikey"
			ref := &auth.APIKey.SecretKeyRef
			filePath := podbuilder.ToolSecretFilePath(tool.Name, ref.Name, ref.Key)
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: ref.Name,
						Items:      []corev1.KeyToPath{{Key: ref.Key, Path: ref.Key}},
					},
				},
			})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: filePath,
				SubPath:   ref.Key,
				ReadOnly:  true,
			})
		}

		for i, header := range auth.Headers {
			volName := fmt.Sprintf("auth-header-%d", i)
			filePath := podbuilder.ToolSecretFilePath(tool.Name, header.SecretKeyRef.Name, header.SecretKeyRef.Key)
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: header.SecretKeyRef.Name,
						Items:      []corev1.KeyToPath{{Key: header.SecretKeyRef.Key, Path: header.SecretKeyRef.Key}},
					},
				},
			})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: filePath,
				SubPath:   header.SecretKeyRef.Key,
				ReadOnly:  true,
			})
		}
	}

	// Container resources.
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	if src.Resources != nil {
		resources = corev1.ResourceRequirements(*src.Resources) //nolint:unconvert

	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: kb.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "agentorc-mcp-ingester",
					Containers: []corev1.Container{
						{
							Name:         "ingester",
							Image:        r.mcpIngesterImageOrDefault(),
							Command:      []string{"/mcp-ingester"},
							Env:          envVars,
							VolumeMounts: volumeMounts,
							Resources:    resources,
						},
					},
					Volumes: volumes,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(1000)),
						FSGroup:      ptr.To(int64(1000)),
					},
				},
			},
		},
	}

	return job
}

// findMCPTool returns the first child Tool CR for the given MCPServer.
func (r *KnowledgeBaseReconciler) findMCPTool(ctx context.Context, server *agentorcv1alpha1.MCPServer) (*agentorcv1alpha1.Tool, error) {
	var tools agentorcv1alpha1.ToolList
	if err := r.List(ctx, &tools,
		client.InNamespace(server.Namespace),
		client.MatchingLabels{
			LabelManagedBy: LabelManagedByMCPServer,
			LabelMCPServer: server.Name,
		},
	); err != nil {
		return nil, err
	}
	if len(tools.Items) == 0 {
		return nil, fmt.Errorf("no Tool CRs found for MCPServer %q", server.Name)
	}
	return &tools.Items[0], nil
}

// hasControllerAccess checks if the MCPServer grants access to the _controller principal.
func hasControllerAccess(server *agentorcv1alpha1.MCPServer) bool {
	return slices.Contains(server.Spec.AllowedAgents, ControllerPrincipal)
}

// mcpIngestJobName returns a DNS-safe Job name for a KB's MCP ingestion source.
func mcpIngestJobName(kbName string, idx int) string {
	safe := strings.Trim(dnsUnsafe.ReplaceAllString(strings.ToLower(kbName), "-"), "-")
	name := fmt.Sprintf("kb-mcp-%s-%d", safe, idx)
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// mcpIngesterImageOrDefault returns the configured ingester image or the built-in default.
func (r *KnowledgeBaseReconciler) mcpIngesterImageOrDefault() string {
	if r.MCPIngesterImage != "" {
		return r.MCPIngesterImage
	}
	return defaultMCPIngesterImage
}

// knowledgeBasesForMCPServer maps an MCPServer change to KnowledgeBases that reference it.
func (r *KnowledgeBaseReconciler) knowledgeBasesForMCPServer(ctx context.Context, obj client.Object) []reconcile.Request {
	server := obj.(*agentorcv1alpha1.MCPServer)

	var kbList agentorcv1alpha1.KnowledgeBaseList
	if err := r.List(ctx, &kbList, client.InNamespace(server.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, kb := range kbList.Items {
		if kb.Spec.Ingestion == nil {
			continue
		}
		for _, mcpSrc := range kb.Spec.Ingestion.MCP {
			if mcpSrc.MCPServerRef == server.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{Name: kb.Name, Namespace: kb.Namespace},
				})
				break
			}
		}
	}
	return requests
}
