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
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/security"
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
	defaultMCPIngesterImage = "ghcr.io/heddles/agent-orca/mcp-ingester:latest"

	// quotaExhaustedMarker is the final log line the mcp-ingester emits when
	// the embedding provider reports per-day quota exhaustion. The Job pod
	// uses terminationMessagePolicy FallbackToLogsOnError, so the controller
	// reads this marker from the pod's termination message and pauses
	// re-ingestion until the provider's daily quota reset instead of
	// retrying into a wall.
	quotaExhaustedMarker = "QUOTA_EXHAUSTED_DAILY"

	// initialIngestionBackoff/maxIngestionBackoff bound the exponential
	// backoff between failed ingestion Job runs (1m, 2m, 4m, ... capped 1h).
	// Daily-quota exhaustion bypasses this and waits for the quota reset.
	initialIngestionBackoff = 1 * time.Minute
	maxIngestionBackoff     = 1 * time.Hour

	// ingestServiceAccountName is the ServiceAccount used by KnowledgeBase
	// ingestion Jobs (MCP and static-source). The chart provisions it in the
	// release namespace; the operator ensures it in each KB namespace.
	ingestServiceAccountName = "agentorca-mcp-ingester"

	// labelKBMCPIngest marks resources created for KnowledgeBase ingestion Jobs
	// (both MCP and static-source Jobs). Defined in the security package so the
	// ingestion NetworkPolicy pod selector matches the Job pod labels.
	labelKBMCPIngest = security.LabelKBIngest
)

// mcpIngestConfig mirrors the IngestConfig struct in cmd/mcp-ingester/main.go.
type mcpIngestConfig struct {
	// SourceType selects the workflow: "mcp" (default when empty) or "static"
	// (ConfigMaps, URLs, and/or S3 declared in the KnowledgeBase spec).
	SourceType     string   `json:"sourceType,omitempty"`
	ConfigMapNames []string `json:"configMapNames,omitempty"`
	URLs           []string `json:"urls,omitempty"`
	S3Bucket       string   `json:"s3Bucket,omitempty"`
	S3Prefix       string   `json:"s3Prefix,omitempty"`
	S3Region       string   `json:"s3Region,omitempty"`
	S3Endpoint     string   `json:"s3Endpoint,omitempty"`

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
	EmbeddingEndpoint  string `json:"embeddingEndpoint"`
	EmbeddingModel     string `json:"embeddingModel"`
	EmbeddingKeyFile   string `json:"embeddingKeyFile"`
	EmbeddingDims      int    `json:"embeddingDims"`
	EmbeddingDocPrompt string `json:"embeddingDocPrompt,omitempty"`
	EmbeddingRPM       int    `json:"embeddingRPM,omitempty"`
	EmbeddingTPM       int    `json:"embeddingTPM,omitempty"`
	ChunkSize          int    `json:"chunkSize"`
	ChunkOverlap       int    `json:"chunkOverlap"`

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
// Returns (true, requeueAfter, nil) when the Job completed or no Job was
// needed (sync interval or failure backoff not elapsed). Returns
// (false, requeueAfter, nil) when a Job is still running. requeueAfter > 0
// asks the caller to reconcile again after that delay (e.g. failure backoff).
func (r *KnowledgeBaseReconciler) runMCPIngestion(
	ctx context.Context,
	kb *agentorcav1alpha1.KnowledgeBase,
	src agentorcav1alpha1.MCPIngestionSource,
	idx int,
) (bool, time.Duration, error) {
	logger := log.FromContext(ctx)

	// Validate MCPServer exists and grants _controller access.
	var server agentorcav1alpha1.MCPServer
	if err := r.Get(ctx, client.ObjectKey{Name: src.MCPServerRef, Namespace: kb.Namespace}, &server); err != nil {
		return false, 0, fmt.Errorf("getting MCPServer %q: %w", src.MCPServerRef, err)
	}
	if !hasControllerAccess(&server) {
		return false, 0, fmt.Errorf("MCPServer %q does not include %q in allowedAgents", src.MCPServerRef, ControllerPrincipal)
	}

	// Find a child Tool CR to get transport/URL/auth/ociRef details.
	toolCR, err := r.findMCPTool(ctx, &server)
	if err != nil {
		return false, 0, fmt.Errorf("finding Tool for MCPServer %q: %w", src.MCPServerRef, err)
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
		return false, 0, fmt.Errorf("checking existing Job: %w", err)
	}

	// No existing Job. Hold off while this source's failure backoff runs.
	if wait, paused := ingestionPaused(kb, jobName); paused {
		logger.Info("ingestion paused by per-source failure backoff",
			"job", jobName,
			"resume_at", kb.Status.IngestionBreakers[jobName].NextAttempt.Format(time.RFC3339),
			"reason", kb.Status.IngestionBreakers[jobName].Reason)
		return false, wait, nil
	}

	// Only create a new one if this source's sync interval has elapsed
	// (per-source stamps: a failing sibling can't starve this cadence).
	if !ingestionSyncIntervalElapsed(kb, jobName) {
		return true, 0, nil
	}

	// Resolve embedding ModelProvider (already pinned in KB status).
	mpName := kb.Status.EmbeddingModelProvider
	if mpName == "" {
		return false, 0, fmt.Errorf("KnowledgeBase %q has no pinned embedding provider yet", kb.Name)
	}
	var mp agentorcav1alpha1.ModelProvider
	if err := r.Get(ctx, client.ObjectKey{Name: mpName, Namespace: kb.Namespace}, &mp); err != nil {
		return false, 0, fmt.Errorf("getting ModelProvider %q: %w", mpName, err)
	}

	// Ensure the ingester ServiceAccount/RBAC and NetworkPolicy exist in the
	// KB namespace before creating the Job. The chart only provisions these
	// in the release namespace; Jobs run in the KB's namespace.
	if err := r.ensureIngestRBAC(ctx, kb); err != nil {
		return false, 0, fmt.Errorf("ensuring ingester RBAC: %w", err)
	}
	if err := r.ensureIngestNetworkPolicy(ctx, kb); err != nil {
		return false, 0, fmt.Errorf("ensuring ingestion NetworkPolicy: %w", err)
	}

	// Build and create resources.
	logger.Info("Creating MCP ingestion Job", "mcpServer", src.MCPServerRef, "job", jobName)

	ingestCfg, err := r.buildIngestConfig(ctx, kb, &server, toolCR, &mp, &src)
	if err != nil {
		return false, 0, fmt.Errorf("building ingest config: %w", err)
	}
	cfgJSON, err := json.Marshal(ingestCfg)
	if err != nil {
		return false, 0, fmt.Errorf("marshaling ingest config: %w", err)
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
		return false, 0, fmt.Errorf("setting owner ref on config ConfigMap: %w", err)
	}
	if err := r.Create(ctx, configCM); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, 0, fmt.Errorf("creating config ConfigMap: %w", err)
	}

	// Build and create the Job.
	job := r.buildIngestJob(kb, toolCR, &src, &mp, jobName, configCMName)
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return false, 0, fmt.Errorf("setting owner ref on Job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return false, 0, fmt.Errorf("creating Job: %w", err)
	}

	// Job just started — caller should requeue.
	return false, 0, nil
}

// handleExistingJob checks the status of an existing ingestion Job.
// Documents are already in Qdrant; the caller queries Qdrant for counts.
//
// Breaker state is keyed per source (by Job name) in
// kb.Status.IngestionBreakers: a sibling source's success only clears its own
// entry, and concurrent source failures count independently.
func (r *KnowledgeBaseReconciler) handleExistingJob(
	ctx context.Context,
	kb *agentorcav1alpha1.KnowledgeBase,
	job *batchv1.Job,
) (bool, time.Duration, error) {
	logger := log.FromContext(ctx)

	if job.Status.Succeeded > 0 {
		logger.Info("ingestion Job completed", "job", job.Name)
		r.cleanupIngestResources(ctx, kb.Namespace, job.Name)
		// Success clears only THIS source's breaker — a static-source Job
		// completing must not cancel an MCP source's quota pause.
		delete(kb.Status.IngestionBreakers, job.Name)
		// Stamp this source's successful sync for the per-source sync-interval
		// gate (a failing sibling must not starve this source's cadence).
		if kb.Status.IngestionSyncTimes == nil {
			kb.Status.IngestionSyncTimes = make(map[string]metav1.Time, 1)
		}
		kb.Status.IngestionSyncTimes[job.Name] = metav1.Now()
		return true, 0, nil
	}

	if job.Status.Failed > 0 {
		dailyQuota := r.ingestJobHitDailyQuota(ctx, kb.Namespace, job)
		breaker := kb.Status.IngestionBreakers[job.Name]
		backoff, reason := ingestionBackoff(breaker.Failures, dailyQuota)
		next := metav1.NewTime(time.Now().Add(backoff))
		if kb.Status.IngestionBreakers == nil {
			kb.Status.IngestionBreakers = make(map[string]agentorcav1alpha1.IngestionBreakerState, 1)
		}
		kb.Status.IngestionBreakers[job.Name] = agentorcav1alpha1.IngestionBreakerState{
			Failures:    breaker.Failures + 1,
			NextAttempt: &next,
			Reason:      reason,
		}
		kb.Status.Message = fmt.Sprintf("ingestion Job %q paused until %s: %s",
			job.Name, next.Format(time.RFC3339), reason)
		logger.Error(nil, "ingestion Job failed; pausing re-ingestion",
			"job", job.Name,
			"backoff", backoff.Round(time.Second),
			"consecutive_failures", breaker.Failures+1,
			"reason", reason,
		)
		r.cleanupIngestResources(ctx, kb.Namespace, job.Name)
		return false, backoff, fmt.Errorf("ingestion Job %q failed: %s", job.Name, reason)
	}

	// Still running. Caller will requeue via Job watch.
	logger.Info("ingestion Job still running", "job", job.Name)
	return false, 0, nil
}

// ingestionSyncIntervalElapsed reports whether a source's sync interval has
// elapsed (or no interval is configured, or the source never synced). It
// reads the per-source stamp so a persistently failing sibling — which keeps
// the KB-level LastSyncTime from advancing — cannot re-create a healthy
// source's Job on every controller resync.
func ingestionSyncIntervalElapsed(kb *agentorcav1alpha1.KnowledgeBase, jobName string) bool {
	if kb.Spec.Ingestion == nil {
		return true
	}
	interval := time.Duration(kb.Spec.Ingestion.SyncIntervalSeconds) * time.Second
	if interval <= 0 {
		return true
	}
	last, ok := kb.Status.IngestionSyncTimes[jobName]
	if !ok {
		return true
	}
	return time.Since(last.Time) >= interval
}

// ingestionPaused returns the remaining backoff pause for a source's Job, or
// (0, false) when the source is healthy to run again.
func ingestionPaused(kb *agentorcav1alpha1.KnowledgeBase, jobName string) (time.Duration, bool) {
	b, ok := kb.Status.IngestionBreakers[jobName]
	if !ok || b.NextAttempt == nil {
		return 0, false
	}
	if wait := time.Until(b.NextAttempt.Time); wait > 0 {
		return wait, true
	}
	return 0, false
}

// ingestionBackoff computes the pause before the next ingestion attempt.
// Daily-quota exhaustion waits for the provider's quota reset (Google resets
// daily quotas at midnight Pacific); anything else backs off exponentially
// with the consecutive failure count. Note the daily-quota floor is 0 — at
// the reset boundary the returned wait may be near-zero, which immediately
// unblocks the source.
func ingestionBackoff(consecutiveFailures int, dailyQuota bool) (time.Duration, string) {
	if dailyQuota {
		now := time.Now()
		return nextQuotaReset(now).Sub(now),
			"embedding provider daily quota exhausted; waiting for quota reset"
	}
	backoff := initialIngestionBackoff << uint(min(consecutiveFailures, 6))
	if backoff > maxIngestionBackoff || backoff <= 0 {
		backoff = maxIngestionBackoff
	}
	return backoff, "ingestion job failed"
}

// nextQuotaReset returns the first midnight Pacific after now, when Google
// resets daily API quotas.
//
// Google-specific by design: day-scope classification (see
// ratelimit.ClassifyQuotaError) currently only recognizes google.rpc
// QuotaFailure bodies, so this reset semantics can only be reached for
// Google endpoints. If another provider gains day-scope classification,
// move the reset policy into the provider config so classification and
// reset semantics travel together. Falls back to a fixed UTC-8 offset if
// tzdata is missing.
func nextQuotaReset(now time.Time) time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.FixedZone("PST", -8*60*60)
	}
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
}

// ingestJobHitDailyQuota reports whether any terminated pod of the Job
// carried the daily-quota exhaustion marker in its termination message.
func (r *KnowledgeBaseReconciler) ingestJobHitDailyQuota(ctx context.Context, namespace string, job *batchv1.Job) bool {
	var pods corev1.PodList
	// Job pods carry both the legacy "job-name" label and
	// "batch.kubernetes.io/job-name"; matching either is sufficient.
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		log.FromContext(ctx).Error(err, "listing ingestion Job pods for quota classification",
			"job", job.Name)
		return false
	}
	return podsReportDailyQuota(pods.Items)
}

// podsReportDailyQuota inspects pod termination messages for the marker.
// Only terminated container states are inspected: with backoffLimit 0 a
// failed pod is terminal by the time the Job reports Failed, so the
// termination message is guaranteed to be present. Pods stuck in crash
// backoff report nothing here (also fine — the Job hasn't failed yet).
func podsReportDailyQuota(pods []corev1.Pod) bool {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			term := cs.State.Terminated
			if term != nil && strings.Contains(term.Message, quotaExhaustedMarker) {
				return true
			}
		}
	}
	return false
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

// hasStaticSources reports whether the ingestion config declares any
// ConfigMap, URL, or S3 sources (i.e. anything ingested by the static-source
// Job rather than an MCP Job).
func hasStaticSources(ing *agentorcav1alpha1.IngestionConfig) bool {
	return len(ing.ConfigMapRefs) > 0 || len(ing.URLs) > 0 || ing.S3 != nil
}

// runSourcesIngestion manages the lifecycle of the static-source ingestion Job
// for a KnowledgeBase (ConfigMaps, URLs, S3). The Job fetches the sources,
// detects document types, extracts text, and writes vectors directly to
// Qdrant — keeping fetching/extraction/embedding CPU work out of the
// operator. Idempotent, mirroring runMCPIngestion.
//
// Returns (true, requeueAfter, nil) when the Job completed or no Job was
// needed (sync interval or failure backoff not elapsed). Returns
// (false, requeueAfter, nil) when a Job is still running.
func (r *KnowledgeBaseReconciler) runSourcesIngestion(ctx context.Context, kb *agentorcav1alpha1.KnowledgeBase) (bool, time.Duration, error) {
	logger := log.FromContext(ctx)

	jobName := sourcesIngestJobName(kb.Name)
	configCMName := jobName + "-config"

	// Check for existing Job — always handle completed/running Jobs.
	var existingJob batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: kb.Namespace}, &existingJob)
	if err == nil {
		return r.handleExistingJob(ctx, kb, &existingJob)
	}
	if !apierrors.IsNotFound(err) {
		return false, 0, fmt.Errorf("checking existing Job: %w", err)
	}

	// No existing Job. Hold off while this source's failure backoff runs.
	if wait, paused := ingestionPaused(kb, jobName); paused {
		logger.Info("ingestion paused by per-source failure backoff",
			"job", jobName,
			"resume_at", kb.Status.IngestionBreakers[jobName].NextAttempt.Format(time.RFC3339),
			"reason", kb.Status.IngestionBreakers[jobName].Reason)
		return false, wait, nil
	}

	// Only create a new one if this source's sync interval has elapsed
	// (per-source stamps: a failing sibling can't starve this cadence).
	if !ingestionSyncIntervalElapsed(kb, jobName) {
		return true, 0, nil
	}

	// Resolve embedding ModelProvider (already pinned in KB status).
	mpName := kb.Status.EmbeddingModelProvider
	if mpName == "" {
		return false, 0, fmt.Errorf("KnowledgeBase %q has no pinned embedding provider yet", kb.Name)
	}
	var mp agentorcav1alpha1.ModelProvider
	if err := r.Get(ctx, client.ObjectKey{Name: mpName, Namespace: kb.Namespace}, &mp); err != nil {
		return false, 0, fmt.Errorf("getting ModelProvider %q: %w", mpName, err)
	}

	// Ensure the ingester ServiceAccount/RBAC and NetworkPolicy exist in the
	// KB namespace before creating the Job. The chart only provisions these
	// in the release namespace; Jobs run in the KB's namespace.
	if err := r.ensureIngestRBAC(ctx, kb); err != nil {
		return false, 0, fmt.Errorf("ensuring ingester RBAC: %w", err)
	}
	if err := r.ensureIngestNetworkPolicy(ctx, kb); err != nil {
		return false, 0, fmt.Errorf("ensuring ingestion NetworkPolicy: %w", err)
	}

	logger.Info("Creating static-source ingestion Job", "job", jobName)
	ingestCfg := r.buildSourcesIngestConfig(kb, &mp)
	cfgJSON, err := json.Marshal(ingestCfg)
	if err != nil {
		return false, 0, fmt.Errorf("marshaling ingest config: %w", err)
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
		return false, 0, fmt.Errorf("setting owner ref on config ConfigMap: %w", err)
	}
	if err := r.Create(ctx, configCM); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, 0, fmt.Errorf("creating config ConfigMap: %w", err)
	}

	// Build and create the Job.
	job := r.buildSourcesIngestJob(kb, &mp, jobName, configCMName)
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return false, 0, fmt.Errorf("setting owner ref on Job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return false, 0, fmt.Errorf("creating Job: %w", err)
	}

	// Job just started — caller should requeue.
	return false, 0, nil
}

// ensureIngestRBAC creates the ingester ServiceAccount, Role, and RoleBinding
// in the KnowledgeBase's namespace. The Helm chart only provisions these in
// the release namespace, but ingestion Jobs run in the KB's namespace. Each
// resource is owned by the KB and self-heals if it was removed (e.g. its
// previous owner KB was deleted while other KBs remain).
func (r *KnowledgeBaseReconciler) ensureIngestRBAC(ctx context.Context, kb *agentorcav1alpha1.KnowledgeBase) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingestServiceAccountName,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelKBMCPIngest:               kb.Name,
				"app.kubernetes.io/managed-by": "agentorca",
			},
		},
	}
	if err := r.ensureIngestObject(ctx, kb, sa); err != nil {
		return fmt.Errorf("ensuring ServiceAccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingestServiceAccountName,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelKBMCPIngest:               kb.Name,
				"app.kubernetes.io/managed-by": "agentorca",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list"},
			},
		},
	}
	if err := r.ensureIngestObject(ctx, kb, role); err != nil {
		return fmt.Errorf("ensuring Role: %w", err)
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingestServiceAccountName,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelKBMCPIngest:               kb.Name,
				"app.kubernetes.io/managed-by": "agentorca",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     ingestServiceAccountName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      ingestServiceAccountName,
			Namespace: kb.Namespace,
		}},
	}
	if err := r.ensureIngestObject(ctx, kb, rb); err != nil {
		return fmt.Errorf("ensuring RoleBinding: %w", err)
	}
	return nil
}

// ensureIngestObject creates obj (owned by the KB) if it does not exist.
func (r *KnowledgeBaseReconciler) ensureIngestObject(ctx context.Context, kb *agentorcav1alpha1.KnowledgeBase, obj client.Object) error {
	if err := ctrl.SetControllerReference(kb, obj, r.Scheme); err != nil {
		return fmt.Errorf("setting owner ref on %T: %w", obj, err)
	}
	if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating %T: %w", obj, err)
	}
	return nil
}

// ensureIngestNetworkPolicy creates (or updates) the NetworkPolicy for the
// KB's ingestion Job pods. The policy is additive — NetworkPolicy rules from
// multiple policies are unioned — so creating it is safe in namespaces
// without default-deny policies and required in namespaces that have them.
func (r *KnowledgeBaseReconciler) ensureIngestNetworkPolicy(ctx context.Context, kb *agentorcav1alpha1.KnowledgeBase) error {
	var extraEgress []agentorcav1alpha1.EgressRule
	if kb.Spec.Ingestion != nil {
		extraEgress = kb.Spec.Ingestion.NetworkEgress
	}
	np := security.BuildIngestNetworkPolicy(kb.Name, kb.Namespace, extraEgress)

	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKey{Name: np.Name, Namespace: kb.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		if err := ctrl.SetControllerReference(kb, np, r.Scheme); err != nil {
			return fmt.Errorf("setting owner ref on ingestion NetworkPolicy: %w", err)
		}
		if err := r.Create(ctx, np); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating ingestion NetworkPolicy: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking ingestion NetworkPolicy: %w", err)
	}

	// Update only on drift so reconcile stays read-only in the steady state.
	if !apiequality.Semantic.DeepEqual(existing.Spec, np.Spec) {
		existing.Spec = np.Spec
		if err := r.Update(ctx, &existing); err != nil {
			return fmt.Errorf("updating ingestion NetworkPolicy: %w", err)
		}
	}
	return nil
}

// buildSourcesIngestConfig builds the JSON config for the static-source
// ingestion Job.
func (r *KnowledgeBaseReconciler) buildSourcesIngestConfig(
	kb *agentorcav1alpha1.KnowledgeBase,
	mp *agentorcav1alpha1.ModelProvider,
) *mcpIngestConfig {
	embeddingEndpoint, embeddingModel, rpm, tpm := embeddingConfig(mp)

	cmNames := make([]string, 0, len(kb.Spec.Ingestion.ConfigMapRefs))
	for _, ref := range kb.Spec.Ingestion.ConfigMapRefs {
		cmNames = append(cmNames, ref.Name)
	}

	s3 := kb.Spec.Ingestion.S3
	cfg := &mcpIngestConfig{
		SourceType:     "static",
		ConfigMapNames: cmNames,
		URLs:           kb.Spec.Ingestion.URLs,

		EmbeddingEndpoint:  embeddingEndpoint,
		EmbeddingModel:     embeddingModel,
		EmbeddingKeyFile:   fmt.Sprintf("/etc/embed-key/%s", mp.Spec.CredentialsRef.Key),
		EmbeddingDims:      kb.Status.EmbeddingDimensions,
		EmbeddingDocPrompt: mp.Spec.DocPrompt,
		EmbeddingRPM:       rpm,
		EmbeddingTPM:       tpm,
		ChunkSize:          kb.Spec.Embedding.ChunkSize,
		ChunkOverlap:       kb.Spec.Embedding.ChunkOverlap,

		QdrantURL:      kb.Status.VectorStoreURL,
		CollectionName: kb.Status.CollectionName,
	}
	if s3 != nil {
		cfg.S3Bucket = s3.Bucket
		cfg.S3Prefix = s3.Prefix
		cfg.S3Region = s3.Region
		cfg.S3Endpoint = s3.Endpoint
	}
	return cfg
}

// buildSourcesIngestJob constructs the Kubernetes Job spec for static-source
// (ConfigMap/URL/S3) ingestion.
func (r *KnowledgeBaseReconciler) buildSourcesIngestJob(
	kb *agentorcav1alpha1.KnowledgeBase,
	mp *agentorcav1alpha1.ModelProvider,
	jobName, configCMName string,
) *batchv1.Job {
	labels := map[string]string{
		labelKBMCPIngest:               kb.Name,
		"app.kubernetes.io/name":       "mcp-ingester",
		"app.kubernetes.io/managed-by": "agentorca",
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

	// POD_NAMESPACE tells the ingester where to read source ConfigMaps from.
	envVars := []corev1.EnvVar{
		{
			Name: "POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
	}

	// S3 credentials come from the referenced Secret as standard AWS env vars.
	if kb.Spec.Ingestion.S3 != nil && kb.Spec.Ingestion.S3.SecretRef != nil {
		secretName := kb.Spec.Ingestion.S3.SecretRef.Name
		envVars = append(envVars,
			corev1.EnvVar{
				Name: "AWS_ACCESS_KEY_ID",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "AWS_ACCESS_KEY_ID",
					},
				},
			},
			corev1.EnvVar{
				Name: "AWS_SECRET_ACCESS_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "AWS_SECRET_ACCESS_KEY",
					},
				},
			},
		)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: kb.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			// BackoffLimit 0: a failing ingestion pod must not be silently
			// restarted inside the Job — each restart re-bursts embedding
			// requests against the provider with a fresh rate limiter. The
			// controller owns retry policy (exponential backoff, quota-aware
			// circuit breaker) instead.
			BackoffLimit: new(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: ingestServiceAccountName,
					Containers: []corev1.Container{
						{
							Name:         "ingester",
							Image:        r.mcpIngesterImageOrDefault(),
							Command:      []string{"/mcp-ingester"},
							Env:          envVars,
							VolumeMounts: volumeMounts,
							// Surface the ingester's final log lines (including the
							// QUOTA_EXHAUSTED_DAILY marker) as the pod termination
							// message so the controller can classify failures.
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
					Volumes: volumes,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(int64(1000)),
						FSGroup:      new(int64(1000)),
					},
				},
			},
		},
	}
}

// buildIngestConfig builds the JSON config for the mcp-ingester binary.
func (r *KnowledgeBaseReconciler) buildIngestConfig(
	ctx context.Context, //nolint:unparam

	kb *agentorcav1alpha1.KnowledgeBase,
	server *agentorcav1alpha1.MCPServer,
	tool *agentorcav1alpha1.Tool,
	mp *agentorcav1alpha1.ModelProvider,
	src *agentorcav1alpha1.MCPIngestionSource,
) (*mcpIngestConfig, error) { //nolint:unparam

	embeddingEndpoint, embeddingModel, rpm, tpm := embeddingConfig(mp)

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

		EmbeddingEndpoint:  embeddingEndpoint,
		EmbeddingModel:     embeddingModel,
		EmbeddingKeyFile:   fmt.Sprintf("/etc/embed-key/%s", mp.Spec.CredentialsRef.Key),
		EmbeddingDims:      dims,
		EmbeddingDocPrompt: mp.Spec.DocPrompt,
		EmbeddingRPM:       rpm,
		EmbeddingTPM:       tpm,
		ChunkSize:          kb.Spec.Embedding.ChunkSize,
		ChunkOverlap:       kb.Spec.Embedding.ChunkOverlap,

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
	kb *agentorcav1alpha1.KnowledgeBase,
	tool *agentorcav1alpha1.Tool,
	src *agentorcav1alpha1.MCPIngestionSource,
	mp *agentorcav1alpha1.ModelProvider,
	jobName, configCMName string,
) *batchv1.Job {
	labels := map[string]string{
		labelKBMCPIngest:               kb.Name,
		"app.kubernetes.io/name":       "mcp-ingester",
		"app.kubernetes.io/managed-by": "agentorca",
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
			// BackoffLimit 0: the controller owns retry policy (exponential
			// backoff, quota-aware circuit breaker); silent in-Job pod restarts
			// would re-burst embedding requests with a fresh rate limiter.
			BackoffLimit: new(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: ingestServiceAccountName,
					Containers: []corev1.Container{
						{
							Name:         "ingester",
							Image:        r.mcpIngesterImageOrDefault(),
							Command:      []string{"/mcp-ingester"},
							Env:          envVars,
							VolumeMounts: volumeMounts,
							// Surface final log lines (incl. QUOTA_EXHAUSTED_DAILY)
							// as the termination message for failure classification.
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
							Resources:                resources,
						},
					},
					Volumes: volumes,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(int64(1000)),
						FSGroup:      new(int64(1000)),
					},
				},
			},
		},
	}

	return job
}

// findMCPTool returns the first child Tool CR for the given MCPServer.
func (r *KnowledgeBaseReconciler) findMCPTool(ctx context.Context, server *agentorcav1alpha1.MCPServer) (*agentorcav1alpha1.Tool, error) {
	var tools agentorcav1alpha1.ToolList
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
func hasControllerAccess(server *agentorcav1alpha1.MCPServer) bool {
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

// sourcesIngestJobName returns a DNS-safe Job name for a KB's static-source
// (ConfigMap/URL/S3) ingestion Job.
func sourcesIngestJobName(kbName string) string {
	safe := strings.Trim(dnsUnsafe.ReplaceAllString(strings.ToLower(kbName), "-"), "-")
	name := "kb-src-" + safe
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// embeddingConfig returns the embedding endpoint, model, and any operator-
// configured rpm/tpm budgets for a ModelProvider. Zero rpm/tpm values mean
// "use the built-in provider presets".
func embeddingConfig(mp *agentorcav1alpha1.ModelProvider) (endpoint, model string, rpm, tpm int) {
	endpoint = mp.Spec.BaseURL
	if endpoint == "" {
		endpoint = embeddingBaseURL(mp.Spec.LiteLLMModel)
	}
	model = mp.Spec.LiteLLMModel
	if idx := strings.Index(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	return endpoint, model, mp.Spec.Constraints.RPM, mp.Spec.Constraints.TPM
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
	server := obj.(*agentorcav1alpha1.MCPServer)

	var kbList agentorcav1alpha1.KnowledgeBaseList
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
